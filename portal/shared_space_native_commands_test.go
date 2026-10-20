package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSharedNativeCreateSpace(t *testing.T) {
	for _, mode := range []string{"create", "replay", "conflict", "lost-ack", "commit-error", "invalid-person"} {
		t.Run(mode, func(t *testing.T) {
			clearSharedSpaceAdminTokenCache(t)
			adminTokenCache.mu.Lock()
			adminTokenCache.token = "synthetic"
			adminTokenCache.expires = time.Now().Add(time.Minute)
			adminTokenCache.mu.Unlock()
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); _ = db.Close() })
			p := &portal{db: db}
			p.cfg.paperclipCompanyID = "company"
			if mode != "invalid-person" {
				m.ExpectExec("SELECT pg_advisory_lock").WillReturnResult(sqlmock.NewResult(0, 1))
				q := m.ExpectQuery("SELECT actor_id,payload_hash,receipt")
				hash := sharedHash([]any{"create_space", "company", map[string]any{"name": "新空间", "description": "目标"}})
				switch mode {
				case "replay":
					q.WillReturnRows(sqlmock.NewRows([]string{"actor", "hash", "receipt"}).AddRow("person", hash, `{"space":{"id":"prior"}}`))
				case "conflict":
					q.WillReturnRows(sqlmock.NewRows([]string{"actor", "hash", "receipt"}).AddRow("other", hash, `{}`))
				default:
					q.WillReturnError(sql.ErrNoRows)
				}
				if mode == "create" || mode == "commit-error" {
					m.ExpectBegin()
					m.ExpectExec("INSERT INTO shared_spaces").WillReturnResult(sqlmock.NewResult(0, 1))
					m.ExpectExec("INSERT INTO shared_space_members").WillReturnResult(sqlmock.NewResult(0, 1))
					m.ExpectExec("INSERT INTO shared_space_bootstrap_commands").WillReturnResult(sqlmock.NewResult(0, 1))
					commit := m.ExpectCommit()
					if mode == "commit-error" {
						commit.WillReturnError(http.ErrAbortHandler)
					}
				}
				sharedNativeUnlock(m)
			}
			creates := 0
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/aep/v1/admin/users/person" {
					status := "active"
					if mode == "invalid-person" {
						status = "disabled"
					}
					writeJSON(w, 200, map[string]any{"id": "person", "status": status, "kind": "human"})
					return
				}
				creates++
				require.Equal(t, "/companies/company/projects", r.URL.Path)
				require.Equal(t, "POST", r.Method)
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, "新空间", body["name"])
				require.NotEmpty(t, body["idempotencyKey"])
				require.Nil(t, body["companyId"])
				if mode == "lost-ack" {
					w.WriteHeader(503)
					return
				}
				writeJSON(w, 201, map[string]any{"id": "project", "companyId": "company", "name": "新空间", "env": "private"})
			})
			p.cfg.aepBaseURL = p.cfg.paperclipBaseURL
			w := httptest.NewRecorder()
			err = p.sharedCreateSpace(w, sharedNativeRequest("POST", "/", `{"requestId":"create","name":"新空间","description":"目标"}`), sharedNativeIdentity)
			if mode == "conflict" || mode == "lost-ack" || mode == "commit-error" || mode == "invalid-person" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotContains(t, w.Body.String(), "private")
			}
			if mode == "create" || mode == "lost-ack" || mode == "commit-error" {
				require.Equal(t, 1, creates)
			} else {
				require.Zero(t, creates)
			}
		})
	}
}

