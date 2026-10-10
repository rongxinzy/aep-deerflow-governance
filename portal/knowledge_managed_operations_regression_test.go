package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newManagedOperationsFixture(t *testing.T, baseType string, upstream http.HandlerFunc) *apiTestPortal {
	t.Helper()
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/me" {
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
			return
		}
		if r.URL.Path == "/api/v1/knowledge-bases/kb-1" {
			_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000,"type":"` + baseType + `"}}`))
			return
		}
		upstream(w, r)
	}))
	t.Cleanup(server.Close)
	h.cfg.knowledgeAPIURL = server.URL
	return h
}

func expectManagedOperationsBase(h *apiTestPortal) {
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).
		WithArgs("demo", "10000", "kb-1").
		WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
}

func expectManagedReadAudit(h *apiTestPortal, action string) {
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", action, "knowledge_base", "kb-1", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "kb-1", "succeeded", http.StatusOK).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestManagedKnowledgeOperationReadEndpointsReturnBoundedAuditedData(t *testing.T) {
	tests := []struct {
		name       string
		baseType   string
		method     string
		path       string
		upstream   func(http.ResponseWriter, *http.Request)
		audit      string
		invoke     func(*apiTestPortal, *httptest.ResponseRecorder, *http.Request)
		wantStatus int
		wantBody   string
	}{
		{
			name:   "tags",
			method: http.MethodGet,
			path:   "/",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/knowledge-bases/kb-1/tags" || r.URL.Query().Get("page_size") != "200" {
					t.Errorf("unexpected tags request: %s", r.URL.String())
				}
				_, _ = w.Write([]byte(`{"data":{"data":[{"id":"tag-1","seq_id":1,"name":"Research"}],"total":1,"page":1,"page_size":200}}`))
			},
			audit: "list_tags",
			invoke: func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
				h.handleManagedKnowledgeTags(w, r, adminIdentityForManagedTest())
			},
			wantStatus: http.StatusOK,
			wantBody:   `"id":"tag-1"`,
		},
		{
			name:   "folders",
			method: http.MethodGet,
			path:   "/",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/knowledge-bases/kb-1/knowledge/folders" {
					t.Errorf("unexpected folders request: %s", r.URL.String())
				}
				_, _ = w.Write([]byte(`{"data":{"root_document_count":1,"total_document_count":2,"folders":[]}}`))
			},
			audit: "list_folders",
			invoke: func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
				h.handleManagedKnowledgeFolders(w, r, adminIdentityForManagedTest())
			},
			wantStatus: http.StatusOK,
			wantBody:   `"total_document_count":2`,
		},
		{
			name:     "FAQ page",
			baseType: "faq",
			method:   http.MethodGet,
			path:     "/?page=2&page_size=50&keyword=policy",
			upstream: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/knowledge-bases/kb-1/faq/entries" || r.URL.Query().Get("page") != "2" || r.URL.Query().Get("page_size") != "50" || r.URL.Query().Get("keyword") != "policy" {
					t.Errorf("unexpected FAQ query forwarding: %s", r.URL.String())
				}
				_, _ = w.Write([]byte(`{"data":{"data":[{"id":7,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","standard_question":"How?"}],"total":1,"page":2,"page_size":50}}`))
			},
			audit: "list_faq",
			invoke: func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
				h.handleManagedKnowledgeFAQ(w, r, adminIdentityForManagedTest())
			},
			wantStatus: http.StatusOK,
			wantBody:   `"standard_question":"How?"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newManagedOperationsFixture(t, tc.baseType, tc.upstream)
			expectManagedOperationsBase(h)
			expectManagedReadAudit(h, tc.audit)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.SetPathValue("baseID", "kb-1")
			response := httptest.NewRecorder()
			tc.invoke(h, response, req)
			assert.Equal(t, tc.wantStatus, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), tc.wantBody)
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedFAQListRejectsInvalidOrIncompleteEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"invalid id", `{"data":{"data":[{"id":0,"knowledge_base_id":"kb-1","knowledge_id":"doc-1"}],"total":1}}`},
		{"foreign base", `{"data":{"data":[{"id":7,"knowledge_base_id":"kb-foreign","knowledge_id":"doc-1"}],"total":1}}`},
		{"missing source id", `{"data":{"data":[{"id":7,"knowledge_base_id":"kb-1"}],"total":1}}`},
		{"missing page envelope", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newManagedOperationsFixture(t, "faq", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			expectManagedOperationsBase(h)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("baseID", "kb-1")
			response := httptest.NewRecorder()
			h.handleManagedKnowledgeFAQ(response, req, adminIdentityForManagedTest())
			require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"code":"KNOWLEDGE_UPSTREAM_INVALID"`)
			assert.NotContains(t, response.Body.String(), `"outcome":"succeeded"`)
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedTagReadRejectsIncompleteOrInvalidMembershipPages(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"truncated page", `{"data":{"data":[{"id":"tag-1","seq_id":1}],"total":2}}`},
		{"invalid tag identity", `{"data":{"data":[{"id":"","seq_id":0}],"total":1}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			expectManagedOperationsBase(h)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("baseID", "kb-1")
			response := httptest.NewRecorder()
			h.handleManagedKnowledgeTags(response, req, adminIdentityForManagedTest())
			require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"code":"KNOWLEDGE_UPSTREAM_INVALID"`)
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeOperationInputsFailBeforeMutation(t *testing.T) {
	tests := []struct {
		name     string
		baseType string
		method   string
		path     string
		body     string
		invoke   func(*apiTestPortal, *httptest.ResponseRecorder, *http.Request)
		status   int
	}{
		{"tag query allowlist", "document", http.MethodGet, "/?page=1", "", func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			h.handleManagedKnowledgeTags(w, r, adminIdentityForManagedTest())
		}, 400},
		{"tag create validation", "document", http.MethodPost, "/", `{}`, func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			h.handleManagedKnowledgeTags(w, r, adminIdentityForManagedTest())
		}, 400},
		{"tag id validation", "document", http.MethodPut, "/", `{}`, func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			r.SetPathValue("tagID", "../foreign")
			h.handleManagedKnowledgeTag(w, r, adminIdentityForManagedTest())
		}, 404},
		{"folder query allowlist", "document", http.MethodGet, "/?page=1", "", func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			h.handleManagedKnowledgeFolders(w, r, adminIdentityForManagedTest())
		}, 400},
		{"folder move requires explicit ids", "document", http.MethodPost, "/", `{"document_ids":[],"folder_path":"new"}`, func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			h.handleManagedKnowledgeFolders(w, r, adminIdentityForManagedTest())
		}, 400},
		{"folder move rejects parent traversal", "document", http.MethodPost, "/", `{"document_ids":["doc-1"],"folder_path":"docs/../outside"}`, func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			h.handleManagedKnowledgeFolders(w, r, adminIdentityForManagedTest())
		}, 400},
		{"folder rename rejects dot segments", "document", http.MethodPut, "/", `{"from":"safe","to":"../outside"}`, func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			h.handleManagedKnowledgeFolders(w, r, adminIdentityForManagedTest())
		}, 400},
		{"FAQ unsupported on document KB", "document", http.MethodGet, "/", "", func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			h.handleManagedKnowledgeFAQ(w, r, adminIdentityForManagedTest())
		}, 409},
		{"FAQ query allowlist", "faq", http.MethodGet, "/?sort_by=private", "", func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			h.handleManagedKnowledgeFAQ(w, r, adminIdentityForManagedTest())
		}, 400},
		{"FAQ duplicate query value", "faq", http.MethodGet, "/?page=1&page=2", "", func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			h.handleManagedKnowledgeFAQ(w, r, adminIdentityForManagedTest())
		}, 400},
		{"FAQ invalid page number", "faq", http.MethodGet, "/?page=abc", "", func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			h.handleManagedKnowledgeFAQ(w, r, adminIdentityForManagedTest())
		}, 400},
		{"FAQ pagination validation", "faq", http.MethodGet, "/?page_size=201", "", func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			h.handleManagedKnowledgeFAQ(w, r, adminIdentityForManagedTest())
		}, 400},
		{"FAQ create validation", "faq", http.MethodPost, "/", `{}`, func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			h.handleManagedKnowledgeFAQEntry(w, r, adminIdentityForManagedTest())
		}, 400},
		{"FAQ batch validation", "faq", http.MethodPut, "/", `{}`, func(h *apiTestPortal, w *httptest.ResponseRecorder, r *http.Request) {
			r.SetPathValue("batchAction", "fields")
			h.handleManagedKnowledgeFAQBatch(w, r, adminIdentityForManagedTest())
		}, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newManagedOperationsFixture(t, tc.baseType, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected mutation/read after validation failure: %s", r.URL.String())
				http.NotFound(w, r)
			})
			expectManagedOperationsBase(h)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.SetPathValue("baseID", "kb-1")
			response := httptest.NewRecorder()
			tc.invoke(h, response, req)
			assert.Equal(t, tc.status, response.Code, response.Body.String())
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeTagMutationsVerifyKBMembershipAndAudit(t *testing.T) {
	catalogReads := 0
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		action     string
		auditCode  int
		upstream   func(http.ResponseWriter, *http.Request)
		wantStatus int
	}{
		{
			name:      "create",
			method:    http.MethodPost,
			path:      "/",
			body:      `{"name":"Research","color":"blue","sort_order":1}`,
			action:    "create_tag",
			auditCode: http.StatusCreated,
			upstream: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/knowledge-bases/kb-1/tags":
					var input map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
					assert.Equal(t, "Research", input["name"])
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"data":{"id":"tag-1","seq_id":1,"name":"Research","color":"blue"}}`))
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/knowledge-bases/kb-1/tags":
					_, _ = w.Write([]byte(`{"data":{"data":[{"id":"tag-1","seq_id":1,"name":"Research","color":"blue"}],"total":1}}`))
				default:
					t.Errorf("unexpected tag create verification request %s %s", r.Method, r.URL.String())
					http.NotFound(w, r)
				}
			},
			wantStatus: http.StatusCreated,
		},
		{
			name:       "update",
			method:     http.MethodPut,
			path:       "/",
			body:       `{"name":"Research v2"}`,
			action:     "update_tag",
			auditCode:  http.StatusOK,
			wantStatus: http.StatusOK,
			upstream: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/knowledge-bases/kb-1/tags":
					_, _ = w.Write([]byte(`{"data":{"data":[{"id":"tag-1","seq_id":1,"name":"Research v2"}],"total":1}}`))
				case r.Method == http.MethodPut && r.URL.Path == "/api/v1/knowledge-bases/kb-1/tags/tag-1":
					_, _ = w.Write([]byte(`{"data":{"id":"tag-1","seq_id":1,"name":"Research v2"}}`))
				default:
					t.Errorf("unexpected tag update request %s %s", r.Method, r.URL.String())
					http.NotFound(w, r)
				}
			},
		},
		{
			name:       "delete",
			method:     http.MethodDelete,
			path:       "/",
			body:       "",
			action:     "delete_tag",
			auditCode:  http.StatusNoContent,
			wantStatus: http.StatusOK,
			upstream: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/knowledge-bases/kb-1/tags":
					catalogReads++
					if catalogReads > 1 {
						_, _ = w.Write([]byte(`{"data":{"data":[],"total":0}}`))
					} else {
						_, _ = w.Write([]byte(`{"data":{"data":[{"id":"tag-1","seq_id":1,"name":"Research"}],"total":1}}`))
					}
				case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/knowledge-bases/kb-1/tags/tag-1":
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected tag delete request %s %s", r.Method, r.URL.String())
					http.NotFound(w, r)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			catalogReads = 0
			h := newManagedOperationsFixture(t, "document", tc.upstream)
			expectManagedOperationsBase(h)
			if tc.method == http.MethodPost {
				h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
					WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", tc.action, "knowledge_tag", "", "kb-1").
					WillReturnResult(sqlmock.NewResult(0, 1))
			} else {
				h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
					WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", tc.action, "knowledge_tag", "tag-1", "kb-1").
					WillReturnResult(sqlmock.NewResult(0, 1))
			}
			h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
				WithArgs(sqlmock.AnyArg(), "tag-1", "succeeded", tc.auditCode).
				WillReturnResult(sqlmock.NewResult(0, 1))
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.SetPathValue("baseID", "kb-1")
			if tc.method != http.MethodPost {
				req.SetPathValue("tagID", "tag-1")
			}
			response := httptest.NewRecorder()
			if tc.method == http.MethodPost {
				h.handleManagedKnowledgeTags(response, req, adminIdentityForManagedTest())
			} else {
				h.handleManagedKnowledgeTag(response, req, adminIdentityForManagedTest())
			}
			assert.Equal(t, tc.wantStatus, response.Code, response.Body.String())
			if tc.method != http.MethodDelete {
				assert.Contains(t, response.Body.String(), `"id":"tag-1"`)
			}
			assert.NotContains(t, response.Body.String(), "api_key")
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedTagAuditIntentFailurePreventsUpstreamWrite(t *testing.T) {
	var upstreamWrites int
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		upstreamWrites++
		t.Errorf("write must not reach WeKnora without audit intent: %s", r.URL.Path)
		http.NotFound(w, r)
	})
	expectManagedOperationsBase(h)
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnError(errors.New("audit storage unavailable"))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"Research"}`))
	req.SetPathValue("baseID", "kb-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeTags(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Equal(t, 0, upstreamWrites)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedOperationAuditCompletionFailureDoesNotClaimSuccess(t *testing.T) {
	var upstreamWrites int
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		upstreamWrites++
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/knowledge/tags" {
			t.Errorf("unexpected mutation: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "update_document_tags", "document", "doc-1", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WillReturnError(errors.New("audit completion unavailable"))
	req := httptest.NewRequest(http.MethodPut, "/", nil)
	response := httptest.NewRecorder()
	h.managedOperationMutation(response, req, adminIdentityForManagedTest(), "test-key", "10000", "kb-1", "update_document_tags", "document", "doc-1", http.MethodPut, "knowledge/tags", map[string]any{"kb_id": "kb-1", "updates": map[string][]string{"doc-1": {"tag-1"}}}, "")
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Equal(t, 1, upstreamWrites, "a successful upstream mutation may occur, but the API must not report success without durable audit completion")
	assert.NotContains(t, response.Body.String(), `"outcome":"succeeded"`)
	assert.Contains(t, response.Body.String(), `"code":"AUDIT_UNAVAILABLE"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedTagMutationWithUnprovableKBMembershipIsNotAuditedSucceeded(t *testing.T) {
	var catalogReads int
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/knowledge-bases/kb-1/tags":
			catalogReads++
			if catalogReads == 1 {
				_, _ = w.Write([]byte(`{"data":{"data":[{"id":"tag-1","seq_id":1,"name":"Old"}],"total":1}}`))
			} else {
				_, _ = w.Write([]byte(`{"data":{"data":[],"total":0}}`))
			}
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/knowledge-bases/kb-1/tags/tag-1":
			_, _ = w.Write([]byte(`{"data":{"id":"tag-1","seq_id":1,"name":"New"}}`))
		default:
			t.Errorf("unexpected tag update request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	expectManagedOperationsBase(h)
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "update_tag", "knowledge_tag", "tag-1", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "", "unverified", http.StatusBadGateway).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"name":"New"}`))
	req.SetPathValue("baseID", "kb-1")
	req.SetPathValue("tagID", "tag-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeTag(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"code":"KNOWLEDGE_UPSTREAM_INVALID"`)
	assert.NotContains(t, response.Body.String(), `"outcome":"succeeded"`)
	assert.Equal(t, 2, catalogReads)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedTagUpdateRejectsTagOutsideCurrentCatalogBeforeAudit(t *testing.T) {
	var writes int
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/knowledge-bases/kb-1/tags" {
			_, _ = w.Write([]byte(`{"data":{"data":[],"total":0}}`))
			return
		}
		if r.Method != http.MethodGet {
			writes++
		}
		t.Errorf("unexpected upstream request for an unregistered tag: %s %s", r.Method, r.URL.String())
		http.NotFound(w, r)
	})
	expectManagedOperationsBase(h)
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"name":"New"}`))
	req.SetPathValue("baseID", "kb-1")
	req.SetPathValue("tagID", "foreign-tag")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeTag(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"code":"TAG_NOT_FOUND"`)
	assert.Zero(t, writes, "tag membership must be established before any mutation")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedDocumentTagMutationValidatesDocumentsAndKBScopedTags(t *testing.T) {
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Doc"}}`))
		case "/api/v1/knowledge-bases/kb-1/tags":
			if r.URL.Query().Get("page") != "1" || r.URL.Query().Get("page_size") != "200" {
				t.Errorf("tag membership query is not bounded: %s", r.URL.String())
			}
			_, _ = w.Write([]byte(`{"data":{"data":[{"id":"tag-1","seq_id":1,"name":"Research"}],"total":1}}`))
		case "/api/v1/knowledge/tags":
			if r.Method != http.MethodPut {
				t.Errorf("expected PUT tag mutation, got %s", r.Method)
			}
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "kb-1", body["kb_id"])
			assert.JSONEq(t, `{"doc-1":["tag-1"]}`, mustJSON(t, body["updates"]))
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			t.Errorf("unexpected upstream request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	})
	expectManagedOperationsBase(h)
	expectManagedOperationsBase(h) // document lookup rechecks its KB registration
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "update_document_tags", "document_batch", "doc-1", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "doc-1", "succeeded", http.StatusOK).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"updates":{"doc-1":["tag-1"]}}`))
	req.SetPathValue("baseID", "kb-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeTagsForDocuments(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"outcome":"succeeded"`)
	assert.NotContains(t, response.Body.String(), "api_key")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedDocumentTagBatchRejectsForeignOrIncompleteTagCatalog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"foreign tag", `{"data":{"data":[],"total":0}}`, http.StatusNotFound},
		{"incomplete catalog", `{"data":{"data":[{"id":"tag-1","seq_id":1}],"total":2}}`, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mutationCalls int
			h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Policy"}}`))
				case "/api/v1/knowledge-bases/kb-1/tags":
					_, _ = w.Write([]byte(tc.body))
				case "/api/v1/knowledge/tags":
					mutationCalls++
				default:
					t.Errorf("unexpected document tag batch request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			expectManagedOperationsBase(h)
			expectManagedOperationsBase(h) // document ownership check
			req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"updates":{"doc-1":["tag-foreign"]}}`))
			req.SetPathValue("baseID", "kb-1")
			response := httptest.NewRecorder()
			h.handleManagedKnowledgeTagsForDocuments(response, req, adminIdentityForManagedTest())
			require.Equal(t, tc.status, response.Code, response.Body.String())
			assert.NotContains(t, response.Body.String(), `"outcome":"succeeded"`)
			assert.Zero(t, mutationCalls)
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedSingleDocumentTagMutationUsesKBScopedTagSet(t *testing.T) {
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Policy"}}`))
		case "/api/v1/knowledge-bases/kb-1/tags":
			_, _ = w.Write([]byte(`{"data":{"data":[{"id":"tag-1","seq_id":1,"name":"Research"}],"total":1}}`))
		case "/api/v1/knowledge/tags":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "kb-1", body["kb_id"])
			assert.JSONEq(t, `{"doc-1":["tag-1"]}`, mustJSON(t, body["updates"]))
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			t.Errorf("unexpected document-tag upstream request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	})
	expectManagedOperationsBase(h) // source base registration
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "update_document_tags", "document", "doc-1", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "doc-1", "succeeded", http.StatusOK).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"tag_ids":["tag-1"]}`))
	req.SetPathValue("baseID", "kb-1")
	req.SetPathValue("documentID", "doc-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeDocumentTags(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"outcome":"succeeded"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return string(raw)
}

func TestManagedFAQCreateVerifiesKBAndDocumentBeforeSuccessAudit(t *testing.T) {
	mutationCalls := 0
	h := newManagedOperationsFixture(t, "faq", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/knowledge-bases/kb-1/faq/entry":
			mutationCalls++
			if r.Method != http.MethodPost {
				t.Errorf("expected FAQ POST, got %s", r.Method)
			}
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "How do I access the policy?", body["standard_question"])
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"data":{"id":7,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","standard_question":"How do I access the policy?"}}`))
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Policy"}}`))
		default:
			t.Errorf("unexpected FAQ upstream request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	})
	expectManagedOperationsBase(h)
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "create_faq", "faq_entry", "", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectManagedOperationsBase(h) // created source is checked against the registered KB
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "7", "succeeded", http.StatusCreated).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"standard_question":"How do I access the policy?","answers":["Use the portal"]}`))
	req.SetPathValue("baseID", "kb-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFAQEntry(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"id":7`)
	assert.Contains(t, response.Body.String(), `"operationId":"kbop-`)
	assert.Equal(t, 1, mutationCalls)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedFAQUpstreamFailureIsAuditedUncertainNotSucceeded(t *testing.T) {
	h := newManagedOperationsFixture(t, "faq", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/knowledge-bases/kb-1/faq/entry" {
			t.Errorf("unexpected FAQ mutation request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
	})
	expectManagedOperationsBase(h)
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "create_faq", "faq_entry", "", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "", "uncertain", http.StatusServiceUnavailable).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"standard_question":"How do I access the policy?"}`))
	req.SetPathValue("baseID", "kb-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFAQEntry(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), `"outcome":"succeeded"`)
	assert.Contains(t, response.Body.String(), `"code":"KNOWLEDGE_UPSTREAM_UNAVAILABLE"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedFAQCreateWithForeignKBResponseIsNotAuditedSucceeded(t *testing.T) {
	h := newManagedOperationsFixture(t, "faq", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":9,"knowledge_base_id":"kb-foreign","knowledge_id":"doc-foreign","standard_question":"How?"}}`))
	})
	expectManagedOperationsBase(h)
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "create_faq", "faq_entry", "", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "", "unverified", http.StatusBadGateway).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"standard_question":"How?"}`))
	req.SetPathValue("baseID", "kb-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFAQEntry(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"code":"KNOWLEDGE_UPSTREAM_INVALID"`)
	assert.NotContains(t, response.Body.String(), `"outcome":"succeeded"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedFAQEntryLookupRejectsForeignKBBeforeMutation(t *testing.T) {
	h := newManagedOperationsFixture(t, "faq", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":7,"knowledge_base_id":"kb-foreign","knowledge_id":"doc-foreign"}}`))
	})
	expectManagedOperationsBase(h)
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"standard_question":"Updated?"}`))
	req.SetPathValue("baseID", "kb-1")
	req.SetPathValue("entryID", "7")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFAQEntry(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"code":"FAQ_NOT_FOUND"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedFAQCreateRejectsForeignOrUnverifiedTagBeforeAudit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
	}{
		{"foreign tag", `{"data":{"data":[{"id":"tag-1","seq_id":1}],"total":1}}`, http.StatusNotFound},
		{"incomplete catalog", `{"data":{"data":[{"id":"tag-1","seq_id":1}],"total":2}}`, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newManagedOperationsFixture(t, "faq", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/knowledge-bases/kb-1/tags" {
					t.Errorf("invalid tag must be rejected before FAQ mutation: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(tc.body))
			})
			expectManagedOperationsBase(h)
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"standard_question":"How?","tag_id":99}`))
			req.SetPathValue("baseID", "kb-1")
			response := httptest.NewRecorder()
			h.handleManagedKnowledgeFAQEntry(response, req, adminIdentityForManagedTest())
			require.Equal(t, tc.status, response.Code, response.Body.String())
			assert.NotContains(t, response.Body.String(), `"outcome":"succeeded"`)
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedFolderReadFailureDoesNotRecordReadSuccess(t *testing.T) {
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/knowledge-bases/kb-1/knowledge/folders" {
			t.Errorf("unexpected folder request: %s", r.URL.String())
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"offline"}`))
	})
	expectManagedOperationsBase(h)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("baseID", "kb-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFolders(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), `"outcome":"succeeded"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedOperationRejectsUnregisteredBaseBeforeUpstreamRead(t *testing.T) {
	var upstreamCalls int
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		t.Errorf("unregistered KB must not be fetched upstream: %s", r.URL.String())
		http.NotFound(w, r)
	})
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).
		WithArgs("demo", "10000", "kb-1").
		WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("baseID", "kb-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFolders(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	assert.Equal(t, 0, upstreamCalls)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedOperationBaseRejectsUnsafeIDWithoutLookup(t *testing.T) {
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unsafe KB ID must not reach upstream: %s", r.URL.String())
		http.NotFound(w, r)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("baseID", "../kb-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFolders(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedChunkRejectsStaleClientRevisionBeforeAuditOrMutation(t *testing.T) {
	var updates int
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Policy"}}`))
		case "/api/v1/chunks/by-id/chunk-1":
			_, _ = w.Write([]byte(`{"data":{"id":"chunk-1","knowledge_id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"seq_id":3,"chunk_type":"text","content_revision":4}}`))
		case "/api/v1/chunks/doc-1/chunk-1":
			updates++
			t.Errorf("stale client revision must not call upstream update")
		default:
			t.Errorf("unexpected chunk request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	expectManagedOperationsBase(h)
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"content":"new content","expected_revision":3}`))
	req.SetPathValue("documentID", "doc-1")
	req.SetPathValue("chunkID", "chunk-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeChunkUpdate(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"code":"KNOWLEDGE_CONFLICT"`)
	assert.Zero(t, updates)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedChunkUpdateMatchesWeKnoraRevisionSemantics(t *testing.T) {
	for _, tc := range []struct {
		name           string
		request        string
		upstreamBody   string
		wantRevision   int
		wantEnabled    bool
		wantContent    string
		wantUpdateBody map[string]any
	}{
		{
			name:           "unchanged content is a no-op",
			request:        `{"content":"stable body","expected_revision":4}`,
			upstreamBody:   `{"data":{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","seq_id":3,"chunk_type":"text","content":"stable body","is_enabled":true,"content_revision":4,"index_status":"ready"}}`,
			wantRevision:   4,
			wantEnabled:    true,
			wantContent:    "stable body",
			wantUpdateBody: map[string]any{"content": "stable body", "expected_revision": float64(4)},
		},
		{
			name:           "enabled-only edit advances content revision",
			request:        `{"is_enabled":false}`,
			upstreamBody:   `{"data":{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","seq_id":3,"chunk_type":"text","content":"stable body","is_enabled":false,"content_revision":5,"index_status":"ready"}}`,
			wantRevision:   5,
			wantEnabled:    false,
			wantContent:    "stable body",
			wantUpdateBody: map[string]any{"is_enabled": false, "expected_revision": float64(4)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Policy"}}`))
				case "/api/v1/chunks/by-id/chunk-1":
					_, _ = w.Write([]byte(`{"data":{"id":"chunk-1","knowledge_id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"seq_id":3,"chunk_type":"text","content":"stable body","is_enabled":true,"content_revision":4,"index_status":"ready"}}`))
				case "/api/v1/chunks/doc-1/chunk-1":
					var body map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					assert.Equal(t, tc.wantUpdateBody, body)
					_, _ = w.Write([]byte(tc.upstreamBody))
				default:
					t.Errorf("unexpected chunk update request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			expectManagedOperationsBase(h)
			h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
				WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "update_chunk", "chunk", "chunk-1", "doc-1").
				WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
				WithArgs(sqlmock.AnyArg(), "chunk-1", "succeeded", http.StatusOK).
				WillReturnResult(sqlmock.NewResult(0, 1))
			req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(tc.request))
			req.SetPathValue("documentID", "doc-1")
			req.SetPathValue("chunkID", "chunk-1")
			response := httptest.NewRecorder()
			h.handleManagedKnowledgeChunkUpdate(response, req, adminIdentityForManagedTest())
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var result struct {
				Data struct {
					Content         string `json:"content"`
					IsEnabled       bool   `json:"is_enabled"`
					ContentRevision int    `json:"content_revision"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
			assert.Equal(t, tc.wantContent, result.Data.Content)
			assert.Equal(t, tc.wantEnabled, result.Data.IsEnabled)
			assert.Equal(t, tc.wantRevision, result.Data.ContentRevision)
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedChunkUpdateDoesNotClaimSuccessForUnexpectedRevision(t *testing.T) {
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Policy"}}`))
		case "/api/v1/chunks/by-id/chunk-1":
			_, _ = w.Write([]byte(`{"data":{"id":"chunk-1","knowledge_id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"seq_id":3,"chunk_type":"text","content":"old body","is_enabled":true,"content_revision":4,"index_status":"ready"}}`))
		case "/api/v1/chunks/doc-1/chunk-1":
			_, _ = w.Write([]byte(`{"data":{"id":"chunk-1","knowledge_id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"seq_id":3,"chunk_type":"text","content":"new body","is_enabled":true,"content_revision":4,"index_status":"ready"}}`))
		default:
			t.Errorf("unexpected chunk update request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	expectManagedOperationsBase(h)
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "update_chunk", "chunk", "chunk-1", "doc-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "chunk-1", "uncertain", http.StatusOK).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"content":"new body","expected_revision":4}`))
	req.SetPathValue("documentID", "doc-1")
	req.SetPathValue("chunkID", "chunk-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeChunkUpdate(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"code":"KNOWLEDGE_UPSTREAM_UNAVAILABLE"`)
	assert.NotContains(t, response.Body.String(), `"outcome":"succeeded"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedFAQUpdateRechecksEntryDocumentAndTagMembership(t *testing.T) {
	faq := `{"id":7,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","standard_question":"How?","tag_id":1}`
	h := newManagedOperationsFixture(t, "faq", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/knowledge-bases/kb-1/faq/entries/7":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"data":` + faq + `}`))
				return
			}
			if r.Method == http.MethodPut {
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(t, "How do I use this?", body["standard_question"])
				_, _ = w.Write([]byte(`{"data":{"id":7,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","standard_question":"How do I use this?","tag_id":1}}`))
				return
			}
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Policy"}}`))
		case "/api/v1/knowledge-bases/kb-1/tags":
			_, _ = w.Write([]byte(`{"data":{"data":[{"id":"tag-1","seq_id":1,"name":"Research"}],"total":1}}`))
		default:
			t.Errorf("unexpected FAQ update upstream request: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})
	expectManagedOperationsBase(h)
	expectManagedOperationsBase(h) // existing entry's source registration
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "update_faq", "faq_entry", "7", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectManagedOperationsBase(h) // updated entry's source registration
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "7", "succeeded", http.StatusOK).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"standard_question":"How do I use this?","tag_id":1}`))
	req.SetPathValue("baseID", "kb-1")
	req.SetPathValue("entryID", "7")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFAQEntry(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"standard_question":"How do I use this?"`)
	assert.NotContains(t, response.Body.String(), "api_key")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedFAQBatchFieldsValidatesEntriesBeforeAuditedMutation(t *testing.T) {
	h := newManagedOperationsFixture(t, "faq", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/knowledge-bases/kb-1/faq/entries/7":
			_, _ = w.Write([]byte(`{"data":{"id":7,"knowledge_base_id":"kb-1","knowledge_id":"doc-1"}}`))
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Policy"}}`))
		case "/api/v1/knowledge-bases/kb-1/faq/entries/fields":
			if r.Method != http.MethodPut {
				t.Errorf("expected batch PUT, got %s", r.Method)
			}
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.JSONEq(t, `{"by_id":{"7":{"is_enabled":false}}}`, mustJSON(t, body))
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			t.Errorf("unexpected FAQ batch upstream request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	})
	expectManagedOperationsBase(h)
	expectManagedOperationsBase(h) // entry source registration
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "update_faq_fields", "faq_batch", "[7]", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "[7]", "succeeded", http.StatusOK).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"by_id":{"7":{"is_enabled":false}}}`))
	req.SetPathValue("baseID", "kb-1")
	req.SetPathValue("batchAction", "fields")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFAQBatch(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"outcome":"succeeded"`)
	assert.NotContains(t, response.Body.String(), "api_key")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedFAQBatchTagsValidatesBothTagAndEntryOwnership(t *testing.T) {
	h := newManagedOperationsFixture(t, "faq", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/knowledge-bases/kb-1/tags":
			_, _ = w.Write([]byte(`{"data":{"data":[{"id":"tag-1","seq_id":1,"name":"Research"}],"total":1}}`))
		case "/api/v1/knowledge-bases/kb-1/faq/entries/7":
			_, _ = w.Write([]byte(`{"data":{"id":7,"knowledge_base_id":"kb-1","knowledge_id":"doc-1"}}`))
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Policy"}}`))
		case "/api/v1/knowledge-bases/kb-1/faq/entries/tags":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.JSONEq(t, `{"7":1}`, mustJSON(t, body["updates"]))
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			t.Errorf("unexpected FAQ tag batch request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	})
	expectManagedOperationsBase(h)
	expectManagedOperationsBase(h) // entry source registration
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "update_faq_tags", "faq_batch", "[7]", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "[7]", "succeeded", http.StatusOK).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"updates":{"7":1}}`))
	req.SetPathValue("baseID", "kb-1")
	req.SetPathValue("batchAction", "tags")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFAQBatch(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"outcome":"succeeded"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedFAQBatchDeleteVerifiesEntriesBeforeDeletion(t *testing.T) {
	h := newManagedOperationsFixture(t, "faq", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/knowledge-bases/kb-1/faq/entries/7":
			_, _ = w.Write([]byte(`{"data":{"id":7,"knowledge_base_id":"kb-1","knowledge_id":"doc-1"}}`))
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Policy"}}`))
		case "/api/v1/knowledge-bases/kb-1/faq/entries":
			if r.Method != http.MethodDelete {
				t.Errorf("expected batch DELETE, got %s", r.Method)
			}
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.JSONEq(t, `{"ids":[7]}`, mustJSON(t, body))
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			t.Errorf("unexpected FAQ delete upstream request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	})
	expectManagedOperationsBase(h)
	expectManagedOperationsBase(h) // entry source registration
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "delete_faq_entries", "faq_batch", "[7]", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "[7]", "succeeded", http.StatusOK).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodDelete, "/", strings.NewReader(`{"ids":[7]}`))
	req.SetPathValue("baseID", "kb-1")
	req.SetPathValue("batchAction", "delete")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFAQBatch(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"outcome":"succeeded"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedFolderRenameAuditsVerifiedTargetPath(t *testing.T) {
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/knowledge-bases/kb-1/knowledge/folders" {
			t.Errorf("unexpected folder mutation: %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, map[string]string{"from": "old-folder", "to": "new-folder"}, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"moved_count":3,"folder_path":"new-folder"}}`))
	})
	expectManagedOperationsBase(h)
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "rename_folder", "knowledge_folder", "old-folder", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "old-folder", "succeeded", http.StatusOK).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"from":"/old-folder/","to":"/new-folder/"}`))
	req.SetPathValue("baseID", "kb-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFolders(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"folderPath":"new-folder"`)
	assert.Contains(t, response.Body.String(), `"affectedCount":3`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedFolderRenameRejectsTraversalAndUpstreamTruncationBeforeMutation(t *testing.T) {
	tooDeep := strings.TrimSuffix(strings.Repeat("folder/", managedFolderMaxDepth+1), "/")
	tooLongSegment := strings.Repeat("x", managedFolderMaxSegmentBytes+1)
	for _, target := range []struct {
		name string
		path string
	}{
		{name: "whitespace padded parent traversal", path: "docs/ .. /outside"},
		{name: "depth beyond WeKnora cap", path: tooDeep},
		{name: "segment beyond WeKnora cap", path: tooLongSegment},
	} {
		t.Run(target.name, func(t *testing.T) {
			var mutations int
			h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					mutations++
					t.Errorf("invalid folder path must not mutate upstream: %s", r.URL.Path)
				}
				http.NotFound(w, r)
			})
			expectManagedOperationsBase(h)
			req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"from":"old","to":"`+target.path+`"}`))
			req.SetPathValue("baseID", "kb-1")
			response := httptest.NewRecorder()
			h.handleManagedKnowledgeFolders(response, req, adminIdentityForManagedTest())
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			assert.Zero(t, mutations)
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedFolderMoveValidatesDocumentBeforeUpstreamMutation(t *testing.T) {
	var mutationCalls int
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000,"name":"Policy"}}`))
		case "/api/v1/knowledge/folder":
			mutationCalls++
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "kb-1", body["kb_id"])
			assert.Equal(t, "research", body["folder_path"])
			assert.Equal(t, []any{"doc-1"}, body["knowledge_ids"])
			_, _ = w.Write([]byte(`{"success":true,"data":{"moved_count":1,"folder_path":"research"}}`))
		default:
			t.Errorf("unexpected folder move upstream request: %s", r.URL.String())
			http.NotFound(w, r)
		}
	})
	expectManagedOperationsBase(h)
	expectManagedOperationsBase(h) // document ownership lookup
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "move_documents", "document_batch", "doc-1", "kb-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), "doc-1", "succeeded", http.StatusOK).
		WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"document_ids":["doc-1"],"folder_path":"/research/"}`))
	req.SetPathValue("baseID", "kb-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFolders(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"affectedCount":1`)
	assert.Equal(t, 1, mutationCalls)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedFolderMoveDoesNotMutateWhenDocumentLookupFails(t *testing.T) {
	var folderMutations int
	h := newManagedOperationsFixture(t, "document", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/knowledge/doc-foreign" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		if r.URL.Path == "/api/v1/knowledge/folder" {
			folderMutations++
		}
		t.Errorf("unexpected upstream request after source lookup failure: %s", r.URL.Path)
		http.NotFound(w, r)
	})
	expectManagedOperationsBase(h)
	expectManagedOperationsBase(h) // lookup is constrained to the registered KB
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"document_ids":["doc-foreign"],"folder_path":"research"}`))
	req.SetPathValue("baseID", "kb-1")
	response := httptest.NewRecorder()
	h.handleManagedKnowledgeFolders(response, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
	assert.Equal(t, 0, folderMutations)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestDecodeManagedPageRejectsIncompleteAndInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"upstream error", http.StatusBadGateway, `{"data":{}}`},
		{"missing data", http.StatusOK, `{}`},
		{"null data", http.StatusOK, `{"data":null}`},
		{"malformed data", http.StatusOK, `{"data":[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}
			var out map[string]any
			assert.Error(t, decodeManagedPage(response, &out))
		})
	}
	tag := managedTag{}
	require.NoError(t, json.Unmarshal([]byte(`{"id":"tag-1","seq_id":2,"knowledge_base_id":"foreign","tenant_id":999}`), &tag))
	assert.Equal(t, "tag-1", tag.ID, "membership is proven by the complete KB-scoped listing, not upstream ownership fields")
	assert.True(t, safeManagedFolderPath("design..notes/v1.2"), "ordinary names may contain multiple dots")
	assert.False(t, safeManagedFolderPath("docs/./draft"))
	assert.False(t, safeManagedFolderPath("docs/../secret"))
	assert.False(t, safeManagedFolderPath("docs/ .. /secret"), "trimmed parent segments must be rejected")
	assert.True(t, managedFolderPathWithinLimits("/manuals/2026/"))
	assert.False(t, managedFolderPathWithinLimits(strings.Repeat("x", managedFolderMaxSegmentBytes+1)))
	assert.False(t, managedFolderPathWithinLimits(strings.TrimSuffix(strings.Repeat("level/", managedFolderMaxDepth+1), "/")))
	assert.False(t, managedFolderPathWithinLimits(strings.Repeat("x", managedFolderMaxPathBytes+1)))
}
