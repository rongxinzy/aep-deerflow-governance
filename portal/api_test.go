package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// apiTestPortal wires a portal with a fake k8s client and a mocked DB.
type apiTestPortal struct {
	*portal
	dbmock sqlmock.Sqlmock
}

func newAPIHarness(t *testing.T, objects ...runtime.Object) *apiTestPortal {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, schemeAddK8s(scheme))
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	p := &portal{
		cfg: config{
			namespace:    "digital-employees",
			policyMode:   "self-service",
			adminUsers:   map[string]bool{"admin-user": true},
			quotaPerUser: 2,
		},
		db:       db,
		k8s:      fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).WithStatusSubresource(&dev1alpha1.DigitalEmployee{}).Build(),
		aep:      &aepAuthenticator{},
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: newSessionStore(time.Hour),
	}
	return &apiTestPortal{portal: p, dbmock: mock}
}

func readyCR(name, owner string) *dev1alpha1.DigitalEmployee {
	return &dev1alpha1.DigitalEmployee{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "digital-employees"},
		Spec:       dev1alpha1.DigitalEmployeeSpec{Owner: owner, DisplayName: name},
		Status:     dev1alpha1.DigitalEmployeeStatus{Phase: dev1alpha1.PhaseReady},
	}
}

func memkey(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "de-" + name + "-memkey", Namespace: "digital-employees"},
		Data:       map[string][]byte{"DEER_FLOW_INTERNAL_AUTH_TOKEN": []byte("tok")},
	}
}

func adminIdentity() identity { return identity{UserID: "admin-user", Roles: []string{"admin"}} }

