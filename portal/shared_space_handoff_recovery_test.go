package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSharedHandoffRecoveryReadsFailClosed(t *testing.T) {
	t.Run("missing epoch is legacy zero but database failure propagates", func(t *testing.T) {
		h := newAPIHarness(t)
		h.dbmock.ExpectQuery("SELECT epoch FROM shared_space_task_epochs").WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"epoch"}))
		epoch, err := h.sharedTaskEpoch(context.Background(), h.db, "space", "task")
		require.NoError(t, err)
		require.Zero(t, epoch)

		h.dbmock.ExpectQuery("SELECT epoch FROM shared_space_task_epochs").WithArgs("space", "task").WillReturnError(errors.New("database unavailable"))
		_, err = h.sharedTaskEpoch(context.Background(), h.db, "space", "task")
		require.ErrorContains(t, err, "database unavailable")
	})

	t.Run("handoff gate permits clear queue, blocks pending request, and propagates query errors", func(t *testing.T) {
		h := newAPIHarness(t)
		query := "SELECT EXISTS\\(SELECT 1 FROM shared_space_handoffs"
		h.dbmock.ExpectQuery(query).WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"pending"}).AddRow(false))
		require.NoError(t, h.sharedHandoffGate(context.Background(), h.db, "space", "task"))

		h.dbmock.ExpectQuery(query).WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"pending"}).AddRow(true))
		err := h.sharedHandoffGate(context.Background(), h.db, "space", "task")
		var sharedErr *sharedError
		require.ErrorAs(t, err, &sharedErr)
		require.Equal(t, http.StatusLocked, sharedErr.code)

		h.dbmock.ExpectQuery(query).WithArgs("space", "task").WillReturnError(errors.New("database unavailable"))
		err = h.sharedHandoffGate(context.Background(), h.db, "space", "task")
		require.ErrorContains(t, err, "database unavailable")
	})

	t.Run("missing active handoff is empty and database failure propagates", func(t *testing.T) {
		h := newAPIHarness(t)
		query := "SELECT request_id,phase,target_participant_id,reason,task_id,actor_id"
		h.dbmock.ExpectQuery(query).WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"request_id"}))
		got, err := h.sharedHandoffDTO(context.Background(), "space", "task")
		require.NoError(t, err)
		require.Nil(t, got)

		h.dbmock.ExpectQuery(query).WithArgs("space", "task").WillReturnError(errors.New("database unavailable"))
		_, err = h.sharedHandoffDTO(context.Background(), "space", "task")
		require.ErrorContains(t, err, "database unavailable")
	})
}

func TestSharedHandoffPublicPhaseHidesInternalState(t *testing.T) {
	cases := []struct {
		phase  string
		public string
		state  string
	}{
		{phase: "stop_pending", public: "stopping", state: "unknown"},
		{phase: "stop_unknown", public: "stopping", state: "unknown"},
		{phase: "patch_pending", public: "assignment_unknown", state: "unknown"},
		{phase: "patch_unknown", public: "assignment_unknown", state: "unknown"},
		{phase: "applied", public: "applied", state: "applied"},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			h := sharedHandoff{
				Phase: tc.phase, RequestID: "request-1", TargetID: "participant-b", TargetName: "Agent B",
				FromID: "participant-a", SourceName: "Agent A", Reason: "handoff reason",
				TaskID: "private-task", Actor: "private-actor", AgentID: "private-agent", SourceAgentID: "private-source-agent",
				BindingID: "private-binding", Epoch: 42, Hash: "private-hash", Stop: []byte(`{"secret":"private"}`),
			}
			require.Equal(t, tc.public, h.publicPhase())
			raw, err := json.Marshal(h)
			require.NoError(t, err)
			var public map[string]any
			require.NoError(t, json.Unmarshal(raw, &public))
			require.Equal(t, tc.public, public["phase"])
			require.Equal(t, tc.state, public["status"])
			require.Equal(t, "participant-a", public["from"].(map[string]any)["id"])
			require.Equal(t, "participant-b", public["to"].(map[string]any)["id"])
			for _, hidden := range []string{"taskId", "actor", "agentId", "sourceAgentId", "bindingId", "epoch", "hash", "stop"} {
				require.NotContains(t, public, hidden)
			}
			require.NotContains(t, string(raw), "private-")
		})
	}
}

