package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func dependencyResultRows(state string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"id", "task", "version", "title", "key", "document", "revision", "hash", "author", "state", "created"})
	if state != "" {
		rows.AddRow("result-a", "a", 1, "A成果", "shared-space-result", "doc-a", "revision-a", sharedHash("A共享正文"), "person", state, timeForDependencyTest)
	}
	return rows
}

var timeForDependencyTest = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

func TestSharedDependenciesReadinessAndScope(t *testing.T) {
	for _, mode := range []string{"waiting", "cancelled", "native-done", "submitted", "accepted", "changed-body", "finalize", "foreign", "diagnostic-scope", "diagnostic-unavailable"} {
		t.Run(mode, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); _ = db.Close() })
			p := &portal{db: db}
			state := ""
			switch mode {
			case "submitted":
				state = "submitted"
			case "accepted", "changed-body", "finalize":
				state = "accepted"
			}
			if mode != "foreign" {
				m.ExpectQuery("SELECT r.id,r.task_id").WithArgs("space", "a").WillReturnRows(dependencyResultRows(state))
			}
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/issues/a":
					status := "done"
					if mode == "waiting" {
						status = "todo"
					}
					if mode == "cancelled" {
						status = "cancelled"
					}
					project := "project"
					if mode == "foreign" {
						project = "private-project"
					}
					writeJSON(w, 200, map[string]any{"id": "a", "companyId": "company", "projectId": project, "title": "前置A", "status": status})
				case "/issues/a/documents/shared-space-result":
					body := "A共享正文"
					if mode == "changed-body" {
						body = "被修改的正文"
					}
					writeJSON(w, 200, sharedDocument{ID: "doc-a", LatestRevisionID: "revision-a", Body: body})
				case "/issues/b/diagnostics/blockers":
					if mode == "diagnostic-unavailable" {
						w.WriteHeader(503)
						return
					}
					id := "b"
					if mode == "diagnostic-scope" {
						id = "foreign"
					}
					ready := mode != "finalize" && mode != "waiting" && mode != "cancelled"
					writeJSON(w, 200, map[string]any{"issue": map[string]any{"id": id}, "readiness": map[string]any{"isDependencyReady": ready}, "blockers": []any{map[string]any{"id": "a", "isDependencyReady": ready, "isPendingFinalize": mode == "finalize"}}})
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			})
			deps, err := p.sharedReadDependencies(t.Context(), sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project"}, map[string]any{"id": "b", "blockedBy": []any{map[string]any{"id": "a"}}})
			if mode == "foreign" || mode == "diagnostic-scope" || mode == "diagnostic-unavailable" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			ready := mode == "native-done" || mode == "accepted"
			require.Equal(t, ready, deps.Ready)
			require.Equal(t, ready, deps.BlockedBy[0].Satisfied)
			require.Equal(t, !ready, sharedDependencyState(deps)["pendingCount"].(int) > 0)
			if mode == "accepted" {
				require.Equal(t, "A共享正文", sharedDependencyContext(deps)[0]["acceptedResult"].(map[string]any)["body"])
				raw, _ := json.Marshal(deps)
				require.NotContains(t, string(raw), "A共享正文")
			}
			if mode == "native-done" {
				require.Nil(t, sharedDependencyContext(deps)[0]["acceptedResult"])
			}
		})
	}
	p := &portal{}
	deps, err := p.sharedReadDependencies(t.Context(), sharedSpace{}, map[string]any{"id": "b"})
	require.NoError(t, err)
	require.True(t, deps.Ready)
	require.Empty(t, deps.BlockedBy)
	_, err = sharedBlockedIDs(map[string]any{"blockedBy": []any{map[string]any{}}})
	require.Error(t, err)
}

func TestSharedDependenciesEditBoundaries(t *testing.T) {
	for _, mode := range []string{"viewer", "running", "native-run", "bound", "unbound", "database"} {
		t.Run(mode, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); _ = db.Close() })
			p := &portal{db: db}
			role := "editor"
			task := map[string]any{"id": "b", "status": "todo"}
			switch mode {
			case "viewer":
				role = "viewer"
			case "running":
				task["status"] = "in_progress"
			case "native-run":
				task["checkoutRunId"] = "native"
			default:
				q := m.ExpectQuery("SELECT EXISTS.*shared_space_runtime_bindings")
				if mode == "database" {
					q.WillReturnError(http.ErrAbortHandler)
				} else {
					q.WillReturnRows(sqlmock.NewRows([]string{"bound"}).AddRow(mode == "bound"))
				}
			}
			reason, err := p.sharedDependencyEditReason(t.Context(), sharedSpace{ID: "space", Role: role}, task)
			if mode == "database" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, mode != "unbound", reason != "")
			}
		})
	}
	for _, body := range []string{`{`, `{}`, `{"requestId":"r","expectedDependencyToken":"token","blockedByIssueIds":[""]}`} {
		require.Error(t, (&portal{}).sharedSetDependencies(httptest.NewRecorder(), sharedNativeRequest("PUT", "/", body), sharedNativeIdentity))
	}
}

func TestSharedDependenciesUnknownReadbackNeverPatches(t *testing.T) {
	for _, mode := range []string{"confirmed", "different", "other-actor", "rejected", "applied"} {
		t.Run(mode, func(t *testing.T) {
			p, m := sharedNativePortal(t, "editor", true)
			in := sharedDependencyInput{RequestID: "set", Token: "old", IDs: []string{"a"}}
			hash := sharedHash([]any{"b", in})
			actor := "person"
			state := "unknown"
			receipt := `{}`
			if mode == "other-actor" {
				actor = "other"
			}
			if mode == "applied" {
				state = "applied"
				receipt = `{"dependencies":{"ready":true}}`
			}
			if mode == "rejected" {
				state = "applied"
				receipt = `{"rejected":true}`
			}
			m.ExpectQuery("SELECT actor_id,action,payload_hash,state,receipt").WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}).AddRow(actor, "dependencies", hash, state, receipt))
			if mode == "confirmed" {
				m.ExpectQuery("SELECT r.id,r.task_id").WillReturnRows(dependencyResultRows(""))
				m.ExpectBegin()
				m.ExpectExec("UPDATE shared_space_commands SET state='applied'").WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectCommit()
			}
			sharedNativeUnlock(m)
			patches := 0
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					patches++
					w.WriteHeader(500)
					return
				}
				switch r.URL.Path {
				case "/issues/b":
					refs := []any{}
					if mode != "different" {
						refs = append(refs, map[string]any{"id": "a"})
					}
					writeJSON(w, 200, map[string]any{"id": "b", "companyId": "company", "projectId": "project", "blockedBy": refs})
				case "/issues/a":
					writeJSON(w, 200, map[string]any{"id": "a", "companyId": "company", "projectId": "project", "title": "A", "status": "done"})
				default:
					writeJSON(w, 200, map[string]any{"issue": map[string]any{"id": "b"}, "readiness": map[string]any{"isDependencyReady": true}})
				}
			})
			raw, _ := json.Marshal(in)
			r := sharedNativeRequest("PUT", "/", string(raw))
			r.SetPathValue("task", "b")
			w := httptest.NewRecorder()
			err := p.sharedSetDependencies(w, r, sharedNativeIdentity)
			if mode == "confirmed" || mode == "applied" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Zero(t, patches)
		})
	}
}
