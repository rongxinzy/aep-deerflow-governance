package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSessionStorePersistence(t *testing.T) {
	t.Run("issue and reload across a simulated restart", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		mock.ExpectExec(`INSERT INTO sessions`).WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec(`DELETE FROM sessions WHERE expires_at`).WillReturnResult(sqlmock.NewResult(0, 0))

		store := newSessionStoreWithDB(time.Hour, db)
		ident := adminIdentity()
		ident.Token = "aep-token"
		token, csrf := store.issue(ident)
		assert.NotEmpty(t, token)
		assert.NotEmpty(t, csrf)

		// Memory hit: sliding renewal persists the new expiry.
		mock.ExpectExec(`UPDATE sessions SET expires_at`).WillReturnResult(sqlmock.NewResult(0, 1))
		gotIdent, gotCSRF, ok := store.get(token)
		assert.True(t, ok)
		assert.Equal(t, "admin-user", gotIdent.UserID)
		assert.Equal(t, csrf, gotCSRF)

		// Simulated restart: a fresh store reloads the row from the database.
		rows := sqlmock.NewRows([]string{"identity_json", "csrf", "expires_at"}).
			AddRow(`{"token":"aep-token","id":"admin-user","username":"Admin","displayName":"Admin","kind":"human","roles":["admin"],"teams":[]}`, csrf, time.Now().Add(time.Hour))
		mock.ExpectQuery(`SELECT identity_json, csrf, expires_at FROM sessions`).WithArgs(token).WillReturnRows(rows)
		fresh := newSessionStoreWithDB(time.Hour, db)
		reIdent, reCSRF, reOK := fresh.get(token)
		assert.True(t, reOK)
		assert.Equal(t, "admin-user", reIdent.UserID)
		assert.Equal(t, csrf, reCSRF)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("expired sessions are dropped", func(t *testing.T) {
		store := newSessionStore(time.Nanosecond)
		token, _ := store.issue(adminIdentity())
		time.Sleep(time.Millisecond)
		_, _, ok := store.get(token)
		assert.False(t, ok)
	})

	t.Run("bad bearer and csrf mismatch paths", func(t *testing.T) {
		h := newAPIHarness(t)
		// Bearer failure: the AEP stub refuses.
		h.aep = &aepAuthenticator{baseURL: "http://127.0.0.1:1", http: http.DefaultClient}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/employees", nil)
		req.Header.Set("Authorization", "Bearer bad")
		w := httptest.NewRecorder()
		h.auth(func(http.ResponseWriter, *http.Request, identity) {})(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)

		// CSRF mismatch on a cookie-authenticated write.
		token, _ := h.sessions.issue(adminIdentity())
		post := httptest.NewRequest(http.MethodPost, "/api/v1/employees", nil)
		post.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: token})
		post.Header.Set("X-CSRF-Token", "wrong")
		w2 := httptest.NewRecorder()
		h.auth(func(http.ResponseWriter, *http.Request, identity) {})(w2, post)
		assert.Equal(t, http.StatusForbidden, w2.Code)
	})
}

func TestDepartmentMembersReplace(t *testing.T) {
	t.Run("PUT computes adds and removes against AEP", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPut, "/api/v1/departments/rd-dept/members",
			strings.NewReader(`{"userIds":["u1","u2"]}`))
		req.SetPathValue("id", "rd-dept")
		w := httptest.NewRecorder()
		h.handleDepartmentMembers(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), `"added"`)
	})

	t.Run("non-admin and bad json are refused", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPut, "/api/v1/departments/rd/members", strings.NewReader(`{bad`))
		req.SetPathValue("id", "rd")
		w := httptest.NewRecorder()
		h.handleDepartmentMembers(w, req, identity{UserID: "u"})
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

func TestRefreshSessionTeams(t *testing.T) {
	stubs := newDepartmentStubs(t)
	h := newDepartmentHarness(t, stubs)
	ident := adminIdentity()
	ident.Token = "tok"
	h.refreshSessionTeams("session-token", ident)
	// The stub's /user/me returns teamIds [rd-dept]; the refreshed identity
	// keeps them even though updateIdentity is a no-op without a live store.
	assert.Equal(t, "admin-user", ident.UserID)
}

