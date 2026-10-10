package main

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEmployeeKnowledgeSearchReturnsVerifiedNativeHitAndSource(t *testing.T) {
	var aepCalls int
	aep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		aepCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"user-1","kind":"human","teamIds":[]},"roles":[],"deploymentId":"dep-1"}`))
	}))
	defer aep.Close()
	p, mock := newKnowledgeAccessPortalFixture(t,
		knowledgeAccessEmployee([]dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}, &dev1alpha1.VisibilitySpec{Mode: "all"}),
		knowledgeAEPResponse(nil))
	// Use the same live AEP fixture for the access verifier.
	p.aep = &aepAuthenticator{baseURL: aep.URL, http: aep.Client()}
	p.cfg.aepDeploymentID = "dep-1"
	p.cfg.managedKnowledge.tenantID = "10000"
	p.cfg.managedKnowledge.apiKeySecretField = "WEKNORA_KNOWLEDGE_API_KEY"
	p.cfg.credentialsSecret = "de-platform-creds"
	p.cfg.knowledgeAPIURL = ""
	require.NoError(t, p.k8s.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "knowledge-test", Name: "de-platform-creds"},
		Data:       map[string][]byte{"WEKNORA_KNOWLEDGE_API_KEY": []byte("server-only-test-key")},
	}))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge-bases/kb-1":
			_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Allowed","tenant_id":10000}}`))
		case "/api/v1/knowledge-bases/kb-1/hybrid-search":
			_, _ = w.Write([]byte(`{"data":[{"id":"chunk-1","content":"answer","knowledge_id":"doc-1","knowledge_title":"untrusted title","score":0.91}]}`))
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Verified title"}}`))
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	p.cfg.knowledgeAPIURL = upstream.URL

	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).WithArgs("stored-aep-token").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	expectKnowledgeGrant(mock, "dep-1", "10000", "kb-1", "user", "user-1", true)
	mock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("dep-1", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Allowed"))
	// managedFetchDocument verifies its KB registration before fetching source metadata.
	mock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("dep-1", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Allowed"))
	mock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "dep-1", "10000", "user-1", "employee_search", "knowledge_base", "kb-1", "").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).WithArgs(sqlmock.AnyArg(), "kb-1", "succeeded", http.StatusOK).WillReturnResult(sqlmock.NewResult(0, 1))

	r := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/employees/employee-1/search", strings.NewReader(`{"query":"question","knowledge_base_ids":["kb-1"],"mode":"hybrid","limit":5}`))
	r.SetPathValue("name", "employee-1")
	r.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: "stored-aep-token"})
	w := httptest.NewRecorder()
	p.handleEmployeeKnowledgeSearch(w, r, identity{Token: "stored-aep-token", UserID: "user-1", DeploymentID: "dep-1"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"score":0.91`)
	require.Contains(t, w.Body.String(), `"knowledge_base_id":"kb-1","document_id":"doc-1","chunk_id":"chunk-1","title":"Verified title"`)
	require.NotContains(t, w.Body.String(), "untrusted title")
	require.Equal(t, 2, aepCalls, "requester identity is revalidated before context creation and access resolution")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEmployeeKnowledgeSearchDoesNotReturnCrossBaseHitContent(t *testing.T) {
	var upstreamCalls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls = append(upstreamCalls, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge-bases/kb-1":
			_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Allowed","tenant_id":10000}}`))
		case "/api/v1/knowledge-bases/kb-1/hybrid-search":
			_, _ = w.Write([]byte(`{"data":[{"id":"chunk-foreign","content":"foreign confidential text","knowledge_id":"doc-foreign","score":0.99}]}`))
		case "/api/v1/knowledge/doc-foreign":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-foreign","knowledge_base_id":"kb-foreign","tenant_id":10000,"name":"Foreign source"}}`))
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	p, mock := newKnowledgeAccessPortalFixture(t,
		knowledgeAccessEmployee([]dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}, &dev1alpha1.VisibilitySpec{Mode: "all"}),
		knowledgeAEPResponse(nil))
	p.cfg.aepDeploymentID = "dep-1"
	p.cfg.managedKnowledge.tenantID = "10000"
	p.cfg.managedKnowledge.apiKeySecretField = "WEKNORA_KNOWLEDGE_API_KEY"
	p.cfg.credentialsSecret = "de-platform-creds"
	p.cfg.knowledgeAPIURL = upstream.URL
	require.NoError(t, p.k8s.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "knowledge-test", Name: "de-platform-creds"},
		Data:       map[string][]byte{"WEKNORA_KNOWLEDGE_API_KEY": []byte("server-only-test-key")},
	}))
	p.sessions = newSessionStore(time.Hour)
	sessionToken, csrf := p.sessions.issue(identity{
		Token: "stored-aep-token", UserID: "user-1", DeploymentID: "dep-1", TeamsFetchedAt: time.Now(),
	})
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
		WithArgs(sessionToken).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	expectKnowledgeGrant(mock, "dep-1", "10000", "kb-1", "user", "user-1", true)
	mock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("dep-1", "10000", "kb-1").
		WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Allowed"))
	mock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("dep-1", "10000", "kb-1").
		WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Allowed"))

	server := httptest.NewServer(p.routes())
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/knowledge/employees/employee-1/search", strings.NewReader(`{"query":"question","knowledge_base_ids":["kb-1"],"mode":"hybrid","limit":5}`))
	require.NoError(t, err)
	request.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: sessionToken})
	request.Header.Set("X-CSRF-Token", csrf)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadGateway, response.StatusCode, string(body))
	assert.NotContains(t, string(body), "foreign confidential text")
	assert.NotContains(t, string(body), "chunk-foreign")
	assert.Equal(t, []string{
		"/api/v1/auth/me",
		"/api/v1/knowledge-bases/kb-1",
		"/api/v1/knowledge-bases/kb-1/hybrid-search",
		"/api/v1/knowledge-bases/kb-1",
		"/api/v1/knowledge/doc-foreign",
	}, upstreamCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEmployeeKnowledgeSearchRejectsInvalidRequestBeforeAuthorization(t *testing.T) {
	for _, body := range []string{
		`{`,
		`{"query":"  ","knowledge_base_ids":["kb-1"],"mode":"hybrid","limit":5}`,
		`{"query":"question","knowledge_base_ids":[],"mode":"hybrid","limit":5}`,
		`{"query":"question","knowledge_base_ids":["kb-1","kb-1"],"mode":"hybrid","limit":5}`,
		`{"query":"question","knowledge_base_ids":[".."],"mode":"hybrid","limit":5}`,
		`{"query":"question","knowledge_base_ids":["kb-1"],"mode":"other","limit":5}`,
		`{"query":"question","knowledge_base_ids":["kb-1"],"mode":"hybrid","limit":31}`,
		`{"query":"question","knowledge_base_ids":["kb-1"],"mode":"hybrid","limit":0}`,
		`{"query":"` + strings.Repeat("q", 4001) + `","knowledge_base_ids":["kb-1"],"mode":"hybrid","limit":5}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.SetPathValue("name", "employee-1")
		response := httptest.NewRecorder()
		(&portal{}).handleEmployeeKnowledgeSearch(response, req, identity{})
		assert.Equal(t, http.StatusBadRequest, response.Code, body)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.SetPathValue("name", "../bad")
	response := httptest.NewRecorder()
	(&portal{}).handleEmployeeKnowledgeSearch(response, req, identity{})
	assert.Equal(t, http.StatusBadRequest, response.Code)
}

func TestEmployeeKnowledgeSearchRejectsMissingOrRevokedSessionBeforeAEP(t *testing.T) {
	requestBody := `{"query":"question","knowledge_base_ids":["kb-1"],"mode":"hybrid","limit":5}`
	for _, tc := range []struct {
		name      string
		cookie    string
		expectSQL bool
	}{
		{name: "missing cookie"},
		{name: "revoked durable session", cookie: "revoked-token", expectSQL: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, mock := newKnowledgeAccessPortalFixture(t, nil, knowledgeAEPResponse(nil))
			if tc.expectSQL {
				mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
					WithArgs(tc.cookie).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
			}
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(requestBody))
			req.SetPathValue("name", "employee-1")
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: tc.cookie})
			}
			response := httptest.NewRecorder()
			p.handleEmployeeKnowledgeSearch(response, req, identity{Token: tc.cookie, UserID: "user-1", DeploymentID: "dep-1"})
			assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
			assert.Equal(t, "private, no-store", response.Header().Get("Cache-Control"))
		})
	}
}

func TestEmployeeKnowledgeDocumentRejectsAmbiguousOrInvalidPaginationBeforeAuthorization(t *testing.T) {
	for _, rawQuery := range []string{"?unknown=x", "?page=1&page=2", "?page=0", "?page_size=101", "?page_size=nope"} {
		req := httptest.NewRequest(http.MethodGet, "/"+rawQuery, nil)
		req.SetPathValue("name", "employee-1")
		req.SetPathValue("documentID", "doc-1")
		response := httptest.NewRecorder()
		(&portal{}).handleEmployeeKnowledgeDocument(response, req, identity{})
		assert.Equal(t, http.StatusBadRequest, response.Code, rawQuery)
	}
}

func TestEmployeeKnowledgeDocumentAlwaysSerializesOptionalMetadata(t *testing.T) {
	for _, tc := range []struct {
		name       string
		docType    string
		chunkPage  string
		wantStatus int
	}{
		{name: "URL source", docType: "url", chunkPage: `{"data":[],"total":0,"page":1,"page_size":20}`, wantStatus: http.StatusOK},
		{name: "FAQ source", docType: "faq", chunkPage: `{"data":[],"total":0,"page":1,"page_size":20}`, wantStatus: http.StatusOK},
		{name: "upstream pagination mismatch", docType: "url", chunkPage: `{"data":[],"total":0,"page":2,"page_size":20}`, wantStatus: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"type":"` + tc.docType + `","name":"Source"}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Allowed","tenant_id":10000}}`))
				case "/api/v1/chunks/doc-1":
					assert.Equal(t, "1", r.URL.Query().Get("page"))
					assert.Equal(t, "20", r.URL.Query().Get("page_size"))
					_, _ = w.Write([]byte(tc.chunkPage))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			p, mock := newKnowledgeAccessPortalFixture(t,
				knowledgeAccessEmployee([]dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}, &dev1alpha1.VisibilitySpec{Mode: "all"}),
				knowledgeAEPResponse(nil))
			p.cfg.aepDeploymentID = "dep-1"
			p.cfg.managedKnowledge.tenantID = "10000"
			p.cfg.managedKnowledge.apiKeySecretField = "WEKNORA_KNOWLEDGE_API_KEY"
			p.cfg.credentialsSecret = "de-platform-creds"
			p.cfg.knowledgeAPIURL = upstream.URL
			require.NoError(t, p.k8s.Create(context.Background(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "knowledge-test", Name: "de-platform-creds"},
				Data:       map[string][]byte{"WEKNORA_KNOWLEDGE_API_KEY": []byte("server-only-test-key")},
			}))
			token := "stored-aep-token"
			mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
				WithArgs(token).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
			mock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("dep-1", "10000", "kb-1").
				WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Allowed"))
			expectKnowledgeGrant(mock, "dep-1", "10000", "kb-1", "user", "user-1", true)
			if tc.wantStatus == http.StatusOK {
				mock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "dep-1", "10000", "user-1", "employee_read_source", "document", "doc-1", "kb-1").
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
					WithArgs(sqlmock.AnyArg(), "doc-1", "succeeded", http.StatusOK).WillReturnResult(sqlmock.NewResult(0, 1))
			}

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("name", "employee-1")
			req.SetPathValue("documentID", "doc-1")
			req.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: token})
			response := httptest.NewRecorder()
			p.handleEmployeeKnowledgeDocument(response, req, identity{Token: token, UserID: "user-1", DeploymentID: "dep-1"})
			require.Equal(t, tc.wantStatus, response.Code, response.Body.String())
			if tc.wantStatus == http.StatusOK {
				assert.Contains(t, response.Body.String(), `"file_name":""`)
				assert.Contains(t, response.Body.String(), `"file_type":""`)
				assert.Contains(t, response.Body.String(), `"source":""`)
			}
			assert.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestEmployeeKnowledgeSourceImageAndDownloadDenyForeignDeploymentKB(t *testing.T) {
	for _, operation := range []string{"source", "image", "download"} {
		t.Run(operation, func(t *testing.T) {
			var upstreamCalls []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls = append(upstreamCalls, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-foreign":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-foreign","knowledge_base_id":"kb-foreign","tenant_id":10000,"name":"Foreign source"}}`))
				default:
					t.Errorf("foreign source read must stop before resource fetch: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			p, mock := newKnowledgeAccessPortalFixture(t,
				knowledgeAccessEmployee([]dev1alpha1.KnowledgeBaseRef{{ID: "kb-bound"}}, &dev1alpha1.VisibilitySpec{Mode: "all"}),
				knowledgeAEPResponse(nil))
			p.cfg.aepDeploymentID = "dep-1"
			p.cfg.managedKnowledge.tenantID = "10000"
			p.cfg.managedKnowledge.apiKeySecretField = "WEKNORA_KNOWLEDGE_API_KEY"
			p.cfg.credentialsSecret = "de-platform-creds"
			p.cfg.knowledgeAPIURL = upstream.URL
			require.NoError(t, p.k8s.Create(context.Background(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: "knowledge-test", Name: "de-platform-creds"},
				Data:       map[string][]byte{"WEKNORA_KNOWLEDGE_API_KEY": []byte("server-only-test-key")},
			}))
			aep := httptest.NewServer(knowledgeAEPResponse(nil))
			defer aep.Close()
			p.aep = &aepAuthenticator{baseURL: aep.URL, http: aep.Client()}
			mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
				WithArgs("session-token").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
			mock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("dep-1", "10000", "kb-foreign").WillReturnError(sql.ErrNoRows)

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.SetPathValue("name", "employee-1")
			r.SetPathValue("documentID", "doc-foreign")
			r.SetPathValue("chunkID", "chunk-foreign")
			r.SetPathValue("index", "0")
			r.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: "session-token"})
			w := httptest.NewRecorder()
			ident := identity{Token: "stored-aep-token", UserID: "user-1", DeploymentID: "dep-1"}
			switch operation {
			case "source":
				p.handleEmployeeKnowledgeDocument(w, r, ident)
			case "image":
				p.handleEmployeeKnowledgeChunkImage(w, r, ident)
			case "download":
				p.handleEmployeeKnowledgeDocumentDownload(w, r, ident)
			}
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			require.Equal(t, []string{"/api/v1/auth/me", "/api/v1/knowledge/doc-foreign"}, upstreamCalls)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
