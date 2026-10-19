package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestSharedSpacesPostgresParticipantRuntimeRecovery(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable localhost PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.True(t, cfg.Host == "localhost" || cfg.Host == "127.0.0.1" || cfg.Host == "::1", "test PostgreSQL must be reached through localhost")
	schema := fmt.Sprintf("shared_participant_recovery_%d", time.Now().UnixNano())
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
	_, err = db.Exec(`INSERT INTO shared_spaces(id,deployment_id,company_id,project_id,name) VALUES('space','deployment-acme','company','project','隔离验收空间');
INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES('space','space-manager','owner',1);
INSERT INTO shared_space_participants(id,space_id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state) VALUES
('participant-editor','space','companion','ws-editor/assistant','Editor Agent','u-x','uid-editor','editor','active',1,'agent-editor','ready'),
('participant-viewer','space','companion','ws-viewer/assistant','Viewer Agent','u-x','uid-viewer','viewer','active',1,'agent-viewer','ready'),
('participant-pending','space','companion','ws-pending/assistant','Pending Agent','u-x','uid-pending','editor','pending',1,'','pending')`)
	require.NoError(t, err)

	h := newApplyHarness(t)
	h.db = db
	h.cfg.aepDeploymentID = "deployment-acme"
	workspaces := map[string]*dev.PersonalWorkspace{}
	for _, spec := range []struct{ name, uid string }{{"ws-editor", "uid-editor"}, {"ws-viewer", "uid-viewer"}, {"ws-pending", "uid-pending"}} {
		ws := runningWorkspace(spec.name, "u-x", "assistant")
		ws.UID = types.UID(spec.uid)
		initializeContacts(ws, nil)
		require.NoError(t, h.k8s.Create(t.Context(), ws))
		workspaces[spec.name] = ws
	}
	var humanMemberships int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM shared_space_members WHERE space_id='space' AND user_id='u-x'`).Scan(&humanMemberships))
	require.Zero(t, humanMemberships, "Agent owner grant must authorize runtime access without human space membership")

	tasks := map[string]map[string]any{
		"task-editor": {"id": "task-editor", "companyId": "company", "projectId": "project", "status": "todo", "updatedAt": "v1"},
		"task-viewer": {"id": "task-viewer", "companyId": "company", "projectId": "project", "status": "todo", "updatedAt": "v1"},
	}
	var mu sync.Mutex
	var checkoutAgents []string
	var registeredAgents []map[string]any
	postCount, agentListGetCount := 0, 0
	dropNextRegistrationAck := false
	pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/companies/company/agents" {
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodPost {
				postCount++
				var a map[string]any
				if json.NewDecoder(r.Body).Decode(&a) != nil {
					http.Error(w, "invalid agent payload", http.StatusBadRequest)
					return
				}
				a["companyId"] = "company"
				a["id"] = "agent-registered-pending"
				registeredAgents = append(registeredAgents, a)
				if dropNextRegistrationAck {
					dropNextRegistrationAck = false
					http.Error(w, "registration ack lost", http.StatusServiceUnavailable)
					return
				}
				writeJSON(w, http.StatusCreated, a)
				return
			}
			agentListGetCount++
			writeJSON(w, http.StatusOK, registeredAgents)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/companies/company/issues" {
			mu.Lock()
			defer mu.Unlock()
			writeJSON(w, http.StatusOK, []map[string]any{tasks["task-editor"], tasks["task-viewer"]})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/issues/") {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			mu.Lock()
			defer mu.Unlock()
			task, ok := tasks[parts[1]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			if r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "checkout" {
				var input map[string]any
				if json.NewDecoder(r.Body).Decode(&input) != nil {
					http.Error(w, "invalid checkout", http.StatusBadRequest)
					return
				}
				agentID, _ := input["agentId"].(string)
				checkoutAgents = append(checkoutAgents, agentID)
				task["assigneeAgentId"] = agentID
				task["status"] = "in_progress"
				task["updatedAt"] = "claimed"
			}
			writeJSON(w, http.StatusOK, task)
			return
		}
		http.NotFound(w, r)
	}))
	defer pc.Close()
	h.cfg.paperclipBaseURL = pc.URL
	h.cfg.paperclipToken = "synthetic-only"

	discover := func(ws *dev.PersonalWorkspace) map[string]any {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.SetPathValue("action", "space-discover")
		w := httptest.NewRecorder()
		require.NoError(t, h.sharedRuntimeAgent(w, r, identity{UserID: "u-x"}, ws, "assistant"))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var response map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
		spaces := response["spaces"].([]any)
		require.Len(t, spaces, 1)
		return spaces[0].(map[string]any)
	}
	claim := func(ws *dev.PersonalWorkspace, body string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.SetPathValue("action", "space-claim")
		w := httptest.NewRecorder()
		err := h.sharedRuntimeAgent(w, r, identity{UserID: "u-x"}, ws, "assistant")
		if err != nil {
			sharedWriteError(w, err)
		}
		var response map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response), w.Body.String())
		return w.Code, response
	}
	editorDiscover := discover(workspaces["ws-editor"])
	editorTasks := editorDiscover["tasks"].([]any)
	require.Len(t, editorTasks, 2)
	var editorToken string
	for _, row := range editorTasks {
		task := row.(map[string]any)
		if task["id"] == "task-editor" {
			editorToken = task["assignment"].(map[string]any)["token"].(string)
		}
	}
	require.NotEmpty(t, editorToken)
	// A caller cannot select a grant by supplying participantId. Reject the
	// attempted override, then claim with the trusted workspace identity only.
	status, rejected := claim(workspaces["ws-editor"], `{"spaceId":"space","taskId":"task-editor","requestId":"forged-participant","expectedAssignmentToken":"`+editorToken+`","participantId":"participant-viewer"}`)
	require.Equal(t, http.StatusBadRequest, status, rejected)
	mu.Lock()
	require.Empty(t, checkoutAgents, "a forged participantId must not reach Paperclip")
	mu.Unlock()
	status, receipt := claim(workspaces["ws-editor"], `{"spaceId":"space","taskId":"task-editor","requestId":"claim-from-discovery","expectedAssignmentToken":"`+editorToken+`"}`)
	require.Equal(t, http.StatusOK, status, receipt)
	require.Equal(t, "agent-editor", receipt["task"].(map[string]any)["assigneeAgentId"])
	require.Equal(t, "participant-editor", receipt["assignment"].(map[string]any)["participant"].(map[string]any)["id"])
	mu.Lock()
	require.Equal(t, []string{"agent-editor"}, checkoutAgents, "only the authenticated Agent's native id may be sent to checkout")
	mu.Unlock()

	viewerDiscover := discover(workspaces["ws-viewer"])
	viewerTasks := viewerDiscover["tasks"].([]any)
	var viewerToken string
	for _, row := range viewerTasks {
		task := row.(map[string]any)
		if task["id"] == "task-viewer" {
			viewerToken = task["assignment"].(map[string]any)["token"].(string)
		}
	}
	require.NotEmpty(t, viewerToken)
	status, denied := claim(workspaces["ws-viewer"], `{"spaceId":"space","taskId":"task-viewer","requestId":"viewer-claim","expectedAssignmentToken":"`+viewerToken+`"}`)
	require.Equal(t, http.StatusForbidden, status, denied)
	mu.Lock()
	require.Equal(t, []string{"agent-editor"}, checkoutAgents, "viewer claims must not reach Paperclip")
	mu.Unlock()

	// A pending acceptance persists its intent before POST. The fake upstream
	// creates the Agent but drops the acknowledgement; replay must reconcile via
	// the exact marker with GET and must never POST a second time.
	mu.Lock()
	dropNextRegistrationAck = true
	mu.Unlock()
	acceptBody := `{"requestId":"accept-lost-ack","expectedVersion":1}`
	accept := func() (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(acceptBody))
		r.SetPathValue("space", "space")
		r.SetPathValue("participant", "participant-pending")
		r.SetPathValue("invitationAction", "accept")
		w := httptest.NewRecorder()
		err := h.sharedAcceptInvitation(w, r, identity{UserID: "u-x"})
		if err != nil {
			sharedWriteError(w, err)
		}
		var response map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response), w.Body.String())
		return w.Code, response
	}
	status, unknown := accept()
	require.Equal(t, http.StatusLocked, status, unknown)
	var state, registration string
	require.NoError(t, db.QueryRow(`SELECT state,registration_state FROM shared_space_participants WHERE id='participant-pending'`).Scan(&state, &registration))
	require.Equal(t, "pending", state)
	require.Equal(t, "unknown", registration)
	var commandState string
	require.NoError(t, db.QueryRow(`SELECT state FROM shared_space_commands WHERE space_id='space' AND request_id='accept-lost-ack'`).Scan(&commandState))
	require.Equal(t, "unknown", commandState)
	status, recovered := accept()
	require.Equal(t, http.StatusOK, status, recovered)
	require.Equal(t, "active", recovered["participant"].(map[string]any)["state"])
	require.NotContains(t, recovered["participant"], "registrationState", "registration internals stay out of the public participant DTO")
	var agentID string
	require.NoError(t, db.QueryRow(`SELECT state,registration_state,agent_id FROM shared_space_participants WHERE id='participant-pending'`).Scan(&state, &registration, &agentID))
	require.Equal(t, "active", state)
	require.Equal(t, "ready", registration)
	require.Equal(t, "agent-registered-pending", agentID)
	mu.Lock()
	require.Equal(t, 1, postCount)
	require.Equal(t, 1, agentListGetCount)
	mu.Unlock()
	require.NoError(t, db.QueryRow(`SELECT state FROM shared_space_commands WHERE space_id='space' AND request_id='accept-lost-ack'`).Scan(&commandState))
	require.Equal(t, "applied", commandState)
}
