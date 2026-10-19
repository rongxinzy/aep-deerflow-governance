package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSharedHandoffPatchUnknownDoesNotApplyOnFinalizationTransactionFailures(t *testing.T) {
	for _, stage := range []string{"begin", "retire-binding", "mark-applied", "finish-receipt", "commit"} {
		t.Run(stage, func(t *testing.T) {
			h := newAPIHarness(t)
			var pcCalls []string
			pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pcCalls = append(pcCalls, r.Method+" "+r.URL.Path)
				if r.Method != http.MethodGet || r.URL.Path != "/issues/task" {
					http.Error(w, "unexpected external mutation", http.StatusMethodNotAllowed)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": "todo"})
			}))
			t.Cleanup(pc.Close)
			h.cfg.paperclipBaseURL = pc.URL
			h.cfg.paperclipToken = "synthetic-paperclip"

			c, err := h.db.Conn(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			handoff := sharedHandoff{
				Phase: "patch_unknown", RequestID: "handoff-finalize", TaskID: "task", Actor: "u-owner",
				BindingID: "old-binding", Epoch: 2,
			}
			if stage == "begin" {
				h.dbmock.ExpectBegin().WillReturnError(sqlmock.ErrCancelled)
			} else {
				h.dbmock.ExpectBegin()
			}

			switch stage {
			case "begin":
				// The failed Begin is the only transaction operation.
			case "retire-binding":
				h.dbmock.ExpectExec(`UPDATE shared_space_runtime_bindings SET binding_state='retired' WHERE id=\$1`).
					WithArgs("old-binding").WillReturnError(sqlmock.ErrCancelled)
				h.dbmock.ExpectRollback()
			case "mark-applied":
				h.dbmock.ExpectExec(`UPDATE shared_space_runtime_bindings SET binding_state='retired' WHERE id=\$1`).
					WithArgs("old-binding").WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectExec(`UPDATE shared_space_handoffs SET phase='applied' WHERE space_id=\$1 AND request_id=\$2`).
					WithArgs("space", handoff.RequestID).WillReturnError(sqlmock.ErrCancelled)
				h.dbmock.ExpectRollback()
			case "finish-receipt":
				h.dbmock.ExpectExec(`UPDATE shared_space_runtime_bindings SET binding_state='retired' WHERE id=\$1`).
					WithArgs("old-binding").WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectExec(`UPDATE shared_space_handoffs SET phase='applied' WHERE space_id=\$1 AND request_id=\$2`).
					WithArgs("space", handoff.RequestID).WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectExec(`UPDATE shared_space_commands SET state='applied',receipt=\$3 WHERE space_id=\$1 AND request_id=\$2`).
					WithArgs("space", handoff.RequestID, sqlmock.AnyArg()).WillReturnError(sqlmock.ErrCancelled)
				h.dbmock.ExpectRollback()
			case "commit":
				h.dbmock.ExpectExec(`UPDATE shared_space_runtime_bindings SET binding_state='retired' WHERE id=\$1`).
					WithArgs("old-binding").WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectExec(`UPDATE shared_space_handoffs SET phase='applied' WHERE space_id=\$1 AND request_id=\$2`).
					WithArgs("space", handoff.RequestID).WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectExec(`UPDATE shared_space_commands SET state='applied',receipt=\$3 WHERE space_id=\$1 AND request_id=\$2`).
					WithArgs("space", handoff.RequestID, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectCommit().WillReturnError(sqlmock.ErrCancelled)
			}

			h.processHandoffLocked(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project"}, &handoff)
			require.Equal(t, "patch_unknown", handoff.Phase, "a failed durable finalization must never claim the handoff is applied")
			require.Equal(t, []string{"GET /issues/task"}, pcCalls, "finalization recovery must verify state but never repeat the external PATCH")
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
