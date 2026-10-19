package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
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
