package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// contactsStubAEP serves the admin-user listing across two cursor pages so
// the pagination loop and the projection filters are both exercised. It also
// answers the session-mint surfaces (password login + user/me) so the route
// registration test can drive the real mux with a cookie session.
func contactsStubAEP(t *testing.T) *httptest.Server {
	t.Helper()
	pages := map[string]string{
		"": `{"items":[
			{"id":"u1","username":"zhangsan","displayName":"张三","kind":"human","status":"active"},
			{"id":"u2","username":"bot","displayName":"机器人","kind":"agent","status":"active"},
			{"id":"u3","username":"lisi","displayName":"李四","kind":"human","status":"disabled"},
			{"id":"u4","username":"wangwu","displayName":"王五","kind":"human","status":"active","teamIds":["rd","ghost-team"]}
		],"nextCursor":"page2"}`,
		"page2": `{"items":[
			{"id":"u5","username":"zhaoliu","displayName":"赵六","kind":"human","status":"active","teamIds":["sales"]}
		],"nextCursor":null}`,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/aep/v1/auth/password/login":
			_, _ = w.Write([]byte(`{"accessToken":"aep-token-user1"}`))
		case "/aep/v1/user/me":
			_, _ = w.Write([]byte(`{"user":{"id":"u1","displayName":"张三","kind":"human"},"roles":[]}`))
		case "/aep/v1/admin/teams":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"teams":[{"id":"rd","name":"研发部"},{"id":"sales","name":"销售部"}]}`))
		case "/aep/v1/admin/users":
			cursor := r.URL.Query().Get("cursor")
			body, ok := pages[cursor]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// resetAdminTokenCache isolates the package-level token cache between tests;
// without it, whether the login path runs depends on test execution order.
func resetAdminTokenCache(t *testing.T) {
	t.Helper()
	adminTokenCache.mu.Lock()
	adminTokenCache.token = ""
	adminTokenCache.expires = time.Time{}
	adminTokenCache.mu.Unlock()
}

func TestHandleContacts(t *testing.T) {
	t.Run("employee lists directory: paginated, filtered, self excluded", func(t *testing.T) {
		resetAdminTokenCache(t)
		stubs := &departmentStubUpstreams{
			aep:     contactsStubAEP(t),
			ov:      httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
			weknora: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
		}
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts", nil)
		w := httptest.NewRecorder()
		// u1 是请求者本人：通讯录不列自己
		h.handleContacts(w, req, identity{UserID: "u1", Username: "zhangsan", DisplayName: "张三"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var got struct {
			Contacts []struct {
				UserID      string `json:"userId"`
				Username    string `json:"username"`
				DisplayName string `json:"displayName"`
				Teams       []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"teams"`
			} `json:"contacts"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
		// 精确断言：且仅为 u4、u5（agent/disabled/本人全被滤除，第二页被跟随）
		require.Len(t, got.Contacts, 2)
		assert.Equal(t, "u4", got.Contacts[0].UserID)
		assert.Equal(t, "wangwu", got.Contacts[0].Username)
		assert.Equal(t, "王五", got.Contacts[0].DisplayName)
		assert.Equal(t, "u5", got.Contacts[1].UserID)
		// 组织架构：teamIds 解析为名称；解析不出（ghost-team）的只丢弃该组，不裸奔 id
		require.Len(t, got.Contacts[0].Teams, 1)
		assert.Equal(t, "研发部", got.Contacts[0].Teams[0].Name)
		require.Len(t, got.Contacts[1].Teams, 1)
		assert.Equal(t, "sales", got.Contacts[1].Teams[0].ID)
	})

	t.Run("team listing failure fails the whole directory closed", func(t *testing.T) {
		resetAdminTokenCache(t)
		// 静默降级会让部门分组时有时无地漂移；与 users 页失败同纪律整体拒绝。
		// fetchTeamNames 先于 users 循环执行；usersHits=0 证明 502 确来自 teams 分支。
		var usersHits int
		stubs := &departmentStubUpstreams{
			aep: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/aep/v1/auth/password/login" {
					_, _ = w.Write([]byte(`{"accessToken":"admin-token"}`))
					return
				}
				if r.URL.Path == "/aep/v1/admin/users" {
					usersHits++
				}
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"type":"about:blank","title":"Unavailable","status":503}`))
			})),
			ov:      httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
			weknora: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
		}
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts", nil)
		w := httptest.NewRecorder()
		h.handleContacts(w, req, identity{UserID: "u1", Username: "zhangsan", DisplayName: "张三"})
		assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
		assert.Zero(t, usersHits, "502 must come from the teams branch, not the users loop")
		// 员工面响应不得透传上游 body（问题原文只进日志）
		assert.Contains(t, w.Body.String(), "listing teams")
		assert.NotContains(t, w.Body.String(), "about:blank")
	})

	t.Run("team names follow nextCursor beyond the first page", func(t *testing.T) {
		resetAdminTokenCache(t)
		pages := map[string]string{
			"":   `{"teams":[{"id":"rd","name":"研发部"}],"nextCursor":"t2"}`,
			"t2": `{"teams":[{"id":"sales","name":"销售部"}],"nextCursor":null}`,
		}
		stubs := &departmentStubUpstreams{
			aep: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/aep/v1/auth/password/login":
					_, _ = w.Write([]byte(`{"accessToken":"admin-token"}`))
				case r.URL.Path == "/aep/v1/user/me":
					_, _ = w.Write([]byte(`{"user":{"id":"u1","displayName":"张三","kind":"human"},"roles":[]}`))
				case r.URL.Path == "/aep/v1/admin/teams":
					body, ok := pages[r.URL.Query().Get("cursor")]
					if !ok {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				case r.URL.Path == "/aep/v1/admin/users":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"items":[{"id":"u9","username":"sunqi","displayName":"孙七","kind":"human","status":"active","teamIds":["sales"]}],"nextCursor":null}`))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})),
			ov:      httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
			weknora: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
		}
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts", nil)
		w := httptest.NewRecorder()
		h.handleContacts(w, req, identity{UserID: "u1", Username: "zhangsan", DisplayName: "张三"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), `"name":"销售部"`, "second-page team must resolve")
	})

	t.Run("AEP problem+json error rejects the listing instead of 200-empty", func(t *testing.T) {
		resetAdminTokenCache(t)
		// 真实 AEP 的错误响应是合法 JSON（RFC 9457 problem+json）——
		// 不检查状态码就会解码成空 items、以 200 空名单落地。
		problem := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/aep/v1/auth/password/login" {
				_, _ = w.Write([]byte(`{"accessToken":"admin-token"}`))
				return
			}
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Unauthorized","status":401}`))
		}))
		stubs := &departmentStubUpstreams{
			aep:     problem,
			ov:      httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
			weknora: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
		}
		h := newDepartmentHarness(t, stubs)
		w := httptest.NewRecorder()
		h.handleContacts(w, httptest.NewRequest(http.MethodGet, "/api/v1/contacts", nil), identity{UserID: "u1"})
		assert.Equal(t, http.StatusBadGateway, w.Code)
	})

	t.Run("a later-page failure rejects the whole listing (no partial directory)", func(t *testing.T) {
		resetAdminTokenCache(t)
		var calls int
		var mu sync.Mutex
		partial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/aep/v1/auth/password/login":
				_, _ = w.Write([]byte(`{"accessToken":"admin-token"}`))
			case "/aep/v1/admin/users":
				mu.Lock()
				calls++
				first := calls == 1
				mu.Unlock()
				if first {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"items":[{"id":"u4","username":"wangwu","displayName":"王五","kind":"human","status":"active"}],"nextCursor":"page2"}`))
					return
				}
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"type":"about:blank","title":"Internal","status":500}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		stubs := &departmentStubUpstreams{
			aep:     partial,
			ov:      httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
			weknora: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
		}
		h := newDepartmentHarness(t, stubs)
		w := httptest.NewRecorder()
		h.handleContacts(w, httptest.NewRequest(http.MethodGet, "/api/v1/contacts", nil), identity{UserID: "u1"})
		assert.Equal(t, http.StatusBadGateway, w.Code, "partial directories must not surface as 200")
	})
}

// fakeCredsSecretClient mirrors the department harness's platform Secret so
// adminToken can run its bootstrap login against the stub AEP.
func fakeCredsSecretClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, k8sscheme.AddToScheme(scheme))
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "de-platform-creds", Namespace: "digital-employees"},
		Data: map[string][]byte{
			"AEP_ADMIN_USER":     []byte("admin"),
			"AEP_ADMIN_PASSWORD": []byte("secret-pass"),
		},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(secret).Build()
}

// Route-registration guard (same rationale as the workbench mux test): a
// routes() regression would 404/405 in production while every handler-level
// test stays green.
func TestContactsRouteRegistered(t *testing.T) {
	resetAdminTokenCache(t)
	aep := contactsStubAEP(t)
	p := &portal{
		cfg: config{
			namespace:         "digital-employees",
			adminUsers:        map[string]bool{"u1": true},
			memoryServer:      "http://127.0.0.1:1",
			knowledgeAPIURL:   "http://127.0.0.1:1",
			credentialsSecret: "de-platform-creds",
			aepDeploymentID:   "demo",
			aepBaseURL:        aep.URL,
		},
		k8s:      fakeCredsSecretClient(t),
		aep:      &aepAuthenticator{baseURL: aep.URL, http: aep.Client()},
		sessions: newSessionStore(time.Hour),
		logger:   discardLogger(),
	}
	server := httptest.NewServer(p.routes())
	t.Cleanup(server.Close)

	// Bearer 直连（与 messenger iframe 经 /api 反代的调用方式一致）
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/contacts", nil)
	req.Header.Set("Authorization", "Bearer aep-token-user1")
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "GET /api/v1/contacts must be registered and employee-reachable")
}
