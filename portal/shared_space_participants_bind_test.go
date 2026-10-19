package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"k8s.io/apimachinery/pkg/types"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSharedRuntimeViewerCanReadButCannotWrite(t *testing.T) {
	h := collaborationHarness(t)
	ws := runtimeWorkspace(t, h)
	b := runtimeTestBinding()
	b.WorkspaceUID = string(ws.UID)
	for _, write := range []bool{false, true} {
		viewer := sqlmock.NewRows([]string{"id", "kind", "target", "name", "owner_id", "resource_uid", "role", "state", "version", "agent_id", "registration_state"}).
			AddRow(b.ParticipantID, "companion", "ws-source/assistant", "assistant", "u-x", string(ws.UID), "viewer", "active", 1, "agent", "ready")
		h.dbmock.ExpectQuery("SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants").
			WithArgs("space", "ws-source/assistant").WillReturnRows(viewer)
		_, err := h.participantRuntimeGrant(t.Context(), sharedSpace{ID: "space", DeploymentID: h.cfg.aepDeploymentID, Role: "editor"}, ws.Name, "assistant", string(ws.UID), "u-x", write)
		if write {
			var rejected *sharedError
			require.ErrorAs(t, err, &rejected)
			require.Equal(t, 403, rejected.code)
		} else {
			require.NoError(t, err)
		}
	}
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestSharedSpacesPostgresRuntimeBindRecovery(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable localhost PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.True(t, cfg.Host == "localhost" || cfg.Host == "127.0.0.1" || cfg.Host == "::1", "test PostgreSQL must be reached through localhost")
	schema := fmt.Sprintf("shared_bind_recovery_%d", time.Now().UnixNano())
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
	_, err = db.Exec(`INSERT INTO shared_spaces(id,deployment_id,company_id,project_id,name) VALUES('space','deployment-acme','company','project','协作空间');
INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES('space','u-x','owner',1);
INSERT INTO shared_space_participants(id,space_id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state) VALUES('participant','space','companion','ws-source/assistant','assistant','u-x','bind-uid','editor','active',1,'agent','ready')`)
	require.NoError(t, err)

	h := collaborationHarness(t)
	h.db = db
	h.cfg.aepDeploymentID = "deployment-acme"
	ws := runtimeWorkspace(t, h)
	ws.UID = types.UID("bind-uid")
	require.NoError(t, h.k8s.Update(t.Context(), ws))

	tasks := map[string]map[string]any{
		"task":            {"id": "task", "companyId": "company", "projectId": "project", "status": "todo", "updatedAt": "v1"},
		"terminal":        {"id": "terminal", "companyId": "company", "projectId": "project", "status": "done", "updatedAt": "v1"},
		"wrong-source":    {"id": "wrong-source", "companyId": "company", "projectId": "project", "status": "todo", "updatedAt": "v1"},
		"already-bound":   {"id": "already-bound", "companyId": "company", "projectId": "project", "status": "todo", "updatedAt": "v1"},
		"unknown-command": {"id": "unknown-command", "companyId": "company", "projectId": "project", "status": "todo", "updatedAt": "v1"},
		"unknown":         {"id": "unknown", "companyId": "company", "projectId": "project", "status": "todo", "updatedAt": "v1"},
	}
	var checkouts atomic.Int32
	pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/issues/") {
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			taskID := parts[1]
			task := tasks[taskID]
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/checkout") {
				checkouts.Add(1)
				task["status"] = "in_progress"
				task["assigneeAgentId"] = "agent"
				if taskID == "unknown" {
					http.Error(w, "checkout acknowledgement lost", http.StatusServiceUnavailable)
					return
				}
			}
			writeJSON(w, http.StatusOK, task)
			return
		}
		http.NotFound(w, r)
	}))
	defer pc.Close()
	h.cfg.paperclipBaseURL = pc.URL
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conversationID := 42
		if strings.HasSuffix(r.URL.Path, "/43") {
			conversationID = 999
		}
		writeJSON(w, http.StatusOK, map[string]any{"conversationId": conversationID})
	}))
	defer source.Close()
	h.workspaceChatOverride = source.URL

	// Fail the binding insert once after the external checkout has succeeded.
	_, err = db.Exec(`CREATE FUNCTION fail_one_binding_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected bind failure'; END $$;
CREATE TRIGGER fail_binding_insert BEFORE INSERT ON shared_space_runtime_bindings FOR EACH ROW EXECUTE FUNCTION fail_one_binding_insert();`)
	require.NoError(t, err)
	bind := func(taskID, requestID string, sourceID int64) error {
		body, marshalErr := json.Marshal(map[string]any{"spaceId": "space", "taskId": taskID, "sourceConversationId": sourceID, "requestId": requestID, "title": "交办", "instruction": "处理任务"})
		require.NoError(t, marshalErr)
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
		r.SetPathValue("action", "space-bind")
		return h.sharedRuntimeAgent(httptest.NewRecorder(), r, identity{UserID: "u-x"}, ws, "assistant")
	}
	err = bind("task", "bind-once", 42)
	require.Error(t, err)
	require.EqualValues(t, 1, checkouts.Load())
	_, err = db.Exec(`DROP TRIGGER fail_binding_insert ON shared_space_runtime_bindings`)
	require.NoError(t, err)
	require.NoError(t, bind("task", "bind-once", 42))
	require.NoError(t, bind("task", "bind-once", 42))
	require.EqualValues(t, 1, checkouts.Load(), "retry must reuse the durable claim receipt without checking out twice")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM shared_space_runtime_bindings WHERE space_id='space' AND task_id='task'`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM shared_space_runtime_outbox WHERE binding_id=$1`, collaborationID("space", "task")).Scan(&count))
	require.Equal(t, 1, count, "same-request retry must not enqueue a second start command")

	var rejected *sharedError
	err = bind("terminal", "bind-terminal", 42)
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, 409, rejected.code)
	err = bind("wrong-source", "bind-wrong-source", 43)
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, 409, rejected.code)
	_, err = db.Exec(`INSERT INTO shared_space_runtime_bindings(id,space_id,task_id,workspace_name,workspace_uid,owner_id,companion,source_conversation_id,generation,execution_id,expected_version,bind_hash,assignment_hash,participant_id,participant_version) VALUES($1,'space','already-bound','ws-source','bind-uid','u-x','assistant',42,1,'prior-execution',0,'prior-hash','prior-assignment','participant',1)`, collaborationID("space", "already-bound"))
	require.NoError(t, err)
	err = bind("already-bound", "bind-already-bound", 42)
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, 409, rejected.code)
	require.EqualValues(t, 1, checkouts.Load(), "invalid bind attempts must not call checkout")
	_, err = db.Exec(`INSERT INTO shared_space_commands(space_id,request_id,task_id,actor_id,action,payload_hash,state) VALUES('space','unknown-command-receipt','unknown-command','u-x','claim','unknown-hash','unknown')`)
	require.NoError(t, err)
	err = bind("unknown-command", "bind-command-unknown", 42)
	var pending *sharedError
	require.ErrorAs(t, err, &pending)
	require.Equal(t, 423, pending.code)
	require.EqualValues(t, 1, checkouts.Load(), "unknown command gate must prevent checkout")
	for range 2 {
		err = bind("unknown", "bind-unknown", 42)
		require.ErrorAs(t, err, &pending)
		require.Equal(t, 423, pending.code)
	}
	require.EqualValues(t, 2, checkouts.Load(), "unknown checkout is not replayed")
}

