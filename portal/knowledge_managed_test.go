package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestManagedKnowledgeFileUploadUsesOnlyFileAndAuditsDocumentID(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	var uploaded string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge-bases/kb-1":
			_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
		case "/api/v1/knowledge-bases/kb-1/knowledge/file":
			if err := r.ParseMultipartForm(2 << 20); err != nil {
				t.Errorf("multipart parse: %v", err)
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Errorf("file field: %v", err)
				return
			}
			defer func() { _ = file.Close() }()
			data, _ := io.ReadAll(file)
			uploaded = string(data)
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","file_name":"note.txt","parse_status":"pending"}}`))
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "upload_file", "document", "", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", "accepted", 202).WillReturnResult(sqlmock.NewResult(0, 1))
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "note.txt")
	require.NoError(t, err)
	_, _ = part.Write([]byte("synthetic text"))
	require.NoError(t, form.Close())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/documents/file", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeUpload(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Equal(t, "synthetic text", uploaded)
	assert.Contains(t, w.Body.String(), `"id":"doc-1"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeUploadRejectsExtraMultipartFieldsAndOversizeBodies(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		name := "extra-field"
		if oversized {
			name = "oversize"
		}
		t.Run(name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			var uploadCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				case "/api/v1/knowledge-bases/kb-1/knowledge/file":
					uploadCalls++
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			var req *http.Request
			if oversized {
				req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 65<<20)))
				req.Header.Set("Content-Type", "multipart/form-data; boundary=unused")
			} else {
				var body bytes.Buffer
				form := multipart.NewWriter(&body)
				part, err := form.CreateFormFile("file", "a.txt")
				require.NoError(t, err)
				_, _ = part.Write([]byte("body"))
				require.NoError(t, form.WriteField("metadata", "not-allowed"))
				require.NoError(t, form.Close())
				req = httptest.NewRequest(http.MethodPost, "/", &body)
				req.Header.Set("Content-Type", form.FormDataContentType())
			}
			req.SetPathValue("baseID", "kb-1")
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeUpload(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Zero(t, uploadCalls)
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeDocumentsRejectUnknownQueryParameter(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	var documentCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge-bases/kb-1":
			_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
		case "/api/v1/knowledge-bases/kb-1/knowledge":
			documentCalls++
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
	req := httptest.NewRequest(http.MethodGet, "/?tenant_id=999", nil)
	req.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeDocuments(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Zero(t, documentCalls)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeDocumentsRejectsInvalidPagination(t *testing.T) {
	for _, query := range []string{"page=0", "page=-1", "page_size=201", "page=1&page=2", "page=invalid"} {
		t.Run(query, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			var documentCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				case "/api/v1/knowledge-bases/kb-1/knowledge":
					documentCalls++
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
			req.SetPathValue("baseID", "kb-1")
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeDocuments(w, req, adminIdentityWithDeployment("demo"))
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Zero(t, documentCalls)
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeRejectsDocumentTenantDrift(t *testing.T) {
	t.Run("list item belongs to different base", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		configureManagedTest(t, h, "10000")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/auth/me":
				_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
			case "/api/v1/knowledge-bases/kb-1":
				_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
			case "/api/v1/knowledge-bases/kb-1/knowledge":
				_, _ = w.Write([]byte(`{"data":[{"id":"doc-1","knowledge_base_id":"other-kb"}],"total":1,"page":1,"page_size":20}`))
			}
		}))
		defer server.Close()
		h.cfg.knowledgeAPIURL = server.URL
		h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
		req := httptest.NewRequest(http.MethodGet, "/?page=1", nil)
		req.SetPathValue("baseID", "kb-1")
		w := httptest.NewRecorder()
		h.handleManagedKnowledgeDocuments(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.Contains(t, w.Body.String(), "KNOWLEDGE_UPSTREAM_INVALID")
		assert.NoError(t, h.dbmock.ExpectationsWereMet())
	})
	t.Run("upload result belongs to different base", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		configureManagedTest(t, h, "10000")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/auth/me":
				_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
			case "/api/v1/knowledge-bases/kb-1":
				_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
			case "/api/v1/knowledge-bases/kb-1/knowledge/file":
				_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"other-kb"}}`))
			}
		}))
		defer server.Close()
		h.cfg.knowledgeAPIURL = server.URL
		h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
		h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnResult(sqlmock.NewResult(0, 1))
		h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "", "unverified", 502).WillReturnResult(sqlmock.NewResult(0, 1))
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("file", "note.txt")
		require.NoError(t, err)
		_, _ = part.Write([]byte("text"))
		require.NoError(t, form.Close())
		req := httptest.NewRequest(http.MethodPost, "/", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		req.SetPathValue("baseID", "kb-1")
		w := httptest.NewRecorder()
		h.handleManagedKnowledgeUpload(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.Contains(t, w.Body.String(), "KNOWLEDGE_UPSTREAM_UNAVAILABLE")
		assert.Contains(t, w.Body.String(), `"operationId":"kbop-`)
		assert.NoError(t, h.dbmock.ExpectationsWereMet())
	})
}

