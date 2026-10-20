package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSharedHumanReturnEnqueuePreservesRetiredSourceWithoutRevision(t *testing.T) {
	for _, mode := range []string{"absent", "lookup-error", "load-error", "wrong-task", "active", "no-conversation", "insert-error", "submitted", "request_changes"} {
		t.Run(mode, func(t *testing.T) {
			h := newAPIHarness(t)
			h.dbmock.ExpectBegin()
			tx, err := h.db.Begin()
			require.NoError(t, err)
			lookup := h.dbmock.ExpectQuery("SELECT source_binding_id FROM \\(SELECT source_binding_id,target_user_id").WithArgs("space", "task", "human")
			b := runtimeTestBinding()
			b.BindingState = "retired"
			result := sharedResult{ID: "human-result", SubmittedBy: "human", Title: "人类成果", RevisionID: "human-revision", Version: 1}
			switch mode {
			case "absent":
				lookup.WillReturnError(sql.ErrNoRows)
			case "lookup-error":
				lookup.WillReturnError(errors.New("lookup"))
			default:
				lookup.WillReturnRows(sqlmock.NewRows([]string{"source_binding_id"}).AddRow(b.ID))
			}
			if mode != "absent" && mode != "lookup-error" {
				switch mode {
				case "wrong-task":
					b.TaskID = "other"
				case "active":
					b.BindingState = "active"
				case "no-conversation":
					b.SourceConversationID = 0
				}
				if mode == "load-error" {
					h.dbmock.ExpectQuery("SELECT id,space_id").WillReturnError(errors.New("load"))
				} else {
					runtimeExpectBinding(h, b)
				}
				if mode == "insert-error" || mode == "submitted" || mode == "request_changes" {
					insert := h.dbmock.ExpectExec("INSERT INTO shared_space_result_bindings").WithArgs(result.ID, b.ID)
					if mode == "insert-error" {
						insert.WillReturnError(errors.New("insert"))
					} else {
						insert.WillReturnResult(sqlmock.NewResult(0, 1))
						h.dbmock.ExpectQuery("SELECT binding_id FROM shared_space_result_bindings").WithArgs(result.ID).WillReturnRows(sqlmock.NewRows([]string{"binding_id"}).AddRow(b.ID))
						runtimeExpectBinding(h, b)
						h.dbmock.ExpectExec("INSERT INTO shared_space_runtime_outbox").WithArgs("return:human-result:submitted", b.ID, "return", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
					}
				}
			}
			err = h.sharedHumanReturnEnqueue(t.Context(), tx, "space", "task", result)
			if mode == "absent" || mode == "submitted" || mode == "request_changes" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if mode == "request_changes" {
				h.dbmock.ExpectQuery("SELECT binding_id FROM shared_space_result_bindings").WithArgs(result.ID).WillReturnRows(sqlmock.NewRows([]string{"binding_id"}).AddRow(b.ID))
				runtimeExpectBinding(h, b)
				h.dbmock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"linked"}).AddRow(true))
				h.dbmock.ExpectExec("INSERT INTO shared_space_runtime_outbox").WithArgs("return:human-result:request_changes", b.ID, "return", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				require.NoError(t, h.sharedRuntimeEnqueue(t.Context(), tx, "space", "task", result, "request_changes", "修改"))
			}
			h.dbmock.ExpectRollback()
			require.NoError(t, tx.Rollback())
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
func TestSharedRuntimeReturnAuthorizationUsesCurrentReadGrant(t *testing.T) {
	for _, mode := range []string{"participant", "member", "revoked", "workspace", "companion", "conversation", "space-error"} {
		t.Run(mode, func(t *testing.T) {
			h := collaborationHarness(t)
			ws := runtimeWorkspace(t, h)
			b := runtimeTestBinding()
			b.BindingState = "retired"
			switch mode {
			case "workspace":
				b.WorkspaceUID = "recreated"
			case "companion":
				b.Companion = "removed"
			case "conversation":
				b.SourceConversationID = 0
			}
			pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "task", "companyId": "company", "projectId": "project"})
			}))
			defer pc.Close()
			h.cfg.paperclipBaseURL = pc.URL
			if mode == "participant" || mode == "member" || mode == "revoked" {
				runtimeExpectSpace(h, "viewer")
				if mode == "participant" {
					runtimeExpectParticipant(h, b)
				} else {
					h.dbmock.ExpectQuery("SELECT id,kind,target").WillReturnError(sql.ErrNoRows)
					if mode == "member" {
						runtimeExpectMemberSpace(h, "viewer")
					} else {
						h.dbmock.ExpectQuery("SELECT s.id,s.deployment_id").WillReturnError(sql.ErrNoRows)
					}
				}
			}
			if mode == "space-error" {
				h.dbmock.ExpectQuery("SELECT id,deployment_id").WillReturnError(errors.New("space"))
			}
			err := h.runtimeReturnAuthorize(t.Context(), b, ws, identity{UserID: b.Owner, DeploymentID: h.cfg.aepDeploymentID})
			if mode == "participant" || mode == "member" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestSharedRuntimeReturnRequiresExactResultAndConversation(t *testing.T) {
	for _, mode := range []string{"linked", "unlinked", "db-error", "conversation", "binding", "result"} {
		t.Run(mode, func(t *testing.T) {
			h := newAPIHarness(t)
			b := runtimeTestBinding()
			payload := map[string]any{"bindingId": b.ID, "conversationId": float64(b.SourceConversationID), "resultId": "result", "revisionId": "revision"}
			switch mode {
			case "conversation":
				payload["conversationId"] = float64(999)
			case "binding":
				payload["bindingId"] = "other"
			case "result":
				delete(payload, "resultId")
			default:
				query := h.dbmock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM shared_space_result_bindings").WithArgs("result", b.ID, b.SpaceID, b.TaskID, "revision")
				if mode == "db-error" {
					query.WillReturnError(errors.New("db"))
				} else {
					query.WillReturnRows(sqlmock.NewRows([]string{"linked"}).AddRow(mode == "linked"))
				}
			}
			require.Equal(t, mode == "linked", h.sharedRuntimeReturnLinked(t.Context(), h.db, b, payload))
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
