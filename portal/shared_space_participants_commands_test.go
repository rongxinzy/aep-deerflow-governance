package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

var participantCommandColumns = []string{"id", "kind", "target", "name", "owner_id", "resource_uid", "role", "state", "version", "agent_id", "registration_state"}

func expectParticipantCommandLock(mock sqlmock.Sqlmock, space string) {
	mock.ExpectExec(`SELECT pg_advisory_lock\(hashtextextended\(\$1,0\)\)`).
		WithArgs("shared-space:" + space).WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectParticipantCommandUnlock(mock sqlmock.Sqlmock, space string) {
	mock.ExpectQuery(`SELECT pg_advisory_unlock\(hashtextextended\(\$1,0\)\)`).
		WithArgs("shared-space:" + space).WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
}

func expectParticipantCommandSpace(mock sqlmock.Sqlmock, space, user, role string) {
	mock.ExpectQuery(`SELECT s.id,s.deployment_id,s.company_id,s.project_id,s.name,m.role FROM shared_spaces s JOIN shared_space_members m ON m.space_id=s.id WHERE s.id=\$1 AND s.deployment_id=\$2 AND m.user_id=\$3 AND m.role<>'revoked'`).
		WithArgs(space, "deployment-acme", user).
		WillReturnRows(sqlmock.NewRows([]string{"id", "deployment_id", "company_id", "project_id", "name", "role"}).AddRow(space, "deployment-acme", "company", "project", "Space", role))
}

func expectParticipantCommandSpaceMissing(mock sqlmock.Sqlmock, space, user string) {
	mock.ExpectQuery(`SELECT s.id,s.deployment_id,s.company_id,s.project_id,s.name,m.role FROM shared_spaces s JOIN shared_space_members m ON m.space_id=s.id WHERE s.id=\$1 AND s.deployment_id=\$2 AND m.user_id=\$3 AND m.role<>'revoked'`).
		WithArgs(space, "deployment-acme", user).
		WillReturnRows(sqlmock.NewRows([]string{"id", "deployment_id", "company_id", "project_id", "name", "role"}))
}

func expectParticipantCommandReplayMiss(mock sqlmock.Sqlmock, space, request string) {
	mock.ExpectQuery(`SELECT actor_id,action,payload_hash,state,receipt FROM shared_space_commands WHERE space_id=\$1 AND request_id=\$2`).
		WithArgs(space, request).WillReturnRows(sqlmock.NewRows([]string{"actor_id", "action", "payload_hash", "state", "receipt"}))
}

func expectParticipantCommandRow(mock sqlmock.Sqlmock, space, id, state string, version int) {
	mock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND id=\$2`).
		WithArgs(space, id).WillReturnRows(sqlmock.NewRows(participantCommandColumns).
		AddRow(id, "companion", "ws-participant-owner/assistant", "assistant", "u-owner", "uid-ws-participant-owner", "editor", state, version, "", "pending"))
}

func callParticipantCommand(t *testing.T, h *apiTestPortal, actor, space, participant, action string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
	r.SetPathValue("space", space)
	if participant != "" {
		r.SetPathValue("participant", participant)
	}
	r.SetPathValue("participantAction", action)
	w := httptest.NewRecorder()
	if err = h.sharedParticipantChange(w, r, identity{UserID: actor, Teams: []string{"sales-dept"}, DeploymentID: "deployment-acme"}); err != nil {
		sharedWriteError(w, err)
	}
	out := map[string]any{}
	if w.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
	}
	return w.Code, out
}

func TestSharedParticipantsHandlerAuthorizesAndReportsDatabaseFailures(t *testing.T) {
	t.Run("space viewer can read participant list", func(t *testing.T) {
		h := participantPermissionHarness(t)
		expectParticipantCommandSpace(h.dbmock, "space", "u-x", "viewer")
		h.dbmock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 ORDER BY name,id`).
			WithArgs("space").WillReturnRows(sqlmock.NewRows(participantCommandColumns))
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetPathValue("space", "space")
		w := httptest.NewRecorder()
		err := h.sharedParticipants(w, r, identity{UserID: "u-x", DeploymentID: "deployment-acme"})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, w.Code)
		require.JSONEq(t, `{"participants":[]}`, w.Body.String())
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("nonmember cannot list participants", func(t *testing.T) {
		h := participantPermissionHarness(t)
		expectParticipantCommandSpaceMissing(h.dbmock, "space", "u-x")
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetPathValue("space", "space")
		w := httptest.NewRecorder()
		err := h.sharedParticipants(w, r, identity{UserID: "u-x", DeploymentID: "deployment-acme"})
		require.Error(t, err)
		sharedWriteError(w, err)
		require.Equal(t, http.StatusNotFound, w.Code)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("participant query failure surfaces as service unavailable", func(t *testing.T) {
		h := participantPermissionHarness(t)
		expectParticipantCommandSpace(h.dbmock, "space", "u-x", "viewer")
		h.dbmock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 ORDER BY name,id`).
			WithArgs("space").WillReturnError(sqlmock.ErrCancelled)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetPathValue("space", "space")
		w := httptest.NewRecorder()
		err := h.sharedParticipants(w, r, identity{UserID: "u-x", DeploymentID: "deployment-acme"})
		require.Error(t, err)
		sharedWriteError(w, err)
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})
}

func TestSharedParticipantChangeRejectsUnauthorizedAndStaleCommands(t *testing.T) {
	t.Run("malformed and request-id-less payloads fail before acquiring a lock", func(t *testing.T) {
		for _, raw := range []string{`{"requestId":"x","unknown":true}`, `{"kind":"companion","target":"ws-participant-owner/assistant","role":"editor"}`} {
			h := participantPermissionHarness(t)
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(raw))
			r.SetPathValue("space", "space")
			w := httptest.NewRecorder()
			err := h.sharedParticipantChange(w, r, identity{UserID: "u-owner", DeploymentID: "deployment-acme"})
			require.Error(t, err)
			sharedWriteError(w, err)
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		}
	})

	t.Run("non-owner cannot invite", func(t *testing.T) {
		h := participantPermissionHarness(t)
		expectParticipantCommandLock(h.dbmock, "space")
		expectParticipantCommandSpace(h.dbmock, "space", "u-x", "editor")
		expectParticipantCommandReplayMiss(h.dbmock, "space", "non-owner-invite")
		expectParticipantCommandUnlock(h.dbmock, "space")
		status, _ := callParticipantCommand(t, h, "u-x", "space", "", "", map[string]any{"kind": "companion", "target": "ws-participant-owner/assistant", "role": "editor", "requestId": "non-owner-invite"})
		require.Equal(t, http.StatusForbidden, status)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("owner cannot assign an invalid participant role", func(t *testing.T) {
		h := participantPermissionHarness(t)
		expectParticipantCommandLock(h.dbmock, "space")
		expectParticipantCommandSpace(h.dbmock, "space", "u-owner", "owner")
		expectParticipantCommandReplayMiss(h.dbmock, "space", "bad-role")
		expectParticipantCommandUnlock(h.dbmock, "space")
		status, _ := callParticipantCommand(t, h, "u-owner", "space", "", "", map[string]any{"kind": "companion", "target": "ws-participant-owner/assistant", "role": "owner", "requestId": "bad-role"})
		require.Equal(t, http.StatusBadRequest, status)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("space owner cannot approve an employee owned by another manager", func(t *testing.T) {
		h := participantPermissionHarness(t)
		expectParticipantCommandLock(h.dbmock, "space")
		expectParticipantCommandSpace(h.dbmock, "space", "u-x", "owner")
		expectParticipantCommandReplayMiss(h.dbmock, "space", "approve-other-manager")
		h.dbmock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND kind=\$2 AND target=\$3`).
			WithArgs("space", "digital_employee", "employee-sales").WillReturnRows(sqlmock.NewRows(participantCommandColumns))
		expectParticipantCommandUnlock(h.dbmock, "space")
		status, out := callParticipantCommand(t, h, "u-x", "space", "", "", map[string]any{"kind": "digital_employee", "target": "employee-sales", "role": "editor", "approve": true, "requestId": "approve-other-manager"})
		require.Equal(t, http.StatusForbidden, status, out)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("existing participant cannot be invited twice", func(t *testing.T) {
		h := participantPermissionHarness(t)
		expectParticipantCommandLock(h.dbmock, "space")
		expectParticipantCommandSpace(h.dbmock, "space", "u-owner", "owner")
		expectParticipantCommandReplayMiss(h.dbmock, "space", "invite-existing")
		h.dbmock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND kind=\$2 AND target=\$3`).
			WithArgs("space", "companion", "ws-participant-owner/assistant").
			WillReturnRows(sqlmock.NewRows(participantCommandColumns).AddRow("old-participant", "companion", "ws-participant-owner/assistant", "assistant", "u-owner", "uid-ws-participant-owner", "editor", "active", 1, "agent-1", "ready"))
		expectParticipantCommandUnlock(h.dbmock, "space")
		status, _ := callParticipantCommand(t, h, "u-owner", "space", "", "", map[string]any{"kind": "companion", "target": "ws-participant-owner/assistant", "role": "editor", "requestId": "invite-existing"})
		require.Equal(t, http.StatusConflict, status)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("accept by a non-resource-manager is denied through invitation fallback", func(t *testing.T) {
		h := participantPermissionHarness(t)
		expectParticipantCommandLock(h.dbmock, "space")
		expectParticipantCommandSpaceMissing(h.dbmock, "space", "u-x")
		h.dbmock.ExpectQuery(`SELECT id,deployment_id,company_id,project_id,name FROM shared_spaces WHERE id=\$1 AND deployment_id=\$2`).
			WithArgs("space", "deployment-acme").
			WillReturnRows(sqlmock.NewRows([]string{"id", "deployment_id", "company_id", "project_id", "name"}).AddRow("space", "deployment-acme", "company", "project", "Space"))
		expectParticipantCommandReplayMiss(h.dbmock, "space", "accept-other-manager")
		expectParticipantCommandRow(h.dbmock, "space", "participant", "pending", 1)
		expectParticipantCommandUnlock(h.dbmock, "space")
		status, _ := callParticipantCommand(t, h, "u-x", "space", "participant", "accept", map[string]any{"requestId": "accept-other-manager", "expectedVersion": 1})
		require.Equal(t, http.StatusForbidden, status)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("stale version and unknown action fail closed", func(t *testing.T) {
		for _, tc := range []struct {
			name, action string
			version      int
			want         int
		}{
			{name: "stale-version", action: "revoke", version: 1, want: http.StatusConflict},
			{name: "unknown-action", action: "unexpected", version: 2, want: http.StatusNotFound},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := participantPermissionHarness(t)
				expectParticipantCommandLock(h.dbmock, "space")
				expectParticipantCommandSpace(h.dbmock, "space", "u-owner", "owner")
				expectParticipantCommandReplayMiss(h.dbmock, "space", tc.name)
				expectParticipantCommandRow(h.dbmock, "space", "participant", "pending", 2)
				expectParticipantCommandUnlock(h.dbmock, "space")
				status, _ := callParticipantCommand(t, h, "u-owner", "space", "participant", tc.action, map[string]any{"requestId": tc.name, "expectedVersion": tc.version})
				require.Equal(t, tc.want, status)
				require.NoError(t, h.dbmock.ExpectationsWereMet())
			})
		}
	})

	t.Run("missing participant is not treated as a command", func(t *testing.T) {
		h := participantPermissionHarness(t)
		expectParticipantCommandLock(h.dbmock, "space")
		expectParticipantCommandSpace(h.dbmock, "space", "u-owner", "owner")
		expectParticipantCommandReplayMiss(h.dbmock, "space", "revoke-missing")
		h.dbmock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND id=\$2`).
			WithArgs("space", "missing").WillReturnRows(sqlmock.NewRows(participantCommandColumns))
		expectParticipantCommandUnlock(h.dbmock, "space")
		status, _ := callParticipantCommand(t, h, "u-owner", "space", "missing", "revoke", map[string]any{"requestId": "revoke-missing", "expectedVersion": 1})
		require.Equal(t, http.StatusNotFound, status)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})
}

func TestSharedParticipantChangeRollsBackFailedPersistence(t *testing.T) {
	for _, stage := range []string{"begin", "participant", "receipt"} {
		t.Run(stage, func(t *testing.T) {
			h := participantPermissionHarness(t)
			expectParticipantCommandLock(h.dbmock, "space")
			expectParticipantCommandSpace(h.dbmock, "space", "u-owner", "owner")
			expectParticipantCommandReplayMiss(h.dbmock, "space", "invite-"+stage)
			h.dbmock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND kind=\$2 AND target=\$3`).
				WithArgs("space", "companion", "ws-participant-owner/assistant").
				WillReturnRows(sqlmock.NewRows(participantCommandColumns))
			if stage == "begin" {
				h.dbmock.ExpectBegin().WillReturnError(sqlmock.ErrCancelled)
				expectParticipantCommandUnlock(h.dbmock, "space")
				status, _ := callParticipantCommand(t, h, "u-owner", "space", "", "", map[string]any{"kind": "companion", "target": "ws-participant-owner/assistant", "role": "editor", "requestId": "invite-" + stage})
				require.Equal(t, http.StatusServiceUnavailable, status)
				require.NoError(t, h.dbmock.ExpectationsWereMet())
				return
			}

			h.dbmock.ExpectBegin()
			h.dbmock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM shared_space_commands WHERE space_id=\$1 AND task_id=\$2 AND state<>'applied'\)`).
				WithArgs("space", sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
			h.dbmock.ExpectExec(`INSERT INTO shared_space_commands`).
				WithArgs("space", "invite-"+stage, sqlmock.AnyArg(), "u-owner", "participant", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
			if stage == "participant" {
				h.dbmock.ExpectExec(`INSERT INTO shared_space_participants`).WillReturnError(sqlmock.ErrCancelled)
			} else {
				h.dbmock.ExpectExec(`INSERT INTO shared_space_participants`).
					WithArgs(sqlmock.AnyArg(), "space", "companion", "ws-participant-owner/assistant", "assistant", "u-owner", "uid-ws-participant-owner", "editor", "pending", 1, "", "pending").
					WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectExec(`UPDATE shared_space_commands SET state='applied',receipt=\$3 WHERE space_id=\$1 AND request_id=\$2`).
					WithArgs("space", "invite-"+stage, sqlmock.AnyArg()).WillReturnError(sqlmock.ErrCancelled)
			}
			h.dbmock.ExpectRollback()
			expectParticipantCommandUnlock(h.dbmock, "space")
			status, _ := callParticipantCommand(t, h, "u-owner", "space", "", "", map[string]any{"kind": "companion", "target": "ws-participant-owner/assistant", "role": "editor", "requestId": "invite-" + stage})
			require.Equal(t, http.StatusServiceUnavailable, status)
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
