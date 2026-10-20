package main

import (
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSharedMetadataInput(t *testing.T) {
	valid := `{"requestId":"r","expectedMetadataToken":"v","title":" 标题 ","description":"正文","priority":"high"}`
	for _, extra := range []string{"status", "assigneeUserId", "assigneeAgentId", "parentId", "blockedBy", "blockedByIssueIds"} {
		var in sharedMetadataInput
		require.Error(t, sharedDecode(httptest.NewRecorder(), httptest.NewRequest("PATCH", "/", strings.NewReader(strings.TrimSuffix(valid, "}")+`,"`+extra+`":"x"}`)), &in))
	}
	for _, raw := range []string{valid, strings.Replace(valid, `"description":"正文",`, "", 1), strings.Replace(valid, `"priority":"high"`, `"priority":""`, 1), strings.Replace(valid, "标题", strings.Repeat("字", 241), 1)} {
		var in sharedMetadataInput
		require.NoError(t, json.Unmarshal([]byte(raw), &in))
		if raw == valid {
			require.NoError(t, in.validate())
			require.Equal(t, "标题", *in.Title)
			require.True(t, in.matches(in.body()))
		} else {
			require.Error(t, in.validate())
		}
	}
	task := map[string]any{"id": "t", "title": "one", "description": "", "priority": "low"}
	token := sharedMetadataToken(task)
	task["title"] = "two"
	require.NotEqual(t, token, sharedMetadataToken(task))
}

func TestSharedMetadataReceiptRecovery(t *testing.T) {
	for _, mode := range []string{"absent", "applied", "conflict", "unknown-match", "unknown-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			defer func() { require.NoError(t, m.ExpectationsWereMet()) }()
			p := &portal{db: db}
			title, desc, priority := "title", "body", "high"
			in := sharedMetadataInput{RequestID: "r", Token: "token", Title: &title, Description: &desc, Priority: &priority}
			state, hash := "unknown", "hash"
			if mode == "applied" {
				state = "applied"
			}
			if mode == "conflict" {
				hash = "other"
			}
			rows := sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"})
			if mode != "absent" {
				rows.AddRow("actor", "task_metadata", hash, state, `{"state":"applied"}`)
			}
			m.ExpectQuery("SELECT actor_id,action").WillReturnRows(rows)
			if strings.HasPrefix(mode, "unknown-") {
				sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, "GET", r.Method)
					task := in.body()
					task["id"] = "task"
					task["companyId"] = "company"
					task["projectId"] = "project"
					if mode == "unknown-mismatch" {
						task["title"] = "other"
					}
					writeJSON(w, 200, task)
				})
				if mode == "unknown-match" {
					m.ExpectBegin()
					m.ExpectExec("UPDATE shared_space_commands SET state='applied'").WillReturnResult(sqlmock.NewResult(0, 1))
					m.ExpectCommit()
				}
			}
			c, err := db.Conn(t.Context())
			require.NoError(t, err)
			defer func() { _ = c.Close() }()
			raw, ok, err := p.sharedMetadataReceipt(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project"}, "task", "actor", "hash", in)
			if mode == "conflict" || mode == "unknown-mismatch" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, mode != "absent", ok)
				if ok {
					require.Contains(t, string(raw), "applied")
				}
			}
		})
	}
}

// A failed acknowledgement or unverifiable read-back must retain the command
// fence. A fresh request cannot dispatch past another unresolved command.
func TestSharedMetadataUncertainDispatch(t *testing.T) {
	for _, mode := range []string{"command-fence", "foreign-task", "native-read-fails", "patch-fails", "readback-fails", "readback-mismatch", "receipt-fails"} {
		t.Run(mode, func(t *testing.T) {
			p, m := sharedNativePortal(t, "editor", true)
			task := map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": "todo", "title": "before", "description": "old", "priority": "low"}
			title, desc, priority := "after", "new", "high"
			in := sharedMetadataInput{RequestID: "edit", Token: sharedMetadataToken(task), Title: &title, Description: &desc, Priority: &priority}
			m.ExpectQuery("SELECT actor_id,action").WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}))
			m.ExpectQuery("SELECT EXISTS.*shared_space_commands").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(mode == "command-fence"))
			if mode != "command-fence" && mode != "foreign-task" && mode != "native-read-fails" {
				m.ExpectQuery("SELECT EXISTS.*shared_space_runtime_bindings").WillReturnRows(sqlmock.NewRows([]string{"bound"}).AddRow(false))
				m.ExpectQuery("SELECT EXISTS.*shared_space_commands").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
				m.ExpectExec("INSERT INTO shared_space_commands").WillReturnResult(sqlmock.NewResult(0, 1))
				if mode == "receipt-fails" {
					m.ExpectBegin()
					m.ExpectExec("UPDATE shared_space_commands SET state='applied'").WillReturnError(errors.New("receipt unavailable"))
					m.ExpectRollback()
				}
				m.ExpectExec("UPDATE shared_space_commands SET state='unknown'").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			sharedNativeUnlock(m)
			gets, patches := 0, 0
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/issues/task", r.URL.Path)
				if r.Method == "PATCH" {
					patches++
					var body map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					require.Equal(t, in.body(), body)
					if mode == "patch-fails" {
						w.WriteHeader(503)
						return
					}
					for key, value := range body {
						task[key] = value
					}
					writeJSON(w, 200, task)
					return
				}
				require.Equal(t, "GET", r.Method)
				gets++
				if mode == "native-read-fails" || (mode == "readback-fails" && gets == 2) {
					w.WriteHeader(503)
					return
				}
				if mode == "foreign-task" {
					task["projectId"] = "foreign"
				}
				if mode == "readback-mismatch" && gets == 2 {
					task["title"] = "concurrent update"
				}
				writeJSON(w, 200, task)
			})
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			err = p.sharedSetMetadata(httptest.NewRecorder(), sharedNativeRequest("PATCH", "/", string(raw)), sharedNativeIdentity)
			require.Error(t, err)
			switch mode {
			case "command-fence":
				require.Zero(t, gets)
				require.Zero(t, patches)
			case "foreign-task", "native-read-fails":
				require.Zero(t, patches)
			default:
				require.Equal(t, 1, patches)
			}
		})
	}
}

