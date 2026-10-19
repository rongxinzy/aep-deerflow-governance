package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSharedTaskCreationValidation(t *testing.T) {
	for _, role := range []string{"owner", "editor", "viewer", "revoked"} {
		require.Equal(t, role == "owner" || role == "editor", sharedCaps(role)["createTask"])
	}
	for _, field := range []string{"companyId", "projectId", "status", "assigneeUserId", "assigneeAgentId"} {
		var b sharedTaskCreation
		require.Error(t, sharedDecode(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader(`{"title":"x","requestId":"r","`+field+`":"evil"}`)), &b))
	}
	for _, b := range []sharedTaskCreation{{Title: " ", RequestID: "r"}, {Title: "x"}, {Title: "x", RequestID: "r", Priority: "urgent"}} {
		require.Error(t, b.validate())
	}
	b := sharedTaskCreation{Title: " x ", RequestID: "r"}
	require.NoError(t, b.validate())
	require.Equal(t, "medium", b.Priority)
	require.Equal(t, "x", b.Title)
}
func TestSharedTaskCreationCommand(t *testing.T) {
	for _, mode := range []string{"success", "empty-description", "null-description", "viewer", "replay", "conflict", "unknown", "lost-ack", "intent-failure", "receipt-failure", "wrong-scope"} {
		t.Run(mode, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { m.ExpectClose(); require.NoError(t, db.Close()) })
			role := "editor"
			if mode == "viewer" {
				role = "viewer"
			}
			m.ExpectExec("SELECT pg_advisory_lock").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectQuery("SELECT s.id,s.deployment_id").WillReturnRows(sqlmock.NewRows([]string{"id", "deployment", "company", "project", "name", "role"}).AddRow("space", "deployment", "company", "project", "空间", role))
			b := sharedTaskCreation{Title: "新任务", Description: "说明", Priority: "high", RequestID: "req"}
			if mode == "empty-description" || mode == "null-description" {
				b.Description = ""
			}
			if mode != "viewer" {
				q := m.ExpectQuery("SELECT actor_id,action,payload_hash,state,receipt")
				switch mode {
				case "replay":
					q.WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}).AddRow("actor", "create_task", sharedHash(b), "applied", `{"state":"applied","task":{"id":"prior"}}`))
				case "conflict":
					q.WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}).AddRow("actor", "create_task", "different", "applied", `{}`))
				case "unknown":
					q.WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}).AddRow("actor", "create_task", sharedHash(b), "sending", nil))
				default:
					q.WillReturnError(sql.ErrNoRows)
					m.ExpectQuery("SELECT EXISTS").WithArgs("space", sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
					insert := m.ExpectExec("INSERT INTO shared_space_commands")
					if mode == "intent-failure" {
						insert.WillReturnError(errors.New("disk"))
					} else {
						insert.WillReturnResult(sqlmock.NewResult(0, 1))
						switch mode {
						case "lost-ack", "wrong-scope":
							m.ExpectExec("UPDATE shared_space_commands SET state='unknown'").WillReturnResult(sqlmock.NewResult(0, 1))
						default:
							m.ExpectBegin()
							if mode == "receipt-failure" {
								m.ExpectExec("UPDATE shared_space_commands SET state='applied'").WillReturnError(errors.New("disk"))
								m.ExpectRollback()
								m.ExpectExec("UPDATE shared_space_commands SET state='unknown'").WillReturnResult(sqlmock.NewResult(0, 1))
							} else {
								m.ExpectExec("UPDATE shared_space_commands SET state='applied'").WillReturnResult(sqlmock.NewResult(0, 1))
								m.ExpectCommit()
							}
						}
					}
				}
			}
			m.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
			calls := 0
			pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				require.Equal(t, "/companies/company/issues", r.URL.Path)
				require.Equal(t, "POST", r.Method)
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, "project", body["projectId"])
				require.Equal(t, "backlog", body["status"])
				require.Nil(t, body["assigneeUserId"])
				require.Len(t, body, 7)
				require.Equal(t, true, body["allowDuplicate"])
				require.NotEmpty(t, body["idempotencyKey"])
				if mode == "lost-ack" {
					w.WriteHeader(503)
					return
				}
				company := "company"
				if mode == "wrong-scope" {
					company = "other"
				}
				var description any = b.Description
				if mode == "null-description" {
					description = nil
				}
				writeJSON(w, 201, map[string]any{"id": "task", "companyId": company, "projectId": "project", "status": "backlog", "title": b.Title, "description": description, "priority": b.Priority, "private": "secret"})
			}))
			defer pc.Close()
			p := &portal{db: db}
			p.cfg.paperclipBaseURL = pc.URL
			p.cfg.paperclipToken = "synthetic"
			raw, _ := json.Marshal(b)
			r := httptest.NewRequest("POST", "/", strings.NewReader(string(raw)))
			r.SetPathValue("space", "space")
			w := httptest.NewRecorder()
			err = p.sharedCreateTask(w, r, identity{UserID: "actor", DeploymentID: "deployment"})
			switch mode {
			case "viewer", "conflict", "intent-failure":
				require.Error(t, err)
			case "lost-ack", "wrong-scope", "receipt-failure", "unknown":
				require.NoError(t, err)
				require.Equal(t, 202, w.Code)
				require.Contains(t, w.Body.String(), "unknown")
			default:
				require.NoError(t, err)
				require.NotContains(t, w.Body.String(), "private")
			}
			if mode == "success" || mode == "empty-description" || mode == "null-description" || mode == "lost-ack" || mode == "wrong-scope" || mode == "receipt-failure" {
				require.Equal(t, 1, calls)
			} else {
				require.Zero(t, calls)
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestSharedTaskCommandReadOnly(t *testing.T) {
	for _, mode := range []string{"applied", "sending", "unknown", "missing", "other-actor", "other-action", "viewer"} {
		t.Run(mode, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { m.ExpectClose(); require.NoError(t, db.Close()) })
			m.ExpectExec("SELECT pg_advisory_lock").WillReturnResult(sqlmock.NewResult(0, 1))
			role := "editor"
			if mode == "viewer" {
				role = "viewer"
			}
			m.ExpectQuery("SELECT s.id,s.deployment_id").WillReturnRows(sqlmock.NewRows([]string{"id", "deployment", "company", "project", "name", "role"}).AddRow("space", "dep", "company", "project", "空间", role))
			if mode != "viewer" {
				q := m.ExpectQuery("SELECT actor_id,action,payload_hash,state,receipt").WithArgs("space", "req")
				if mode == "missing" {
					q.WillReturnError(sql.ErrNoRows)
				} else {
					actor, action := "actor", "create_task"
					if mode == "other-actor" {
						actor = "other"
					}
					if mode == "other-action" {
						action = "review"
					}
					q.WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}).AddRow(actor, action, "hash", mode, `{"state":"applied","task":{"id":"task"}}`))
				}
			}
			m.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
			p := &portal{db: db}
			p.cfg.paperclipBaseURL = "http://127.0.0.1:1"
			r := httptest.NewRequest("GET", "/", nil)
			r.SetPathValue("space", "space")
			r.SetPathValue("request", "req")
			w := httptest.NewRecorder()
			err = p.sharedTaskCommand(w, r, identity{UserID: "actor", DeploymentID: "dep"})
			if mode == "missing" || mode == "other-actor" || mode == "other-action" || mode == "viewer" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, 200, w.Code)
				if mode != "applied" {
					require.Contains(t, w.Body.String(), `"state":"unknown"`)
				}
			}
			require.NoError(t, m.ExpectationsWereMet())
		})
	}
}

func TestSharedCreatedDescriptionMatches(t *testing.T) {
	require.True(t, sharedCreatedDescriptionMatches(nil, ""))
	require.True(t, sharedCreatedDescriptionMatches("", ""))
	require.False(t, sharedCreatedDescriptionMatches(nil, "说明"))
	require.False(t, sharedCreatedDescriptionMatches("changed", "说明"))
	require.False(t, sharedCreatedDescriptionMatches(1, ""))
}
