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

func TestSharedHumanCandidatesHideNativeDirectoryFromCollaborators(t *testing.T) {
	h := newApplyHarness(t)
	humanTestDirectory(t, h, `{"users":[{"principalId":"native-1","status":"active","user":{"id":"native-1","name":"任务人员","email":"fixture@example.test"}}]}`)
	h.dbmock.ExpectQuery("SELECT s.id,s.deployment_id").WithArgs("space", "deployment-acme", "u-x").WillReturnRows(sqlmock.NewRows([]string{"id", "deployment", "company", "project", "name", "role"}).AddRow("space", "deployment-acme", "company", "project", "空间", "editor"))
	h.dbmock.ExpectQuery("SELECT user_id FROM shared_space_members").WithArgs("space").WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("u-x"))
	humanRole(h.dbmock, "u-x", "editor")
	humanMappingRow(h.dbmock, "u-x", "native-1")
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("space", "space")
	w := httptest.NewRecorder()
	require.NoError(t, h.sharedHumanAssignees(w, r, identity{UserID: "u-x", DeploymentID: "deployment-acme"}))
	var response struct {
		Members      []sharedHumanCandidate `json:"members"`
		NativeUsers  []sharedNativeHuman    `json:"nativeUsers"`
		Capabilities map[string]bool        `json:"capabilities"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Empty(t, response.NativeUsers)
	require.False(t, response.Capabilities["manageHumanMappings"])
	require.True(t, response.Members[0].CanClaim)
	require.True(t, response.Members[0].CanAssign)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

// A readable people picker explains incomplete links and temporary directory
// outages while keeping native directory details hidden from read-only staff.
func TestSharedHumanRosterReflectsMemberReadiness(t *testing.T) {
	for _, mode := range []string{"viewer-linked", "waiting-for-link", "native-account-removed", "directory-offline", "former-member"} {
		t.Run(mode, func(t *testing.T) {
			h := newApplyHarness(t)
			native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/companies/company/user-directory", r.URL.Path)
				if mode == "directory-offline" {
					w.WriteHeader(503)
					return
				}
				id := "native-1"
				if mode == "native-account-removed" {
					id = "another-native"
				}
				writeJSON(w, 200, map[string]any{"users": []any{map[string]any{"principalId": id, "status": "active", "user": map[string]any{"id": id, "name": "任务系统人员"}}}})
			}))
			defer native.Close()
			h.cfg.paperclipBaseURL = native.URL
			h.dbmock.ExpectQuery("SELECT s.id,s.deployment_id").WillReturnRows(sqlmock.NewRows([]string{"id", "deployment", "company", "project", "name", "role"}).AddRow("space", "deployment-acme", "company", "project", "空间", "viewer"))
			h.dbmock.ExpectQuery("SELECT user_id FROM shared_space_members").WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow("u-x"))
			if mode == "former-member" {
				h.dbmock.ExpectQuery("SELECT role FROM shared_space_members").WillReturnError(sql.ErrNoRows)
			} else {
				role := "editor"
				if mode == "viewer-linked" {
					role = "viewer"
				}
				humanRole(h.dbmock, "u-x", role)
				id := "native-1"
				if mode == "waiting-for-link" {
					id = ""
				}
				humanMappingRow(h.dbmock, "u-x", id)
			}
			r := httptest.NewRequest("GET", "/", nil)
			r.SetPathValue("space", "space")
			w := httptest.NewRecorder()
			require.NoError(t, h.sharedHumanAssignees(w, r, identity{UserID: "u-x", DeploymentID: "deployment-acme"}))
			var out struct {
				Members      []sharedHumanCandidate `json:"members"`
				NativeUsers  []sharedNativeHuman    `json:"nativeUsers"`
				Capabilities map[string]bool        `json:"capabilities"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
			require.Len(t, out.Members, 1)
			require.Empty(t, out.NativeUsers)
			require.False(t, out.Capabilities["manageHumanMappings"])
			member := out.Members[0]
			require.False(t, member.CanAssign)
			require.False(t, member.CanClaim)
			require.NotEmpty(t, member.UnavailableReason)
			switch mode {
			case "viewer-linked":
				require.Equal(t, "linked", member.Mapping.State)
				require.Equal(t, "viewer", member.Role)
				require.Equal(t, "用户甲", member.DisplayName)
			case "waiting-for-link":
				require.Equal(t, "unlinked", member.Mapping.State)
			default:
				require.Equal(t, "unavailable", member.Mapping.State)
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
