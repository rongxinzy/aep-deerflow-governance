package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagedKnowledgeURLImportRejectsPrivateAndUnsafeURLs(t *testing.T) {
	for _, raw := range []string{
		"ftp://example.com/file.txt",
		"http://127.0.0.1/private",
		"http://[::ffff:127.0.0.1]/private",
		"http://localhost/",
		"http://user:pass@example.com/file",
		"https://example.com/file#fragment",
	} {
		t.Run(raw, func(t *testing.T) {
			err := validateManagedKnowledgeURL(context.Background(), raw)
			assert.Error(t, err)
		})
	}
	for _, raw := range []string{"https://8.8.8.8/file.pdf", "http://1.1.1.1/guide"} {
		t.Run(raw, func(t *testing.T) {
			assert.NoError(t, validateManagedKnowledgeURL(context.Background(), raw))
		})
	}
}

func TestManagedKnowledgeURLImportAuditsAndReturnsAllowlistedDocument(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge-bases/kb-1":
			_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
		case "/api/v1/knowledge-bases/kb-1/knowledge/url":
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode input: %v", err)
			}
			if input["url"] != "https://8.8.8.8/guide" || input["title"] != "Guide" || len(input) != 2 {
				t.Errorf("unexpected allowlisted request: %#v", input)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","type":"url","title":"Guide","source":"https://8.8.8.8/guide","parse_status":"processing","enable_status":"disabled"}}`))
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "create_url", "document", "", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", "accepted", http.StatusAccepted).WillReturnResult(sqlmock.NewResult(0, 1))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"url":"https://8.8.8.8/guide","title":"Guide"}`))
	req.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeImportURL(w, req, adminIdentityForManagedTest())
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"operationId":"kbop-`)
	assert.Contains(t, w.Body.String(), `"id":"doc-1"`)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeURLImportRejectsUnknownFields(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"url":"https://8.8.8.8/guide","tenant_id":"attack"}`))
	req.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeImportURL(w, req, adminIdentityForManagedTest())
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeManualImportRejectsUnknownFieldsBeforeWrites(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"title":"Guide","content":"# Safe","status":"draft"}`))
	req.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeImportManual(w, req, adminIdentityForManagedTest())
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "INVALID_REQUEST")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeDocumentOwnershipRejectsUnregisteredParent(t *testing.T) {
	stub := newServicesStubs(t)
	h := newServicesHarness(t, stub)
	configureManagedTest(t, h, "10000")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/knowledge/doc-1":
			_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"unregistered","file_name":"safe.txt"}}`))
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "unregistered").WillReturnError(sql.ErrNoRows)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("documentID", "doc-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeGetDocument(w, req, adminIdentityForManagedTest())
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "unregistered")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestManagedKnowledgeDownloadStreamsOnlyAfterOwnershipAndNoRedirect(t *testing.T) {
	for _, mode := range []string{"stream", "redirect-refused", "unknown-length", "unknown-empty", "read-error", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			redirect := mode == "redirect-refused"
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			var redirectedTargetHit bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","file_name":"report.pdf"}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				case "/api/v1/knowledge/doc-1/download":
					if redirect {
						http.Redirect(w, r, "/api/v1/should-not-follow", http.StatusFound)
						return
					}
					if mode == "unknown-length" || mode == "unknown-empty" {
						w.Header().Set("Transfer-Encoding", "chunked")
						w.(http.Flusher).Flush()
						if mode == "unknown-length" {
							_, _ = w.Write([]byte("pdf-bytes"))
						}
						return
					}
					if mode == "read-error" {
						w.Header().Set("Transfer-Encoding", "chunked")
						w.(http.Flusher).Flush()
						_, _ = w.Write([]byte("partial"))
						w.(http.Flusher).Flush()
						conn, _, hijackErr := w.(http.Hijacker).Hijack()
						if hijackErr != nil {
							t.Errorf("hijack upstream response: %v", hijackErr)
							return
						}
						_ = conn.Close()
						return
					}
					if mode == "oversized" {
						w.Header().Set("Content-Length", "268435457")
						_, _ = w.Write([]byte("large"))
						return
					}
					w.Header().Set("Content-Type", "text/html")
					_, _ = w.Write([]byte("pdf-bytes"))
				case "/api/v1/should-not-follow":
					redirectedTargetHit = true
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "read_download", "document", "doc-1", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
			outcome, status := "succeeded", http.StatusOK
			if redirect {
				outcome, status = "failed", http.StatusFound
			} else if mode == "unknown-empty" || mode == "read-error" {
				outcome, status = "failed", http.StatusBadGateway
			} else if mode == "oversized" {
				outcome, status = "failed", http.StatusRequestEntityTooLarge
			}
			h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", outcome, status).WillReturnResult(sqlmock.NewResult(0, 1))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("documentID", "doc-1")
			req.SetPathValue("fileKind", "download")
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeFile(w, req, adminIdentityForManagedTest())
			if redirect || mode == "unknown-empty" || mode == "read-error" {
				assert.Equal(t, http.StatusBadGateway, w.Code)
				assert.NotContains(t, w.Body.String(), "should-not-follow")
				assert.NotContains(t, w.Body.String(), "pdf-bytes")
				assert.NotContains(t, w.Body.String(), "partial")
			} else if mode == "unknown-length" {
				assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.Equal(t, "pdf-bytes", w.Body.String())
				assert.Equal(t, "9", w.Header().Get("Content-Length"))
			} else if mode == "oversized" {
				assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
				assert.NotContains(t, w.Body.String(), "large")
			} else {
				assert.Equal(t, http.StatusOK, w.Code)
				assert.Equal(t, "pdf-bytes", w.Body.String())
				assert.Equal(t, "application/pdf", w.Header().Get("Content-Type"))
				assert.Contains(t, w.Header().Get("Content-Disposition"), "report.pdf")
				assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
			}
			assert.False(t, redirectedTargetHit)
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

type managedKnowledgeReadErrorReader struct{ read bool }

func (r *managedKnowledgeReadErrorReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, errors.New("synthetic upstream read failure")
	}
	r.read = true
	return copy(p, "partial"), nil
}

func TestManagedKnowledgeFileSpoolIsBoundedAndCleansUp(t *testing.T) {
	for _, tc := range []struct {
		name         string
		reader       io.Reader
		declaredSize int64
		maxBytes     int64
		wantSize     int64
		wantErr      error
	}{
		{name: "unknown-length success", reader: strings.NewReader("pdf bytes"), declaredSize: -1, maxBytes: 16, wantSize: 9},
		{name: "unknown-length oversized", reader: strings.NewReader("123456"), declaredSize: -1, maxBytes: 5, wantErr: errManagedKnowledgeFileTooLarge},
		{name: "upstream read error", reader: &managedKnowledgeReadErrorReader{}, declaredSize: -1, maxBytes: 16, wantErr: errors.New("synthetic upstream read failure")},
		{name: "empty stream", reader: strings.NewReader(""), declaredSize: -1, maxBytes: 16, wantErr: errors.New("knowledge file is empty")},
		{name: "declared length mismatch", reader: strings.NewReader("short"), declaredSize: 6, maxBytes: 16, wantErr: errors.New("knowledge file length mismatch")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			spool, err := spoolManagedKnowledgeFile(tc.reader, tc.declaredSize, tc.maxBytes, tempDir)
			if tc.wantErr != nil {
				assert.Error(t, err)
				if errors.Is(tc.wantErr, errManagedKnowledgeFileTooLarge) {
					assert.ErrorIs(t, err, errManagedKnowledgeFileTooLarge)
				} else {
					assert.EqualError(t, err, tc.wantErr.Error())
				}
				assert.Nil(t, spool)
				entries, readErr := os.ReadDir(tempDir)
				require.NoError(t, readErr)
				assert.Empty(t, entries)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, spool)
			assert.Equal(t, tc.wantSize, spool.size)
			info, statErr := spool.file.Stat()
			require.NoError(t, statErr)
			if runtime.GOOS != "windows" {
				assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			}
			spool.close()
			entries, readErr := os.ReadDir(tempDir)
			require.NoError(t, readErr)
			assert.Empty(t, entries)
		})
	}
}

func TestManagedKnowledgeDocumentMutationsUseFixedRoutesAndAudit(t *testing.T) {
	for _, action := range []string{"reparse", "cancel-parse", "delete"} {
		t.Run(action, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","file_name":"guide.md"}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				case "/api/v1/knowledge/doc-1/" + action:
					if action == "delete" {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","file_name":"guide.md","parse_status":"pending"}}`))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			verb := action
			if action == "cancel-parse" {
				verb = "cancel_parse"
			} else if action == "delete" {
				verb = "delete_document"
			}
			h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", verb, "document", "doc-1", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
			auditStatus := http.StatusOK
			responseStatus := http.StatusAccepted
			if action == "delete" {
				responseStatus = http.StatusOK
			}
			h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", "succeeded", auditStatus).WillReturnResult(sqlmock.NewResult(0, 1))
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if action == "delete" {
				req.Method = http.MethodDelete
			}
			req.SetPathValue("documentID", "doc-1")
			w := httptest.NewRecorder()
			h.managedDocumentAction(action)(w, req, adminIdentityForManagedTest())
			assert.Equal(t, responseStatus, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), `"operationId":"kbop-`)
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeChunkAndSpanResponsesAreScopedAndProjected(t *testing.T) {
	for _, kind := range []string{"chunks", "spans"} {
		t.Run(kind, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","file_name":"guide.md"}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				case "/api/v1/knowledge/doc-1/spans":
					_, _ = w.Write([]byte(`{"data":{"knowledge_id":"doc-1","attempt":1,"latest_attempt":1,"parse_status":"completed","current_stage":"postprocess","trace":{"knowledge_id":"doc-1","span_id":"root","name":"root","kind":"root","status":"done","input":{"token":"never-return"},"children":[{"knowledge_id":"doc-1","span_id":"stage-1","name":"chunking","kind":"stage","status":"done","error_detail":"secret"}]}}}`))
				case "/api/v1/chunks/doc-1":
					_, _ = w.Write([]byte(`{"data":[{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","seq_id":1,"content":"safe text","chunk_type":"text","is_enabled":true,"image_info":"[{\"url\":\"https://external.invalid/private-image\"}]"}],"total":1,"page":1,"page_size":10}`))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			action := "read_chunks"
			if kind == "spans" {
				action = "read_spans"
			}
			h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", action, "document", "doc-1", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", "succeeded", http.StatusOK).WillReturnResult(sqlmock.NewResult(0, 1))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("documentID", "doc-1")
			w := httptest.NewRecorder()
			if kind == "chunks" {
				h.handleManagedKnowledgeChunks(w, req, adminIdentityForManagedTest())
			} else {
				h.handleManagedKnowledgeSpans(w, req, adminIdentityForManagedTest())
			}
			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), `"knowledge_id":"doc-1"`)
			assert.NotContains(t, w.Body.String(), "never-return")
			assert.NotContains(t, w.Body.String(), "external.invalid")
			assert.NotContains(t, w.Body.String(), "error_detail")
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedChunkImageProxyRequiresVerifiedChunkAndImageBytes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		chunkJSON string
		imageType string
		want      int
	}{
		{name: "success", chunkJSON: `{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","image_info":"[{\"url\":\"resource://abcdefghijklmnopqrstuv\"}]"}`, imageType: "image/png", want: 200},
		{name: "cross tenant", chunkJSON: `{"id":"chunk-1","tenant_id":90000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","image_info":"[{\"url\":\"resource://abcdefghijklmnopqrstuv\"}]"}`, imageType: "image/png", want: 404},
		{name: "cross KB", chunkJSON: `{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-other","knowledge_id":"doc-1","image_info":"[{\"url\":\"resource://abcdefghijklmnopqrstuv\"}]"}`, imageType: "image/png", want: 404},
		{name: "external URL", chunkJSON: `{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","image_info":"[{\"url\":\"https://example.invalid/image.png\"}]"}`, imageType: "image/png", want: 404},
		{name: "wrong MIME bytes", chunkJSON: `{"id":"chunk-1","tenant_id":10000,"knowledge_base_id":"kb-1","knowledge_id":"doc-1","image_info":"[{\"url\":\"resource://abcdefghijklmnopqrstuv\"}]"}`, imageType: "image/png", want: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newServicesStubs(t)
			h := newServicesHarness(t, stub)
			configureManagedTest(t, h, "10000")
			imageHits := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","file_name":"guide.md"}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
				case "/api/v1/chunks/by-id/chunk-1":
					_, _ = w.Write([]byte(`{"data":` + tc.chunkJSON + `}`))
				case "/api/v1/knowledge/doc-1/chunks/chunk-1/images/0":
					imageHits++
					w.Header().Set("Content-Type", tc.imageType)
					if tc.name == "wrong MIME bytes" {
						_, _ = w.Write([]byte("not an image"))
					} else {
						_, _ = w.Write([]byte("\x89PNG\r\n\x1a\nsynthetic image"))
					}
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			if tc.name == "success" || tc.name == "wrong MIME bytes" {
				h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "read_chunk_image", "document", "doc-1", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.SetPathValue("documentID", "doc-1")
			req.SetPathValue("chunkID", "chunk-1")
			req.SetPathValue("index", "0")
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeChunkImage(w, req, adminIdentityForManagedTest())
			assert.Equal(t, tc.want, w.Code, w.Body.String())
			assert.NotContains(t, w.Body.String(), "resource://")
			assert.NotContains(t, w.Body.String(), "example.invalid")
			if tc.name == "success" {
				assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
				assert.Contains(t, w.Body.String(), "synthetic image")
				assert.Equal(t, 1, imageHits)
			} else if tc.name == "cross tenant" || tc.name == "cross KB" || tc.name == "external URL" {
				assert.Zero(t, imageHits, "unverified ownership/reference must not reach the image endpoint")
			}
			assert.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedKnowledgeEnableStatusContract(t *testing.T) {
	t.Run("disable processing document with durable audit", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		configureManagedTest(t, h, "10000")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/auth/me":
				_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
			case "/api/v1/knowledge/doc-1":
				_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","parse_status":"processing","enable_status":"enabled"}}`))
			case "/api/v1/knowledge-bases/kb-1":
				_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
			case "/api/v1/knowledge/doc-1/enable-status":
				assert.Equal(t, http.MethodPut, r.Method)
				body, _ := io.ReadAll(r.Body)
				assert.JSONEq(t, `{"enabled":false}`, string(body))
				_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","parse_status":"processing","enable_status":"disabled"}}`))
			default:
				t.Errorf("unexpected upstream request %s", r.URL.Path)
			}
		}))
		defer server.Close()
		h.cfg.knowledgeAPIURL = server.URL
		h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
		h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "set_enabled", "document", "doc-1", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
		h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "doc-1", "succeeded", http.StatusOK).WillReturnResult(sqlmock.NewResult(0, 1))
		req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"enabled":false}`))
		req.SetPathValue("documentID", "doc-1")
		w := httptest.NewRecorder()
		h.handleManagedKnowledgeSetDocumentEnabled(w, req, adminIdentityForManagedTest())
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), `"enable_status":"disabled"`)
		assert.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("cannot enable before parsing completes", func(t *testing.T) {
		stub := newServicesStubs(t)
		h := newServicesHarness(t, stub)
		configureManagedTest(t, h, "10000")
		mutations := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/auth/me":
				_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
			case "/api/v1/knowledge/doc-1":
				_, _ = w.Write([]byte(`{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1","parse_status":"processing","enable_status":"disabled"}}`))
			case "/api/v1/knowledge-bases/kb-1":
				_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
			case "/api/v1/knowledge/doc-1/enable-status":
				mutations++
			default:
				t.Errorf("unexpected upstream request %s", r.URL.Path)
			}
		}))
		defer server.Close()
		h.cfg.knowledgeAPIURL = server.URL
		h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
		req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"enabled":true}`))
		req.SetPathValue("documentID", "doc-1")
		w := httptest.NewRecorder()
		h.handleManagedKnowledgeSetDocumentEnabled(w, req, adminIdentityForManagedTest())
		assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
		assert.Zero(t, mutations)
		assert.NoError(t, h.dbmock.ExpectationsWereMet())
	})
}

func TestManagedKnowledgeMalformedSpansDoNotRecordSuccessAudit(t *testing.T) {
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
		case "/api/v1/knowledge/doc-1/spans":
			_, _ = w.Write([]byte(`{"data":{"knowledge_id":"doc-1","attempt":1,"latest_attempt":1,"parse_status":"completed","current_stage":"completed","trace":{"attempt":"not-an-integer"}}}`))
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("demo", "10000", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetPathValue("documentID", "doc-1")
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeSpans(w, req, adminIdentityForManagedTest())
	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "KNOWLEDGE_UPSTREAM_INVALID")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())
}

func adminIdentityForManagedTest() identity {
	return identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}}
}

func TestManagedFileNameHeadersAreSanitized(t *testing.T) {
	got := mime.FormatMediaType("attachment", map[string]string{"filename": managedSafeFileName("..\\bad\r\n.txt", "fallback")})
	assert.NotContains(t, got, "\r")
	assert.NotContains(t, got, "\n")
	assert.NotContains(t, got, "../")
	assert.Contains(t, got, "bad.txt")
}

func TestManagedKnowledgeURLRejectsPrivateDNSResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for name, ips := range map[string][]net.IPAddr{
		"private":           {{IP: net.ParseIP("10.2.3.4")}},
		"mapped-loopback":   {{IP: net.ParseIP("::ffff:127.0.0.1")}},
		"carrier-grade-nat": {{IP: net.ParseIP("100.64.0.1")}},
		"mixed-public-private": {
			{IP: net.ParseIP("8.8.8.8")},
			{IP: net.ParseIP("192.168.1.2")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			lookup := func(context.Context, string) ([]net.IPAddr, error) { return ips, nil }
			assert.Error(t, validateManagedKnowledgeURLWithLookup(ctx, "https://public.example/resource", lookup))
		})
	}
	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		assert.Equal(t, "public.example", host)
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	assert.NoError(t, validateManagedKnowledgeURLWithLookup(ctx, "https://public.example/resource", lookup))
}
