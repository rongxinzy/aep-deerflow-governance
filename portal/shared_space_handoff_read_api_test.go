package main

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

var handoffReadColumns = []string{
	"request_id", "phase", "target_participant_id", "reason", "task_id", "actor_id", "target_agent_id", "target_version",
	"binding_id", "epoch", "payload_hash", "stop_payload", "source_participant_id", "source_name", "target_name", "source_agent_id",
}

func handoffReadRow(request, phase, actor string, epoch int) []driver.Value {
	return []driver.Value{request, phase, "target-participant", "handoff reason", "task", actor, "agent-b", 2, "", epoch, "hash-" + request, []byte(`{}`), "participant", "Agent A", "Agent B", "agent-a"}
}

func expectHandoffReadRow(mock sqlmock.Sqlmock, request, phase, actor string, epoch int) {
	mock.ExpectQuery(`SELECT request_id,phase,target_participant_id,reason,task_id,actor_id,target_agent_id,target_version,binding_id,epoch,payload_hash,stop_payload,source_participant_id,source_name,target_name,source_agent_id FROM shared_space_handoffs WHERE space_id=\$1 AND request_id=\$2`).
		WithArgs("space", request).WillReturnRows(sqlmock.NewRows(handoffReadColumns).AddRow(handoffReadRow(request, phase, actor, epoch)...))
}

func expectHandoffReadMember(mock sqlmock.Sqlmock, actor, role string) {
	mock.ExpectQuery(`SELECT s.id,s.deployment_id,s.company_id,s.project_id,s.name,m.role FROM shared_spaces s JOIN shared_space_members m ON m.space_id=s.id WHERE s.id=\$1 AND s.deployment_id=\$2 AND m.user_id=\$3 AND m.role<>'revoked'`).
		WithArgs("space", "deployment-acme", actor).WillReturnRows(sqlmock.NewRows([]string{"id", "deployment_id", "company_id", "project_id", "name", "role"}).AddRow("space", "deployment-acme", "company", "project", "Space", role))
}