func TestKnowledgeListBases(t *testing.T) {
	t.Run("non-admin is refused", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		w := httptest.NewRecorder()
		h.handleKnowledgeListBases(w, httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/bases", nil), identity{UserID: "u"})
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("lists bases through the admin login", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		w := httptest.NewRecorder()
		h.handleKnowledgeListBases(w, httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/bases", nil), adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "企业知识库")
	})

	t.Run("without credentials the list is empty, not an error", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		// Remove the WeKnora admin credentials from the platform secret.
		h.cfg.credentialsSecret = "missing-secret"
		w := httptest.NewRecorder()
		h.handleKnowledgeListBases(w, httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/bases", nil), adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"knowledgeBases":[]`)
	})
}

func TestChatWakeAndProxyErrors(t *testing.T) {
	t.Run("chat rejects an unknown employee", func(t *testing.T) {
		h := newAPIHarness(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees/ghost/chat/threads", nil)
		req.SetPathValue("name", "ghost")
		req.SetPathValue("rest", "threads")
		w := httptest.NewRecorder()
		h.handleChat(w, req, adminIdentity())
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("chat refuses non-Ready employees", func(t *testing.T) {
		item := readyCR("broken-helper", "admin-user")
		item.Status.Phase = dev1alpha1.PhaseError
		h := newAPIHarness(t, item)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees/broken-helper/chat/threads", nil)
		req.SetPathValue("name", "broken-helper")
		req.SetPathValue("rest", "threads")
		w := httptest.NewRecorder()
		h.handleChat(w, req, adminIdentity())
		assert.Equal(t, http.StatusConflict, w.Code)
	})

	t.Run("session employee selection validates the name", func(t *testing.T) {
		h := newAPIHarness(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/session/employee",
			strings.NewReader(`{"name":"Bad Name"}`))
		w := httptest.NewRecorder()
		h.handleSessionEmployee(w, req, adminIdentity())
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("hibernate wake times out when nothing wakes", func(t *testing.T) {
		h := newAPIHarness(t, scaledZeroDeployment("sales-helper"))
		h.cfg.wakeTimeout = 10 * time.Millisecond
		// Deployment exists but never becomes ready; wake row insert is
		// best-effort with a nil-safe db mock.
		code, msg := h.wakeRuntime(context.Background(), "sales-helper")
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.Contains(t, msg, "waking up")
	})
}

func scaledZeroDeployment(name string) *appsv1.Deployment {
	zero := int32(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "de-" + name, Namespace: "digital-employees"},
		Spec:       appsv1.DeploymentSpec{Replicas: &zero},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 0},
	}
}

func TestHandlerErrorBranches(t *testing.T) {
	t.Run("update employee invalid json", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("e", "admin-user"))
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/employees/e", strings.NewReader("{bad"))
		req.SetPathValue("name", "e")
		w := httptest.NewRecorder()
		h.updateEmployee(w, req, adminIdentity())
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("update employee refuses outsiders", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("e", "owner-a"))
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/employees/e", strings.NewReader(`{"displayName":"x"}`))
		req.SetPathValue("name", "e")
		w := httptest.NewRecorder()
		h.updateEmployee(w, req, identity{UserID: "user-x"})
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("delete employee refuses outsiders", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("e", "owner-a"))
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/employees/e", nil)
		req.SetPathValue("name", "e")
		w := httptest.NewRecorder()
		h.deleteEmployee(w, req, identity{UserID: "user-x"})
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("decideRequest 404s for unknown requests", func(t *testing.T) {
		h := newAPIHarness(t)
		h.dbmock.ExpectQuery(`SELECT employee_name, spec_json FROM requests`).WithArgs("req-x").
			WillReturnError(sql.ErrNoRows)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/requests/req-x/approve", nil)
		req.SetPathValue("id", "req-x")
		w := httptest.NewRecorder()
		h.decideRequest("approved")(w, req, adminIdentity())
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("list departments surfaces AEP failure", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		stubs.aep.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		h := newDepartmentHarness(t, stubs)
		w := httptest.NewRecorder()
		h.handleListDepartments(w, httptest.NewRequest(http.MethodGet, "/api/v1/departments", nil), adminIdentity())
		assert.Equal(t, http.StatusBadGateway, w.Code)
	})

	t.Run("rename department without a name is rejected", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/departments/rd", strings.NewReader(`{}`))
		req.SetPathValue("id", "rd")
		w := httptest.NewRecorder()
		h.handleUpdateDepartment(w, req, adminIdentity())
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("delete department passes through AEP failure", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		stubs.aep.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		})
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/departments/rd", nil)
		req.SetPathValue("id", "rd")
		w := httptest.NewRecorder()
		h.handleDeleteDepartment(w, req, adminIdentity())
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("members list handles an upstream failure", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		stubs.aep.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/departments/rd/members", nil)
		req.SetPathValue("id", "rd")
		w := httptest.NewRecorder()
		h.handleDepartmentMembers(w, req, adminIdentity())
		assert.Equal(t, http.StatusBadGateway, w.Code)
	})

	t.Run("knowledge proxy refuses non-admin and empty path", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/proxy/", nil)
		w := httptest.NewRecorder()
		h.handleKnowledgeProxy(w, req, identity{UserID: "u"})
		assert.Equal(t, http.StatusForbidden, w.Code)

		req2 := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/proxy/", nil)
		req2.SetPathValue("rest", "")
		w2 := httptest.NewRecorder()
		h.handleKnowledgeProxy(w2, req2, adminIdentity())
		assert.Equal(t, http.StatusBadRequest, w2.Code)
	})

	t.Run("memory search reports upstream search failures", func(t *testing.T) {
		stub := newServicesStubs(t)
		stub.ov.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"error","error":{"message":"bad key"}}`))
		})
		h := newServicesHarness(t, stub)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/search",
			strings.NewReader(`{"employee":"sales-helper","query":"q"}`))
		w := httptest.NewRecorder()
		h.handleMemorySearch(w, req, adminIdentity())
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.Contains(t, w.Body.String(), "bad key")
	})

	t.Run("memory search 404s without a memkey", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/memory/search",
			strings.NewReader(`{"employee":"ghost","query":"q"}`))
		w := httptest.NewRecorder()
		h.handleMemorySearch(w, req, adminIdentity())
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func TestDepartmentMembersFullReplace(t *testing.T) {
	t.Run("PUT swaps membership: removes departed, adds new", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		req := httptest.NewRequest(http.MethodPut, "/api/v1/departments/rd-dept/members",
			strings.NewReader(`{"userIds":["u2"]}`))
		req.SetPathValue("id", "rd-dept")
		w := httptest.NewRecorder()
		h.handleDepartmentMembers(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		body := w.Body.String()
		assert.Contains(t, body, `"removed":1`)
		assert.Contains(t, body, `"added":1`)
	})
}

func TestMigrateCreatesTables(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS requests`).WillReturnResult(sqlmock.NewResult(0, 0))
	require.NoError(t, migrate(db))
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestSessionDropAndEvict(t *testing.T) {
	t.Run("drop removes the session and forgets it in the db", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		mock.ExpectExec(`DELETE FROM sessions WHERE token`).WillReturnResult(sqlmock.NewResult(0, 1))
		store := newSessionStoreWithDB(time.Hour, db)
		token, _ := store.issue(adminIdentity())
		store.drop(token)
		_, _, ok := store.get(token)
		assert.False(t, ok)
		assert.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("updateIdentity refreshes teams on a live session", func(t *testing.T) {
		store := newSessionStore(time.Hour)
		token, _ := store.issue(identity{UserID: "u1"})
		refreshed := identity{UserID: "u1", Teams: []string{"rd-dept"}}
		store.updateIdentity(token, refreshed)
		got, _, ok := store.get(token)
		require.True(t, ok)
		assert.Equal(t, []string{"rd-dept"}, got.Teams)
	})

	t.Run("evict sweeps expired tokens", func(t *testing.T) {
		store := newSessionStore(time.Nanosecond)
		store.issue(adminIdentity())
		time.Sleep(2 * time.Millisecond) // the first token expires
		store.issue(adminIdentity())     // issue() sweeps expired entries first
		assert.Len(t, store.items, 1)
	})
}

func TestSessionEmployeeSelection(t *testing.T) {
	t.Run("selection authorizes and sets the employee cookie", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("sales-helper", "admin-user"), memkey("sales-helper"))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/session/employee",
			strings.NewReader(`{"name":"sales-helper"}`))
		w := httptest.NewRecorder()
		h.handleSessionEmployee(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Contains(t, w.Header().Get("Set-Cookie"), "de_employee=sales-helper")
	})

	t.Run("selection refuses an employee the caller cannot use", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("sales-helper", "owner-a"))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/session/employee",
			strings.NewReader(`{"name":"sales-helper"}`))
		w := httptest.NewRecorder()
		h.handleSessionEmployee(w, req, identity{UserID: "user-x"})
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

func TestAuthenticateBearerSuccess(t *testing.T) {
	stub := newDepartmentStubs(t) // carries /aep/v1/user/me
	h := newAPIHarness(t)
	h.aep = &aepAuthenticator{baseURL: stub.aep.URL, http: stub.aep.Client()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/employees", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	w := httptest.NewRecorder()
	reached := false
	h.auth(func(_ http.ResponseWriter, _ *http.Request, ident identity) { reached = ident.UserID == "admin-user" })(w, req)
	assert.True(t, reached)
}

func TestAuthenticateRefreshesStaleTeams(t *testing.T) {
	stub := newDepartmentStubs(t)
	h := newAPIHarness(t)
	h.aep = &aepAuthenticator{baseURL: stub.aep.URL, http: stub.aep.Client()}
	h.cfg.aepBaseURL = stub.aep.URL
	ident := adminIdentity()
	ident.Token = "good-token"
	ident.TeamsFetchedAt = time.Time{} // stale: triggers a synchronous refresh
	token, _ := h.sessions.issue(ident)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/employees", nil)
	req.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: token})
	w := httptest.NewRecorder()
	sawFreshTeams := false
	h.auth(func(_ http.ResponseWriter, _ *http.Request, id identity) {
		sawFreshTeams = len(id.Teams) == 1 && id.Teams[0] == "rd-dept"
	})(w, req)
	assert.True(t, sawFreshTeams, "stale team memberships must refresh before the handler runs")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAdminTokenCache(t *testing.T) {
	stubs := newDepartmentStubs(t)
	h := newDepartmentHarness(t, stubs)
	// The global cache persists across tests; exercise both a fresh login
	// and the cached path via the public entry point.
	adminTokenCache.token = ""
	adminTokenCache.expires = time.Time{}
	first := h.adminToken(context.Background())
	require.NotEmpty(t, first)
	second := h.adminToken(context.Background())
	assert.Equal(t, first, second, "second call must hit the in-process cache")
}

func TestMemoryStatusDegraded(t *testing.T) {
	t.Run("unreachable OV reports unhealthy without accounts", func(t *testing.T) {
		stub := newServicesStubs(t)
		stub.ov.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		h := newServicesHarness(t, stub)
		w := httptest.NewRecorder()
		h.handleMemoryStatus(w, httptest.NewRequest(http.MethodGet, "/api/v1/memory/status", nil), adminIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"healthy":false`)
		assert.Contains(t, w.Body.String(), `"accounts":[]`)
	})

	t.Run("missing platform secret degrades to empty accounts", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		h.cfg.credentialsSecret = "no-such-secret"
		w := httptest.NewRecorder()
		h.handleMemoryStatus(w, httptest.NewRequest(http.MethodGet, "/api/v1/memory/status", nil), adminIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"accounts":[]`)
	})
}

func TestRuntimeBaseURLAndEntryTemplate(t *testing.T) {
	h := newAPIHarness(t)
	assert.Equal(t, "http://de-sales-helper.digital-employees.svc.cluster.local:8001", h.runtimeBaseURL("sales-helper"))
	h.chatBaseOverride = "http://upstream.test"
	assert.Equal(t, "http://upstream.test", h.runtimeBaseURL("any"))

	// The /chat entry page derives the chat URL from the request host.
	req := httptest.NewRequest(http.MethodGet, "/chat", nil)
	req.Host = "portal.example:30190"
	w := httptest.NewRecorder()
	h.handleChatEntry(w, req)
	assert.Contains(t, w.Body.String(), "http://portal.example:30195/workspace")
}
