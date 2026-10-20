package main

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSharedHumanSubmitRequiresCurrentMappedAssignee(t *testing.T) {
	for _, mode := range []string{"colleague", "external"} {
		t.Run(mode, func(t *testing.T) {
			h := newApplyHarness(t)
			h.dbmock.ExpectExec("SELECT pg_advisory_lock").WillReturnResult(sqlmock.NewResult(0, 1))
			runtimeExpectMemberSpace(h, "editor")
			q := h.dbmock.ExpectQuery("SELECT user_id FROM shared_space_human_mappings")
			if mode == "external" {
				q.WillReturnError(sql.ErrNoRows)
			} else {
				q.WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("u-x"))
				humanRole(h.dbmock, "u-x", "editor")
				humanMappingRow(h.dbmock, "u-x", "native-x")
			}
			runtimeExpectUnlock(h)
			sharedNativeServer(t, h.portal, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "GET", r.Method, "authorization failure must perform no native mutation")
				if r.URL.Path == "/companies/company/user-directory" {
					writeJSON(w, 200, map[string]any{"users": []any{map[string]any{"principalId": "native-x", "status": "active", "user": map[string]any{"id": "native-x", "name": "Native"}}}})
					return
				}
				writeJSON(w, 200, map[string]any{"id": "task", "companyId": "company", "projectId": "project", "assigneeUserId": "native-x", "status": "in_progress"})
			})
			_, _, err := h.sharedSubmitInput(t.Context(), "space", "task", identity{UserID: "u-owner", DeploymentID: "deployment-acme"}, sharedSubmission{RequestID: "request", Title: "成果", Body: "正文"})
			require.Error(t, err)
			require.Equal(t, 403, err.(*sharedError).code)
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
