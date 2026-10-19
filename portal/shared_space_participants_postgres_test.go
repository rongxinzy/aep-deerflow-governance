package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

// TestSharedSpacesPostgresParticipants exercises assignment serialization and
// the external-owner fences against PostgreSQL plus a synthetic Paperclip API.
func TestSharedSpacesPostgresParticipants(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable localhost PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.True(t, cfg.Host == "localhost" || cfg.Host == "127.0.0.1" || cfg.Host == "::1", "test PostgreSQL must be reached through localhost")
	schema := fmt.Sprintf("shared_participants_%d", time.Now().UnixNano())
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		require.NoError(t, cleanupErr)
	})
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(sharedSpaceSchema)
	require.NoError(t, err)
	_, err = db.Exec(sharedRuntimeSchema)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO shared_spaces(id,deployment_id,company_id,project_id,name) VALUES('space','deployment','company','project','协作空间');
INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES('space','u-x','owner',1);
INSERT INTO shared_spaces(id,deployment_id,company_id,project_id,name) VALUES('space-invite','deployment','company','project-invite','邀请空间');
INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES('space-invite','space-owner','owner',1);
INSERT INTO shared_space_participants(id,space_id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state) VALUES
('participant-a','space','companion','ws-agent-a/assistant','Agent A','u-x','uid-ws-agent-a','editor','active',1,'agent-a','ready'),
('participant-b','space','companion','ws-agent-b/assistant','Agent B','u-x','uid-ws-agent-b','editor','active',1,'agent-b','ready')`)
	require.NoError(t, err)

	h := newApplyHarness(t)
	h.db = db
	h.cfg.aepDeploymentID = "deployment"
	for _, ws := range []*dev.PersonalWorkspace{runningWorkspace("ws-agent-a", "u-x", "assistant"), runningWorkspace("ws-agent-b", "u-x", "assistant")} {
		ws.UID = types.UID("uid-" + ws.Name)
		initializeContacts(ws, nil)
		require.NoError(t, h.k8s.Create(t.Context(), ws))
	}
	inviteWorkspace := runningWorkspace("ws-invite-owner", "u-x", "assistant")
	inviteWorkspace.UID = "uid-invite-owner"
	initializeContacts(inviteWorkspace, nil)
	require.NoError(t, h.k8s.Create(t.Context(), inviteWorkspace))

	var mu sync.Mutex
	var registeredAgents []map[string]any
	tasks := map[string]map[string]any{
		"task":           {"id": "task", "companyId": "company", "projectId": "project", "status": "todo", "updatedAt": "v1"},
		"external":       {"id": "external", "companyId": "company", "projectId": "project", "status": "todo", "assigneeAgentId": "outside-agent", "updatedAt": "v1"},
		"native-running": {"id": "native-running", "companyId": "company", "projectId": "project", "status": "todo", "executionRunId": "paperclip-run-1", "updatedAt": "v1"},
		"unknown":        {"id": "unknown", "companyId": "company", "projectId": "project", "status": "todo", "updatedAt": "v1"},
	}
	var checkouts atomic.Int32
	pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/companies/company/agents" {
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodPost {
				var agent map[string]any
				if json.NewDecoder(r.Body).Decode(&agent) != nil {
					http.Error(w, "bad agent", http.StatusBadRequest)
					return
				}
				agent["companyId"] = "company"
				agent["id"] = fmt.Sprintf("registered-agent-%d", len(registeredAgents)+1)
				registeredAgents = append(registeredAgents, agent)
				writeJSON(w, http.StatusCreated, agent)
				return
			}
			writeJSON(w, http.StatusOK, registeredAgents)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 2 || parts[0] != "issues" {
			http.NotFound(w, r)
			return
		}
		id := parts[1]
		mu.Lock()
		defer mu.Unlock()
		task, ok := tasks[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "checkout" {
			checkouts.Add(1)
			var input map[string]any
			if json.NewDecoder(r.Body).Decode(&input) != nil || input["agentId"] == nil {
				http.Error(w, "bad checkout", http.StatusBadRequest)
				return
			}
			task["assigneeAgentId"] = input["agentId"]
			task["status"] = "in_progress"
			task["updatedAt"] = fmt.Sprintf("v%d", checkouts.Load()+1)
			if id == "unknown" {
				http.Error(w, "acknowledgement lost", http.StatusServiceUnavailable)
				return
			}
		}
		writeJSON(w, http.StatusOK, task)
	}))
	defer pc.Close()
	h.cfg.paperclipBaseURL = pc.URL
	h.cfg.paperclipToken = "synthetic-only"

	token := sharedHash([]any{"task", nil, nil, "todo", "v1", 0})
	type outcome struct {
		status int
		body   map[string]any
	}
	claim := func(actor, participant, request, assignmentToken, taskID string) outcome {
		t.Helper()
		body, marshalErr := json.Marshal(sharedAssignmentInput{RequestID: request, ExpectedAssignmentToken: assignmentToken, ParticipantID: participant})
		require.NoError(t, marshalErr)
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		r.SetPathValue("space", "space")
		r.SetPathValue("task", taskID)
		r.SetPathValue("assignmentAction", "claim")
		w := httptest.NewRecorder()
		err := h.sharedAssignmentChange(w, r, identity{UserID: actor, DeploymentID: "deployment"})
		if err != nil {
			sharedWriteError(w, err)
		}
		var response map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response), w.Body.String())
		return outcome{status: w.Code, body: response}
	}

	results := make(chan outcome, 2)
	go func() { results <- claim("u-x", "participant-a", "claim-a", token, "task") }()
	go func() { results <- claim("u-x", "participant-b", "claim-b", token, "task") }()
	a, b := <-results, <-results
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, []int{a.status, b.status})
	require.EqualValues(t, 1, checkouts.Load(), "only one competing Agent can claim the task")
	mu.Lock()
	winner := tasks["task"]["assigneeAgentId"].(string)
	mu.Unlock()
	wrongParticipant := "participant-a"
	if winner == "agent-a" {
		wrongParticipant = "participant-b"
	}
	currentToken := sharedHash([]any{"task", winner, nil, "in_progress", tasks["task"]["updatedAt"], 1})
	wrong := claim("u-x", wrongParticipant, "wrong-agent", currentToken, "task")
	require.Equal(t, http.StatusConflict, wrong.status, wrong.body)
	stale := claim("u-x", wrongParticipant, "stale-token", token, "task")
	require.Equal(t, http.StatusConflict, stale.status, stale.body)
	require.EqualValues(t, 1, checkouts.Load())

	// Native external ownership and Paperclip execution runs are never taken over.
	externalToken := sharedHash([]any{"external", "outside-agent", nil, "todo", "v1", 0})
	external := claim("u-x", "participant-a", "external-claim", externalToken, "external")
	require.Equal(t, http.StatusConflict, external.status, external.body)
	nativeToken := sharedHash([]any{"native-running", nil, nil, "todo", "v1", 0})
	native := claim("u-x", "participant-a", "native-claim", nativeToken, "native-running")
	require.Equal(t, http.StatusConflict, native.status, native.body)
	require.EqualValues(t, 1, checkouts.Load(), "fenced claims must not write to Paperclip")

	// A lost checkout acknowledgement is durable as unknown and cannot be replayed.
	unknownToken := sharedHash([]any{"unknown", nil, nil, "todo", "v1", 0})
	first := claim("u-x", "participant-a", "unknown-claim", unknownToken, "unknown")
	require.Equal(t, http.StatusLocked, first.status, first.body)
	second := claim("u-x", "participant-a", "unknown-claim", unknownToken, "unknown")
	require.Equal(t, http.StatusLocked, second.status, second.body)
	require.EqualValues(t, 2, checkouts.Load(), "unknown outcomes are never blindly resent")
	var state string
	require.NoError(t, db.QueryRow(`SELECT state FROM shared_space_commands WHERE space_id='space' AND request_id='unknown-claim'`).Scan(&state))
	require.Equal(t, "unknown", state)

	// A resource owner can confirm an invite from the invitation inbox without
	// receiving human task access. Revocation and rejoin increment the grant epoch.
	participantCall := func(actor, space, action, id string, expected int, requestID string, body any, accept bool) outcome {
		t.Helper()
		raw, marshalErr := json.Marshal(body)
		require.NoError(t, marshalErr)
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
		r.SetPathValue("space", space)
		r.SetPathValue("participant", id)
		if accept {
			r.SetPathValue("invitationAction", action)
			w := httptest.NewRecorder()
			err := h.sharedAcceptInvitation(w, r, identity{UserID: actor, DeploymentID: "deployment"})
			if err != nil {
				sharedWriteError(w, err)
			}
			var response map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response), w.Body.String())
			return outcome{status: w.Code, body: response}
		}
		r.SetPathValue("participantAction", action)
		w := httptest.NewRecorder()
		err := h.sharedParticipantChange(w, r, identity{UserID: actor, DeploymentID: "deployment"})
		if err != nil {
			sharedWriteError(w, err)
		}
		var response map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response), w.Body.String())
		return outcome{status: w.Code, body: response}
	}
	invite := participantCall("space-owner", "space-invite", "", "", 0, "invite-one", map[string]any{"kind": "companion", "target": "ws-invite-owner/assistant", "role": "editor", "requestId": "invite-one"}, false)
	require.Equal(t, http.StatusOK, invite.status, invite.body)
	participant := invite.body["participant"].(map[string]any)
	participantID := participant["id"].(string)
	require.Equal(t, "pending", participant["state"])
	var humanMemberships int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM shared_space_members WHERE space_id='space-invite' AND user_id='u-x'`).Scan(&humanMemberships))
	require.Zero(t, humanMemberships)
	accepted := participantCall("u-x", "space-invite", "accept", participantID, 1, "accept-one", map[string]any{"requestId": "accept-one", "expectedVersion": 1}, true)
	require.Equal(t, http.StatusOK, accepted.status, accepted.body)
	participant = accepted.body["participant"].(map[string]any)
	require.Equal(t, "active", participant["state"])
	var firstAgentID string
	require.NoError(t, db.QueryRow(`SELECT agent_id FROM shared_space_participants WHERE id=$1`, participantID).Scan(&firstAgentID))
	require.NotEmpty(t, firstAgentID)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM shared_space_members WHERE space_id='space-invite' AND user_id='u-x'`).Scan(&humanMemberships))
	require.Zero(t, humanMemberships, "confirming an Agent invitation must not grant human space membership")

	revoked := participantCall("space-owner", "space-invite", "revoke", participantID, 1, "revoke-one", map[string]any{"requestId": "revoke-one", "expectedVersion": 1}, false)
	require.Equal(t, http.StatusOK, revoked.status, revoked.body)
	require.Equal(t, "revoked", revoked.body["participant"].(map[string]any)["state"])
	require.EqualValues(t, 2, revoked.body["participant"].(map[string]any)["version"])
	reinvited := participantCall("space-owner", "space-invite", "", "", 0, "invite-two", map[string]any{"kind": "companion", "target": "ws-invite-owner/assistant", "role": "editor", "requestId": "invite-two"}, false)
	require.Equal(t, http.StatusOK, reinvited.status, reinvited.body)
	require.EqualValues(t, 3, reinvited.body["participant"].(map[string]any)["version"])
	rejoined := participantCall("u-x", "space-invite", "accept", participantID, 3, "accept-two", map[string]any{"requestId": "accept-two", "expectedVersion": 3}, true)
	require.Equal(t, http.StatusOK, rejoined.status, rejoined.body)
	participant = rejoined.body["participant"].(map[string]any)
	require.EqualValues(t, 3, participant["version"])
	var rejoinedAgentID string
	require.NoError(t, db.QueryRow(`SELECT agent_id FROM shared_space_participants WHERE id=$1`, participantID).Scan(&rejoinedAgentID))
	require.NotEqual(t, firstAgentID, rejoinedAgentID, "rejoin registers a new native Agent marker")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM shared_space_members WHERE space_id='space-invite' AND user_id='u-x'`).Scan(&humanMemberships))
	require.Zero(t, humanMemberships)

	oldBinding := runtimeTestBinding()
	oldBinding.SpaceID = "space-invite"
	oldBinding.TaskID = "task"
	oldBinding.Workspace = inviteWorkspace.Name
	oldBinding.WorkspaceUID = string(inviteWorkspace.UID)
	oldBinding.Owner = "u-x"
	oldBinding.Companion = "assistant"
	oldBinding.ParticipantID = participantID
	oldBinding.ParticipantVersion = 1
	oldBinding.AssignmentHash = runtimeAssignment(map[string]any{"assigneeAgentId": rejoinedAgentID})
	staleErr := h.runtimeAuthorize(t.Context(), oldBinding, inviteWorkspace, identity{UserID: "u-x", DeploymentID: "deployment"})
	var staleGrant *sharedError
	require.ErrorAs(t, staleErr, &staleGrant)
	require.Equal(t, http.StatusForbidden, staleGrant.code, "old binding versions stay revoked after rejoin")
	_, err = h.sharedLoad(t.Context(), db, "space-invite", identity{UserID: "u-x", DeploymentID: "deployment"})
	require.Error(t, err, "Agent owner confirmation does not create human task access")
}
