package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSharedHumanAssignmentNativeWritesAndFences(t *testing.T) {
	for _, mode := range []string{"assign", "claim", "unknown", "stale", "bound", "ended", "missing", "viewer", "other-claim", "replay", "blocked", "native-run", "dependencies", "conflicting-input"} {
		t.Run(mode, func(t *testing.T) {
			h := newApplyHarness(t)
			s := sharedSpace{ID: "space", DeploymentID: "deployment-acme", CompanyID: "company", ProjectID: "project", Role: "editor"}
			i := identity{UserID: "u-x", DeploymentID: "deployment-acme"}
			task := map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": "todo", "updatedAt": "old"}
			if mode == "ended" {
				task["status"] = "done"
			}
			if mode == "native-run" {
				task["executionRunId"] = "native-run"
			}
			b := sharedAssignmentInput{RequestID: "request", ExpectedAssignmentToken: sharedAssignmentToken(task, 0), MemberUserID: "u-x"}
			claim := mode == "claim" || mode == "other-claim" || mode == "dependencies"
			if mode == "missing" {
				b.RequestID = ""
			}
			if mode == "viewer" {
				s.Role = "viewer"
			}
			if mode == "other-claim" {
				b.MemberUserID = "u-owner"
			}
			if mode == "stale" {
				b.ExpectedAssignmentToken = "stale"
			}
			if mode == "conflicting-input" {
				b.ParticipantID = "agent"
			}
			early := mode == "missing" || mode == "viewer" || mode == "other-claim" || mode == "conflicting-input"
			if !early {
				replay := h.dbmock.ExpectQuery("SELECT actor_id,action,payload_hash,state,receipt")
				if mode == "replay" {
					replay.WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}).AddRow(i.UserID, "assign", sharedHash([]any{"task", b}), "applied", `{"state":"applied"}`))
				} else {
					replay.WillReturnError(sql.ErrNoRows)
					h.dbmock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
				}
				if mode != "stale" && mode != "replay" {
					humanRole(h.dbmock, "u-x", "editor")
					humanMappingRow(h.dbmock, "u-x", "native-x")
				}
				if mode != "stale" && mode != "replay" && mode != "ended" && mode != "native-run" {
					h.dbmock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(mode == "blocked"))
					if mode != "blocked" {
						h.dbmock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"handoff"}).AddRow(false))
						h.dbmock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"bound"}).AddRow(mode == "bound"))
					}
					if mode != "bound" && mode != "blocked" && mode != "dependencies" {
						h.dbmock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
						h.dbmock.ExpectExec("INSERT INTO shared_space_commands").WillReturnResult(sqlmock.NewResult(0, 1))
						if mode == "unknown" {
							h.dbmock.ExpectExec("UPDATE shared_space_commands SET state='unknown'").WillReturnResult(sqlmock.NewResult(0, 1))
						} else {
							h.dbmock.ExpectQuery("SELECT user_id FROM shared_space_human_mappings").WillReturnRows(sqlmock.NewRows([]string{"user"}).AddRow("u-x"))
							humanRole(h.dbmock, "u-x", "editor")
							humanMappingRow(h.dbmock, "u-x", "native-x")
							h.dbmock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
							h.dbmock.ExpectBegin()
							h.dbmock.ExpectExec("UPDATE shared_space_commands").WillReturnResult(sqlmock.NewResult(0, 1))
							h.dbmock.ExpectCommit()
						}
					}
				}
			}
			patches := 0
			sharedNativeServer(t, h.portal, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/companies/company/user-directory" {
					writeJSON(w, 200, map[string]any{"users": []any{map[string]any{"principalId": "native-x", "status": "active", "user": map[string]any{"id": "native-x", "name": "Native"}}}})
					return
				}
				if r.Method == "PATCH" {
					patches++
					var body map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					require.Nil(t, body["assigneeAgentId"])
					require.Equal(t, "native-x", body["assigneeUserId"])
					require.NotEqual(t, "u-x", body["assigneeUserId"])
					for k, v := range body {
						task[k] = v
					}
					if mode == "unknown" {
						w.WriteHeader(503)
						return
					}
				}
				if r.URL.Path == "/issues/task/diagnostics" {
					writeJSON(w, 200, map[string]any{"readiness": map[string]any{"isDependencyReady": mode != "dependencies"}, "blockers": []any{}})
					return
				}
				if mode == "dependencies" {
					task["blockedBy"] = []any{map[string]any{"id": "other"}}
				}
				writeJSON(w, 200, task)
			})
			c, err := h.db.Conn(t.Context())
			require.NoError(t, err)
			defer func() { _ = c.Close() }()
			_, err = h.sharedAssignmentLocked(t.Context(), c, s, "task", i, b, claim)
			if mode == "assign" || mode == "claim" || mode == "replay" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if mode == "assign" || mode == "claim" || mode == "unknown" {
				require.Equal(t, 1, patches)
			} else {
				require.Zero(t, patches)
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