func expectHandoffReadTask(t *testing.T, task map[string]any, calls *[]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodGet {
			http.Error(w, "read only", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, task)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSharedTaskCommandReadsHandoffReceiptOnlyForActorWithoutAdvancingIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		actor     string
		wantError bool
	}{
		{name: "original actor can read", actor: "u-owner"},
		{name: "another owner cannot read", actor: "u-other", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := participantPermissionHarness(t)
			var calls []string
			task := assignmentTask("todo")
			task["assigneeAgentId"] = "agent-a"
			h.cfg.paperclipBaseURL = expectHandoffReadTask(t, task, &calls).URL
			h.cfg.paperclipToken = "synthetic-paperclip"
			handoffExpectLock(h.dbmock)
			expectHandoffReadMember(h.dbmock, tc.actor, "owner")
			expectHandoffReadRow(h.dbmock, "handoff-read", "stop_pending", "u-owner", 4)
			if !tc.wantError {
				handoffExpectAssignmentParticipant(h.dbmock, "agent-a", true)
				assignmentEpochRow(h.dbmock, "task", 4)
			}
			handoffExpectUnlock(h.dbmock)

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.SetPathValue("space", "space")
			r.SetPathValue("request", "handoff-read")
			w := httptest.NewRecorder()
			err := h.sharedTaskCommand(w, r, identity{UserID: tc.actor, DeploymentID: "deployment-acme"})
			if tc.wantError {
				require.Error(t, err)
				var rejected *sharedError
				require.ErrorAs(t, err, &rejected)
				require.Equal(t, http.StatusForbidden, rejected.code)
				require.Empty(t, calls, "a foreign actor must not trigger reads or progress")
			} else {
				require.NoError(t, err)
				require.Equal(t, http.StatusAccepted, w.Code)
				require.Contains(t, w.Body.String(), `"requestId":"handoff-read"`)
				require.Equal(t, []string{"GET /issues/task"}, calls, "receipt reads may inspect task state but must never issue STOP or PATCH")
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestSharedTaskDetailHandoffStateClosesAndRestoresCapabilities(t *testing.T) {
	for _, phase := range []string{"stop_pending", "applied"} {
		t.Run(phase, func(t *testing.T) {
			h := participantPermissionHarness(t)
			task := assignmentTask("todo")
			task["assigneeAgentId"] = "agent-a"
			var calls []string
			h.cfg.paperclipBaseURL = expectHandoffReadTask(t, task, &calls).URL
			h.cfg.paperclipToken = "synthetic-paperclip"
			handoffExpectMember(h.dbmock, "owner")
			h.dbmock.ExpectQuery(`SELECT r.id,r.task_id`).WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"id", "task_id", "version", "title", "document_key", "document_id", "revision_id", "body_hash", "submitted_by", "state", "created_at"}))
			h.dbmock.ExpectQuery(`SELECT v.result_id,v.decision,v.reason,v.actor_id,v.created_at`).WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"result_id", "decision", "reason", "actor_id", "created_at"}))
			commandState := "unknown"
			if phase == "applied" {
				commandState = "applied"
			}
			h.dbmock.ExpectQuery(`SELECT request_id,state,action FROM shared_space_commands WHERE space_id=\$1 AND task_id=\$2 ORDER BY created_at`).
				WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"request_id", "state", "action"}).AddRow("handoff-read", commandState, "handoff"))
			handoffExpectAssignmentParticipant(h.dbmock, "agent-a", true)
			assignmentEpochRow(h.dbmock, "task", 5)
			if phase == "stop_pending" {
				h.dbmock.ExpectQuery(`SELECT request_id,phase,target_participant_id,reason,task_id,actor_id,target_agent_id,target_version,binding_id,epoch,payload_hash,stop_payload,source_participant_id,source_name,target_name,source_agent_id FROM shared_space_handoffs WHERE space_id=\$1 AND task_id=\$2 AND phase<>'applied'`).
					WithArgs("space", "task").WillReturnRows(sqlmock.NewRows(handoffReadColumns).AddRow(handoffReadRow("handoff-read", phase, "u-owner", 5)...))
			} else {
				h.dbmock.ExpectQuery(`SELECT request_id,phase,target_participant_id,reason,task_id,actor_id,target_agent_id,target_version,binding_id,epoch,payload_hash,stop_payload,source_participant_id,source_name,target_name,source_agent_id FROM shared_space_handoffs WHERE space_id=\$1 AND task_id=\$2 AND phase<>'applied'`).
					WithArgs("space", "task").WillReturnError(sql.ErrNoRows)
			}
			// History is intentionally the full set, including an applied handoff.
			history := sqlmock.NewRows(handoffReadColumns)
			if phase == "stop_pending" {
				history.AddRow(handoffReadRow("handoff-read", phase, "u-owner", 5)...)
			} else {
				history.AddRow(handoffReadRow("handoff-read", "applied", "u-owner", 5)...)
			}
			h.dbmock.ExpectQuery(`SELECT request_id,phase,target_participant_id,reason,task_id,actor_id,target_agent_id,target_version,binding_id,epoch,payload_hash,stop_payload,source_participant_id,source_name,target_name,source_agent_id FROM shared_space_handoffs WHERE space_id=\$1 AND task_id=\$2 ORDER BY epoch`).
				WithArgs("space", "task").WillReturnRows(history)

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.SetPathValue("space", "space")
			r.SetPathValue("task", "task")
			w := httptest.NewRecorder()
			err := h.sharedTaskDetail(w, r, identity{UserID: "u-owner", DeploymentID: "deployment-acme"})
			require.NoError(t, err)
			var response struct {
				Handoff  *sharedHandoff  `json:"handoff"`
				Handoffs []sharedHandoff `json:"handoffs"`
				Caps     map[string]any  `json:"capabilities"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response), w.Body.String())
			if phase == "stop_pending" {
				require.NotNil(t, response.Handoff)
				require.Equal(t, false, response.Caps["submit"])
				require.Equal(t, false, response.Caps["review"])
				require.Equal(t, false, response.Caps["assign"])
				require.Equal(t, false, response.Caps["claim"])
				require.Equal(t, false, response.Caps["handoff"])
			} else {
				require.Nil(t, response.Handoff)
				require.Equal(t, true, response.Caps["submit"])
				require.Equal(t, true, response.Caps["assign"])
				require.Equal(t, true, response.Caps["claim"])
				require.Equal(t, true, response.Caps["handoff"])
			}
			require.Len(t, response.Handoffs, 1)
			require.Equal(t, "handoff-read", response.Handoffs[0].RequestID)
			require.Equal(t, []string{"GET /issues/task"}, calls)
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