func TestSharedHandoffRequestBoundariesAndIdempotency(t *testing.T) {
	t.Run("required fields are rejected before database access", func(t *testing.T) {
		h := newAPIHarness(t)
		for _, body := range []string{`{`, `{}`, `{"requestId":"r","unexpected":true}`, `{"requestId":"r"}`, `{"requestId":"r","expectedAssignmentToken":"t","reason":"  "}`} {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req.SetPathValue("space", "space")
			req.SetPathValue("task", "task")
			err := h.sharedHandoffTask(httptest.NewRecorder(), req, identity{UserID: "u-owner", DeploymentID: "deployment-acme"})
			var rejected *sharedError
			require.ErrorAs(t, err, &rejected)
			require.Equal(t, http.StatusBadRequest, rejected.code)
		}
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("terminal tasks cannot be reopened through handoff", func(t *testing.T) {
		for _, status := range []string{"done", "cancelled"} {
			t.Run(status, func(t *testing.T) {
				h := participantPermissionHarness(t)
				current := assignmentTask(status)
				current["assigneeAgentId"] = "agent-a"
				handoffExpectLock(h.dbmock)
				handoffExpectMember(h.dbmock, "owner")
				handoffExpectReplayMiss(h.dbmock, "handoff-"+status)
				h.cfg.paperclipBaseURL = handoffExpectTaskServer(t, current).URL
				handoffExpectAssignmentParticipant(h.dbmock, "agent-a", true)
				assignmentEpochRow(h.dbmock, "task", 0)
				handoffExpectUnlock(h.dbmock)

				body, err := json.Marshal(map[string]any{"requestId": "handoff-" + status, "expectedAssignmentToken": sharedAssignmentToken(current, 0), "participantId": "target", "reason": "handoff permission test"})
				require.NoError(t, err)
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
				req.SetPathValue("space", "space")
				req.SetPathValue("task", "task")
				err = h.sharedHandoffTask(httptest.NewRecorder(), req, identity{UserID: "u-owner", DeploymentID: "deployment-acme"})
				var rejected *sharedError
				require.ErrorAs(t, err, &rejected)
				require.Equal(t, http.StatusConflict, rejected.code)
				require.NoError(t, h.dbmock.ExpectationsWereMet())
			})
		}
	})

	t.Run("same request id cannot be reused with changed content", func(t *testing.T) {
		h := participantPermissionHarness(t)
		handoffExpectLock(h.dbmock)
		handoffExpectMember(h.dbmock, "owner")
		h.dbmock.ExpectQuery(`SELECT request_id,phase,target_participant_id,reason,task_id,actor_id,target_agent_id,target_version,binding_id,epoch,payload_hash,stop_payload,source_participant_id,source_name,target_name,source_agent_id FROM shared_space_handoffs WHERE space_id=\$1 AND request_id=\$2`).
			WithArgs("space", "handoff-replay").WillReturnRows(sqlmock.NewRows([]string{"request_id", "phase", "target_participant_id", "reason", "task_id", "actor_id", "target_agent_id", "target_version", "binding_id", "epoch", "payload_hash", "stop_payload", "source_participant_id", "source_name", "target_name", "source_agent_id"}).
			AddRow("handoff-replay", "stop_unknown", "target", "original reason", "task", "u-owner", "agent-b", 1, "binding", 1, "original-payload-hash", []byte(`{}`), "source", "Agent A", "Agent B", "agent-a"))
		handoffExpectUnlock(h.dbmock)

		body, err := json.Marshal(map[string]any{"requestId": "handoff-replay", "expectedAssignmentToken": "token", "participantId": "target", "reason": "changed reason"})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		req.SetPathValue("space", "space")
		req.SetPathValue("task", "task")
		err = h.sharedHandoffTask(httptest.NewRecorder(), req, identity{UserID: "u-owner", DeploymentID: "deployment-acme"})
		var rejected *sharedError
		require.ErrorAs(t, err, &rejected)
		require.Equal(t, http.StatusConflict, rejected.code)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("current Agent cannot be selected as its own target", func(t *testing.T) {
		h := participantPermissionHarness(t)
		current := assignmentTask("todo")
		current["assigneeAgentId"] = "agent-a"
		handoffExpectLock(h.dbmock)
		handoffExpectMember(h.dbmock, "owner")
		handoffExpectReplayMiss(h.dbmock, "handoff-self")
		h.cfg.paperclipBaseURL = handoffExpectTaskServer(t, current).URL
		handoffExpectAssignmentParticipant(h.dbmock, "agent-a", true)
		assignmentEpochRow(h.dbmock, "task", 0)
		h.dbmock.ExpectQuery(`SELECT r.id,r.task_id`).WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"id", "task_id", "version", "title", "document_key", "document_id", "revision_id", "body_hash", "submitted_by", "state", "created_at"}))
		handoffNoPendingCommands(h.dbmock)
		handoffExpectUnlock(h.dbmock)

		body, err := json.Marshal(map[string]any{"requestId": "handoff-self", "expectedAssignmentToken": sharedAssignmentToken(current, 0), "participantId": "source-participant", "reason": "handoff permission test"})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		req.SetPathValue("space", "space")
		req.SetPathValue("task", "task")
		err = h.sharedHandoffTask(httptest.NewRecorder(), req, identity{UserID: "u-owner", DeploymentID: "deployment-acme"})
		var rejected *sharedError
		require.ErrorAs(t, err, &rejected)
		require.Equal(t, http.StatusConflict, rejected.code)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("target that disappeared before handoff is not guessed", func(t *testing.T) {
		h := participantPermissionHarness(t)
		current := assignmentTask("todo")
		current["assigneeAgentId"] = "agent-a"
		handoffExpectLock(h.dbmock)
		handoffExpectMember(h.dbmock, "owner")
		handoffExpectReplayMiss(h.dbmock, "handoff-missing-target")
		h.cfg.paperclipBaseURL = handoffExpectTaskServer(t, current).URL
		handoffExpectAssignmentParticipant(h.dbmock, "agent-a", true)
		assignmentEpochRow(h.dbmock, "task", 0)
		h.dbmock.ExpectQuery(`SELECT r.id,r.task_id`).WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"id", "task_id", "version", "title", "document_key", "document_id", "revision_id", "body_hash", "submitted_by", "state", "created_at"}))
		handoffNoPendingCommands(h.dbmock)
		h.dbmock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND id=\$2`).
			WithArgs("space", "missing-target").WillReturnRows(sqlmock.NewRows(participantCommandColumns))
		handoffExpectUnlock(h.dbmock)

		body, err := json.Marshal(map[string]any{"requestId": "handoff-missing-target", "expectedAssignmentToken": sharedAssignmentToken(current, 0), "participantId": "missing-target", "reason": "handoff permission test"})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		req.SetPathValue("space", "space")
		req.SetPathValue("task", "task")
		err = h.sharedHandoffTask(httptest.NewRecorder(), req, identity{UserID: "u-owner", DeploymentID: "deployment-acme"})
		var rejected *sharedError
		require.ErrorAs(t, err, &rejected)
		require.Equal(t, http.StatusNotFound, rejected.code)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("viewer participant cannot receive a task by handoff", func(t *testing.T) {
		h := participantPermissionHarness(t)
		current := assignmentTask("todo")
		current["assigneeAgentId"] = "agent-a"
		handoffExpectLock(h.dbmock)
		handoffExpectMember(h.dbmock, "owner")
		handoffExpectReplayMiss(h.dbmock, "handoff-viewer-target")
		h.cfg.paperclipBaseURL = handoffExpectTaskServer(t, current).URL
		handoffExpectAssignmentParticipant(h.dbmock, "agent-a", true)
		assignmentEpochRow(h.dbmock, "task", 0)
		h.dbmock.ExpectQuery(`SELECT r.id,r.task_id`).WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"id", "task_id", "version", "title", "document_key", "document_id", "revision_id", "body_hash", "submitted_by", "state", "created_at"}))
		handoffNoPendingCommands(h.dbmock)
		h.dbmock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND id=\$2`).
			WithArgs("space", "viewer-target").WillReturnRows(sqlmock.NewRows(participantCommandColumns).
			AddRow("viewer-target", "companion", "ws-participant-owner/assistant", "Viewer", "u-owner", "uid-ws-participant-owner", "viewer", "active", 1, "agent-viewer", "ready"))
		handoffExpectUnlock(h.dbmock)

		body, err := json.Marshal(map[string]any{"requestId": "handoff-viewer-target", "expectedAssignmentToken": sharedAssignmentToken(current, 0), "participantId": "viewer-target", "reason": "handoff permission test"})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		req.SetPathValue("space", "space")
		req.SetPathValue("task", "task")
		err = h.sharedHandoffTask(httptest.NewRecorder(), req, identity{UserID: "u-owner", DeploymentID: "deployment-acme"})
		var rejected *sharedError
		require.ErrorAs(t, err, &rejected)
		require.Equal(t, http.StatusForbidden, rejected.code)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})
}

