package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// servicesStub stands in for OpenViking (health/accounts/sessions/search)
// and WeKnora (MCP probe + knowledge bases + API proxy).
type servicesStub struct {
	ov      *httptest.Server
	weknora *httptest.Server
}

func newServicesStubs(t *testing.T) *servicesStub {
	t.Helper()
	stub := &servicesStub{}
	stub.ov = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/v1/admin/accounts":
			_, _ = w.Write([]byte(`{"result":[{"account_id":"zhiyuan","user_count":3,"created_at":"2026-09-01"}]}`))
		case r.URL.Path == "/api/v1/sessions":
			_, _ = w.Write([]byte(`{"result":[{"mod_time":"2026-09-28T10:00:00"},{"mod_time":"2026-09-29T11:00:00"}]}`))
		case r.URL.Path == "/api/v1/search/find":
			_, _ = w.Write([]byte(`{"status":"ok","result":{"memories":[{"uri":"mem://a","score":0.9,"abstract":"alpha"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(stub.ov.Close)
	stub.weknora = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/login") {
			_, _ = w.Write([]byte(`{"token":"wk-token"}`))
			return
		}
		if r.URL.Path == "/api/v1/knowledge-bases" {
			_, _ = w.Write([]byte(`{"data":[{"id":"kb-1","name":"企业知识库","description":"docs","document_count":2,"created_at":"2026-10-01"}]}`))
			return
		}
		// MCP endpoint answers non-5xx for the reachability probe; the
		// generic path doubles as the knowledge API proxy target.
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(stub.weknora.Close)
	return stub
}

func newServicesHarness(t *testing.T, stub *servicesStub, objects ...runtime.Object) *apiTestPortal {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, k8sscheme.AddToScheme(scheme))
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	defaults := []runtime.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "de-platform-creds", Namespace: "digital-employees"},
			Data: map[string][]byte{
				"OPENVIKING_ROOT_API_KEY": []byte("ov-root"),
				"WEKNORA_API_KEY":         []byte("wk-key"),
				"WEKNORA_ADMIN_USER":      []byte("wk-admin"),
				"WEKNORA_ADMIN_PASSWORD":  []byte("wk-pass"),
			},
		},
	}
	ready := readyCR("sales-helper", "admin-user")
	ready.Status.MemoryUser = "sales-helper"
	defaults = append(defaults, ready)
	memkeySecret := memkey("sales-helper")
	memkeySecret.Data["OPENVIKING_API_KEY"] = []byte("user-key")
	defaults = append(defaults, memkeySecret)
	defaults = append(defaults, objects...)
	p := &portal{
		cfg: config{
			namespace:         "digital-employees",
			adminUsers:        map[string]bool{"admin-user": true},
			memoryServer:      stub.ov.URL,
			memoryAccount:     "zhiyuan",
			knowledgeMCPURL:   stub.weknora.URL + "/mcp",
			knowledgeAPIURL:   stub.weknora.URL,
			credentialsSecret: "de-platform-creds",
		},
		db:       db,
		k8s:      fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(defaults...).Build(),
		aep:      &aepAuthenticator{},
		logger:   discardLogger(),
		sessions: newSessionStore(0),
	}
	return &apiTestPortal{portal: p, dbmock: mock}
}

func TestServiceStatusHandlers(t *testing.T) {
	t.Run("memory status reports health, accounts, and employee activity", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		rows := sqlmock.NewRows([]string{"id"}).AddRow("run-1")
		h.dbmock.ExpectQuery(`SELECT count`).WillReturnRows(rows)
		rows2 := sqlmock.NewRows([]string{"cnt"}).AddRow(4)
		h.dbmock.ExpectQuery(`SELECT count`).WillReturnRows(rows2)

		w := httptest.NewRecorder()
		h.portal.handleMemoryStatus(w, httptest.NewRequest(http.MethodGet, "/api/v1/memory/status", nil), adminIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		body := w.Body.String()
		assert.Contains(t, body, `"healthy":true`)
		assert.Contains(t, body, "sales-helper")
	})

	t.Run("memory search returns ranked memories", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/search",
			strings.NewReader(`{"employee":"sales-helper","query":"报价"}`))
		w := httptest.NewRecorder()
		h.portal.handleMemorySearch(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "mem://a")
	})

	t.Run("memory search rejects missing fields", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/search", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		h.portal.handleMemorySearch(w, req, adminIdentity())
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("knowledge status reports uiURL, bases, and health", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/status", nil)
		req.Host = "console.example:30196"
		w := httptest.NewRecorder()
		h.portal.handleKnowledgeStatus(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		body := w.Body.String()
		assert.Contains(t, body, `"healthy":true`)
		assert.Contains(t, body, "http://console.example:30163")
		assert.Contains(t, body, "企业知识库")
	})

	t.Run("usage stats aggregates portal and k8s state", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		for _, q := range []string{
			`SELECT COUNT(*) FROM threads_meta`,
			`SELECT COUNT(*) FROM runs`,
			`SELECT count(*) FROM threads_meta WHERE created_at >= CURRENT_DATE`,
			`SELECT count(*) FROM runs WHERE created_at >= CURRENT_DATE`,
		} {
			h.dbmock.ExpectQuery(q).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		}
		rows := sqlmock.NewRows([]string{"dept", "cnt"}).AddRow("rd-dept", 3)
		h.dbmock.ExpectQuery(`SELECT COALESCE\(tenant_id`).WillReturnRows(rows)
		rows2 := sqlmock.NewRows([]string{"model", "cnt"}).AddRow("bench-glm", 7)
		h.dbmock.ExpectQuery(`SELECT COALESCE\(model_name`).WillReturnRows(rows2)

		w := httptest.NewRecorder()
		h.portal.handleUsageStats(w, httptest.NewRequest(http.MethodGet, "/api/v1/usage/stats", nil), adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "byDepartment")
	})

	t.Run("usage stats is admin-only", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		w := httptest.NewRecorder()
		h.portal.handleUsageStats(w, httptest.NewRequest(http.MethodGet, "/api/v1/usage/stats", nil), identity{UserID: "user-x"})
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

func TestKnowledgeProxy(t *testing.T) {
	t.Run("proxies the subtree with server-side auth", func(t *testing.T) {
		var seenPath, seenAuth string
		sawLogin := false
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seenPath = r.URL.Path
			seenAuth = r.Header.Get("Authorization")
			if strings.HasSuffix(r.URL.Path, "/auth/login") {
				sawLogin = true
				_, _ = w.Write([]byte(`{"token":"wk-token"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer target.Close()
		scheme := runtime.NewScheme()
		require.NoError(t, k8sscheme.AddToScheme(scheme))
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { _ = mock.ExpectationsWereMet() })
		p := &portal{
			cfg: config{
				namespace:         "digital-employees",
				adminUsers:        map[string]bool{"admin-user": true},
				knowledgeAPIURL:   target.URL,
				credentialsSecret: "de-platform-creds",
			},
			db: db,
			k8s: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "de-platform-creds", Namespace: "digital-employees"},
				Data:       map[string][]byte{"WEKNORA_ADMIN_USER": []byte("wk-admin"), "WEKNORA_ADMIN_PASSWORD": []byte("wk-pass")},
			}).Build(),
			aep:      &aepAuthenticator{},
			logger:   discardLogger(),
			sessions: newSessionStore(0),
		}
		h := &apiTestPortal{portal: p, dbmock: mock}
		// Login + knowledge bases proxied calls.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/proxy/knowledge-bases", nil)
		req.SetPathValue("rest", "knowledge-bases")
		w := httptest.NewRecorder()
		h.portal.handleKnowledgeProxy(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.True(t, sawLogin, "proxy must authenticate server-side first")
		assert.Contains(t, seenPath, "knowledge-bases")
		assert.Equal(t, "Bearer wk-token", seenAuth)
	})
}
