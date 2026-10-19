package main

import (
	"context"
	"io"
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
	// createdTeamBodies captures the AEP POST /admin/teams payloads so the
	// create specs can assert exactly what crossed the boundary.
	createdTeamBodies []string
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
			body, _ := io.ReadAll(r.Body)
			stubs.createdTeamBodies = append(stubs.createdTeamBodies, string(body))
			_, _ = w.Write([]byte(`{"id":"rd-dept"}`))
		case r.URL.Path == "/aep/v1/admin/teams" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"teams":[{"id":"eng","name":"研发中心"},{"id":"rd-dept","name":"研发部","parentId":"eng"}]}`))
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

		provisioningStore: newMemoryProvisioningStore(),
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
			strings.NewReader(`{"name":"研发部"}`))
		w := httptest.NewRecorder()
		h.handleCreateDepartment(w, req, adminIdentity())

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var status departmentStatus
		require.NoError(t, jsonUnmarshal(w.Body.String(), &status))
		assert.Equal(t, "ready", status.Phase)
		assert.Equal(t, "rd-dept", status.AepTeamID)
		assert.Equal(t, "tenant-rd-dept", status.OvAccountID)
		assert.Equal(t, stubs.aep.URL, h.cfg.aepBaseURL)
		assert.GreaterOrEqual(t, stubs.ovCalls, 1)
		assert.GreaterOrEqual(t, stubs.wkCalls, 1)
	})

	t.Run("create is admin-only", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/departments",
			strings.NewReader(`{"name":"x"}`))
		w := httptest.NewRecorder()
		h.handleCreateDepartment(w, req, identity{UserID: "user-x"})
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("create reports partial when WeKnora fails", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		stubs.weknora.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/departments",
			strings.NewReader(`{"name":"研发部"}`))
		w := httptest.NewRecorder()
		h.handleCreateDepartment(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		var status departmentStatus
		require.NoError(t, jsonUnmarshal(w.Body.String(), &status))
		assert.Equal(t, "partial", status.Phase)
		assert.Contains(t, status.Message, "weknora")
	})

	t.Run("create with parentId forwards the hierarchy link to AEP", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/departments",
			strings.NewReader(`{"name":"前端组","parentId":"rd-dept"}`))
		w := httptest.NewRecorder()
		h.handleCreateDepartment(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Len(t, stubs.createdTeamBodies, 1)
		// 精确断言跨界负载：parentId 必须原样到达 AEP（建队路径/深度在 AEP 侧定死）
		assert.JSONEq(t, `{"name":"前端组","parentId":"rd-dept"}`, stubs.createdTeamBodies[0])
	})

	t.Run("create without parentId stays a root team", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/departments",
			strings.NewReader(`{"name":"研发部"}`))
		w := httptest.NewRecorder()
		h.handleCreateDepartment(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Len(t, stubs.createdTeamBodies, 1)
		assert.JSONEq(t, `{"name":"研发部"}`, stubs.createdTeamBodies[0], "absent parent must not become a null key")
	})

	t.Run("AEP rejects the parent as 4xx surfaces 400 not 502", func(t *testing.T) {
		// 未知父部门是调用方错误：按 400 带细节返回，而不是网关故障的 502。
		stubs := &departmentStubUpstreams{
			aep: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/auth/password/login"):
					_, _ = w.Write([]byte(`{"accessToken":"admin-token"}`))
				case r.URL.Path == "/aep/v1/admin/teams" && r.Method == http.MethodPost:
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"title":"Not Found","detail":"parent team ghost-dept not found","status":404}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})),
			ov:      httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
			weknora: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
		}
		t.Cleanup(stubs.aep.Close)
		t.Cleanup(stubs.ov.Close)
		t.Cleanup(stubs.weknora.Close)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/departments",
			strings.NewReader(`{"name":"前端组","parentId":"ghost-dept"}`))
		w := httptest.NewRecorder()
		h.handleCreateDepartment(w, req, adminIdentity())
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "ghost-dept not found")
	})

	t.Run("list returns the AEP teams", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/departments", nil)
		w := httptest.NewRecorder()
		h.handleListDepartments(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"name":"研发部","parentId":"eng"`, "hierarchy link must survive the listing")
		assert.Contains(t, w.Body.String(), `"id":"eng","name":"研发中心"`)
	})

	t.Run("rename forwards to AEP", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/departments/rd-dept",
			strings.NewReader(`{"name":"平台研发部"}`))
		req.SetPathValue("id", "rd-dept")
		w := httptest.NewRecorder()
		h.handleUpdateDepartment(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("delete removes the AEP team", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/departments/rd-dept", nil)
		req.SetPathValue("id", "rd-dept")
		w := httptest.NewRecorder()
		h.handleDeleteDepartment(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	})

	t.Run("members list filters by team", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/departments/rd-dept/members", nil)
		req.SetPathValue("id", "rd-dept")
		w := httptest.NewRecorder()
		h.handleDepartmentMembers(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "zhangsan")
	})

	t.Run("enrichTeams pulls teamIds from the user token", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		ident := identity{UserID: "u1", Token: "user-token"}
		h.enrichTeams(context.Background(), &ident)
		assert.Equal(t, []string{"rd-dept"}, ident.Teams)
	})
}