func TestSharedHandoffStopAndHistoryFailuresFailClosed(t *testing.T) {
	t.Run("cannot dispatch stop unless stopping phase is persisted", func(t *testing.T) {
		h := participantPermissionHarness(t)
		ctx := context.Background()
		wsName := "ws-participant-owner"
		require.NoError(t, h.k8s.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: workspaceMemkeySecretName(wsName, "assistant"), Namespace: h.cfg.namespace},
			Data:       map[string][]byte{"WORKSPACE_INTERNAL_TOKEN": []byte("synthetic-only")},
		}))
		stopCalls := 0
		pi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stopCalls++
			writeJSON(w, http.StatusOK, map[string]any{})
		}))
		defer pi.Close()
		h.workspaceChatOverride = pi.URL

		binding := runtimeTestBinding()
		binding.Workspace = wsName
		binding.WorkspaceUID = "uid-ws-participant-owner"
		binding.Owner = "u-owner"
		binding.Companion = "assistant"
		h.dbmock.ExpectQuery("SELECT id,space_id,task_id,workspace_name").WithArgs(binding.ID).WillReturnRows(runtimeTestRows(binding))
		h.dbmock.ExpectExec("UPDATE shared_space_handoffs SET phase").WithArgs("space", "request", "stop_unknown").WillReturnError(errors.New("phase write failed"))
		conn, err := h.db.Conn(ctx)
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		handoff := sharedHandoff{Phase: "stop_pending", RequestID: "request", BindingID: binding.ID, Stop: []byte(`{}`)}
		h.processHandoffLocked(ctx, conn, sharedSpace{ID: binding.SpaceID}, &handoff)
		require.Equal(t, "stop_pending", handoff.Phase)
		require.Zero(t, stopCalls, "the stop command must not dispatch before its durable phase fence")
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("history read errors are not reported as empty history", func(t *testing.T) {
		h := newAPIHarness(t)
		query := "SELECT request_id,phase,target_participant_id,reason,task_id,actor_id"
		h.dbmock.ExpectQuery(query).WithArgs("space", "task").WillReturnError(errors.New("history query failed"))
		_, err := h.sharedHandoffHistory(context.Background(), "space", "task")
		require.ErrorContains(t, err, "history query failed")

		h.dbmock.ExpectQuery(query).WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"request_id"}).AddRow("malformed-row"))
		_, err = h.sharedHandoffHistory(context.Background(), "space", "task")
		require.Error(t, err, "malformed history must fail rather than disappear")
	})
}