func TestSharedMetadataRejectsInvalidFormsBeforeLock(t *testing.T) {
	p := &portal{}
	for _, raw := range []string{`{"title":"x","status":"todo"}`, `{"requestId":"r","expectedMetadataToken":"v","title":"x","priority":"low"}`} {
		err := p.sharedSetMetadata(httptest.NewRecorder(), sharedNativeRequest("PATCH", "/", raw), sharedNativeIdentity)
		var failure *sharedError
		require.ErrorAs(t, err, &failure)
		require.Equal(t, 400, failure.code)
	}
}

func TestSharedMetadataCommandOwnership(t *testing.T) {
	for _, mode := range []string{"actor-conflict", "task-conflict", "receipt-read-fails", "revoked-member", "binding-read-fails", "intent-write-fails"} {
		t.Run(mode, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			defer func() { require.NoError(t, m.ExpectationsWereMet()) }()
			p := &portal{db: db}
			title, desc, priority := "new", "body", "high"
			task := map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": "todo", "title": "old", "description": "body", "priority": "low"}
			in := sharedMetadataInput{RequestID: "edit", Token: sharedMetadataToken(task), Title: &title, Description: &desc, Priority: &priority}
			m.ExpectExec("SELECT pg_advisory_lock").WillReturnResult(sqlmock.NewResult(0, 1))
			load := m.ExpectQuery("SELECT s.id,s.deployment_id")
			if mode == "revoked-member" {
				load.WillReturnRows(sqlmock.NewRows([]string{"id", "deployment", "company", "project", "name", "role"}))
			} else {
				load.WillReturnRows(sqlmock.NewRows([]string{"id", "deployment", "company", "project", "name", "role"}).AddRow("space", "deployment", "company", "project", "name", "editor"))
				receipt := m.ExpectQuery("SELECT actor_id,action")
				if mode == "receipt-read-fails" {
					receipt.WillReturnError(errors.New("receipt read failed"))
				} else {
					rows := sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"})
					if mode == "actor-conflict" || mode == "task-conflict" {
						actor, hash := "person", sharedHash([]any{"task", in})
						if mode == "actor-conflict" {
							actor = "another-editor"
						} else {
							hash = sharedHash([]any{"another-task", in})
						}
						rows.AddRow(actor, "task_metadata", hash, "applied", `{}`)
					}
					receipt.WillReturnRows(rows)
					if mode == "binding-read-fails" || mode == "intent-write-fails" {
						m.ExpectQuery("SELECT EXISTS.*shared_space_commands").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
						binding := m.ExpectQuery("SELECT EXISTS.*shared_space_runtime_bindings")
						if mode == "binding-read-fails" {
							binding.WillReturnError(errors.New("binding unavailable"))
						} else {
							binding.WillReturnRows(sqlmock.NewRows([]string{"bound"}).AddRow(false))
							m.ExpectQuery("SELECT EXISTS.*shared_space_commands").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
							m.ExpectExec("INSERT INTO shared_space_commands").WillReturnError(errors.New("intent unavailable"))
						}
						sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
							require.Equal(t, "GET", r.Method)
							writeJSON(w, 200, task)
						})
					}
				}
			}
			sharedNativeUnlock(m)
			raw, err := json.Marshal(in)
			require.NoError(t, err)
			require.Error(t, p.sharedSetMetadata(httptest.NewRecorder(), sharedNativeRequest("PATCH", "/", string(raw)), sharedNativeIdentity))
		})
	}
}