func TestAPICRUDHandlers(t *testing.T) {
	t.Run("get employee returns the summary for the owner", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("sales-helper", "admin-user"))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/employees/sales-helper", nil)
		req.SetPathValue("name", "sales-helper")
		w := httptest.NewRecorder()
		h.getEmployee(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		var out struct {
			Employee map[string]any `json:"employee"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		assert.Equal(t, "sales-helper", out.Employee["name"])
	})

	t.Run("get employee refuses non-owner", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("sales-helper", "someone-else"))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/employees/sales-helper", nil)
		req.SetPathValue("name", "sales-helper")
		w := httptest.NewRecorder()
		h.getEmployee(w, req, adminIdentity())
		// admin bypasses; use a plain identity instead
		plain := identity{UserID: "user-x"}
		w = httptest.NewRecorder()
		h.getEmployee(w, req, plain)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("update employee patches displayName and team", func(t *testing.T) {
		h := newApplyHarness(t)
		target := readyCR("sales-helper", "u-owner")
		target.Spec.Team = "sales-dept"
		require.NoError(t, h.k8s.Create(context.Background(), target))
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/employees/sales-helper",
			strings.NewReader(`{"displayName":"销售专员","team":"rd-dept"}`))
		req.SetPathValue("name", "sales-helper")
		w := httptest.NewRecorder()
		h.updateEmployee(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var item dev1alpha1.DigitalEmployee
		require.NoError(t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: "digital-employees", Name: "sales-helper"}, &item))
		assert.Equal(t, "销售专员", item.Spec.DisplayName)
		assert.Equal(t, "rd-dept", item.Spec.Team)
	})

	t.Run("delete employee removes the CR", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("old-helper", "admin-user"))
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/employees/old-helper", nil)
		req.SetPathValue("name", "old-helper")
		w := httptest.NewRecorder()
		h.deleteEmployee(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var item dev1alpha1.DigitalEmployee
		err := h.k8s.Get(context.Background(), client.ObjectKey{Namespace: "digital-employees", Name: "old-helper"}, &item)
		assert.True(t, strings.Contains(err.Error(), "not found"))
	})

	t.Run("apply creates directly in direct mode", func(t *testing.T) {
		h := newApplyHarness(t)
		h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
			strings.NewReader(`{"name":"new-helper","displayName":"新员工","description":"整理资料","team":"rd-dept","visibility":{"mode":"restricted","teams":["rd-dept"]}}`))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, adminIdentity())
		assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		var item dev1alpha1.DigitalEmployee
		require.NoError(t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: "digital-employees", Name: "new-helper"}, &item))
		assert.Equal(t, "rd-dept", item.Spec.Team)
	})

	t.Run("apply rejects malformed names", func(t *testing.T) {
		h := newAPIHarness(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
			strings.NewReader(`{"name":"Bad Name!"}`))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, adminIdentity())
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("apply conflicts with an existing employee", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("dup-helper", "admin-user"))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
			strings.NewReader(`{"name":"dup-helper"}`))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, adminIdentity())
		assert.Equal(t, http.StatusConflict, w.Code)
	})

	t.Run("apply parks a request in approval mode", func(t *testing.T) {
		h := newApplyHarness(t)
		h.cfg.policyMode = "approval"
		h.dbmock.ExpectQuery(`SELECT id FROM requests WHERE employee_name=`).
			WithArgs("parked-helper").WillReturnError(errNoRowsForMock)
		h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
			strings.NewReader(`{"name":"parked-helper","description":"整理资料","team":"rd-dept","visibility":{"mode":"restricted","teams":["rd-dept"]}}`))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, identity{UserID: "user-x", Kind: "human"})
		assert.Equal(t, http.StatusAccepted, w.Code)
	})

	t.Run("list employees filters by team for non-admins", func(t *testing.T) {
		inTeam := readyCR("in-helper", "owner-a")
		inTeam.Spec.Team = "rd-dept"
		outTeam := readyCR("out-helper", "owner-b")
		outTeam.Spec.Team = "sales-dept"
		h := newAPIHarness(t, inTeam, outTeam)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/employees?team=rd-dept", nil)
		w := httptest.NewRecorder()
		member := identity{UserID: "user-x", Teams: []string{"rd-dept"}}
		h.listEmployees(w, req, member)
		assert.Equal(t, http.StatusOK, w.Code)
		body := w.Body.String()
		assert.Contains(t, body, "in-helper")
		assert.NotContains(t, body, "out-helper")
	})

	t.Run("decideRequest rejects with reason", func(t *testing.T) {
		h := newAPIHarness(t)
		rows := sqlmock.NewRows([]string{"employee_name", "spec_json"}).AddRow("parked-helper", `{"metadata":{"name":"parked-helper"}}`)
		h.dbmock.ExpectQuery(`SELECT employee_name, spec_json FROM requests`).WithArgs("req-1").WillReturnRows(rows)
		h.dbmock.ExpectExec(`UPDATE requests SET state=`).WillReturnResult(sqlmock.NewResult(0, 1))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/requests/req-1/reject",
			strings.NewReader(`{"reason":"容量不足"}`))
		req.SetPathValue("id", "req-1")
		w := httptest.NewRecorder()
		h.decideRequest("rejected")(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("listRequests scopes non-admins to their own rows", func(t *testing.T) {
		h := newAPIHarness(t)
		rows := sqlmock.NewRows([]string{"id", "employee_name", "owner_id", "owner_name", "display_name", "state", "decision_reason", "created_at"}).
			AddRow("req-1", "helper", "user-x", "User X", "助手", "pending", "", time.Now())
		h.dbmock.ExpectQuery(`SELECT id, employee_name`).WithArgs("pending").WillReturnRows(rows)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/requests", nil)
		w := httptest.NewRecorder()
		h.listRequests(w, req, identity{UserID: "user-x"})
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "helper")
	})
}

var errNoRowsForMock = errMockNoRows{}

type errMockNoRows struct{}

func (errMockNoRows) Error() string { return "sql: no rows in result set" }

func schemeAddK8s(scheme *runtime.Scheme) error {
	return k8sscheme.AddToScheme(scheme)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func jsonUnmarshal(raw string, out any) error { return json.Unmarshal([]byte(raw), out) }

func objectKey(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: "digital-employees", Name: name}
}

// IsAdminForTest adapts identity.isAdmin to the boolean-only evaluatePolicy
// signature without changing production code.
func (i identity) IsAdminForTest() bool {
	return i.isAdmin(config{adminUsers: map[string]bool{"admin-user": true}, adminRoles: map[string]bool{"admin": true}})
}

func TestApplyWithModelSelection(t *testing.T) {
	apply := func(t *testing.T, models string) (*httptest.ResponseRecorder, *dev1alpha1.DigitalEmployee) {
		h := newApplyHarness(t)
		h.cfg.gatewayBaseURL = "http://higress-gateway.higress-system.svc.cluster.local"
		h.cfg.modelKeyEnv = "BIGMODEL_API_KEY"
		h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
			strings.NewReader(`{"name":"model-helper","displayName":"模型员工","description":"整理资料","team":"rd-dept","models":`+models+`,"visibility":{"mode":"restricted","teams":["rd-dept"]}}`))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, identity{Token: "admin-token", UserID: "admin-user", DisplayName: "Admin", Kind: "human"})
		if w.Code != http.StatusCreated {
			return w, nil
		}
		item := &dev1alpha1.DigitalEmployee{}
		require.NoError(t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: "digital-employees", Name: "model-helper"}, item))
		return w, item
	}

	t.Run("derives runtime routes from the catalog", func(t *testing.T) {
		_, item := apply(t, `["bench-anthropic","bench-qwen"]`)
		require.NotNil(t, item)
		require.Len(t, item.Spec.Runtime.Models, 2)
		first := item.Spec.Runtime.Models[0]
		assert.Equal(t, "bench-anthropic", first.Name)
		assert.Equal(t, "anthropic", first.Provider)
		assert.Equal(t, "glm-5.3-flash", first.NativeModel)
		assert.Equal(t, "http://higress-gateway.higress-system.svc.cluster.local/bench-anthropic", first.BaseURL)
		assert.Equal(t, "BIGMODEL_API_KEY", first.ApiKeyEnv)
		second := item.Spec.Runtime.Models[1]
		assert.Equal(t, "openai", second.Provider)
		assert.Equal(t, "http://172.18.5.123:8000/v1", second.BaseURL)
		assert.Equal(t, "Qwen3.6-35B-A3B", second.NativeModel)
		assert.Empty(t, item.Spec.Runtime.Model, "legacy singular fields stay empty when models are explicit")
	})

	t.Run("rejects unknown or disabled models", func(t *testing.T) {
		w, _ := apply(t, `["missing-model"]`)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "not an enabled gateway model")
		w, _ = apply(t, `["disabled-model"]`)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("deduplicates repeated ids", func(t *testing.T) {
		_, item := apply(t, `["bench-qwen","bench-qwen"]`)
		require.NotNil(t, item)
		assert.Len(t, item.Spec.Runtime.Models, 1)
	})
}

func TestRenderSpecDefaultsToPostgresConversations(t *testing.T) {
	p := &portal{cfg: config{namespace: "digital-employees"}}
	spec := p.renderSpec(applyRequest{Name: "probe-helper", DisplayName: "Probe"}, applyContext{OwnerID: "u1", OwnerName: "U"}, nil)
	assert.Equal(t, "postgres", spec.Spec.ConversationStore.Backend)
	assert.Empty(t, spec.Spec.ConversationStore.Database, "portal employees take the per-employee database default")
}

// newApplyHarness wires a portal against fake AEP + WeKnora upstreams and
// the platform Secret holding their credentials, so the full apply
// validation path runs offline. The fake AEP serves the admin catalogs the
// extended create flow validates against; the fake WeKnora answers the
// tenant-key knowledge-base listing.
func newApplyHarness(t *testing.T) *apiTestPortal {
	t.Helper()
	aep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/aep/v1/auth/password/login":
			_, _ = w.Write([]byte(`{"accessToken":"admin-token"}`))
		case strings.HasPrefix(r.URL.Path, "/aep/v1/admin/users/"):
			// Precise per-id lookups (the directory paginates at 200, so
			// validation must never rely on listings).
			switch strings.TrimPrefix(r.URL.Path, "/aep/v1/admin/users/") {
			case "u-owner":
				_, _ = w.Write([]byte(`{"id":"u-owner","username":"lisi","displayName":"李四","status":"active","kind":"human"}`))
			case "u-x":
				_, _ = w.Write([]byte(`{"id":"u-x","username":"userx","displayName":"用户甲","status":"active","kind":"human"}`))
			case "u-off":
				_, _ = w.Write([]byte(`{"id":"u-off","username":"off","displayName":"停用者","status":"disabled","kind":"human"}`))
			case "u-agent":
				_, _ = w.Write([]byte(`{"id":"u-agent","username":"bot","displayName":"机器人","status":"active","kind":"agent"}`))
			default:
				http.NotFound(w, r)
			}
		case strings.HasPrefix(r.URL.Path, "/aep/v1/admin/teams/"):
			switch strings.TrimPrefix(r.URL.Path, "/aep/v1/admin/teams/") {
			case "boom": // simulates a catalog outage for one lookup
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"upstream down"}`))
				return
			case "rd-dept":
				_, _ = w.Write([]byte(`{"id":"rd-dept","name":"研发部"}`))
			case "sales-dept":
				_, _ = w.Write([]byte(`{"id":"sales-dept","name":"销售部"}`))
			default:
				http.NotFound(w, r)
			}
		case r.URL.Path == "/aep/v1/admin/teams":
			_, _ = w.Write([]byte(`{"teams":[{"id":"rd-dept","name":"研发部"},{"id":"sales-dept","name":"销售部"}]}`))
		case r.URL.Path == "/aep/v1/admin/users":
			_, _ = w.Write([]byte(`{"items":[
				{"id":"u-owner","username":"lisi","displayName":"李四","status":"active","kind":"human"},
				{"id":"u-x","username":"userx","displayName":"用户甲","status":"active","kind":"human"},
				{"id":"u-off","username":"off","displayName":"停用者","status":"disabled","kind":"human"},
				{"id":"u-agent","username":"bot","displayName":"机器人","status":"active","kind":"agent"}]}`))
		case r.URL.Path == "/aep/v1/admin/skills":
			_, _ = w.Write([]byte(`{"skills":[
				{"id":"report-writer","name":"报告撰写","state":"active","versions":[{"version":"1.0.0","state":"published"},{"version":"1.2.0","state":"published"},{"version":"2.0.0","state":"draft"}]},
				{"id":"retired","name":"退役技能","state":"withdrawn","versions":[{"version":"1.0.0","state":"published"}]}]}`))
		case r.URL.Path == "/aep/v1/admin/models":
			// Only the portal admin token may read the catalog — regular
			// applicants exercise the admin-token fallback.
			if r.Header.Get("Authorization") != "Bearer admin-token" {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"code":"ACCESS_DENIED"}`))
				return
			}
			_, _ = w.Write([]byte(`{"models":[
				{"id":"bench-anthropic","sourceType":"gateway","protocol":"anthropic","endpoint":"https://open.bigmodel.cn/api/anthropic","upstreamModel":"glm-5.3-flash","enabled":true},
				{"id":"bench-qwen","sourceType":"gateway","protocol":"openai-compatible","endpoint":"http://172.18.5.123:8000/v1","upstreamModel":"Qwen3.6-35B-A3B","enabled":true},
				{"id":"disabled-model","sourceType":"gateway","protocol":"anthropic","endpoint":"https://x.example","upstreamModel":"x","enabled":false}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(aep.Close)
	weknora := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/knowledge-bases", r.URL.Path)
		assert.Equal(t, "wk-key", r.Header.Get("X-API-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"kb-1","name":"销售知识库"},{"id":"kb-2","name":"产品知识库"}]}`))
	}))
	t.Cleanup(weknora.Close)
	h := newAPIHarness(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "de-platform-creds", Namespace: "digital-employees"},
		Data: map[string][]byte{
			"AEP_ADMIN_USER":     []byte("admin"),
			"AEP_ADMIN_PASSWORD": []byte("pw"),
			"WEKNORA_API_KEY":    []byte("wk-key"),
		},
	})
	h.cfg.aepBaseURL = aep.URL
	h.cfg.knowledgeAPIURL = weknora.URL
	h.cfg.credentialsSecret = "de-platform-creds"
	h.cfg.model = "bench-glm"
	return h
}

