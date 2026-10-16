package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeManagedOperationResultProjectsStableDTO(t *testing.T) {
	got, err := decodeManagedOperationResult([]byte(`{"success":true,"data":{"moved_count":3,"folder_path":"manuals/2026"}}`), "move_documents", map[string]any{"folder_path": "manuals/2026"})
	require.NoError(t, err)
	assert.Equal(t, "succeeded", got.Outcome)
	require.NotNil(t, got.AffectedCount)
	assert.EqualValues(t, 3, *got.AffectedCount)
	assert.Equal(t, "manuals/2026", got.FolderPath)

	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, `{"outcome":"succeeded","affectedCount":3,"folderPath":"manuals/2026"}`, string(encoded))
}

func TestDecodeManagedOperationResultRejectsUnverifiedResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		action string
		input  any
	}{
		{name: "missing success", raw: `{}`, action: "update_faq_tags"},
		{name: "false success", raw: `{"success":false}`, action: "update_faq_tags"},
		{name: "unknown top level", raw: `{"success":true,"token":"must-not-pass"}`, action: "update_faq_tags"},
		{name: "trailing JSON", raw: `{"success":true} {"success":true}`, action: "update_faq_tags"},
		{name: "missing folder result", raw: `{"success":true}`, action: "move_documents", input: map[string]string{"folder_path": "new"}},
		{name: "negative count", raw: `{"success":true,"data":{"moved_count":-1,"folder_path":"new"}}`, action: "move_documents", input: map[string]string{"folder_path": "new"}},
		{name: "mismatched path", raw: `{"success":true,"data":{"moved_count":1,"folder_path":"other"}}`, action: "rename_folder", input: map[string]string{"to": "new"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeManagedOperationResult([]byte(tc.raw), tc.action, tc.input)
			assert.Error(t, err)
		})
	}
	// The pinned WeKnora FAQ batch DTO legitimately includes message/code;
	// the portal accepts but never reflects these upstream strings.
	got, err := decodeManagedOperationResult([]byte(`{"success":true,"message":"updated","code":"OK"}`), "update_faq_tags", nil)
	require.NoError(t, err)
	assert.Equal(t, managedOperationResult{Outcome: "succeeded"}, got)
}

func TestManagedChunkIndexStatusIsNotCollapsed(t *testing.T) {
	for _, status := range []string{"ready", "processing", "failed"} {
		assert.True(t, managedChunkIndexStatusValid(status))
	}
	for _, status := range []string{"", "unknown", "success", "pending"} {
		assert.False(t, managedChunkIndexStatusValid(status))
	}
}

func TestManagedFAQBatchDeleteRouteIsRegisteredAtHandlerContractPath(t *testing.T) {
	h := newAPIHarness(t)
	routes := h.routes()

	newPath := httptest.NewRequest("DELETE", "/api/v1/knowledge/managed/bases/kb-1/faq/batch/delete", nil)
	newResponse := httptest.NewRecorder()
	routes.ServeHTTP(newResponse, newPath)
	assert.Equal(t, 401, newResponse.Code, "registered route reaches the authentication boundary")

	oldPath := httptest.NewRequest("DELETE", "/api/v1/knowledge/managed/bases/kb-1/faq/entries", nil)
	oldResponse := httptest.NewRecorder()
	routes.ServeHTTP(oldResponse, oldPath)
	assert.Equal(t, 405, oldResponse.Code, "legacy entries path remains read/create/update only")
}

