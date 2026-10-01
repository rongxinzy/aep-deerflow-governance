package main

import (
	"context"
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

// departmentStubUpstreams stands in for AEP (team admin API), OpenViking
// (account provisioning), and WeKnora (tenant API). Each handler counts its
// calls so the orchestration can be asserted end-to-end.
type departmentStubUpstreams struct {
	aep      *httptest.Server
	ov       *httptest.Server
	weknora  *httptest.Server
	aepCalls int
	ovCalls  int
	wkCalls  int
}

func newDepartmentStubs(t *testing.T) *departmentStubUpstreams {
	t.Helper()
	stubs := &departmentStubUpstreams{}
	stubs.aep = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stubs.aepCalls++
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/password/login"):
			_, _ = w.Write([]byte(`{"accessToken":"admin-token"}`))
		case r.URL.Path == "/aep/v1/admin/teams" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":"rd-dept"}`))
		case r.URL.Path == "/aep/v1/admin/teams" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"teams":[{"id":"rd-dept","name":"研发部"}]}`))
		case strings.HasPrefix(r.URL.Path, "/aep/v1/admin/teams/") && r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/aep/v1/admin/teams/") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/aep/v1/user/me":
			_, _ = w.Write([]byte(`{"user":{"id":"admin-user","teamIds":["rd-dept"]}}`))
		case r.URL.Path == "/aep/v1/admin/users":
			_, _ = w.Write([]byte(`{"items":[{"id":"u1","username":"zhangsan","displayName":"张三","teamIds":["rd-dept"],"roleIds":["member"]},{"id":"u2","username":"lisi","displayName":"李四","teamIds":[],"roleIds":[]}]}`))
		case strings.HasSuffix(r.URL.Path, "/rbac"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(stubs.aep.Close)
	stubs.ov = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stubs.ovCalls++
		if r.Header.Get("X-API-Key") != "ov-root-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(stubs.ov.Close)
	stubs.weknora = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stubs.wkCalls++
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/login"):
			_, _ = w.Write([]byte(`{"token":"wk-token"}`))
		case r.URL.Path == "/api/v1/tenants":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(stubs.weknora.Close)
	return stubs
}

func newDepartmentHarness(t *testing.T, stubs *departmentStubUpstreams) *apiTestPortal {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, k8sscheme.AddToScheme(scheme))
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "de-platform-creds", Namespace: "digital-employees"},
		Data: map[string][]byte{
			"OPENVIKING_ROOT_API_KEY": []byte("ov-root-key"),
			"AEP_ADMIN_USER":          []byte("admin"),
			"AEP_ADMIN_PASSWORD":      []byte("secret-pass"),
			"WEKNORA_ADMIN_USER":      []byte("wk-admin"),
			"WEKNORA_ADMIN_PASSWORD":  []byte("wk-pass"),
		},
	}
	p := &portal{
		cfg: config{
			namespace:         "digital-employees",
			policyMode:        "self-service",
			adminUsers:        map[string]bool{"admin-user": true},
			memoryServer:      stubs.ov.URL,
			memoryAccount:     "zhiyuan",
			knowledgeAPIURL:   stubs.weknora.URL,
			credentialsSecret: "de-platform-creds",
			aepDeploymentID:   "demo",
		},
		db:       db,
		k8s:      fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(secret).Build(),
		aep:      &aepAuthenticator{},
		logger:   discardLogger(),
		sessions: newSessionStore(0),
	}
	// Point the portal's AEP and probe client at the stubs.
	p.cfg.aepBaseURL = stubs.aep.URL
	return &apiTestPortal{portal: p, dbmock: mock}
}

func TestDepartmentOrchestration(t *testing.T) {
	t.Run("create provisions AEP team, OV account, and WeKnora tenant", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/departments",
			strings.NewReader(`{"id":"rd-dept","name":"研发部"}`))
		w := httptest.NewRecorder()
		h.portal.handleCreateDepartment(w, req, adminIdentity())

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var status departmentStatus
		require.NoError(t, jsonUnmarshal(w.Body.String(), &status))
		assert.Equal(t, "ready", status.Phase)
		assert.Equal(t, "rd-dept", status.AepTeamID)
		assert.Equal(t, "tenant-rd-dept", status.OvAccountID)
		assert.Equal(t, stubs.aep.URL, h.portal.cfg.aepBaseURL)
		assert.GreaterOrEqual(t, stubs.ovCalls, 1)
		assert.GreaterOrEqual(t, stubs.wkCalls, 1)
	})

	t.Run("create is admin-only", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/departments",
			strings.NewReader(`{"id":"rd","name":"x"}`))
		w := httptest.NewRecorder()
		h.portal.handleCreateDepartment(w, req, identity{UserID: "user-x"})
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("create reports partial when WeKnora fails", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		stubs.weknora.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/departments",
			strings.NewReader(`{"id":"rd-dept","name":"研发部"}`))
		w := httptest.NewRecorder()
		h.portal.handleCreateDepartment(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		var status departmentStatus
		require.NoError(t, jsonUnmarshal(w.Body.String(), &status))
		assert.Equal(t, "partial", status.Phase)
		assert.Contains(t, status.Message, "weknora")
	})

	t.Run("list returns the AEP teams", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/departments", nil)
		w := httptest.NewRecorder()
		h.portal.handleListDepartments(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "研发部")
	})

	t.Run("rename forwards to AEP", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/departments/rd-dept",
			strings.NewReader(`{"name":"平台研发部"}`))
		req.SetPathValue("id", "rd-dept")
		w := httptest.NewRecorder()
		h.portal.handleUpdateDepartment(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("delete removes the AEP team", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/departments/rd-dept", nil)
		req.SetPathValue("id", "rd-dept")
		w := httptest.NewRecorder()
		h.portal.handleDeleteDepartment(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("members list filters by team", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/departments/rd-dept/members", nil)
		req.SetPathValue("id", "rd-dept")
		w := httptest.NewRecorder()
		h.portal.handleDepartmentMembers(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "zhangsan")
	})

	t.Run("enrichTeams pulls teamIds from the user token", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		ident := identity{UserID: "u1", Token: "user-token"}
		h.portal.enrichTeams(context.Background(), &ident)
		assert.Equal(t, []string{"rd-dept"}, ident.Teams)
	})
}
