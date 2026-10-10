package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
)

func TestManagedDocumentContextFailsClosedOnOwnershipAndRegistryErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		baseID     string
		docJSON    string
		registry   string
		wantStatus int
	}{
		{name: "foreign tenant document", docJSON: `{"data":{"id":"doc-1","tenant_id":90000,"knowledge_base_id":"kb-1"}}`, wantStatus: http.StatusBadGateway},
		{name: "registered base lookup transport failure", docJSON: `{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1"}}`, wantStatus: http.StatusBadGateway},
		{name: "registry database failure", baseID: "kb-1", docJSON: `{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1"}}`, registry: "error", wantStatus: http.StatusBadGateway},
		{name: "unregistered base", baseID: "kb-1", docJSON: `{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1"}}`, registry: "missing", wantStatus: http.StatusNotFound},
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
					_, _ = w.Write([]byte(tc.docJSON))
				case "/api/v1/knowledge-bases/kb-1":
					if tc.name == "registered base lookup transport failure" {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			if tc.registry != "" {
				expect := h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1")
				if tc.registry == "error" {
					expect.WillReturnError(errors.New("registry unavailable"))
				} else {
					expect.WillReturnError(sql.ErrNoRows)
				}
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("documentID", "doc-1")
			req.SetPathValue("baseID", tc.baseID)
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeGetDocument(w, req, adminIdentityForManagedTest())
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			assert.NotContains(t, w.Body.String(), `"data":{"id":"doc-1"}`)
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeEnableStatusFailsClosedAfterAuditIntent(t *testing.T) {
	for _, tc := range []struct {
		name           string
		upstreamStatus int
		upstreamBody   string
		failIntent     bool
		failOutcome    bool
		wantStatus     int
		wantOutcome    string
	}{
		{name: "audit intent unavailable", failIntent: true, wantStatus: http.StatusServiceUnavailable},
		{name: "upstream conflict", upstreamStatus: http.StatusConflict, upstreamBody: `{"error":"conflict"}`, wantStatus: http.StatusBadGateway, wantOutcome: "unverified"},
		{name: "malformed success", upstreamStatus: http.StatusOK, upstreamBody: `{"not_data":true}`, wantStatus: http.StatusBadGateway, wantOutcome: "unverified"},
		{name: "foreign tenant response", upstreamStatus: http.StatusOK, upstreamBody: `{"data":{"id":"doc-1","tenant_id":90000,"knowledge_base_id":"kb-1","enable_status":"disabled"}}`, wantStatus: http.StatusBadGateway, wantOutcome: "unverified"},
		{name: "audit outcome unavailable", upstreamStatus: http.StatusOK, upstreamBody: `{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","enable_status":"disabled"}}`, failOutcome: true, wantStatus: http.StatusServiceUnavailable, wantOutcome: "succeeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			var mutationCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","parse_status":"completed","enable_status":"enabled"}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				case "/api/v1/knowledge/doc-1/enable-status":
					mutationCalls++
					assert.Equal(t, http.MethodPut, r.Method)
					w.WriteHeader(tc.upstreamStatus)
					_, _ = w.Write([]byte(tc.upstreamBody))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			insert := h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "set_enabled", "document", "doc-1", "kb-1")
			if tc.failIntent {
				insert.WillReturnError(errors.New("audit intent unavailable"))
			} else {
				insert.WillReturnResult(sqlmock.NewResult(0, 1))
				outcome := h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", tc.wantOutcome, sqlmock.AnyArg())
				if tc.failOutcome {
					outcome.WillReturnError(errors.New("audit outcome unavailable"))
				} else {
					outcome.WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"enabled":false}`))
			req.SetPathValue("documentID", "doc-1")
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeSetDocumentEnabled(w, req, adminIdentityForManagedTest())
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.failIntent {
				assert.Zero(t, mutationCalls, "no upstream mutation may occur without durable intent")
			} else {
				assert.Equal(t, 1, mutationCalls)
			}
			if !tc.failIntent {
				assert.Contains(t, w.Body.String(), `"operationId":"kbop-`)
			}
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeEnableCompletedDocument(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","parse_status":"completed","enable_status":"disabled"}}`))
		case "/api/v1/knowledge-bases/kb-1":
			_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
		case "/api/v1/knowledge/doc-1/enable-status":
			var body map[string]bool
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, map[string]bool{"enabled": true}, body)
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","parse_status":"completed","enable_status":"enabled"}}`))
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "set_enabled", "document", "doc-1", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", "succeeded", http.StatusOK).WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"enabled":true}`))
	req.SetPathValue("documentID", "doc-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeSetDocumentEnabled(w, req, adminIdentityForManagedTest())
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"enable_status":"enabled"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeActionsRejectUnknownAndAuditFailure(t *testing.T) {
	for _, tc := range []struct {
		name         string
		action       string
		failIntent   bool
		upstreamCode int
		wantStatus   int
		wantOutcome  string
	}{
		{name: "unknown action", action: "force", wantStatus: http.StatusNotFound},
		{name: "audit intent unavailable", action: "reparse", failIntent: true, wantStatus: http.StatusServiceUnavailable},
		{name: "upstream rejected", action: "reparse", upstreamCode: http.StatusInternalServerError, wantStatus: http.StatusBadGateway, wantOutcome: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			var actionCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1"}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				case "/api/v1/knowledge/doc-1/reparse":
					actionCalls++
					w.WriteHeader(tc.upstreamCode)
					_, _ = w.Write([]byte(`{"error":"rejected"}`))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			if tc.action != "force" {
				insert := h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "reparse", "document", "doc-1", "kb-1")
				if tc.failIntent {
					insert.WillReturnError(errors.New("audit intent unavailable"))
				} else {
					insert.WillReturnResult(sqlmock.NewResult(0, 1))
					h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", tc.wantOutcome, tc.upstreamCode).WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.SetPathValue("documentID", "doc-1")
			req.SetPathValue("action", tc.action)
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeDocumentAction(w, req, adminIdentityForManagedTest())
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.wantOutcome == "" {
				assert.Zero(t, actionCalls)
			} else {
				assert.Equal(t, 1, actionCalls)
			}
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeActionTransportFailureIsRecordedUncertain(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1"}}`))
		case "/api/v1/knowledge-bases/kb-1":
			_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
		case "/api/v1/knowledge/doc-1/reparse":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack upstream action request: %v", err)
				return
			}
			_ = conn.Close()
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "reparse", "document", "doc-1", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", "uncertain", http.StatusBadGateway).WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.SetPathValue("documentID", "doc-1")
	req.SetPathValue("action", "reparse")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeDocumentAction(w, req, adminIdentityForManagedTest())
	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"operationId":"kbop-`)
	assert.Contains(t, w.Body.String(), `"resourceId":"doc-1"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeChunksRejectInvalidFiltersAndForeignRows(t *testing.T) {
	for _, tc := range []struct {
		name         string
		query        string
		response     string
		wantStatus   int
		wantUpstream bool
	}{
		{name: "page lower bound", query: "page=0", wantStatus: http.StatusBadRequest},
		{name: "page size upper bound", query: "page_size=101", wantStatus: http.StatusBadRequest},
		{name: "unsupported chunk type", query: "chunk_type=unknown", wantStatus: http.StatusBadRequest},
		{name: "too many chunk types", query: "chunk_type=text&chunk_type=faq&chunk_type=entity&chunk_type=summary&chunk_type=parent_text&chunk_type=image_ocr&chunk_type=image_caption&chunk_type=relationship&chunk_type=web_search&chunk_type=table_summary&chunk_type=table_column&chunk_type=wiki_page&chunk_type=text", wantStatus: http.StatusBadRequest},
		{name: "foreign tenant chunk", query: "page=1", response: `{"data":[{"id":"chunk-1","tenant_id":90000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","content":"private"}],"total":1,"page":1,"page_size":20}`, wantStatus: http.StatusBadGateway, wantUpstream: true},
		{name: "foreign knowledge base chunk", query: "page=1", response: `{"data":[{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-other","knowledge_id":"doc-1","content":"private"}],"total":1,"page":1,"page_size":20}`, wantStatus: http.StatusBadGateway, wantUpstream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			var chunkCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1"}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				case "/api/v1/chunks/doc-1":
					chunkCalls++
					_, _ = w.Write([]byte(tc.response))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			req := httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil)
			req.SetPathValue("documentID", "doc-1")
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeChunks(w, req, adminIdentityForManagedTest())
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.wantUpstream {
				assert.Equal(t, 1, chunkCalls)
				assert.NotContains(t, w.Body.String(), "private")
			} else {
				assert.Zero(t, chunkCalls)
			}
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeChunkImageRejectsInvalidReferencesAndAuditFailures(t *testing.T) {
	validChunk := `{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","image_info":"[{\"url\":\"resource://abcdefghijklmnopqrstuv\"}]"}`
	for _, tc := range []struct {
		name       string
		index      string
		chunkCode  int
		chunkJSON  string
		imageCode  int
		failIntent bool
		wantStatus int
	}{
		{name: "negative index", index: "-1", chunkJSON: validChunk, wantStatus: http.StatusNotFound},
		{name: "chunk upstream rejected", index: "0", chunkCode: http.StatusInternalServerError, wantStatus: http.StatusNotFound},
		{name: "malformed image metadata", index: "0", chunkJSON: `{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","image_info":"not-json"}`, wantStatus: http.StatusNotFound},
		{name: "audit intent unavailable", index: "0", chunkJSON: validChunk, failIntent: true, wantStatus: http.StatusServiceUnavailable},
		{name: "image upstream rejected", index: "0", chunkJSON: validChunk, imageCode: http.StatusServiceUnavailable, wantStatus: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			var chunkCalls, imageCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1"}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				case "/api/v1/chunks/by-id/chunk-1":
					chunkCalls++
					if tc.chunkCode != 0 {
						w.WriteHeader(tc.chunkCode)
						return
					}
					_, _ = w.Write([]byte(`{"data":` + tc.chunkJSON + `}`))
				case "/api/v1/knowledge/doc-1/chunks/chunk-1/images/0":
					imageCalls++
					w.Header().Set("Content-Type", "image/png")
					if tc.imageCode != 0 {
						w.WriteHeader(tc.imageCode)
						return
					}
					_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nimage"))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			if tc.name == "audit intent unavailable" {
				h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnError(errors.New("audit unavailable"))
			} else if tc.name == "image upstream rejected" {
				h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", "failed", http.StatusBadGateway).WillReturnResult(sqlmock.NewResult(0, 1))
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("documentID", "doc-1")
			req.SetPathValue("chunkID", "chunk-1")
			req.SetPathValue("index", tc.index)
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeChunkImage(w, req, adminIdentityForManagedTest())
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.index == "-1" {
				assert.Zero(t, chunkCalls)
			} else if tc.chunkCode != 0 || tc.name == "malformed image metadata" {
				assert.Equal(t, 1, chunkCalls)
				assert.Zero(t, imageCalls)
			} else if tc.failIntent {
				assert.Equal(t, 1, chunkCalls)
				assert.Zero(t, imageCalls)
			} else {
				assert.Equal(t, 1, imageCalls)
			}
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgePreviewUsesSafeInlineAndErrorResponses(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fileName    string
		kind        string
		upstream    int
		mode        string
		failAudit   bool
		wantStatus  int
		wantType    string
		wantDisplay string
		wantOutcome string
	}{
		{name: "text preview inline", fileName: "notes.txt", kind: "preview", wantStatus: http.StatusOK, wantType: "text/plain; charset=utf-8", wantDisplay: "inline", wantOutcome: "succeeded"},
		{name: "unknown preview forced attachment", fileName: "notes.bin", kind: "preview", wantStatus: http.StatusOK, wantType: "application/octet-stream", wantDisplay: "attachment", wantOutcome: "succeeded"},
		{name: "upstream rejected", fileName: "notes.txt", kind: "preview", upstream: http.StatusServiceUnavailable, wantStatus: http.StatusBadGateway, wantOutcome: "failed"},
		{name: "truncated upstream file", fileName: "notes.txt", kind: "download", mode: "truncated", wantStatus: http.StatusBadGateway, wantOutcome: "failed"},
		{name: "audit intent unavailable", fileName: "notes.txt", kind: "preview", failAudit: true, wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			var fileCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","file_name":"` + tc.fileName + `"}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				case "/api/v1/knowledge/doc-1/" + tc.kind:
					fileCalls++
					if tc.upstream != 0 {
						w.WriteHeader(tc.upstream)
						return
					}
					if tc.mode == "truncated" {
						w.Header().Set("Content-Length", "32")
						_, _ = w.Write([]byte("short"))
						return
					}
					_, _ = w.Write([]byte("safe preview"))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			if tc.failAudit {
				h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnError(errors.New("audit unavailable"))
			} else {
				h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "read_"+tc.kind, "document", "doc-1", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
				outcome, status := tc.wantOutcome, tc.wantStatus
				if tc.upstream != 0 {
					status = tc.upstream
				}
				if tc.mode == "truncated" {
					status = http.StatusBadGateway
				}
				h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", outcome, status).WillReturnResult(sqlmock.NewResult(0, 1))
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("documentID", "doc-1")
			req.SetPathValue("fileKind", tc.kind)
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeFile(w, req, adminIdentityForManagedTest())
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.failAudit {
				assert.Zero(t, fileCalls, "file bytes must not be requested before a durable audit intent")
			} else {
				assert.Equal(t, 1, fileCalls)
			}
			if tc.wantType != "" {
				assert.Equal(t, tc.wantType, w.Header().Get("Content-Type"))
				assert.Contains(t, w.Header().Get("Content-Disposition"), tc.wantDisplay)
			}
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeChunkImageReturnsBusyBeforeReadingBytes(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	for i := 0; i < cap(managedKnowledgeFileSpoolSlots); i++ {
		managedKnowledgeFileSpoolSlots <- struct{}{}
	}
	defer func() {
		for len(managedKnowledgeFileSpoolSlots) > 0 {
			<-managedKnowledgeFileSpoolSlots
		}
	}()
	var imageCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1"}}`))
		case "/api/v1/knowledge-bases/kb-1":
			_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
		case "/api/v1/chunks/by-id/chunk-1":
			_, _ = w.Write([]byte(`{"data":{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","image_info":"[{\"url\":\"resource://abcdefghijklmnopqrstuv\"}]"}}`))
		case "/api/v1/knowledge/doc-1/chunks/chunk-1/images/0":
			imageCalls++
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "read_chunk_image", "document", "doc-1", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", "failed", http.StatusServiceUnavailable).WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("documentID", "doc-1")
	req.SetPathValue("chunkID", "chunk-1")
	req.SetPathValue("index", "0")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeChunkImage(w, req, adminIdentityForManagedTest())
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Zero(t, imageCalls)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}
