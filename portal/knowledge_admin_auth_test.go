package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const freshAdminUserJSON = `{"user":{"id":"admin-user","displayName":"Admin","kind":"user","teamIds":[]},"roles":["platform-admin"],"deploymentId":"demo"}`

func issueManagedKnowledgeSession(h *apiTestPortal) (string, string) {
	return h.sessions.issue(identity{
		Token:          "aep-access-token",
		UserID:         "admin-user",
		Username:       "Admin",
		DeploymentID:   "demo",
		Roles:          []string{"platform-admin"},
		TeamsFetchedAt: time.Now(),
	})
}

func configureRoleOnlyAdmin(h *apiTestPortal) {
	h.cfg.aepDeploymentID = "demo"
	h.cfg.adminUsers = map[string]bool{}
	h.cfg.adminRoles = map[string]bool{"platform-admin": true}
	h.cfg.managedKnowledge.tenantID = "tenant-1"
}

func TestManagedKnowledgeGrantsRejectCachedAdminAfterRoleRevocation(t *testing.T) {
	h := newAPIHarness(t)
	configureRoleOnlyAdmin(h)
	aep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/aep/v1/user/me", r.URL.Path)
		require.Equal(t, "Bearer aep-access-token", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"user":{"id":"admin-user","displayName":"Admin","kind":"user","teamIds":[]},"roles":[],"deploymentId":"demo"}`))
	}))
	defer aep.Close()
	h.aep = &aepAuthenticator{baseURL: aep.URL, http: aep.Client()}
	sessionToken, csrf := issueManagedKnowledgeSession(h)
	routes := h.routes()

	for _, tc := range []struct {
		method string
		body   string
	}{
		{method: http.MethodGet},
		{method: http.MethodPut, body: `{"grants":[{"type":"user","id":"admin-user"}]}`},
	} {
		t.Run(tc.method, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/v1/knowledge/managed/bases/kb-1/grants", strings.NewReader(tc.body))
			req.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: sessionToken})
			if tc.method == http.MethodPut {
				req.Header.Set("X-CSRF-Token", csrf)
			}
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, req)
			require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
		})
	}
	// Neither the grant read nor attempted self-grant may reach the handler's
	// registry, audit, or transaction queries after fresh AEP role denial.
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeGrantsFreshAdminUsesVerifiedIdentity(t *testing.T) {
	h := newAPIHarness(t)
	configureRoleOnlyAdmin(h)
	aep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/aep/v1/user/me", r.URL.Path)
		_, _ = w.Write([]byte(freshAdminUserJSON))
	}))
	defer aep.Close()
	h.aep = &aepAuthenticator{baseURL: aep.URL, http: aep.Client()}
	sessionToken, _ := h.sessions.issue(identity{
		Token:          "aep-access-token",
		UserID:         "admin-user",
		Username:       "Admin",
		DeploymentID:   "demo",
		TeamsFetchedAt: time.Now(),
	})

	h.dbmock.ExpectQuery(`SELECT knowledge_base_id FROM knowledge_base_registry WHERE deployment_id=\$1 AND weknora_tenant_id=\$2 AND knowledge_base_id=\$3`).
		WithArgs("demo", "tenant-1", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"knowledge_base_id"}).AddRow("kb-1"))
	h.dbmock.ExpectQuery(`SELECT subject_type,subject_id,granted_by,created_at`).
		WithArgs("demo", "tenant-1", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"subject_type", "subject_id", "granted_by", "created_at"}))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "tenant-1", "admin-user", "list_grants", "knowledge_base", "kb-1", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "kb-1", "succeeded", http.StatusOK).WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/grants", nil)
	req.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: sessionToken})
	response := httptest.NewRecorder()
	h.routes().ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"knowledge_base_id":"kb-1"`)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeGrantsFailClosedWhenAEPRoleCheckFails(t *testing.T) {
	h := newAPIHarness(t)
	configureRoleOnlyAdmin(h)
	aep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "AEP unavailable", http.StatusBadGateway)
	}))
	defer aep.Close()
	h.aep = &aepAuthenticator{baseURL: aep.URL, http: aep.Client()}
	sessionToken, csrf := issueManagedKnowledgeSession(h)
	routes := h.routes()

	for _, tc := range []struct {
		method string
		body   string
	}{
		{method: http.MethodGet},
		{method: http.MethodPut, body: `{"grants":[{"type":"user","id":"admin-user"}]}`},
	} {
		t.Run(tc.method, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/v1/knowledge/managed/bases/kb-1/grants", strings.NewReader(tc.body))
			req.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: sessionToken})
			if tc.method == http.MethodPut {
				req.Header.Set("X-CSRF-Token", csrf)
			}
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, req)
			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
		})
	}
	// Verification failure is closed before registry access or grant mutation.
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestFreshAdminOnlyRejectsMissingOrChangedIdentity(t *testing.T) {
	t.Run("missing token", func(t *testing.T) {
		h := newAPIHarness(t)
		called := false
		h.freshAdminOnly(func(http.ResponseWriter, *http.Request, identity) { called = true })(
			httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), identity{UserID: "admin-user"},
		)
		require.False(t, called)
	})

	t.Run("missing authenticator", func(t *testing.T) {
		h := newAPIHarness(t)
		h.aep = nil
		called := false
		response := httptest.NewRecorder()
		h.freshAdminOnly(func(http.ResponseWriter, *http.Request, identity) { called = true })(
			response, httptest.NewRequest(http.MethodGet, "/", nil), identity{Token: "token", UserID: "admin-user"},
		)
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.False(t, called)
	})

	t.Run("different verified user", func(t *testing.T) {
		h := newAPIHarness(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"user":{"id":"other-user","displayName":"Other","kind":"user","teamIds":[]},"roles":["platform-admin"],"deploymentId":"demo"}`))
		}))
		defer server.Close()
		h.aep = &aepAuthenticator{baseURL: server.URL, http: server.Client()}
		h.cfg.aepDeploymentID = "demo"
		h.cfg.adminRoles = map[string]bool{"platform-admin": true}
		called := false
		response := httptest.NewRecorder()
		h.freshAdminOnly(func(http.ResponseWriter, *http.Request, identity) { called = true })(
			response, httptest.NewRequest(http.MethodGet, "/", nil), identity{Token: "token", UserID: "admin-user"},
		)
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.False(t, called)
	})
}