func TestManagedKnowledgeReadinessSanitizesUpstreamStatusesAndRedirects(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			h.cfg.knowledgeAPIURL = "http://placeholder.invalid"
			var destinationHits int
			dest := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationHits++ }))
			defer dest.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == http.StatusFound {
					http.Redirect(w, r, dest.URL, http.StatusFound)
					return
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"synthetic private upstream detail"}`))
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeReadiness(w, httptest.NewRequest(http.MethodGet, "/", nil), identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
			assert.Equal(t, http.StatusBadGateway, w.Code)
			assert.NotContains(t, w.Body.String(), "private upstream detail")
			assert.Empty(t, w.Header().Get("Location"))
			assert.Zero(t, destinationHits)
		})
	}
}

func TestManagedKnowledgeRequiresVerifiedDeployment(t *testing.T) {
	for _, deployment := range []string{"", "other-deployment"} {
		t.Run("deployment="+deployment, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			h.cfg.aepDeploymentID = "demo"
			upstreamCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls++
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL

			req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/managed/readiness", nil)
			w := httptest.NewRecorder()
			ident := adminIdentity()
			ident.DeploymentID = deployment
			h.handleManagedKnowledgeReadiness(w, req, ident)
			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Contains(t, w.Body.String(), "DEPLOYMENT_MISMATCH")
			assert.Zero(t, upstreamCalls)
		})
	}
}

func TestManagedKnowledgeRejectsWeKnoraTenantMismatchBeforeModelReads(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "90000")
	var businessCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/me" {
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
			return
		}
		businessCalls++
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeReadiness(w, httptest.NewRequest(http.MethodGet, "/", nil), identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "KNOWLEDGE_TENANT_MISMATCH")
	assert.Zero(t, businessCalls)
}

func TestManagedKnowledgeReadinessReportsConfiguredAndUnverifiedModelStates(t *testing.T) {
	tests := []struct {
		name       string
		modelID    string
		modelsJSON string
		wantState  string
	}{
		{name: "not configured", modelsJSON: `[]`, wantState: "not_configured"},
		{name: "configured model absent", modelID: "emb-1", modelsJSON: `[]`, wantState: "unavailable"},
		{name: "configured model wrong type", modelID: "emb-1", modelsJSON: `[ {"id":"emb-1","type":"chat"} ]`, wantState: "unavailable"},
		{name: "configured embedding", modelID: "emb-1", modelsJSON: `[ {"id":"emb-1","type":"Embedding","parameters":{"embedding_parameters":{"dimension":768}},"provider_secret":"do-not-return"} ]`, wantState: "configured"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			h.cfg.managedKnowledge.embeddingModelID = tt.modelID
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/models":
					_, _ = w.Write([]byte(`{"data":` + tt.modelsJSON + `}`))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			w := httptest.NewRecorder()
			ident := adminIdentity()
			ident.DeploymentID = "demo"
			h.handleManagedKnowledgeReadiness(w, httptest.NewRequest(http.MethodGet, "/", nil), ident)
			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var response struct {
				EmbeddingModel struct {
					State        string `json:"state"`
					Availability string `json:"availability"`
				} `json:"embeddingModel"`
				Storage string `json:"storage"`
				Parser  string `json:"parser"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
			assert.Equal(t, tt.wantState, response.EmbeddingModel.State)
			assert.Equal(t, "unverified", response.EmbeddingModel.Availability)
			assert.Equal(t, "unverified", response.Storage)
			assert.Equal(t, "unverified", response.Parser)
			assert.NotContains(t, w.Body.String(), "do-not-return")
		})
	}
}

