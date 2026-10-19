package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func handoffExpectLock(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`SELECT pg_advisory_lock\(hashtextextended\(\$1,0\)\)`).WithArgs("shared-space:space").WillReturnResult(sqlmock.NewResult(0, 1))
}

func handoffExpectUnlock(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT pg_advisory_unlock\(hashtextextended\(\$1,0\)\)`).WithArgs("shared-space:space").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
}

func handoffExpectMember(mock sqlmock.Sqlmock, role string) {
	mock.ExpectQuery(`SELECT s.id,s.deployment_id,s.company_id,s.project_id,s.name,m.role FROM shared_spaces s JOIN shared_space_members m ON m.space_id=s.id WHERE s.id=\$1 AND s.deployment_id=\$2 AND m.user_id=\$3 AND m.role<>'revoked'`).
		WithArgs("space", "deployment-acme", "u-owner").WillReturnRows(sqlmock.NewRows([]string{"id", "deployment_id", "company_id", "project_id", "name", "role"}).AddRow("space", "deployment-acme", "company", "project", "Space", role))
}

func handoffExpectReplayMiss(mock sqlmock.Sqlmock, request string) {
	mock.ExpectQuery(`SELECT request_id,phase,target_participant_id,reason,task_id,actor_id,target_agent_id,target_version,binding_id,epoch,payload_hash,stop_payload,source_participant_id,source_name,target_name,source_agent_id FROM shared_space_handoffs WHERE space_id=\$1 AND request_id=\$2`).
		WithArgs("space", request).WillReturnRows(sqlmock.NewRows([]string{"request_id"}))
}

func handoffExpectAssignmentParticipant(mock sqlmock.Sqlmock, agent string, present bool) {
	query := mock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND agent_id=\$2`).WithArgs("space", agent)
	rows := sqlmock.NewRows(participantCommandColumns)
	if present {
		rows.AddRow("source-participant", "companion", "ws-participant-owner/assistant", "Assistant", "u-owner", "uid-ws-participant-owner", "editor", "active", 1, agent, "ready")
	}
	query.WillReturnRows(rows)
}

func handoffNoPendingCommands(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM shared_space_commands WHERE space_id=\$1 AND task_id=\$2 AND state<>'applied'\)`).
		WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
}

func handoffExpectTaskServer(t *testing.T, task map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/issues/task", r.URL.Path)
		_ = json.NewEncoder(w).Encode(task)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSharedHandoffRejectsStaleOrUnsafeTaskWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name       string
		role       string
		task       map[string]any
		token      string
		results    bool
		wantStatus int
	}{
		{name: "viewer cannot hand off", role: "viewer", task: assignmentTask("todo"), wantStatus: 403},
		{name: "stale assignment token", role: "owner", task: assignmentTask("todo"), token: "stale-token", wantStatus: 409},
		{name: "external owner is not transferable", role: "owner", task: map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": "todo", "assigneeAgentId": "external-agent"}, wantStatus: 409},
		{name: "in review task is immutable", role: "owner", task: map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": "in_review", "assigneeAgentId": "agent-a"}, wantStatus: 409},
		{name: "native execution cannot be transferred", role: "owner", task: map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": "todo", "assigneeAgentId": "agent-a", "checkoutRunId": "native-run"}, wantStatus: 409},
		{name: "unreviewed result blocks handoff", role: "owner", task: assignmentTask("todo"), results: true, wantStatus: 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := participantPermissionHarness(t)
			if tc.task["assigneeAgentId"] == nil && tc.task["assigneeUserId"] == nil {
				tc.task["assigneeAgentId"] = "agent-a"
			}
			handoffExpectLock(h.dbmock)
			handoffExpectMember(h.dbmock, tc.role)
			handoffExpectReplayMiss(h.dbmock, "handoff-check")
			h.cfg.paperclipBaseURL = handoffExpectTaskServer(t, tc.task).URL
			switch tc.task["assigneeAgentId"] {
			case "agent-a":
				handoffExpectAssignmentParticipant(h.dbmock, "agent-a", true)
			case "external-agent":
				handoffExpectAssignmentParticipant(h.dbmock, "external-agent", false)
			}
			assignmentEpochRow(h.dbmock, "task", 0)
			inputToken := sharedAssignmentToken(tc.task, 0)
			if tc.token != "" {
				inputToken = tc.token
			}
			if tc.results {
				h.dbmock.ExpectQuery(`SELECT r.id,r.task_id`).WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"id", "task_id", "version", "title", "document_key", "document_id", "revision_id", "body_hash", "submitted_by", "state", "created_at"}).AddRow("result", "task", 1, "Result", "key", "doc", "rev", "hash", "u-owner", "submitted", time.Now()))
			}
			handoffExpectUnlock(h.dbmock)

			body, err := json.Marshal(map[string]any{"requestId": "handoff-check", "expectedAssignmentToken": inputToken, "participantId": "other-participant", "reason": "handoff permission test"})
			require.NoError(t, err)
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
			r.SetPathValue("space", "space")
			r.SetPathValue("task", "task")
			w := httptest.NewRecorder()
			err = h.sharedHandoffTask(w, r, identity{UserID: "u-owner", DeploymentID: "deployment-acme"})
			require.Error(t, err)
			var rejected *sharedError
			require.ErrorAs(t, err, &rejected)
			require.Equal(t, tc.wantStatus, rejected.code, err)
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestSharedHandoffRejectsPendingOrRevokedTargetWithoutWrites(t *testing.T) {
	for _, state := range []string{"pending", "revoked"} {
		t.Run(state, func(t *testing.T) {
			h := participantPermissionHarness(t)
			current := assignmentTask("todo")
			current["assigneeAgentId"] = "agent-a"
			handoffExpectLock(h.dbmock)
			handoffExpectMember(h.dbmock, "owner")
			handoffExpectReplayMiss(h.dbmock, "handoff-target-"+state)
			h.cfg.paperclipBaseURL = handoffExpectTaskServer(t, current).URL
			handoffExpectAssignmentParticipant(h.dbmock, "agent-a", true)
			assignmentEpochRow(h.dbmock, "task", 0)
			h.dbmock.ExpectQuery(`SELECT r.id,r.task_id`).WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"id", "task_id", "version", "title", "document_key", "document_id", "revision_id", "body_hash", "submitted_by", "state", "created_at"}))
			handoffNoPendingCommands(h.dbmock)
			h.dbmock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND id=\$2`).
				WithArgs("space", "target-participant").WillReturnRows(sqlmock.NewRows(participantCommandColumns).AddRow("target-participant", "companion", "ws-participant-owner/assistant", "Assistant", "u-owner", "uid-ws-participant-owner", "editor", state, 1, "", "pending"))
			handoffExpectUnlock(h.dbmock)

			body, err := json.Marshal(map[string]any{"requestId": "handoff-target-" + state, "expectedAssignmentToken": sharedAssignmentToken(current, 0), "participantId": "target-participant", "reason": "handoff permission test"})
			require.NoError(t, err)
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
			r.SetPathValue("space", "space")
			r.SetPathValue("task", "task")
			w := httptest.NewRecorder()
			err = h.sharedHandoffTask(w, r, identity{UserID: "u-owner", DeploymentID: "deployment-acme"})
			require.Error(t, err)
			var rejected *sharedError
			require.ErrorAs(t, err, &rejected)
			require.Equal(t, 403, rejected.code, err)
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
