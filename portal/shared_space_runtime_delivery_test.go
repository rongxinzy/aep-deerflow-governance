package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Dispatch decisions must use current authorization and execution state. A
// passive historical return is allowed across generations; executable work is not.
func TestSharedRuntimeDeliveryAuthorizationAndRecovery(t *testing.T) {
	for _, mode := range []string{"revoked", "terminal", "unresolved-command", "historical-return", "review-revise", "receipt-absent", "malformed-payload", "ack-write-fails", "dispatch-intent-fails"} {
		t.Run(mode, func(t *testing.T) {
			h := collaborationHarness(t)
			_ = runtimeWorkspace(t, h)
			b := runtimeTestBinding()
			action := "revise"
			if mode == "historical-return" {
				action = "return"
			}
			payload := map[string]any{"executionId": b.ExecutionID}
			if action == "return" {
				payload["executionId"] = "old-execution"
				payload["bindingId"] = b.ID
				payload["conversationId"] = b.SourceConversationID
				payload["resultId"] = "result"
				payload["revisionId"] = "revision"
			}
			raw, _ := json.Marshal(payload)
			if mode == "malformed-payload" {
				raw = []byte(`{`)
			}
			hash := sharedHash(payload)
			state := "pending"
			if mode == "receipt-absent" {
				state = "unknown"
			}
			h.dbmock.ExpectQuery("SELECT command_id,binding_id,action,state,payload,payload_hash").WillReturnRows(sqlmock.NewRows([]string{"command_id", "binding_id", "action", "state", "payload", "payload_hash"}).AddRow("delivery", b.ID, action, state, raw, hash))
			runtimeExpectBinding(h, b)
			h.dbmock.ExpectExec("SELECT pg_advisory_lock").WillReturnResult(sqlmock.NewResult(0, 1))
			runtimeExpectBinding(h, b)
			h.dbmock.ExpectExec("UPDATE shared_space_runtime_outbox SET next_attempt_at").WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectQuery("SELECT state FROM shared_space_runtime_outbox").WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow(state))
			calls := []string{}
			pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				status := "todo"
				if mode == "terminal" {
					status = "cancelled"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": status, "assigneeAgentId": "agent"})
			}))
			defer pc.Close()
			h.cfg.paperclipBaseURL = pc.URL
			pi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				if mode == "receipt-absent" {
					http.Error(w, "absent", 404)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"commandId": "delivery", "payloadHash": hash, "state": "applied"})
			}))
			defer pi.Close()
			h.workspaceChatOverride = pi.URL
			if mode != "malformed-payload" {
				if action == "return" {
					h.dbmock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM shared_space_result_bindings").WillReturnRows(sqlmock.NewRows([]string{"linked"}).AddRow(true))
				}
				if action != "return" {
					runtimeExpectTaskEpoch(h, b.SpaceID, b.TaskID, b.AssignmentEpoch)
				}
				role := "editor"
				if mode == "revoked" {
					role = "viewer"
				}
				runtimeExpectSpace(h, role)
				if mode == "revoked" {
					h.dbmock.ExpectQuery("SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants").WillReturnError(sql.ErrNoRows)
					h.dbmock.ExpectExec("UPDATE shared_space_runtime_outbox SET state='blocked'").WillReturnResult(sqlmock.NewResult(0, 1))
				} else {
					runtimeExpectParticipant(h, runtimeTestBinding())
					if action != "return" {
						runtimeExpectSpace(h, "editor")
						if mode == "terminal" {
							h.dbmock.ExpectExec("UPDATE shared_space_runtime_outbox SET state='blocked'").WillReturnResult(sqlmock.NewResult(0, 1))
						} else {
							h.dbmock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(mode == "unresolved-command"))
						}
					}
					if mode != "terminal" && mode != "unresolved-command" {
						if state == "pending" {
							intent := h.dbmock.ExpectExec("UPDATE shared_space_runtime_outbox SET state='unknown'")
							if mode == "dispatch-intent-fails" {
								intent.WillReturnError(errors.New("cannot persist intent"))
							} else {
								intent.WillReturnResult(sqlmock.NewResult(0, 1))
							}
						}
						if mode != "receipt-absent" && mode != "dispatch-intent-fails" {
							h.dbmock.ExpectBegin()
							ack := h.dbmock.ExpectExec("UPDATE shared_space_runtime_outbox SET state='applied'")
							if mode == "ack-write-fails" {
								ack.WillReturnError(errors.New("cannot commit acknowledgement"))
								h.dbmock.ExpectRollback()
							} else {
								ack.WillReturnResult(sqlmock.NewResult(0, 1))
								h.dbmock.ExpectCommit()
							}
						}
					}
				}
			}
			runtimeExpectUnlock(h)
			h.processSharedRuntime(t.Context())
			require.NoError(t, h.dbmock.ExpectationsWereMet())
			switch mode {
			case "historical-return":
				require.Equal(t, []string{"POST /assistant/internal/shared-space/return"}, calls)
			case "review-revise", "ack-write-fails":
				require.Equal(t, []string{"POST /assistant/internal/shared-space/revise"}, calls)
			case "receipt-absent":
				require.Equal(t, []string{"GET /assistant/internal/shared-space/receipt"}, calls)
			default:
				require.Empty(t, calls)
			}
		})
	}
}