func TestManagedKnowledgeReadinessRejectsMalformedModelCatalog(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	h.cfg.managedKnowledge.embeddingModelID = "emb-1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/me" {
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
			return
		}
		if r.URL.Path == "/api/v1/models" {
			_, _ = w.Write([]byte(`{"data":{"id":"emb-1","type":"Embedding"}}`))
			return
		}
		t.Errorf("unexpected upstream request %s", r.URL.Path)
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeReadiness(w, httptest.NewRequest(http.MethodGet, "/", nil), adminIdentityWithDeployment("demo"))
	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "KNOWLEDGE_UPSTREAM_UNAVAILABLE")
}

func TestManagedKnowledgeContextRejectsMissingConfigCredentialAndInvalidTenantResponse(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*apiTestPortal)
		authBody    string
		authStatus  int
		wantStatus  int
		wantCode    string
		wantAuthHit bool
	}{
		{name: "tenant not configured", configure: func(h *apiTestPortal) { h.cfg.managedKnowledge.tenantID = "" }, wantStatus: http.StatusServiceUnavailable, wantCode: "KNOWLEDGE_NOT_CONFIGURED"},
		{name: "secret field not configured", configure: func(h *apiTestPortal) { h.cfg.managedKnowledge.apiKeySecretField = "" }, wantStatus: http.StatusServiceUnavailable, wantCode: "KNOWLEDGE_NOT_CONFIGURED"},
		{name: "secret missing", configure: func(h *apiTestPortal) { h.cfg.managedKnowledge.apiKeySecretField = "MISSING_KEY" }, wantStatus: http.StatusServiceUnavailable, wantCode: "KNOWLEDGE_CREDENTIAL_UNAVAILABLE"},
		{name: "upstream tenant rejected", authStatus: http.StatusForbidden, authBody: `{"error":"no"}`, wantStatus: http.StatusBadGateway, wantCode: "KNOWLEDGE_UPSTREAM_UNAVAILABLE", wantAuthHit: true},
		{name: "upstream tenant malformed", authStatus: http.StatusOK, authBody: `not-json`, wantStatus: http.StatusBadGateway, wantCode: "KNOWLEDGE_UPSTREAM_UNAVAILABLE", wantAuthHit: true},
		{name: "upstream tenant missing", authStatus: http.StatusOK, authBody: `{"data":{"tenant":{}}}`, wantStatus: http.StatusBadGateway, wantCode: "KNOWLEDGE_UPSTREAM_UNAVAILABLE", wantAuthHit: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			if tt.configure != nil {
				tt.configure(h)
			}
			authHits := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/auth/me" {
					t.Errorf("unexpected business request %s", r.URL.Path)
					return
				}
				authHits++
				w.WriteHeader(tt.authStatus)
				_, _ = w.Write([]byte(tt.authBody))
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			w := httptest.NewRecorder()
			ident := adminIdentity()
			ident.DeploymentID = "demo"
			h.handleManagedKnowledgeReadiness(w, httptest.NewRequest(http.MethodGet, "/", nil), ident)
			assert.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), tt.wantCode)
			if tt.wantAuthHit {
				assert.Equal(t, 1, authHits)
			} else {
				assert.Zero(t, authHits)
			}
		})
	}
}

func configureManagedTest(t *testing.T, h *apiTestPortal, tenant string) {
	t.Helper()
	h.cfg.aepDeploymentID = "demo"
	h.cfg.managedKnowledge.tenantID = tenant
	h.cfg.managedKnowledge.apiKeySecretField = "WEKNORA_KNOWLEDGE_API_KEY"
	var secret corev1.Secret
	require.NoError(t, h.k8s.Get(context.Background(), types.NamespacedName{Namespace: "digital-employees", Name: "de-platform-creds"}, &secret))
	secret.Data[h.cfg.managedKnowledge.apiKeySecretField] = []byte("synthetic-test-key")
	require.NoError(t, h.k8s.Update(context.Background(), &secret))
}