func TestManagedKnowledgeOperationsRoutesAreRegisteredBehindAdminAuth(t *testing.T) {
	h := newAPIHarness(t)
	routes := h.routes()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/tags"},
		{http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/tags"},
		{http.MethodPut, "/api/v1/knowledge/managed/bases/kb-1/tags/tag-1"},
		{http.MethodDelete, "/api/v1/knowledge/managed/bases/kb-1/tags/tag-1"},
		{http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/folders"},
		{http.MethodPut, "/api/v1/knowledge/managed/bases/kb-1/folders"},
		{http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/folders/move"},
		{http.MethodPut, "/api/v1/knowledge/managed/bases/kb-1/document-tags"},
		{http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/faq/entries"},
		{http.MethodPost, "/api/v1/knowledge/managed/bases/kb-1/faq/entries"},
		{http.MethodPut, "/api/v1/knowledge/managed/bases/kb-1/faq/entries/1"},
		{http.MethodPut, "/api/v1/knowledge/managed/bases/kb-1/faq/batch/fields"},
		{http.MethodPut, "/api/v1/knowledge/managed/bases/kb-1/faq/batch/tags"},
		{http.MethodDelete, "/api/v1/knowledge/managed/bases/kb-1/faq/batch/delete"},
		{http.MethodPut, "/api/v1/knowledge/managed/documents/doc-1/tags"},
		{http.MethodPut, "/api/v1/knowledge/managed/documents/doc-1/chunks/chunk-1"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, req)
			assert.Equal(t, http.StatusUnauthorized, response.Code, "registered operation routes must enter auth boundary")
		})
	}
}

func TestManagedOperationMutationAuditsAndReturnsOnlyStableDTO(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantStatus  int
		wantOutcome string
		wantCode    string
		auditStatus int
	}{
		{name: "success allowlisted", status: 200, body: `{"success":true,"message":"private upstream text","code":"OK"}`, wantStatus: 200, wantOutcome: "succeeded", auditStatus: 200},
		{name: "malformed success", status: 200, body: `{}`, wantStatus: 502, wantOutcome: "unverified", wantCode: "KNOWLEDGE_UPSTREAM_INVALID", auditStatus: 502},
		{name: "conflict", status: 409, body: `{"success":false}`, wantStatus: 409, wantOutcome: "conflict", wantCode: "KNOWLEDGE_CONFLICT", auditStatus: 409},
		{name: "server error uncertain", status: 503, body: `upstream secret`, wantStatus: 502, wantOutcome: "uncertain", wantCode: "KNOWLEDGE_UPSTREAM_UNAVAILABLE", auditStatus: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAPIHarness(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "update_faq_tags", "faq_batch", "1", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "1", tc.wantOutcome, tc.auditStatus).WillReturnResult(sqlmock.NewResult(0, 1))

			req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"updates":{"1":null}}`))
			w := httptest.NewRecorder()
			h.managedOperationMutation(w, req, identity{UserID: "admin-user", DeploymentID: "demo"}, "mock-key", "10000", "kb-1", "update_faq_tags", "faq_batch", "1", http.MethodPut, "knowledge-bases/kb-1/faq/entries/tags", map[string]any{"updates": map[string]any{"1": nil}}, "")
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.wantCode != "" {
				assert.Contains(t, w.Body.String(), tc.wantCode)
				assert.NotContains(t, w.Body.String(), "upstream secret")
			} else {
				assert.Contains(t, w.Body.String(), `"outcome":"succeeded"`)
				assert.NotContains(t, w.Body.String(), "private upstream text")
			}
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedOperationMutationAuditIntentFailureDoesNotCallUpstream(t *testing.T) {
	h := newAPIHarness(t)
	serverCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serverCalls++
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnError(errors.New("db unavailable"))

	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.managedOperationMutation(w, req, identity{UserID: "admin-user", DeploymentID: "demo"}, "mock-key", "10000", "kb-1", "update_faq_tags", "faq_batch", "1", http.MethodPut, "knowledge-bases/kb-1/faq/entries/tags", map[string]any{"updates": map[string]any{"1": nil}}, "")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Zero(t, serverCalls)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedChunkUpdateUsesContentRevisionCASAndExposesIndexFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		updateCode int
		body       string
		wantStatus int
		outcome    string
	}{
		{name: "saved but index failed", updateCode: http.StatusOK, body: `{"data":{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","seq_id":7,"chunk_type":"text","content":"new body","is_enabled":true,"content_revision":5,"index_status":"failed"}}`, wantStatus: 200, outcome: "succeeded"},
		{name: "revision conflict", updateCode: http.StatusConflict, body: `{"error":"conflict"}`, wantStatus: 409, outcome: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","type":"file","file_name":"a.txt","parse_status":"completed"}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","tenant_id":10000,"name":"Docs","embedding_model_id":"emb-1","type":"document"}}`))
				case "/api/v1/chunks/by-id/chunk-1":
					_, _ = w.Write([]byte(`{"data":{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","seq_id":7,"chunk_type":"text","content_revision":4,"content":"old body","is_enabled":true}}`))
				case "/api/v1/chunks/doc-1/chunk-1":
					var payload map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
					assert.Equal(t, float64(4), payload["expected_revision"])
					w.WriteHeader(tc.updateCode)
					_, _ = w.Write([]byte(tc.body))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "update_chunk", "chunk", "chunk-1", "doc-1", `["kb-1"]`).WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "chunk-1", tc.outcome, tc.updateCode).WillReturnResult(sqlmock.NewResult(0, 1))

			req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"content":"new body","expected_revision":4}`))
			req.SetPathValue("documentID", "doc-1")
			req.SetPathValue("chunkID", "chunk-1")
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeChunkUpdate(w, req, adminIdentityForManagedTest())
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.updateCode == http.StatusOK {
				assert.Contains(t, w.Body.String(), `"content_revision":5`)
				assert.Contains(t, w.Body.String(), `"index_status":"failed"`)
			} else {
				assert.Contains(t, w.Body.String(), "KNOWLEDGE_CONFLICT")
			}
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedFAQBatchRejectsMethodActionMismatchBeforeUpstream(t *testing.T) {
	for _, tc := range []struct{ method, action string }{
		{"PUT", "delete"}, {"DELETE", "fields"}, {"DELETE", "tags"}, {"POST", "delete"}, {"DELETE", ""},
	} {
		t.Run(tc.method+"/"+tc.action, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, "/", nil)
			request.SetPathValue("batchAction", tc.action)
			response := httptest.NewRecorder()
			(&portal{}).handleManagedKnowledgeFAQBatch(response, request, identity{})
			assert.Equal(t, 404, response.Code)
		})
	}
}