func TestSharedRuntimeAuthorizationRejectsNativeExecutionRuns(t *testing.T) {
	for _, field := range []string{"executionRunId", "checkoutRunId"} {
		t.Run(field, func(t *testing.T) {
			h := collaborationHarness(t)
			ws := runtimeWorkspace(t, h)
			b := runtimeTestBinding()
			b.WorkspaceUID = string(ws.UID)
			runtimeExpectSpace(h, "editor")
			runtimeExpectParticipant(h, b)
			pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/issues/task", r.URL.Path)
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": "in_progress", "assigneeAgentId": "agent", field: "native-run"})
			}))
			defer pc.Close()
			h.cfg.paperclipBaseURL = pc.URL
			err := h.runtimeAuthorize(t.Context(), b, ws, identity{UserID: b.Owner, DeploymentID: h.cfg.aepDeploymentID})
			var rejected *sharedError
			require.ErrorAs(t, err, &rejected)
			require.Equal(t, 409, rejected.code)
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestSharedParticipantRegistrationUnknownReconcilesWithoutSecondPost(t *testing.T) {
	h := newApplyHarness(t)
	s := sharedSpace{ID: "space", DeploymentID: h.cfg.aepDeploymentID, CompanyID: "company"}
	x := sharedParticipant{ID: "participant", Version: 2, Registration: "pending"}
	var posts, gets int
	pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			posts++
			w.WriteHeader(http.StatusGatewayTimeout)
		case http.MethodGet:
			gets++
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			t.Fatalf("unexpected Paperclip request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer pc.Close()
	h.cfg.paperclipBaseURL = pc.URL

	h.dbmock.ExpectExec("UPDATE shared_space_participants SET registration_state='unknown' WHERE id=\\$1").WithArgs(x.ID).WillReturnResult(sqlmock.NewResult(0, 1))
	conn, err := h.db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	err = h.participantRegister(t.Context(), conn, s, &x)
	var rejected *sharedError
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, 423, rejected.code)
	require.Equal(t, "unknown", x.Registration)
	require.Equal(t, 1, posts)

	// The durable unknown state recovers only by looking for the exact marker.
	x.Registration = "unknown"
	err = h.participantRegister(t.Context(), conn, s, &x)
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, 423, rejected.code)
	require.Equal(t, 1, posts)
	require.Equal(t, 1, gets)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestSharedParticipantRegistrationUnknownAcceptsOnlyExactMarker(t *testing.T) {
	h := newApplyHarness(t)
	s := sharedSpace{ID: "space", DeploymentID: h.cfg.aepDeploymentID, CompanyID: "company"}
	x := sharedParticipant{ID: "participant", Version: 2, Registration: "unknown"}
	marker := "aep-shared-participant:" + s.DeploymentID + ":" + x.ID + ":2"
	pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "agent-id", "companyId": "company", "name": marker, "role": "general", "adapterType": "http", "adapterConfig": map[string]any{"url": "http://127.0.0.1:9/unused"}, "runtimeConfig": map[string]any{"aepSharedParticipant": marker, "heartbeat": map[string]any{"enabled": false, "wakeOnDemand": false}}}})
	}))
	defer pc.Close()
	h.cfg.paperclipBaseURL = pc.URL
	conn, err := h.db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, h.participantRegister(t.Context(), conn, s, &x))
	require.Equal(t, "agent-id", x.AgentID)
	require.Equal(t, "ready", x.Registration)
	require.Equal(t, "active", x.State)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestSharedRuntimeNativeRunFenceIsFailClosedOnLookupFailure(t *testing.T) {
	h := collaborationHarness(t)
	ws := runtimeWorkspace(t, h)
	b := runtimeTestBinding()
	b.WorkspaceUID = string(ws.UID)
	runtimeExpectSpace(h, "editor")
	runtimeExpectParticipant(h, b)
	pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer pc.Close()
	h.cfg.paperclipBaseURL = pc.URL
	err := h.runtimeAuthorize(t.Context(), b, ws, identity{UserID: b.Owner, DeploymentID: h.cfg.aepDeploymentID})
	require.Error(t, err)
	require.Error(t, err, "upstream lookup failures must not authorize the binding")
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}
