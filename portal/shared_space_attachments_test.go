package main

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSharedAttachmentUpload(t *testing.T) {
	for _, mode := range []string{"applied", "unknown", "rejected", "viewer", "conversation", "replay", "empty", "malformed", "invalid-request", "missing-file", "receipt-error", "start-error", "finish-error", "commit-error", "wrong-receipt"} {
		t.Run(mode, func(t *testing.T) {
			role := "editor"
			if mode == "viewer" {
				role = "viewer"
			}
			p, m := sharedNativePortal(t, role, true)
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					if mode == "conversation" {
						writeJSON(w, 200, map[string]any{"id": "task", "companyId": "company", "projectId": "project", "conversationAgentId": "agent"})
					} else {
						sharedNativeTask(w)
					}
					return
				}
				require.NoError(t, r.ParseMultipartForm(1024))
				f, h, e := r.FormFile("file")
				require.NoError(t, e)
				defer func() { _ = f.Close() }()
				data, e := io.ReadAll(f)
				require.NoError(t, e)
				require.Equal(t, []byte("原始字节"), data)
				require.Equal(t, "中文.txt", h.Filename)
				if mode == "unknown" {
					w.WriteHeader(503)
					return
				}
				if mode == "rejected" {
					writeJSON(w, 422, map[string]string{"error": "too large"})
					return
				}
				task := "task"
				if mode == "wrong-receipt" {
					task = "other"
				}
				writeJSON(w, 201, map[string]any{"id": "a", "issueId": task, "originalFilename": "中文.txt", "byteSize": len(data)})
			})
			var b bytes.Buffer
			mw := multipart.NewWriter(&b)
			request := "r"
			if mode == "invalid-request" {
				request = ""
			}
			require.NoError(t, mw.WriteField("requestId", request))
			if mode != "missing-file" {
				f, e := mw.CreateFormFile("file", "中文.txt")
				require.NoError(t, e)
				if mode != "empty" {
					_, e = f.Write([]byte("原始字节"))
					require.NoError(t, e)
				}
			}
			require.NoError(t, mw.Close())
			early := mode == "viewer" || mode == "conversation" || mode == "empty" || mode == "malformed" || mode == "invalid-request" || mode == "missing-file"
			if !early {
				q := m.ExpectQuery("SELECT actor_id,action,task_id,payload_hash,state,receipt")
				switch mode {
				case "receipt-error":
					q.WillReturnError(io.ErrUnexpectedEOF)
				case "replay":
					q.WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "task", "hash", "state", "receipt"}).AddRow("person", "upload_attachment", "attachment:task:r", sharedHash([]any{"中文.txt", "application/octet-stream", []byte("原始字节")}), "applied", `{"requestId":"r","status":"applied"}`))
				default:
					q.WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "task", "hash", "state", "receipt"}))
					blocked := m.ExpectQuery("SELECT EXISTS")
					blocked.WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(mode == "start-error"))
					if mode != "start-error" {
						m.ExpectExec("INSERT INTO shared_space_commands").WillReturnResult(sqlmock.NewResult(0, 1))
						switch mode {
						case "unknown", "wrong-receipt":
							m.ExpectExec("UPDATE shared_space_commands SET state='unknown'").WillReturnResult(sqlmock.NewResult(0, 1))
						case "rejected":
							m.ExpectExec("DELETE FROM shared_space_commands").WillReturnResult(sqlmock.NewResult(0, 1))
						default:
							m.ExpectBegin()
							q := m.ExpectExec("UPDATE shared_space_commands SET state='applied'")
							if mode == "finish-error" {
								q.WillReturnError(io.ErrUnexpectedEOF)
								m.ExpectRollback()
							} else {
								q.WillReturnResult(sqlmock.NewResult(0, 1))
								if mode == "commit-error" {
									m.ExpectCommit().WillReturnError(io.ErrUnexpectedEOF)
								} else {
									m.ExpectCommit()
								}
							}
							if mode == "finish-error" || mode == "commit-error" {
								m.ExpectExec("UPDATE shared_space_commands SET state='unknown'").WillReturnResult(sqlmock.NewResult(0, 1))
							}
						}
					}
				}
			}
			sharedNativeUnlock(m)
			r := sharedNativeRequest("POST", "/", "")
			r.Body = io.NopCloser(&b)
			r.Header.Set("Content-Type", mw.FormDataContentType())
			if mode == "malformed" {
				r.Header.Set("Content-Type", "text/plain")
			}
			w := httptest.NewRecorder()
			err := p.sharedUploadAttachment(w, r, sharedNativeIdentity)
			switch mode {
			case "applied":
				require.NoError(t, err)
				require.Equal(t, 201, w.Code)
				require.Contains(t, w.Body.String(), `"status":"applied"`)
			case "unknown", "wrong-receipt", "finish-error", "commit-error":
				require.NoError(t, err)
				require.Equal(t, 202, w.Code)
			case "replay":
				require.NoError(t, err)
				require.Equal(t, 200, w.Code)
			default:
				require.Error(t, err)
				if mode == "rejected" {
					sharedWriteError(w, err)
					require.Equal(t, 422, w.Code)
					require.Contains(t, w.Body.String(), "too large")
				}
			}
		})
	}
}
func TestSharedAttachmentReads(t *testing.T) {
	for _, mode := range []string{"list", "foreign-list", "list-error", "content", "range", "unsatisfiable", "wrong", "upstream-error", "scope", "conversation"} {
		t.Run(mode, func(t *testing.T) {
			p, _ := sharedNativePortal(t, "viewer", false)
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/issues/task":
					if mode == "scope" {
						writeJSON(w, 200, map[string]any{"id": "other"})
					} else {
						sharedNativeTask(w)
					}
				case "/issues/task/attachments":
					if mode == "list-error" {
						w.WriteHeader(503)
						return
					}
					task := "task"
					if mode == "foreign-list" {
						task = "other"
					}
					writeJSON(w, 200, []any{map[string]any{"id": "a", "issueId": task, "originalFilename": "中文.txt", "contentType": "text/plain", "byteSize": 6, "provider": "secret"}})
				default:
					if mode == "upstream-error" {
						w.WriteHeader(503)
						return
					}
					require.Equal(t, "true", r.URL.Query().Get("download"))
					http.ServeContent(w, r, "中文.txt", time.Time{}, bytes.NewReader([]byte("abcdef")))
				}
			})
			r := sharedNativeRequest("GET", "/?download=1", "")
			r.SetPathValue("attachment", "a")
			if mode == "wrong" {
				r.SetPathValue("attachment", "wrong")
			}
			switch mode {
			case "range":
				r.Header.Set("Range", "bytes=1-3")
			}
			if mode == "unsatisfiable" {
				r.Header.Set("Range", "bytes=99-100")
			}
			w := httptest.NewRecorder()
			var err error
			if mode == "list" || mode == "foreign-list" || mode == "list-error" || mode == "conversation" {
				err = p.sharedAttachments(w, r, sharedNativeIdentity)
			} else {
				err = p.sharedAttachmentContent(w, r, sharedNativeIdentity)
			}
			switch mode {
			case "foreign-list", "list-error", "scope", "wrong", "upstream-error":
				require.Error(t, err)
			default:
				require.NoError(t, err)
				switch mode {
				case "range":
					require.Equal(t, 206, w.Code)
					require.Equal(t, "bcd", w.Body.String())
				case "unsatisfiable":
					require.Equal(t, 416, w.Code)
				case "content":
					require.Equal(t, "abcdef", w.Body.String())
				default:
					require.NotContains(t, w.Body.String(), "secret")
					require.Contains(t, w.Body.String(), `"uploadAttachment":false`)
				}
			}
		})
	}
}