func TestAdminTokenCacheTTLBelowAccessTokenLifetime(t *testing.T) {
	stubs := newDepartmentStubs(t)
	h := newDepartmentHarness(t, stubs)

	// adminTokenCache is package-global; clear it before and after so this
	// test is hermetic and cannot hand a stale token to other tests.
	resetCache := func() {
		adminTokenCache.mu.Lock()
		defer adminTokenCache.mu.Unlock()
		adminTokenCache.token = ""
		adminTokenCache.expires = time.Time{}
	}
	resetCache()
	t.Cleanup(resetCache)

	ctx := context.Background()
	before := stubs.aepCalls
	token := h.adminToken(ctx)
	require.Equal(t, "admin-token", token)
	require.Equal(t, before+1, stubs.aepCalls, "first call must log in")

	adminTokenCache.mu.Lock()
	remaining := time.Until(adminTokenCache.expires)
	adminTokenCache.mu.Unlock()
	// The cached lifetime must stay below the AEP access-token lifetime
	// (AEP_ACCESS_TTL, default 15m); anything at or above it reuses a token
	// the control plane has already expired. The upper bound distinguishes
	// the fixed 10m TTL from the previous 20m one.
	assert.LessOrEqual(t, remaining, 11*time.Minute, "cache TTL must stay below the 15m token lifetime")
	assert.Greater(t, remaining, 9*time.Minute, "cache TTL must not collapse roughly to zero")

	// A second call inside the TTL must reuse the cached token without a login.
	again := h.adminToken(ctx)
	assert.Equal(t, "admin-token", again)
	assert.Equal(t, before+1, stubs.aepCalls, "second call within the TTL must not re-login")
}

func TestEnrichTeamsFallsBackToAdminListing(t *testing.T) {
	// 滚动重启后恢复的会话：mint 时的 AEP token 已过期（/user/me 401），
	// 部门归属必须经 admin 用户列表自愈，而不是空着直到下次登录。
	stubs := &departmentStubUpstreams{
		aep: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/aep/v1/auth/password/login":
				_, _ = w.Write([]byte(`{"accessToken":"admin-token"}`))
			case "/aep/v1/user/me":
				w.WriteHeader(http.StatusUnauthorized)
			case "/aep/v1/admin/users/u9":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"u9","teamIds":["rd-dept"]}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})),
		ov:      httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
		weknora: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
	}
	h := newDepartmentHarness(t, stubs)
	ident := identity{UserID: "u9", Token: "expired-aep-token"}
	h.enrichTeams(context.Background(), &ident)
	assert.Equal(t, []string{"rd-dept"}, ident.Teams, "expired token must self-heal via the admin listing")
	assert.False(t, ident.TeamsFetchedAt.IsZero())
}

func TestEnrichTeamsPrefersUserToken(t *testing.T) {
	// 活会话的自身 token 路径优先：admin 列表不应被触碰。
	var adminHits int
	stubs := &departmentStubUpstreams{
		aep: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/aep/v1/user/me":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"user":{"id":"u9","displayName":"张三","kind":"human","teamIds":["sales-dept"]}}`))
			case "/aep/v1/admin/users":
				adminHits++
				w.WriteHeader(http.StatusForbidden)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})),
		ov:      httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
		weknora: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
	}
	h := newDepartmentHarness(t, stubs)
	ident := identity{UserID: "u9", Token: "live-aep-token"}
	h.enrichTeams(context.Background(), &ident)
	assert.Equal(t, []string{"sales-dept"}, ident.Teams)
	assert.Zero(t, adminHits)
}
