package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func humanTestDirectory(t *testing.T, h *apiTestPortal, body string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/companies/company/user-directory", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	h.cfg.paperclipBaseURL = server.URL
	h.cfg.paperclipToken = "synthetic-token"
}
func humanRole(mock sqlmock.Sqlmock, id, role string) {
	mock.ExpectQuery("SELECT role FROM shared_space_members").WithArgs("space", id).WillReturnRows(sqlmock.NewRows([]string{"role"}).AddRow(role))
}
func humanMappingRow(mock sqlmock.Sqlmock, id, native string) {
	q := mock.ExpectQuery("SELECT native_user_id FROM shared_space_human_mappings").WithArgs("deployment-acme", "company", id)
	if native == "" {
		q.WillReturnError(sql.ErrNoRows)
	} else {
		q.WillReturnRows(sqlmock.NewRows([]string{"native_user_id"}).AddRow(native))
	}
}
func TestSharedHumanExplicitMappingAndNativeDirectory(t *testing.T) {
	h := newApplyHarness(t)
	humanTestDirectory(t, h, `{"users":[{"principalId":"native-1","status":"active","user":{"id":"native-1","name":"任务人员"}},{"principalId":"inactive","status":"disabled","user":{"id":"inactive","name":"停用"}},{"principalId":"ghost","status":"active","user":null},{"principalId":"wrong","status":"active","user":{"id":"other"}}]}`)
	s := sharedSpace{ID: "space", DeploymentID: "deployment-acme", CompanyID: "company", Role: "owner"}
	humanRole(h.dbmock, "u-x", "editor")
	humanMappingRow(h.dbmock, "u-x", "native-1")
	human, err := h.sharedHumanForUser(t.Context(), s, "u-x")
	require.NoError(t, err)
	require.Equal(t, "native-1", human.NativeUserID)
	require.Equal(t, "u-x", human.UserID)
	raw, err := json.Marshal(human)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "native-1")
	users, err := h.sharedNativeHumans(t.Context(), s)
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}
func TestSharedHumanNeverGuessesNativeIdentity(t *testing.T) {
	h := newApplyHarness(t)
	s := sharedSpace{ID: "space", DeploymentID: "deployment-acme", CompanyID: "company"}
	humanRole(h.dbmock, "u-x", "editor")
	humanMappingRow(h.dbmock, "u-x", "")
	_, err := h.sharedHumanForUser(t.Context(), s, "u-x")
	require.Error(t, err)
	require.Equal(t, 409, err.(*sharedError).code)
	h.dbmock.ExpectQuery("SELECT user_id FROM shared_space_human_mappings").WithArgs("deployment-acme", "company", "u-x").WillReturnError(sql.ErrNoRows)
	human, err := h.sharedHumanForNative(t.Context(), s, "u-x")
	require.NoError(t, err)
	require.Nil(t, human)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}
func TestSharedHumanRejectsInactiveAndViewer(t *testing.T) {
	for _, tc := range []struct{ id, role string }{{"u-off", "editor"}, {"u-agent", "editor"}, {"u-x", "viewer"}} {
		t.Run(tc.id+tc.role, func(t *testing.T) {
			h := newApplyHarness(t)
			humanRole(h.dbmock, tc.id, tc.role)
			_, err := h.sharedHumanForUser(t.Context(), sharedSpace{ID: "space"}, tc.id)
			require.Error(t, err)
			require.Equal(t, 403, err.(*sharedError).code)
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
func TestSharedHumanMappingCreatesOnceAndRejectsRebinding(t *testing.T) {
	for _, existing := range []string{"native-1", "native-other"} {
		t.Run(existing, func(t *testing.T) {
			h := newApplyHarness(t)
			humanTestDirectory(t, h, `{"users":[{"principalId":"native-1","status":"active","user":{"id":"native-1","name":"任务人员"}}]}`)
			h.dbmock.ExpectQuery("SELECT s.id,s.deployment_id").WithArgs("space", "deployment-acme", "u-owner").WillReturnRows(sqlmock.NewRows([]string{"id", "deployment", "company", "project", "name", "role"}).AddRow("space", "deployment-acme", "company", "project", "空间", "owner"))
			humanRole(h.dbmock, "u-x", "editor")
			h.dbmock.ExpectExec("INSERT INTO shared_space_human_mappings").WithArgs("deployment-acme", "company", "u-x", "native-1", "u-owner").WillReturnResult(sqlmock.NewResult(0, 0))
			humanMappingRow(h.dbmock, "u-x", existing)
			if existing == "native-1" {
				humanRole(h.dbmock, "u-x", "editor")
				humanMappingRow(h.dbmock, "u-x", existing)
			}
			r := httptest.NewRequest("PUT", "/", strings.NewReader(`{"requestId":"same-intent","paperclipUserId":"native-1"}`))
			r.SetPathValue("space", "space")
			r.SetPathValue("user", "u-x")
			w := httptest.NewRecorder()
			err := h.sharedHumanMapping(w, r, identity{UserID: "u-owner", DeploymentID: "deployment-acme"})
			if existing == "native-1" {
				require.NoError(t, err)
				require.Equal(t, 200, w.Code)
			} else {
				require.Error(t, err)
				require.Equal(t, 409, err.(*sharedError).code)
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
