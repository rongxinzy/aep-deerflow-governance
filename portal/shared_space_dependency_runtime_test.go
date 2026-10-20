package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestDependencyCheckoutUsesOneDurableInternalCommand(t *testing.T) {
	for _, mode := range []string{"applied", "lost-ack", "wrong-assignee", "already-running", "invalid-phase", "prior-applied", "prior-unknown"} {
		t.Run(mode, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); _ = db.Close() })
			p := &portal{db: db}
			b := runtimeTestBinding()
			task := map[string]any{"id": "task", "status": "todo", "assigneeAgentId": "agent"}
			request := "dependency-claim:binding:1"
			hash := sharedHash([]any{b.ID, b.Generation, b.AssignmentEpoch, b.AssignmentHash})
			switch mode {
			case "already-running":
				task["status"] = "in_progress"
			case "invalid-phase":
				task["status"] = "blocked"
			default:
				q := m.ExpectQuery("SELECT actor_id,action,payload_hash,state,receipt")
				if mode == "prior-applied" || mode == "prior-unknown" {
					state := "applied"
					if mode == "prior-unknown" {
						state = "unknown"
					}
					q.WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}).AddRow(b.Owner, "dependency_claim", hash, state, `{}`))
				} else {
					q.WillReturnError(sql.ErrNoRows)
					m.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
					m.ExpectExec("INSERT INTO shared_space_commands").WithArgs(b.SpaceID, request, b.TaskID, b.Owner, "dependency_claim", hash, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
					if mode == "lost-ack" || mode == "wrong-assignee" {
						m.ExpectExec("UPDATE shared_space_commands SET state='unknown'").WillReturnResult(sqlmock.NewResult(0, 1))
					} else {
						m.ExpectBegin()
						m.ExpectExec("UPDATE shared_space_commands SET state='applied'").WillReturnResult(sqlmock.NewResult(0, 1))
						m.ExpectCommit()
					}
				}
			}
			posts := 0
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts++
				}
				if mode == "lost-ack" {
					w.WriteHeader(503)
					return
				}
				agent := "agent"
				if mode == "wrong-assignee" {
					agent = "changed"
				}
				writeJSON(w, 200, map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": "in_progress", "assigneeAgentId": agent})
			})
			c, err := db.Conn(t.Context())
			require.NoError(t, err)
			defer func() { _ = c.Close() }()
			err = p.runtimeDependencyCheckout(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project"}, b, task)
			if mode == "applied" || mode == "already-running" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if mode == "applied" || mode == "lost-ack" || mode == "wrong-assignee" {
				require.Equal(t, 1, posts)
			} else {
				require.Zero(t, posts)
			}
		})
	}
}

func TestDependencyCheckoutReadbackFences(t *testing.T) {
	for _, mode := range []string{"confirmed", "no-command", "already-applied", "changed-binding", "changed-assignee", "database"} {
		t.Run(mode, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); _ = db.Close() })
			p := &portal{db: db}
			b := runtimeTestBinding()
			task := map[string]any{"id": "task", "status": "in_progress", "assigneeAgentId": "agent", "blockedBy": []any{map[string]any{"id": "a"}}}
			hash := sharedHash([]any{b.ID, b.Generation, b.AssignmentEpoch, b.AssignmentHash})
			expected := `{"bindingId":"binding","generation":1}`
			if mode == "changed-binding" {
				expected = `{"bindingId":"old-binding","generation":1}`
			}
			if mode == "changed-assignee" {
				task["assigneeAgentId"] = "other"
			}
			q := m.ExpectQuery("SELECT actor_id,action,payload_hash,state,expected")
			switch mode {
			case "no-command":
				q.WillReturnError(sql.ErrNoRows)
			case "database":
				q.WillReturnError(http.ErrAbortHandler)
			default:
				state := "unknown"
				if mode == "already-applied" {
					state = "applied"
				}
				q.WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "expected"}).AddRow(b.Owner, "dependency_claim", hash, state, expected))
			}
			if mode == "confirmed" {
				m.ExpectBegin()
				m.ExpectExec("UPDATE shared_space_commands SET state='applied'").WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectCommit()
			}
			c, err := db.Conn(t.Context())
			require.NoError(t, err)
			defer func() { _ = c.Close() }()
			err = p.runtimeRecoverDependencyCheckout(t.Context(), c, sharedSpace{ID: "space"}, b, task)
			if mode == "confirmed" || mode == "no-command" || mode == "already-applied" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestDependencyStartSnapshotOnlyAfterReadiness(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); _ = db.Close() })
			p := &portal{db: db}
			m.ExpectQuery("SELECT r.id,r.task_id").WillReturnRows(dependencyResultRows(""))
			if ready {
				m.ExpectExec("UPDATE shared_space_runtime_outbox SET payload").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			calls := 0
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path == "/issues/a" {
					status := "todo"
					if ready {
						status = "done"
					}
					writeJSON(w, 200, map[string]any{"id": "a", "companyId": "company", "projectId": "project", "status": status, "title": "A"})
					return
				}
				writeJSON(w, 200, map[string]any{"issue": map[string]any{"id": "task"}, "readiness": map[string]any{"isDependencyReady": ready}})
			})
			b := runtimeTestBinding()
			payload := map[string]any{"instruction": "交办内容"}
			task := map[string]any{"id": "task", "status": "in_progress", "blockedBy": []any{map[string]any{"id": "a"}}}
			c, err := db.Conn(t.Context())
			require.NoError(t, err)
			defer func() { _ = c.Close() }()
			got, hash, err := p.runtimePrepareDependencies(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project"}, b, task, "start:binding", payload)
			require.NoError(t, err)
			require.Equal(t, ready, got)
			require.Equal(t, 2, calls)
			if ready {
				require.Equal(t, sharedHash(payload), hash)
				raw, _ := json.Marshal(payload)
				require.Contains(t, string(raw), `"acceptedResult":null`)
			} else {
				require.Empty(t, hash)
				require.NotContains(t, payload, "dependencyContext")
			}
		})
	}
}