func TestSharedRuntimeDiscoveryDependencyFailures(t *testing.T) {
	for _, mode := range []string{"database", "row", "directory-revoked", "paperclip"} {
		t.Run(mode, func(t *testing.T) {
			h := collaborationHarness(t)
			ws := runtimeWorkspace(t, h)
			query := h.dbmock.ExpectQuery("SELECT s.id FROM shared_spaces")
			switch mode {
			case "database":
				query.WillReturnError(errors.New("db unavailable"))
			case "row":
				query.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(nil))
			default:
				query.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("space"))
			}
			if mode == "directory-revoked" {
				runtimeExpectSpace(h, "editor")
				h.dbmock.ExpectQuery("SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants").WillReturnError(sql.ErrNoRows)
			}
			if mode == "paperclip" {
				runtimeExpectSpace(h, "editor")
				runtimeExpectParticipant(h, runtimeTestBinding())
				pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, "dependency unavailable", http.StatusServiceUnavailable)
				}))
				defer pc.Close()
				h.cfg.paperclipBaseURL = pc.URL
			}
			req := httptest.NewRequest("GET", "/", nil)
			req.SetPathValue("action", "space-discover")
			w := httptest.NewRecorder()
			err := h.sharedRuntimeAgent(w, req, identity{UserID: "u-x"}, ws, "assistant")
			if mode == "directory-revoked" {
				require.NoError(t, err)
				require.Contains(t, w.Body.String(), `"spaces":[]`)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
	h := collaborationHarness(t)
	h.db = nil
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
	require.ErrorContains(t, h.sharedRuntimeAgent(httptest.NewRecorder(), req, identity{}, runtimeWorkspace(t, h), "assistant"), "未配置")
}

func TestSharedRuntimeReviewMetadataFailuresAbortTransaction(t *testing.T) {
	for _, stage := range []string{"lookup", "binding", "provenance", "generation", "return"} {
		t.Run(stage, func(t *testing.T) {
			h := newAPIHarness(t)
			b := runtimeTestBinding()
			h.dbmock.ExpectBegin()
			tx, err := h.db.Begin()
			require.NoError(t, err)
			lookup := h.dbmock.ExpectQuery("SELECT binding_id FROM shared_space_result_bindings WHERE result_id=\\$1")
			if stage == "lookup" {
				lookup.WillReturnError(errors.New("lookup unavailable"))
			} else {
				lookup.WithArgs("result").WillReturnRows(sqlmock.NewRows([]string{"binding_id"}).AddRow(b.ID))
				if stage == "binding" {
					h.dbmock.ExpectQuery("SELECT id,space_id").WillReturnError(errors.New("binding unavailable"))
				} else {
					runtimeExpectBinding(h, b)
					provenance := h.dbmock.ExpectQuery("SELECT EXISTS")
					if stage == "provenance" {
						provenance.WillReturnError(errors.New("provenance unavailable"))
					} else {
						provenance.WillReturnRows(sqlmock.NewRows([]string{"linked"}).AddRow(true))
						if stage == "generation" {
							h.dbmock.ExpectExec("UPDATE shared_space_runtime_bindings SET generation").WillReturnError(errors.New("generation unavailable"))
						} else {
							h.dbmock.ExpectExec("INSERT INTO shared_space_runtime_outbox").WillReturnError(errors.New("return unavailable"))
						}
					}
				}
			}
			decision := "request_changes"
			if stage == "return" {
				decision = "accept"
			}
			err = h.sharedRuntimeEnqueue(t.Context(), tx, "space", "task", sharedResult{ID: "result", SubmittedBy: b.Owner, Version: 1}, decision, "reason")
			require.ErrorContains(t, err, stage+" unavailable")
			h.dbmock.ExpectRollback()
			require.NoError(t, tx.Rollback())
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestSharedRuntimeDeliveryNeverRunsWithoutCurrentDurableIntent(t *testing.T) {
	for _, stage := range []string{"outbox-error", "binding-error", "lock-error", "reload-error", "already-applied", "missing-workspace"} {
		t.Run(stage, func(t *testing.T) {
			h := collaborationHarness(t)
			b := runtimeTestBinding()
			if stage == "missing-workspace" {
				b.Workspace = "deleted-workspace"
			}
			query := h.dbmock.ExpectQuery("SELECT command_id,binding_id,action,state,payload,payload_hash")
			if stage == "outbox-error" {
				query.WillReturnError(errors.New("outbox unavailable"))
			} else {
				query.WillReturnRows(sqlmock.NewRows([]string{"command_id", "binding_id", "action", "state", "payload", "payload_hash"}).AddRow("delivery", b.ID, "start", "pending", []byte(`{}`), "hash"))
				if stage == "binding-error" {
					h.dbmock.ExpectQuery("SELECT id,space_id").WillReturnError(sql.ErrNoRows)
				} else {
					runtimeExpectBinding(h, b)
					lock := h.dbmock.ExpectExec("SELECT pg_advisory_lock")
					if stage == "lock-error" {
						lock.WillReturnError(errors.New("lock unavailable"))
					} else {
						lock.WillReturnResult(sqlmock.NewResult(0, 1))
						if stage == "reload-error" {
							h.dbmock.ExpectQuery("SELECT id,space_id").WillReturnError(sql.ErrNoRows)
						} else {
							runtimeExpectBinding(h, b)
							h.dbmock.ExpectExec("UPDATE shared_space_runtime_outbox SET next_attempt_at").WillReturnResult(sqlmock.NewResult(0, 1))
							state := "pending"
							if stage == "already-applied" {
								state = "applied"
							}
							h.dbmock.ExpectQuery("SELECT state FROM shared_space_runtime_outbox").WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow(state))
						}
						runtimeExpectUnlock(h)
					}
				}
			}
			calls := 0
			pi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "unexpected dispatch", 500) }))
			defer pi.Close()
			h.workspaceChatOverride = pi.URL
			h.processSharedRuntime(t.Context())
			require.Zero(t, calls, "missing or applied intents must not execute")
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
