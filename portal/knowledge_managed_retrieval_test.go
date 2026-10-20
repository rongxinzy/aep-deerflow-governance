package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func managedRetrievalFixture(t *testing.T, identityJSON string, upstream http.HandlerFunc) *apiTestPortal {
	t.Helper()
	h := newAPIHarness(t) // No employees, bindings or grants exist in this fixture.
	configureRoleOnlyAdmin(h)
	h.cfg.managedKnowledge.tenantID = "10000"
	h.cfg.managedKnowledge.apiKeySecretField = "WEKNORA_KNOWLEDGE_API_KEY"
	h.cfg.credentialsSecret = "platform-creds"
	require.NoError(t, h.k8s.Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: h.cfg.namespace, Name: "platform-creds"},
		Data:       map[string][]byte{"WEKNORA_KNOWLEDGE_API_KEY": []byte("synthetic-server-key")},
	}))
	aep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/aep/v1/user/me", r.URL.Path)
		_, _ = w.Write([]byte(identityJSON))
	}))
	t.Cleanup(aep.Close)
	h.aep = &aepAuthenticator{baseURL: aep.URL, http: aep.Client()}
	weknora := httptest.NewServer(upstream)
	t.Cleanup(weknora.Close)
	h.cfg.knowledgeAPIURL = weknora.URL
	return h
}

func managedRetrievalUpstream(t *testing.T, foreignDocument, foreignChunk bool) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "synthetic-server-key", r.Header.Get("X-API-Key"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge-bases/kb-1":
			_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
		case "/api/v1/knowledge-bases/kb-1/hybrid-search":
			require.Equal(t, http.MethodPost, r.Method)
			var input map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
			require.Equal(t, map[string]any{"query_text": "question", "vector_threshold": 0.5, "keyword_threshold": 0.3, "match_count": float64(5)}, input)
			_, _ = w.Write([]byte(`{"data":[{"id":"chunk-1","content":"synthetic passage","knowledge_id":"doc-1","knowledge_title":"untrusted title","score":0.91}]}`))
		case "/api/v1/knowledge/doc-1":
			base := "kb-1"
			if foreignDocument {
				base = "kb-other"
			}
			_, _ = fmt.Fprintf(w, `{"data":{"id":"doc-1","knowledge_base_id":%q,"tenant_id":10000,"name":"Verified title","file_name":"policy.txt","file_type":"txt"}}`, base)
		case "/api/v1/chunks/doc-1":
			require.False(t, foreignDocument, "foreign documents must never reach chunk retrieval")
			require.Equal(t, "text", r.URL.Query().Get("chunk_type"))
			tenant := 10000
			if foreignChunk {
				tenant = 20000
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"chunk-1","knowledge_id":"doc-1","knowledge_base_id":"kb-1","tenant_id":%d,"seq_id":1,"chunk_type":"text","content":"synthetic passage"}],"total":40,"page":%d,"page_size":%d}`, tenant, page, size)
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

func beginRetrievalAudit(mock sqlmock.Sqlmock, action, resourceType, resourceID, parent string) {
	mock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", action, resourceType, resourceID, parent).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func finishManagedReadAudit(mock sqlmock.Sqlmock, resourceID, outcome string, status int) {
	mock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), resourceID, outcome, status).WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectManagedReadRegistry(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").
		WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
}

func managedReadRequest(h *apiTestPortal, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer synthetic-aep-token")
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, r)
	return w
}

func TestManagedKnowledgeAdminSearchAndSourceWithoutPersonalGrantOrEmployee(t *testing.T) {
	h := managedRetrievalFixture(t, freshAdminUserJSON, managedRetrievalUpstream(t, false, false))
	beginRetrievalAudit(h.dbmock, "admin_search", "knowledge_base", "kb-1", "")
	expectManagedReadRegistry(h.dbmock)
	expectManagedReadRegistry(h.dbmock) // Verify every returned document's parent.
	finishManagedReadAudit(h.dbmock, "kb-1", "succeeded", 200)
	w := managedReadRequest(h, http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/search", `{"query":"question","mode":"hybrid","limit":5}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"content":"synthetic passage"`)
	require.Contains(t, w.Body.String(), `"title":"Verified title"`)
	require.NotContains(t, w.Body.String(), "untrusted title")
	require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))

	beginRetrievalAudit(h.dbmock, "admin_read_source", "document", "doc-1", "kb-1")
	expectManagedReadRegistry(h.dbmock)
	finishManagedReadAudit(h.dbmock, "doc-1", "succeeded", 200)
	w = managedReadRequest(h, http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/documents/doc-1/source?page=2&page_size=20", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"page":2`)
	require.Contains(t, w.Body.String(), `"total":40`)
	require.Contains(t, w.Body.String(), `"content":"synthetic passage"`)
	require.NoError(t, h.dbmock.ExpectationsWereMet(), "no grant query or employee binding lookup is permitted on administrator inspection")
}