func TestManagedKnowledgeListOnlyRegisteredAndRejectsTenantDrift(t *testing.T) {
	t.Run("registered base is listed only after upstream tenant confirmation", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		configureManagedTest(t, h, "10000")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/auth/me":
				_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
			case "/api/v1/knowledge-bases/kb-owned":
				_, _ = w.Write([]byte(`{"data":{"id":"kb-owned","name":"Managed","tenant_id":10000,"embedding_model_id":"emb-1"}}`))
			default:
				t.Errorf("unexpected request %s", r.URL.Path)
			}
		}))
		defer server.Close()
		h.cfg.knowledgeAPIURL = server.URL
		h.dbmock.ExpectQuery(`SELECT knowledge_base_id,name_snapshot,created_by,created_at,updated_at FROM knowledge_base_registry`).WithArgs("demo", "10000").WillReturnRows(sqlmock.NewRows([]string{"knowledge_base_id", "name_snapshot", "created_by", "created_at", "updated_at"}).AddRow("kb-owned", "Managed", "u1", time.Now(), time.Now()))
		w := httptest.NewRecorder()
		h.handleManagedKnowledgeBases(w, httptest.NewRequest(http.MethodGet, "/", nil), identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
		assert.Equal(t, 200, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), `"id":"kb-owned"`)
		assert.NotContains(t, w.Body.String(), `"api_key"`)
		assert.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("upstream tenant drift fails closed", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		configureManagedTest(t, h, "10000")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/auth/me" {
				_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"kb-owned","name":"Other tenant","tenant_id":90000}}`))
		}))
		defer server.Close()
		h.cfg.knowledgeAPIURL = server.URL
		h.dbmock.ExpectQuery(`SELECT knowledge_base_id,name_snapshot,created_by,created_at,updated_at FROM knowledge_base_registry`).WithArgs("demo", "10000").WillReturnRows(sqlmock.NewRows([]string{"knowledge_base_id", "name_snapshot", "created_by", "created_at", "updated_at"}).AddRow("kb-owned", "Managed", "u1", time.Now(), time.Now()))
		w := httptest.NewRecorder()
		h.handleManagedKnowledgeBases(w, httptest.NewRequest(http.MethodGet, "/", nil), identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Contains(t, w.Body.String(), "KNOWLEDGE_TENANT_MISMATCH")
		assert.NoError(t, h.dbmock.ExpectationsWereMet())
	})
}

func TestManagedKnowledgeGetRegisteredBase(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/me" {
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","description":"managed","tenant_id":10000,"type":"document"}}`))
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeGetBase(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"description":"managed"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeDocumentsForwardsWhitelistedPagination(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	var docsQuery bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge-bases/kb-1":
			_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
		case "/api/v1/knowledge-bases/kb-1/knowledge":
			docsQuery = r.URL.Query().Get("page") == "2" && r.URL.Query().Get("page_size") == "20" && r.URL.Query().Get("keyword") == "a/b"
			_, _ = w.Write([]byte(`{"data":[{"id":"doc-1","knowledge_base_id":"kb-1","name":"Note","file_name":"note.txt","parse_status":"completed","created_at":"2026-10-09T05:00:00Z","provider_secret":"must-not-return"}],"total":1,"page":2,"page_size":20}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
	req := httptest.NewRequest(http.MethodGet, "/?page=2&page_size=20&keyword=a%2Fb", nil)
	req.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeDocuments(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, docsQuery)
	assert.Contains(t, w.Body.String(), `"id":"doc-1"`)
	assert.NotContains(t, w.Body.String(), "provider_secret")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeUnregisteredBaseDoesNotCallUpstreamOrDelete(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	baseCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/knowledge-bases") {
			baseCalls++
		}
		_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "old-kb").WillReturnError(sql.ErrNoRows)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	req.SetPathValue("baseID", "old-kb")
	h.handleManagedKnowledgeDeleteBase(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Zero(t, baseCalls)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeDeleteReferenceCheckIncludesPendingRequests(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	h.dbmock.ExpectQuery(`SELECT spec_json FROM requests WHERE state='pending'`).WillReturnRows(sqlmock.NewRows([]string{"spec_json"}).AddRow(`{"spec":{"knowledge":{"type":"weknora","bases":[{"id":"kb-1","name":"Docs"}]}}}`))
	used, err := h.managedKnowledgeReferenced(context.Background(), "kb-1")
	require.NoError(t, err)
	assert.True(t, used)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeDeleteReferenceCheckIncludesEmployeeCRs(t *testing.T) {
	stub := newServicesStubs(t)
	base := readyCR("linked-helper", "admin-user")
	base.Spec.Knowledge = &dev1alpha1.KnowledgeSpec{Type: "weknora", Bases: []dev1alpha1.KnowledgeBaseRef{{ID: "kb-1", Name: "Docs"}}}
	h := newServicesHarness(t, stub, base)
	used, err := h.managedKnowledgeReferenced(context.Background(), "kb-1")
	require.NoError(t, err)
	assert.True(t, used)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeCreateRequiresConfiguredEmbeddingModel(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	h.cfg.managedKnowledge.embeddingModelID = "embed-1"
	calls := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/api/v1/auth/me" {
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
			return
		}
		if r.URL.Path == "/api/v1/models" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL

	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/managed/bases", strings.NewReader(`{"name":"Docs","description":"test"}`))
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeCreateBase(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "KNOWLEDGE_MODEL_UNAVAILABLE")
	assert.Equal(t, []string{"GET /api/v1/auth/me", "GET /api/v1/models"}, calls)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeCreateRejectsNonEmbeddingModel(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	h.cfg.managedKnowledge.embeddingModelID = "model-1"
	var createCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-1","type":"LLM"}]}`))
		case "/api/v1/knowledge-bases":
			createCalls++
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/managed/bases", strings.NewReader(`{"name":"Docs"}`))
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeCreateBase(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Zero(t, createCalls)
	assert.Contains(t, w.Body.String(), "KNOWLEDGE_MODEL_UNAVAILABLE")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeCreateUsesFixedPayloadAndPersistsAuditIntent(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	h.cfg.managedKnowledge.embeddingModelID = "emb-1"
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"emb-1","name":"Embedding","type":"Embedding","api_key":"never-forward"}]}`))
		case "/api/v1/knowledge-bases":
			if r.Method != http.MethodPost {
				t.Errorf("method = %s", r.Method)
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&posted))
			_, _ = w.Write([]byte(`{"data":{"id":"kb-new","name":"Docs","description":"test","tenant_id":10000,"embedding_model_id":"emb-1","type":"document"}}`))
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectBegin()
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET outcome='succeeded'`).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_base_registry`).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectCommit()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/managed/bases", strings.NewReader(`{"name":"Docs","description":"test"}`))
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeCreateBase(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, "document", posted["type"])
	assert.Equal(t, "emb-1", posted["embedding_model_id"])
	assert.NotContains(t, posted, "tenant_id")
	assert.NotContains(t, posted, "api_key")
	assert.NotContains(t, posted, "storage_config")
	chunking := posted["chunking_config"].(map[string]any)
	assert.Equal(t, float64(512), chunking["chunk_size"])
	assert.Equal(t, false, chunking["enable_multimodal"])
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeCreateRegistryFailureReturnsOperationIDWithoutCompensationDelete(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	h.cfg.managedKnowledge.embeddingModelID = "emb-1"
	var deleteCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"emb-1","type":"Embedding"}]}`))
		case "/api/v1/knowledge-bases":
			_, _ = w.Write([]byte(`{"data":{"id":"kb-new","name":"Docs","tenant_id":10000}}`))
		default:
			if r.Method == http.MethodDelete {
				deleteCalls++
			}
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectBegin()
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET outcome='succeeded'`).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_base_registry`).WillReturnError(errors.New("registry unavailable"))
	h.dbmock.ExpectRollback()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/managed/bases", strings.NewReader(`{"name":"Docs"}`))
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeCreateBase(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), `"operationId":"kbop-`)
	assert.Contains(t, w.Body.String(), `"resourceId":"kb-new"`)
	assert.Zero(t, deleteCalls)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeCreateAuditOutcomeFailureKeepsOperationID(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	h.cfg.managedKnowledge.embeddingModelID = "emb-1"
	var deletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"emb-1","type":"Embedding"}]}`))
		case "/api/v1/knowledge-bases":
			_, _ = w.Write([]byte(`{"data":{"id":"kb-new","name":"Docs","tenant_id":10000}}`))
		default:
			if r.Method == http.MethodDelete {
				deletes++
			}
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "kb-new").WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectBegin()
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET outcome='succeeded'`).WillReturnError(errors.New("audit update unavailable"))
	h.dbmock.ExpectRollback()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"Docs"}`))
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeCreateBase(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), `"operationId":"kbop-`)
	assert.Zero(t, deletes)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeMutationAuditIntentFailureBlocksUpdateDeleteAndUpload(t *testing.T) {
	for _, action := range []string{"update", "delete", "upload"} {
		t.Run(action, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			var mutations int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge-bases/kb-1":
					if r.Method != http.MethodGet {
						mutations++
					}
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				case "/api/v1/knowledge-bases/kb-1/knowledge/file":
					mutations++
				default:
					t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			if action == "delete" {
				h.dbmock.ExpectQuery(`SELECT spec_json FROM requests WHERE state='pending'`).WillReturnRows(sqlmock.NewRows([]string{"spec_json"}))
			}
			h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnError(errors.New("audit unavailable"))
			var req *http.Request
			switch action {
			case "update":
				req = httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"name":"Changed"}`))
			case "delete":
				req = httptest.NewRequest(http.MethodDelete, "/", nil)
			case "upload":
				var body bytes.Buffer
				form := multipart.NewWriter(&body)
				part, err := form.CreateFormFile("file", "a.txt")
				require.NoError(t, err)
				_, _ = part.Write([]byte("body"))
				require.NoError(t, form.Close())
				req = httptest.NewRequest(http.MethodPost, "/", &body)
				req.Header.Set("Content-Type", form.FormDataContentType())
			}
			req.SetPathValue("baseID", "kb-1")
			w := httptest.NewRecorder()
			ident := identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}}
			switch action {
			case "update":
				h.handleManagedKnowledgeUpdateBase(w, req, ident)
			case "delete":
				h.handleManagedKnowledgeDeleteBase(w, req, ident)
			case "upload":
				h.handleManagedKnowledgeUpload(w, req, ident)
			}
			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.Zero(t, mutations)
			assert.Contains(t, w.Body.String(), "AUDIT_UNAVAILABLE")
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeAuditIntentFailureBlocksUpstreamCreate(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	h.cfg.managedKnowledge.embeddingModelID = "emb-1"
	var createCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"emb-1","type":"Embedding"}]}`))
		case "/api/v1/knowledge-bases":
			createCalls++
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnError(errors.New("audit unavailable"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/managed/bases", strings.NewReader(`{"name":"Docs"}`))
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeCreateBase(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Zero(t, createCalls)
	assert.Contains(t, w.Body.String(), "AUDIT_UNAVAILABLE")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeRejectsUnknownCreateFields(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/managed/bases", strings.NewReader(`{"name":"Docs","tenant_id":"attacker"}`))
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeCreateBase(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_REQUEST")
	assert.NotContains(t, w.Body.String(), "attacker")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeUpdateRejectsUnknownFields(t *testing.T) {
	for _, body := range []string{`{"name":"Docs","config":{"tenant_id":99}}`, `{"name":"Docs"} {"name":"extra"}`} {
		t.Run(body, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/knowledge/managed/bases/kb-1", strings.NewReader(body))
			req.SetPathValue("baseID", "kb-1")
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeUpdateBase(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "INVALID_REQUEST")
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeUpdateBaseSuccessUpdatesUpstreamAuditAndSnapshot(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge-bases/kb-1":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Old","tenant_id":10000}}`))
				return
			}
			if r.Method == http.MethodPut {
				var body map[string]string
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(t, "New", body["name"])
				_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"New","description":"updated","tenant_id":10000}}`))
				return
			}
			t.Errorf("unexpected method %s", r.Method)
		default:
			t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Old"))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "update", "knowledge-base", "kb-1", "").WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "kb-1", "succeeded", 200).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_base_registry SET name_snapshot`).WithArgs("demo", "10000", "kb-1", "New").WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"name":" New "}`))
	req.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeUpdateBase(w, req, adminIdentityWithDeployment("demo"))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"name":"New"`)
	assert.Contains(t, w.Body.String(), `"operationId":"kbop-`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeDeleteBaseSuccessDeletesUpstreamAndRegistry(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge-bases/kb-1":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				return
			}
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			t.Errorf("unexpected method %s", r.Method)
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
	h.dbmock.ExpectQuery(`SELECT spec_json FROM requests WHERE state='pending'`).WillReturnRows(sqlmock.NewRows([]string{"spec_json"}))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "delete", "knowledge-base", "kb-1", "").WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "kb-1", "succeeded", 200).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`DELETE FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodDelete, "/", nil)
	req.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeDeleteBase(w, req, adminIdentityWithDeployment("demo"))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"deleted":true`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeDeleteBaseHandlesUpstreamAndRegistryFailures(t *testing.T) {
	for _, failRegistry := range []bool{false, true} {
		name, upstreamStatus, outcome, auditStatus := "upstream-rejected", http.StatusForbidden, "failed", http.StatusForbidden
		if failRegistry {
			name, upstreamStatus, outcome, auditStatus = "registry-delete-failed", http.StatusNoContent, "succeeded", http.StatusOK
		}
		t.Run(name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge-bases/kb-1":
					if r.Method == http.MethodGet {
						_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
						return
					}
					w.WriteHeader(upstreamStatus)
				default:
					t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			h.dbmock.ExpectQuery(`SELECT spec_json FROM requests WHERE state='pending'`).WillReturnRows(sqlmock.NewRows([]string{"spec_json"}))
			h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "kb-1", outcome, auditStatus).WillReturnResult(sqlmock.NewResult(0, 1))
			if failRegistry {
				h.dbmock.ExpectExec(`DELETE FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnError(errors.New("registry unavailable"))
			}
			req := httptest.NewRequest(http.MethodDelete, "/", nil)
			req.SetPathValue("baseID", "kb-1")
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeDeleteBase(w, req, adminIdentityWithDeployment("demo"))
			if failRegistry {
				assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
				assert.Contains(t, w.Body.String(), "REGISTRY_WRITE_FAILED")
			} else {
				assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
				assert.Contains(t, w.Body.String(), "KNOWLEDGE_UPSTREAM_REJECTED")
			}
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeGetBaseRejectsInvalidUnregisteredAndDriftedIDs(t *testing.T) {
	tests := []struct {
		name         string
		id           string
		registryRows *sqlmock.Rows
		upstreamBody string
		wantStatus   int
		wantQueries  bool
	}{
		{name: "invalid id", id: "../other", wantStatus: http.StatusNotFound},
		{name: "not registered", id: "kb-1", registryRows: sqlmock.NewRows([]string{"name_snapshot"}), wantStatus: http.StatusNotFound, wantQueries: true},
		{name: "upstream tenant drift", id: "kb-1", registryRows: sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"), upstreamBody: `{"data":{"id":"kb-1","name":"Docs","tenant_id":99999}}`, wantStatus: http.StatusBadGateway, wantQueries: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			upstreamReads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge-bases/kb-1":
					upstreamReads++
					_, _ = w.Write([]byte(tt.upstreamBody))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			if tt.wantQueries {
				h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", tt.id).WillReturnRows(tt.registryRows)
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("baseID", tt.id)
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeGetBase(w, req, adminIdentityWithDeployment("demo"))
			assert.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			if tt.name == "upstream tenant drift" {
				assert.Equal(t, 1, upstreamReads)
			} else {
				assert.Zero(t, upstreamReads)
			}
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeListRegistryFailsClosedOnDatabaseAndUpstreamErrors(t *testing.T) {
	tests := []struct {
		name         string
		rows         *sqlmock.Rows
		queryErr     error
		upstream     string
		wantStatus   int
		wantQuery    bool
		wantUpstream bool
	}{
		{name: "query error", queryErr: errors.New("database unavailable"), wantStatus: http.StatusInternalServerError, wantQuery: true},
		{name: "scan error", rows: sqlmock.NewRows([]string{"knowledge_base_id", "name_snapshot", "created_by", "created_at", "updated_at"}).AddRow("kb-1", "Docs", "admin", "not-a-timestamp", time.Now()), wantStatus: http.StatusInternalServerError, wantQuery: true},
		{name: "upstream tenant drift", rows: sqlmock.NewRows([]string{"knowledge_base_id", "name_snapshot", "created_by", "created_at", "updated_at"}).AddRow("kb-1", "Docs", "admin", time.Now(), time.Now()), upstream: `{"data":{"id":"kb-1","name":"Docs","tenant_id":99999}}`, wantStatus: http.StatusConflict, wantQuery: true, wantUpstream: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			upstreamReads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge-bases/kb-1":
					upstreamReads++
					_, _ = w.Write([]byte(tt.upstream))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			if tt.wantQuery {
				expect := h.dbmock.ExpectQuery(`SELECT knowledge_base_id,name_snapshot,created_by,created_at,updated_at FROM knowledge_base_registry`).WithArgs("demo", "10000")
				if tt.queryErr != nil {
					expect.WillReturnError(tt.queryErr)
				} else {
					expect.WillReturnRows(tt.rows)
				}
			}
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeBases(w, httptest.NewRequest(http.MethodGet, "/", nil), adminIdentityWithDeployment("demo"))
			assert.Equal(t, tt.wantStatus, w.Code, w.Body.String())
			if tt.wantUpstream {
				assert.Equal(t, 1, upstreamReads)
			} else {
				assert.Zero(t, upstreamReads)
			}
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func adminIdentityWithDeployment(deployment string) identity {
	ident := adminIdentity()
	ident.DeploymentID = deployment
	return ident
}

func TestManagedAuditUpdatesRequireExactlyOnePersistedIntent(t *testing.T) {
	tests := []struct {
		name     string
		remember bool
		result   driver.Result
		queryErr error
		wantErr  bool
	}{
		{name: "finish update error", queryErr: errors.New("write failed"), wantErr: true},
		{name: "finish rows affected error", result: sqlmock.NewErrorResult(errors.New("rows affected failed")), wantErr: true},
		{name: "finish missing intent", result: sqlmock.NewResult(0, 0), wantErr: true},
		{name: "finish succeeds", result: sqlmock.NewResult(0, 1)},
		{name: "remember update error", remember: true, queryErr: errors.New("write failed"), wantErr: true},
		{name: "remember rows affected error", remember: true, result: sqlmock.NewErrorResult(errors.New("rows affected failed")), wantErr: true},
		{name: "remember missing intent", remember: true, result: sqlmock.NewResult(0, 0), wantErr: true},
		{name: "remember succeeds", remember: true, result: sqlmock.NewResult(0, 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			query := `UPDATE knowledge_audit_events SET resource_id=$2,outcome=$3,status_code=$4 WHERE operation_id=$1`
			args := []driver.Value{"kbop-1", "kb-1", "succeeded", 200}
			if tt.remember {
				query = `UPDATE knowledge_audit_events SET resource_id=$2 WHERE operation_id=$1`
				args = []driver.Value{"kbop-1", "kb-1"}
			}
			expect := h.dbmock.ExpectExec(regexp.QuoteMeta(query)).WithArgs(args...)
			if tt.queryErr != nil {
				expect.WillReturnError(tt.queryErr)
			} else {
				expect.WillReturnResult(tt.result)
			}
			var err error
			if tt.remember {
				err = h.rememberManagedAuditResource(context.Background(), "kbop-1", "kb-1")
			} else {
				err = h.finishManagedAudit(context.Background(), "kbop-1", "kb-1", "succeeded", 200)
			}
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeHasNoImplicitLegacyKeyFallback(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	h.cfg.aepDeploymentID = "demo"
	h.cfg.managedKnowledge.tenantID = "10000"
	h.cfg.managedKnowledge.apiKeySecretField = "WEKNORA_KNOWLEDGE_API_KEY"
	h.cfg.knowledgeAPIURL = stub.weknora.URL
	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/managed/readiness", nil)
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeReadiness(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "KNOWLEDGE_CREDENTIAL_UNAVAILABLE")
	assert.NotContains(t, w.Body.String(), "wk-key")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}