func TestSharedNativeCommentCommands(t *testing.T) {
	for _, mode := range []string{"post", "viewer", "lost-ack", "replay", "different-body", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			role := "editor"
			if mode == "viewer" {
				role = "viewer"
			}
			p, m := sharedNativePortal(t, role, true)
			if mode != "viewer" {
				q := m.ExpectQuery("SELECT actor_id,action,payload_hash,state,receipt")
				b := struct {
					Body            string `json:"body"`
					ClientRequestID string `json:"clientRequestId"`
				}{"请讨论", "00000000-0000-4000-8000-000000000000"}
				hash := sharedHash([]any{"task", b})
				switch mode {
				case "replay":
					q.WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}).AddRow("person", "comment", hash, "applied", `{"comment":{"id":"comment"}}`))
				case "different-body":
					q.WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}).AddRow("person", "comment", "other", "applied", `{}`))
				case "unknown":
					q.WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}).AddRow("person", "comment", hash, "unknown", nil))
				default:
					q.WillReturnError(sql.ErrNoRows)
					m.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
					m.ExpectExec("INSERT INTO shared_space_commands").WillReturnResult(sqlmock.NewResult(0, 1))
					if mode == "lost-ack" {
						m.ExpectExec("UPDATE shared_space_commands SET state='unknown'").WillReturnResult(sqlmock.NewResult(0, 1))
					} else {
						m.ExpectBegin()
						m.ExpectExec("UPDATE shared_space_commands SET state='applied'").WillReturnResult(sqlmock.NewResult(0, 1))
						m.ExpectCommit()
					}
				}
			}
			sharedNativeUnlock(m)
			posts := 0
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/issues/task" {
					sharedNativeTask(w)
					return
				}
				posts++
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, "请讨论\n\n同事 · 经知远发布", body["body"])
				require.Nil(t, body["onBehalfOfUserId"])
				if mode == "lost-ack" {
					w.WriteHeader(503)
					return
				}
				writeJSON(w, 201, map[string]any{"id": "comment", "issueId": "task", "body": body["body"], "metadata": "private"})
			})
			w := httptest.NewRecorder()
			err := p.sharedAddComment(w, sharedNativeRequest("POST", "/", `{"body":"请讨论","clientRequestId":"00000000-0000-4000-8000-000000000000"}`), sharedNativeIdentity)
			if mode == "post" || mode == "replay" {
				require.NoError(t, err)
				require.NotContains(t, w.Body.String(), "private")
			} else {
				require.Error(t, err)
			}
			if mode == "post" || mode == "lost-ack" {
				require.Equal(t, 1, posts)
			} else {
				require.Zero(t, posts)
			}
		})
	}
}
func TestSharedNativeCommentAuthors(t *testing.T) {
	p, m := sharedNativePortal(t, "viewer", false)
	m.ExpectQuery("SELECT actor_id,receipt").WillReturnRows(sqlmock.NewRows([]string{"actor", "receipt"}).AddRow("person", `{"comment":{"id":"comment","author":{"displayName":"同事"}}}`))
	sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/issues/task" {
			sharedNativeTask(w)
			return
		}
		writeJSON(w, 200, []any{map[string]any{"id": "comment", "issueId": "task", "body": "正文", "metadata": "private"}, map[string]any{"id": "native", "issueId": "task", "body": "同事 · 经知远发布"}})
	})
	w := httptest.NewRecorder()
	require.NoError(t, p.sharedComments(w, sharedNativeRequest("GET", "/", ""), sharedNativeIdentity))
	require.Contains(t, w.Body.String(), "Paperclip 发布者")
	require.Contains(t, w.Body.String(), `"userId":"person"`)
	require.NotContains(t, w.Body.String(), "private")
}

func TestSharedNativeCommentsDoNotMutateNativeLifecycle(t *testing.T) {
	for _, field := range []string{"done", "cancelled", "blocked", "executionPolicy", "executionState", "executionRunId", "checkoutRunId"} {
		t.Run(field, func(t *testing.T) {
			p, m := sharedNativePortal(t, "editor", true)
			m.ExpectQuery("SELECT actor_id,action,payload_hash,state,receipt").WillReturnError(sql.ErrNoRows)
			sharedNativeUnlock(m)
			task := map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": "todo", "assigneeAgentId": "agent"}
			switch field {
			case "done", "cancelled", "blocked":
				task["status"] = field
			case "executionPolicy", "executionState":
				task[field] = map[string]any{"status": "pending"}
			default:
				task[field] = "native-run"
			}
			require.False(t, sharedCanComment(task))
			require.NotEmpty(t, sharedCommentUnavailableReason(task))
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					t.Error("comment POST would implicitly reopen or approve native work")
					task["status"] = "todo"
					w.WriteHeader(500)
					return
				}
				writeJSON(w, 200, task)
			})
			before := task["status"]
			err := p.sharedAddComment(httptest.NewRecorder(), sharedNativeRequest("POST", "/", `{"body":"approve","clientRequestId":"00000000-0000-4000-8000-000000000005"}`), sharedNativeIdentity)
			require.Error(t, err)
			require.Equal(t, before, task["status"])
		})
	}
}

func TestSharedNativeParentTaskScope(t *testing.T) {
	for _, mode := range []string{"same-space", "foreign-space"} {
		t.Run(mode, func(t *testing.T) {
			p, m := sharedNativePortal(t, "editor", true)
			m.ExpectQuery("SELECT actor_id,action,payload_hash,state,receipt").WillReturnError(sql.ErrNoRows)
			if mode == "same-space" {
				m.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
				m.ExpectExec("INSERT INTO shared_space_commands").WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectBegin()
				m.ExpectExec("UPDATE shared_space_commands SET state='applied'").WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectCommit()
			}
			sharedNativeUnlock(m)
			creates := 0
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					project := "project"
					if mode == "foreign-space" {
						project = "foreign"
					}
					writeJSON(w, 200, map[string]any{"id": "parent", "companyId": "company", "projectId": project})
					return
				}
				creates++
				var b map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
				require.Equal(t, "parent", b["parentId"])
				b["id"] = "child"
				b["companyId"] = "company"
				writeJSON(w, 201, b)
			})
			w := httptest.NewRecorder()
			err := p.sharedCreateTask(w, sharedNativeRequest("POST", "/", `{"title":"子任务","requestId":"parent-child","parentId":"parent"}`), sharedNativeIdentity)
			if mode == "same-space" {
				require.NoError(t, err)
				require.Equal(t, 1, creates)
				require.Contains(t, w.Body.String(), `"parentId":"parent"`)
			} else {
				require.Error(t, err)
				require.Zero(t, creates)
			}
		})
	}
}