func TestManagedKnowledgeRetrievalRejectsRevokedAdminOnBothRoutes(t *testing.T) {
	h := managedRetrievalFixture(t, `{"user":{"id":"admin-user","kind":"user","teamIds":[]},"roles":[],"deploymentId":"demo"}`, func(_ http.ResponseWriter, _ *http.Request) { t.Error("revoked role reached WeKnora") })
	session, csrf := issueManagedKnowledgeSession(h)
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/search", `{"query":"question","mode":"hybrid","limit":5}`},
		{http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/documents/doc-1/source", ""},
	} {
		r := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
		r.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: session})
		r.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, r)
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "ADMIN_REQUIRED")
	}
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeRetrievalDoesNotExposeCrossBaseOrTenantContent(t *testing.T) {
	for _, tc := range []struct {
		name                                  string
		search, foreignDocument, foreignChunk bool
	}{
		{"search document belongs to another base", true, true, false},
		{"source document belongs to another base", false, true, false},
		{"source chunk belongs to another tenant", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := managedRetrievalFixture(t, freshAdminUserJSON, managedRetrievalUpstream(t, tc.foreignDocument, tc.foreignChunk))
			method, path, body, action, resourceType, resourceID, parent := http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/documents/doc-1/source", "", "admin_read_source", "document", "doc-1", "kb-1"
			if tc.search {
				method, path, body, action, resourceType, resourceID, parent = http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/search", `{"query":"question","mode":"hybrid","limit":5}`, "admin_search", "knowledge_base", "kb-1", ""
			}
			beginRetrievalAudit(h.dbmock, action, resourceType, resourceID, parent)
			expectManagedReadRegistry(h.dbmock)
			if tc.search {
				expectManagedReadRegistry(h.dbmock)
			}
			finishManagedReadAudit(h.dbmock, resourceID, "failed", 502)
			w := managedReadRequest(h, method, path, body)
			require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), "synthetic passage")
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeSearchRejectsUnregisteredBaseAndRecordsFailure(t *testing.T) {
	h := managedRetrievalFixture(t, freshAdminUserJSON, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/auth/me", r.URL.Path, "unregistered bases must not reach data endpoints")
		_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
	})
	beginRetrievalAudit(h.dbmock, "admin_search", "knowledge_base", "kb-1", "")
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnError(sql.ErrNoRows)
	finishManagedReadAudit(h.dbmock, "kb-1", "failed", 403)
	w := managedReadRequest(h, http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/search", `{"query":"question","mode":"hybrid","limit":5}`)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeReadAuditFailureDoesNotExposeResults(t *testing.T) {
	h := managedRetrievalFixture(t, freshAdminUserJSON, managedRetrievalUpstream(t, false, false))
	beginRetrievalAudit(h.dbmock, "admin_search", "knowledge_base", "kb-1", "")
	expectManagedReadRegistry(h.dbmock)
	expectManagedReadRegistry(h.dbmock)
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WillReturnError(errors.New("audit offline"))
	w := managedReadRequest(h, http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/search", `{"query":"question","mode":"hybrid","limit":5}`)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "AUDIT_UNAVAILABLE")
	require.NotContains(t, w.Body.String(), "synthetic passage")
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeRetrievalRejectsDeploymentAndTenantMismatch(t *testing.T) {
	for _, mismatch := range []string{"deployment", "tenant"} {
		t.Run(mismatch, func(t *testing.T) {
			identityJSON := freshAdminUserJSON
			if mismatch == "deployment" {
				identityJSON = strings.ReplaceAll(identityJSON, `"deploymentId":"demo"`, `"deploymentId":"other"`)
			}
			h := managedRetrievalFixture(t, identityJSON, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "tenant", mismatch, "deployment mismatch must stop before any upstream request")
				require.Equal(t, "/api/v1/auth/me", r.URL.Path)
				_, _ = w.Write([]byte(`{"data":{"tenant":{"id":20000}}}`))
			})
			for _, request := range []struct{ method, path, body string }{
				{http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/search", `{"query":"question","mode":"hybrid","limit":5}`},
				{http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/documents/doc-1/source", ""},
			} {
				w := managedReadRequest(h, request.method, request.path, request.body)
				require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeRetrievalRejectsCallerScopeAndInvalidPagination(t *testing.T) {
	h := managedRetrievalFixture(t, freshAdminUserJSON, func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached WeKnora") })
	for _, request := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/search", `{"query":"question","mode":"hybrid","limit":5,"knowledge_base_ids":["foreign"]}`},
		{http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/search?tenant_id=20000", `{"query":"question","mode":"hybrid","limit":5}`},
		{http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/search", `{"query":"question","mode":"hybrid","limit":31}`},
		{http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/search", `{"query":"  ","mode":"hybrid","limit":5}`},
		{http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/documents/doc-1/source?page=0", ""},
		{http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/documents/doc-1/source?page_size=101", ""},
		{http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/documents/doc-1/source?page=1&page=2", ""},
		{http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/documents/doc-1/source?tenant_id=20000", ""},
	} {
		w := managedReadRequest(h, request.method, request.path, request.body)
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

// Administrator inspection must not change the ordinary employee route. Even
// an administrator still needs the explicit personal grant there.
func TestManagedInspectionDoesNotBypassEmployeeKnowledgeGrants(t *testing.T) {
	h := managedRetrievalFixture(t, freshAdminUserJSON, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/auth/me", r.URL.Path)
		_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
	})
	employee := readyCR("employee-1", "admin-user")
	employee.Spec.Knowledge = &dev1alpha1.KnowledgeSpec{Bases: []dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}}
	require.NoError(t, h.k8s.Create(t.Context(), employee))
	expectKnowledgeGrant(h.dbmock, "demo", "10000", "kb-1", "user", "admin-user", false)
	w := managedReadRequest(h, http.MethodPost, "/api/v1/knowledge/employees/employee-1/search", `{"query":"question","mode":"hybrid","limit":5,"knowledge_base_ids":["kb-1"]}`)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "KNOWLEDGE_NOT_GRANTED")
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}