func TestApplyExtendedFields(t *testing.T) {
	apply := func(t *testing.T, h *apiTestPortal, ident identity, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, ident)
		return w
	}

	t.Run("full payload lands on the created CR and summary", func(t *testing.T) {
		h := newApplyHarness(t)
		h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		w := apply(t, h, adminIdentity(), `{
			"name":"full-helper","displayName":"全字段员工","description":"整理销售资料并辅助生成周报。",
			"team":"sales-dept","models":["bench-qwen"],"owner":"u-owner",
			"knowledgeBases":["kb-1","kb-2"],"skills":[{"id":"report-writer","version":"1.2.0"}],
			"visibility":{"mode":"restricted","teams":["sales-dept"],"users":["u-x"]}}`)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		item := &dev1alpha1.DigitalEmployee{}
		require.NoError(t, h.k8s.Get(context.Background(), objectKey("full-helper"), item))
		assert.Equal(t, "整理销售资料并辅助生成周报。", item.Spec.Description)
		assert.Equal(t, "u-owner", item.Spec.Owner)
		assert.Equal(t, "李四", item.Spec.OwnerName)
		assert.Equal(t, "u-owner", item.Spec.Governance.DefaultAEPUser, "the employee acts with the designated owner's scope")
		require.Len(t, item.Spec.Knowledge.Bases, 2)
		assert.Equal(t, "kb-1", item.Spec.Knowledge.Bases[0].ID)
		assert.Equal(t, "销售知识库", item.Spec.Knowledge.Bases[0].Name)
		require.Len(t, item.Spec.Skills, 1)
		assert.Equal(t, "1.2.0", item.Spec.Skills[0].Version)
		require.NotNil(t, item.Spec.Visibility)
		assert.Equal(t, "restricted", item.Spec.Visibility.Mode)
		require.Len(t, item.Spec.Visibility.Teams, 1)
		assert.Equal(t, "销售部", item.Spec.Visibility.Teams[0].Name)
		var out struct {
			Employee map[string]any `json:"employee"`
		}
		require.NoError(t, jsonUnmarshal(w.Body.String(), &out))
		assert.Equal(t, "bench-qwen", out.Employee["model"], "first explicit model is the default")
		assert.NotContains(t, out.Employee, "knowledgeBases-for-typo")
		assert.NotNil(t, out.Employee["knowledgeBases"])
		assert.NotNil(t, out.Employee["visibility"])
		assert.NotNil(t, out.Employee["skills"])
	})

	t.Run("empty knowledge list is an explicit deny-all policy", func(t *testing.T) {
		h := newApplyHarness(t)
		h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		w := apply(t, h, adminIdentity(), `{"name":"deny-helper","displayName":"无知识库","description":"整理资料","team":"rd-dept","knowledgeBases":[],"visibility":{"mode":"restricted","teams":["rd-dept"]}}`)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		item := &dev1alpha1.DigitalEmployee{}
		require.NoError(t, h.k8s.Get(context.Background(), objectKey("deny-helper"), item))
		require.NotNil(t, item.Spec.Knowledge.Bases)
		assert.Empty(t, item.Spec.Knowledge.Bases)
	})

	t.Run("empty knowledge list does not require WeKnora to be reachable", func(t *testing.T) {
		h := newApplyHarness(t)
		h.cfg.knowledgeAPIURL = "http://127.0.0.1:1" // unreachable
		h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		w := apply(t, h, adminIdentity(), `{"name":"nowek-helper","displayName":"无知识服务","description":"整理资料","team":"rd-dept","knowledgeBases":[],"visibility":{"mode":"restricted","teams":["rd-dept"]}}`)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		item := &dev1alpha1.DigitalEmployee{}
		require.NoError(t, h.k8s.Get(context.Background(), objectKey("nowek-helper"), item))
		require.NotNil(t, item.Spec.Knowledge.Bases, "explicit deny-all survives without a WeKnora round-trip")
		assert.Empty(t, item.Spec.Knowledge.Bases)
	})

	t.Run("absent knowledge list keeps the legacy unrestricted policy", func(t *testing.T) {
		h := newApplyHarness(t)
		h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		w := apply(t, h, adminIdentity(), `{"name":"legacy-helper","displayName":"遗留","description":"整理资料","team":"rd-dept","visibility":{"mode":"restricted","teams":["rd-dept"]}}`)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		item := &dev1alpha1.DigitalEmployee{}
		require.NoError(t, h.k8s.Get(context.Background(), objectKey("legacy-helper"), item))
		assert.Nil(t, item.Spec.Knowledge.Bases)
		var out struct {
			Employee map[string]any `json:"employee"`
		}
		require.NoError(t, jsonUnmarshal(w.Body.String(), &out))
		assert.Nil(t, out.Employee["knowledgeBases"])
	})

	t.Run("owner override by a non-admin is refused", func(t *testing.T) {
		h := newApplyHarness(t)
		w := apply(t, h, identity{UserID: "user-x", Kind: "human"}, `{"name":"steal-helper","displayName":"越权","description":"整理资料","team":"rd-dept","owner":"u-owner","visibility":{"mode":"restricted","teams":["rd-dept"]}}`)
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Contains(t, w.Body.String(), "管理员")
	})

	t.Run("visibility all needs admin or approval mode", func(t *testing.T) {
		h := newApplyHarness(t)
		w := apply(t, h, identity{UserID: "user-x", Kind: "human"}, `{"name":"all-helper","displayName":"全员","description":"整理资料","team":"rd-dept","visibility":{"mode":"all"}}`)
		assert.Equal(t, http.StatusForbidden, w.Code)

		h2 := newApplyHarness(t)
		h2.cfg.policyMode = "approval"
		h2.dbmock.ExpectQuery(`SELECT id FROM requests WHERE employee_name=`).WithArgs("all-helper").WillReturnError(errNoRowsForMock)
		h2.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		w = apply(t, h2, identity{UserID: "user-x", Kind: "human"}, `{"name":"all-helper","displayName":"全员","description":"整理资料","team":"rd-dept","visibility":{"mode":"all"}}`)
		assert.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	})

	t.Run("violations report every failed release condition and reference", func(t *testing.T) {
		h := newApplyHarness(t)
		w := apply(t, h, adminIdentity(), `{"name":"bad-helper","displayName":"缺项","team":"nope-dept","knowledgeBases":["kb-x"],"skills":[{"id":"report-writer","version":"2.0.0"},{"id":"retired","version":"1.0.0"}],"visibility":{"mode":"restricted","teams":["rd-dept"],"users":["ghost"]}}`)
		require.Equal(t, http.StatusBadRequest, w.Code)
		var out struct {
			Violations []violation `json:"violations"`
		}
		require.NoError(t, jsonUnmarshal(w.Body.String(), &out))
		fields := map[string]bool{}
		for _, item := range out.Violations {
			fields[item.Field] = true
		}
		for _, expected := range []string{"description", "team", "knowledgeBases[0]", "skills[0].id", "skills[1].id", "visibility.users[0]"} {
			assert.True(t, fields[expected], "expected violation for %s, got %+v", expected, out.Violations)
		}
		var item dev1alpha1.DigitalEmployee
		err := h.k8s.Get(context.Background(), objectKey("bad-helper"), &item)
		assert.True(t, strings.Contains(err.Error(), "not found"), "no CR may be created on violations")
	})

	t.Run("upstream unavailability fails closed without creating", func(t *testing.T) {
		h := newApplyHarness(t)
		h.cfg.aepBaseURL = "http://127.0.0.1:1" // unreachable
		w := apply(t, h, adminIdentity(), `{"name":"down-helper","displayName":"上游挂","description":"整理资料","team":"rd-dept","visibility":{"mode":"restricted","teams":["rd-dept"]}}`)
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.Contains(t, w.Body.String(), "暂不可用")
		var item dev1alpha1.DigitalEmployee
		err := h.k8s.Get(context.Background(), objectKey("down-helper"), &item)
		assert.True(t, strings.Contains(err.Error(), "not found"))
	})

	t.Run("empty skill version auto-pins the latest published", func(t *testing.T) {
		h := newApplyHarness(t)
		h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		w := apply(t, h, adminIdentity(), `{"name":"autoskill-helper","displayName":"自动版本","description":"整理资料","team":"rd-dept","skills":[{"id":"report-writer"}],"visibility":{"mode":"restricted","teams":["rd-dept"]}}`)
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		item := &dev1alpha1.DigitalEmployee{}
		require.NoError(t, h.k8s.Get(context.Background(), objectKey("autoskill-helper"), item))
		require.Len(t, item.Spec.Skills, 1)
		assert.Equal(t, "1.2.0", item.Spec.Skills[0].Version, "draft 2.0.0 must not be picked")
	})
}