func TestSharedAttachmentCommands(t *testing.T) {
	for _, mode := range []string{"applied", "unknown", "sending", "absent", "actor", "action", "task", "database", "bad-receipt"} {
		t.Run(mode, func(t *testing.T) {
			p, m := sharedNativePortal(t, "editor", true)
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) { sharedNativeTask(w) })
			q := m.ExpectQuery("SELECT actor_id,action,task_id,payload_hash,state,receipt")
			rows := sqlmock.NewRows([]string{"actor", "action", "task", "hash", "state", "receipt"})
			actor, action, task, state, receipt := "person", "upload_attachment", "attachment:task:r", mode, `{"requestId":"r","status":"applied"}`
			switch mode {
			case "actor":
				actor = "other"
			case "action":
				action = "claim"
			case "task":
				task = "attachment:wrong:r"
			case "bad-receipt":
				state = "applied"
				receipt = "{"
			}
			switch mode {
			case "database":
				q.WillReturnError(io.ErrUnexpectedEOF)
			case "absent":
				q.WillReturnRows(rows)
			default:
				q.WillReturnRows(rows.AddRow(actor, action, task, "hash", state, receipt))
			}
			sharedNativeUnlock(m)
			r := sharedNativeRequest("GET", "/", "")
			r.SetPathValue("attachment", "commands")
			r.SetPathValue("operation", "r")
			w := httptest.NewRecorder()
			err := p.sharedAttachmentReadRoute(w, r, sharedNativeIdentity)
			switch mode {
			case "applied", "unknown", "sending":
				require.NoError(t, err)
				require.Contains(t, w.Body.String(), `"status":"`+mode+`"`)
			default:
				require.Error(t, err)
			}
		})
	}
	p := &portal{}
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("operation", "missing")
	require.Error(t, p.sharedAttachmentReadRoute(httptest.NewRecorder(), r, identity{}))
	require.Empty(t, sharedAttachmentReason("owner", map[string]any{"status": "done", "executionPolicy": map[string]any{"active": true}}))
	require.NotEmpty(t, sharedAttachmentReason("editor", map[string]any{"conversationAgentId": "a"}))
}
