package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
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
			defer file.Close()
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
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "upload_file", "document").WillReturnResult(sqlmock.NewResult(0, 1))
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

func TestManagedKnowledgeRequiresVerifiedDeployment(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	h.cfg.aepDeploymentID = "demo"
	upstreamCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL

	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/managed/readiness", nil)
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeReadiness(w, req, adminIdentity())
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "DEPLOYMENT_MISMATCH")
	assert.Zero(t, upstreamCalls)
}

func configureManagedTest(t *testing.T, h *apiTestPortal, tenant string) {
	t.Helper()
	h.cfg.aepDeploymentID = "demo"
	h.cfg.knowledgeTenantID = tenant
	h.cfg.knowledgeAPIKeySecretField = "WEKNORA_KNOWLEDGE_API_KEY"
	var secret corev1.Secret
	require.NoError(t, h.k8s.Get(context.Background(), types.NamespacedName{Namespace: "digital-employees", Name: "de-platform-creds"}, &secret))
	secret.Data[h.cfg.knowledgeAPIKeySecretField] = []byte("synthetic-test-key")
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

func TestManagedKnowledgeCreateRequiresConfiguredEmbeddingModel(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	h.cfg.knowledgeEmbeddingModelID = "embed-1"
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

func TestManagedKnowledgeCreateUsesFixedPayloadAndPersistsAuditIntent(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	h.cfg.knowledgeEmbeddingModelID = "emb-1"
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
	h.dbmock.ExpectExec(`INSERT INTO knowledge_base_registry`).WillReturnResult(sqlmock.NewResult(0, 1))

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
	h.cfg.knowledgeEmbeddingModelID = "emb-1"
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
	h.dbmock.ExpectExec(`INSERT INTO knowledge_base_registry`).WillReturnError(errors.New("registry unavailable"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/managed/bases", strings.NewReader(`{"name":"Docs"}`))
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeCreateBase(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), `"operationId":"kbop-`)
	assert.Zero(t, deleteCalls)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeAuditIntentFailureBlocksUpstreamCreate(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	h.cfg.knowledgeEmbeddingModelID = "emb-1"
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
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/knowledge/managed/bases/kb-1", strings.NewReader(`{"name":"Docs","config":{"tenant_id":99}}`))
	req.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeUpdateBase(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_REQUEST")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeHasNoImplicitLegacyKeyFallback(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	h.cfg.aepDeploymentID = "demo"
	h.cfg.knowledgeTenantID = "10000"
	h.cfg.knowledgeAPIKeySecretField = "WEKNORA_KNOWLEDGE_API_KEY"
	h.cfg.knowledgeAPIURL = stub.weknora.URL
	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/managed/readiness", nil)
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeReadiness(w, req, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "KNOWLEDGE_CREDENTIAL_UNAVAILABLE")
	assert.NotContains(t, w.Body.String(), "wk-key")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}