func TestRequestDecisionCorruptSpecFails(t *testing.T) {
	h := newAPIHarness(t)
	rows := sqlmock.NewRows([]string{"employee_name", "spec_json"}).AddRow("broken", `{"metadata":`)
	h.dbmock.ExpectQuery(`SELECT employee_name, spec_json FROM requests`).WithArgs("req-x").WillReturnRows(rows)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/requests/req-x/approve", nil)
	req.SetPathValue("id", "req-x")
	w := httptest.NewRecorder()
	h.decideRequest("approved")(w, req, adminIdentity())
	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
}

func TestDeleteAndGetEmployeeGuards(t *testing.T) {
	h := newAPIHarness(t, readyCR("guard-helper", "owner-a"))
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/employees/guard-helper", nil)
	req.SetPathValue("name", "guard-helper")
	w := httptest.NewRecorder()
	h.deleteEmployee(w, req, identity{UserID: "stranger"})
	assert.Equal(t, http.StatusForbidden, w.Code)

	missing := httptest.NewRequest(http.MethodGet, "/api/v1/employees/ghost", nil)
	missing.SetPathValue("name", "ghost")
	w = httptest.NewRecorder()
	h.getEmployee(w, missing, adminIdentity())
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestApplyModelCatalogAdminFallback(t *testing.T) {
	// A regular applicant (no models.read) submits an explicit model list:
	// the requester-token catalog read gets 403 and the portal must retry
	// with its admin token instead of failing the apply.
	h := newApplyHarness(t)
	h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
		strings.NewReader(`{"name":"fallback-helper","displayName":"回退","description":"整理资料","team":"rd-dept","models":["bench-qwen"],"visibility":{"mode":"restricted","teams":["rd-dept"]}}`))
	w := httptest.NewRecorder()
	h.applyForEmployee(w, req, identity{Token: "member-token", UserID: "u-x", DisplayName: "用户甲", Kind: "human"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	item := &dev1alpha1.DigitalEmployee{}
	require.NoError(t, h.k8s.Get(context.Background(), objectKey("fallback-helper"), item))
	require.Len(t, item.Spec.Runtime.Models, 1)
	assert.Equal(t, "bench-qwen", item.Spec.Runtime.Models[0].Name)
}

func TestApplyVisibilityAllRefusedForNonAdminInSelfService(t *testing.T) {
	h := newApplyHarness(t)
	h.cfg.policyMode = "self-service"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
		strings.NewReader(`{"name":"all-helper","displayName":"全员","description":"整理资料","team":"rd-dept","visibility":{"mode":"all"}}`))
	w := httptest.NewRecorder()
	h.applyForEmployee(w, req, identity{UserID: "u-x", Kind: "human"})
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestApplyFailsClosedWithoutAdminCredentials(t *testing.T) {
	h := newAPIHarness(t) // no platform secret → no admin token
	h.cfg.aepBaseURL = "http://unused.invalid"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
		strings.NewReader(`{"name":"nocreds-helper","displayName":"无凭证","description":"整理资料","team":"rd-dept","skills":[{"id":"any"}],"visibility":{"mode":"restricted","teams":["rd-dept"]}}`))
	w := httptest.NewRecorder()
	h.applyForEmployee(w, req, adminIdentity())
	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
}

func TestApplyFailsClosedWhenTeamLookupErrors(t *testing.T) {
	h := newApplyHarness(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
		strings.NewReader(`{"name":"boomblock-helper","displayName":"故障","description":"整理资料","team":"boom","visibility":{"mode":"restricted","teams":["rd-dept"]}}`))
	w := httptest.NewRecorder()
	h.applyForEmployee(w, req, adminIdentity())
	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	var item dev1alpha1.DigitalEmployee
	assert.Error(t, h.k8s.Get(context.Background(), objectKey("boomblock-helper"), &item), "fail closed: nothing may be created")
}

func TestGetEmployeeVisibility(t *testing.T) {
	employee := readyCR("scoped-helper", "owner-a")
	employee.Spec.Visibility = &dev1alpha1.VisibilitySpec{
		Mode:  "restricted",
		Teams: []dev1alpha1.SubjectRef{{ID: "rd-dept", Name: "研发部"}},
		Users: []dev1alpha1.SubjectRef{{ID: "u-x", Name: "用户甲"}},
	}
	h := newAPIHarness(t, employee)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/employees/scoped-helper", nil)
	req.SetPathValue("name", "scoped-helper")

	w := httptest.NewRecorder()
	h.getEmployee(w, req, identity{UserID: "u-x"})
	assert.Equal(t, http.StatusOK, w.Code, "listed user may read the detail view")

	w = httptest.NewRecorder()
	h.getEmployee(w, req, identity{UserID: "stranger"})
	assert.Equal(t, http.StatusForbidden, w.Code)

	member := identity{UserID: "stranger", Teams: []string{"rd-dept"}}
	w = httptest.NewRecorder()
	h.getEmployee(w, req, member)
	assert.Equal(t, http.StatusOK, w.Code, "visibility team members may read the detail view")
}
