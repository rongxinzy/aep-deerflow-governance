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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// This acceptance uses a disposable local PostgreSQL schema and synthetic
// Paperclip/companion endpoints. It verifies write ordering and the durable
// handoff boundary without contacting production services.
func TestSharedSpacesPostgresHandoffRecoveryAndHistory(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable localhost PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"localhost", "127.0.0.1", "::1"}, cfg.Host, "test PostgreSQL must be reached through localhost")
	schema := fmt.Sprintf("shared_handoff_%d", time.Now().UnixNano())
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
	_, err = db.Exec(`INSERT INTO shared_spaces(id,deployment_id,company_id,project_id,name) VALUES('space','deployment-acme','company','project','handoff-test');
INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES('space','u-x','owner',1);
INSERT INTO shared_space_participants(id,space_id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state) VALUES
('participant-a','space','companion','ws-a/assistant','Agent A','u-x','uid-ws-a','editor','active',1,'agent-a','ready'),
('participant-b','space','companion','ws-b/assistant','Agent B','u-x','uid-ws-b','editor','active',1,'agent-b','ready');
INSERT INTO shared_space_runtime_bindings(id,space_id,task_id,workspace_name,workspace_uid,owner_id,companion,source_conversation_id,pi_task_id,generation,execution_id,expected_version,bind_hash,assignment_hash,participant_id,participant_version,binding_state,assignment_epoch)
VALUES('binding-a','space','task','ws-a','uid-ws-a','u-x','assistant',731,'pi-task-a',1,'execution-a',4,'bind-hash','assignment-hash','participant-a',1,'active',9);
INSERT INTO shared_space_runtime_bindings(id,space_id,task_id,workspace_name,workspace_uid,owner_id,companion,source_conversation_id,pi_task_id,generation,execution_id,expected_version,bind_hash,assignment_hash,participant_id,participant_version,binding_state,assignment_epoch)
VALUES('binding-source-change','space','task-source-change','ws-a','uid-ws-a','u-x','assistant',731,'pi-source-change',1,'execution-source-change',4,'bind-hash-2','assignment-hash-2','participant-a',1,'active',9),
('binding-target-change','space','task-target-change','ws-a','uid-ws-a','u-x','assistant',731,'pi-target-change',1,'execution-target-change',4,'bind-hash-3','assignment-hash-3','participant-a',1,'active',9);
INSERT INTO shared_space_runtime_outbox(command_id,binding_id,action,payload,payload_hash,state) VALUES('late-submit','binding-a','submit','{}','hash','pending')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO shared_space_results(id,space_id,task_id,version,title,document_key,document_id,revision_id,body_hash,submitted_by) VALUES('result-from-a','space','task',1,'A的成果','review-doc','doc-a','rev-a','bodyhash','agent-a');
INSERT INTO shared_space_reviews(id,result_id,decision,reason,actor_id) VALUES('review-from-a','result-from-a','request_changes','补充证据','u-x');
INSERT INTO shared_space_result_bindings(result_id,binding_id) VALUES('result-from-a','binding-a')`)
	require.NoError(t, err)

	h := newApplyHarness(t)
	h.db = db
	h.cfg.paperclipBaseURL = ""
	h.cfg.paperclipToken = "synthetic-paperclip"
	h.cfg.namespace = "digital-employees"
	pcTasks := map[string]map[string]any{
		"task":               {"id": "task", "companyId": "company", "projectId": "project", "status": "in_progress", "assigneeAgentId": "agent-a", "updatedAt": "v1"},
		"task-unknown":       {"id": "task-unknown", "companyId": "company", "projectId": "project", "status": "todo", "assigneeAgentId": "agent-a", "updatedAt": "v1"},
		"task-race":          {"id": "task-race", "companyId": "company", "projectId": "project", "status": "todo", "assigneeAgentId": "agent-a", "updatedAt": "v1"},
		"task-release":       {"id": "task-release", "companyId": "company", "projectId": "project", "status": "todo", "assigneeAgentId": "agent-a", "updatedAt": "v1"},
		"task-source-change": {"id": "task-source-change", "companyId": "company", "projectId": "project", "status": "in_progress", "assigneeAgentId": "agent-a", "updatedAt": "v1"},
		"task-target-change": {"id": "task-target-change", "companyId": "company", "projectId": "project", "status": "in_progress", "assigneeAgentId": "agent-a", "updatedAt": "v1"},
	}
	var pcMu sync.Mutex
	patches := 0
	checkouts := 0
	patchesByTask := map[string]int{}
	dropPatchAck := true
	pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pcMu.Lock()
		defer pcMu.Unlock()
		taskID := partsID(r.URL.Path)
		task, ok := pcTasks[taskID]
		if !ok || !strings.HasPrefix(r.URL.Path, "/issues/") {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/checkout") {
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				http.Error(w, "bad checkout", 400)
				return
			}
			checkouts++
			task["assigneeAgentId"] = body["agentId"]
			task["assigneeUserId"] = nil
			task["status"] = "in_progress"
			task["updatedAt"] = fmt.Sprintf("checkout-%d", checkouts)
			_ = json.NewEncoder(w).Encode(task)
			return
		}
		if r.Method == http.MethodPatch {
			patches++
			patchesByTask[taskID]++
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				http.Error(w, "bad patch", 400)
				return
			}
			if taskID == "task-unknown" && dropPatchAck {
				dropPatchAck = false
				http.Error(w, "assignment acknowledgement unknown", http.StatusServiceUnavailable)
				return
			}
			task["assigneeAgentId"] = body["assigneeAgentId"]
			task["assigneeUserId"] = nil
			task["status"] = body["status"]
			task["updatedAt"] = fmt.Sprintf("v%d", patches+1)
		}
		_ = json.NewEncoder(w).Encode(task)
	}))
	defer pc.Close()
	h.cfg.paperclipBaseURL, h.cfg.paperclipToken = pc.URL, "synthetic-paperclip"

	piCalls := map[string]int{"POST /assistant/internal/shared-space/stop": 0, "GET /assistant/internal/shared-space/receipt": 0}
	var piMu sync.Mutex
	var stopReceipt map[string]any
	stopCounts := map[string]int{}
	stopReceipts := map[string]map[string]any{}
	receiptCounts := map[string]int{}
	pi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		piMu.Lock()
		defer piMu.Unlock()
		key := r.Method + " " + r.URL.Path
		piCalls[key]++
		if r.URL.Path == "/assistant/internal/shared-space/stop" {
			var request struct {
				CommandID   string         `json:"commandId"`
				PayloadHash string         `json:"payloadHash"`
				Payload     map[string]any `json:"payload"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				http.Error(w, "bad stop", 400)
				return
			}
			stopCounts[request.CommandID]++
			stopReceipt = map[string]any{"commandId": request.CommandID, "payloadHash": request.PayloadHash, "state": "applied", "bindingId": request.Payload["bindingId"], "executionId": request.Payload["executionId"], "assignmentEpoch": request.Payload["assignmentEpoch"], "fenced": true, "schedulerStopped": true}
			stopReceipts[request.CommandID] = stopReceipt
			if stopCounts[request.CommandID] == 1 {
				http.Error(w, "acknowledgement lost", http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, 200, stopReceipt)
			return
		}
		if r.URL.Path == "/assistant/internal/shared-space/receipt" {
			commandID := r.URL.Query().Get("commandId")
			receiptCounts[commandID]++
			if stopReceipts[commandID] == nil {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, 200, stopReceipts[commandID])
			return
		}
		http.NotFound(w, r)
	}))
	defer pi.Close()
	h.workspaceChatOverride = pi.URL
	for _, name := range []string{"ws-a", "ws-b"} {
		ws := runningWorkspace(name, "u-x", "assistant")
		ws.UID = types.UID("uid-" + name)
		initializeContacts(ws, nil)
		require.NoError(t, h.k8s.Create(t.Context(), ws))
		require.NoError(t, h.k8s.Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: workspaceMemkeySecretName(name, "assistant"), Namespace: h.cfg.namespace}, Data: map[string][]byte{"WORKSPACE_INTERNAL_TOKEN": []byte("synthetic-only")}}))
	}

	// Keep an exact stop receipt and only expose it to the GET recovery path.
	identity := identity{UserID: "u-x", DeploymentID: "deployment-acme"}
	assignment, err := h.sharedAssignmentDTO(t.Context(), sharedSpace{ID: "space", DeploymentID: "deployment-acme", CompanyID: "company", ProjectID: "project", Role: "owner"}, pcTasks["task"], identity)
	require.NoError(t, err)
	handoff := func(taskID, requestID, target, token string) (int, map[string]any) {
		t.Helper()
		var reader strings.Reader
		var participant any
		if target != "" {
			participant = target
		}
		body, marshalErr := json.Marshal(map[string]any{"requestId": requestID, "expectedAssignmentToken": token, "participantId": participant, "reason": "交接验收"})
		require.NoError(t, marshalErr)
		reader = *strings.NewReader(string(body))
		req := httptest.NewRequest(http.MethodPost, "/", &reader)
		req.SetPathValue("space", "space")
		req.SetPathValue("task", taskID)
		w := httptest.NewRecorder()
		err := h.sharedHandoffTask(w, req, identity)
		if err != nil {
			sharedWriteError(w, err)
		}
		var out map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
		return w.Code, out
	}
	code, first := handoff("task", "handoff-a", "participant-b", assignment.Token)
	require.Equal(t, http.StatusAccepted, code, first)
	require.Equal(t, "unknown", first["state"])
	require.Equal(t, "stopping", first["phase"])
	pcMu.Lock()
	require.Zero(t, patches, "Paperclip assignment must not change before stop receipt is confirmed")
	pcMu.Unlock()
	code, recovered := handoff("task", "handoff-a", "participant-b", assignment.Token)
	require.Equal(t, http.StatusOK, code, recovered)
	require.Equal(t, "applied", recovered["state"])
	require.Equal(t, "applied", recovered["phase"])
	// A retry after exact stop receipt dispatches assignment patch once and
	// commits applied after GET readback; the fake below records both writes.
	piMu.Lock()
	require.Equal(t, 1, stopCounts["handoff-stop:space:handoff-a"])
	require.Equal(t, 1, receiptCounts["handoff-stop:space:handoff-a"])
	piMu.Unlock()
	pcMu.Lock()
	require.Equal(t, 1, patches)
	require.Equal(t, "agent-b", pcTasks["task"]["assigneeAgentId"])
	pcMu.Unlock()

	// A stop that is durably confirmed cannot overwrite a Paperclip owner that
	// changed while the receipt was unknown.
	sourceChangedAssignment, err := h.sharedAssignmentDTO(t.Context(), sharedSpace{ID: "space", DeploymentID: "deployment-acme", CompanyID: "company", ProjectID: "project", Role: "owner"}, pcTasks["task-source-change"], identity)
	require.NoError(t, err)
	code, sourcePending := handoff("task-source-change", "handoff-source-change", "participant-b", sourceChangedAssignment.Token)
	require.Equal(t, http.StatusAccepted, code, sourcePending)
	pcMu.Lock()
	pcTasks["task-source-change"]["assigneeAgentId"] = "external-owner"
	pcTasks["task-source-change"]["status"] = "todo"
	pcTasks["task-source-change"]["updatedAt"] = "externally-reassigned"
	pcMu.Unlock()
	code, sourceStillPending := handoff("task-source-change", "handoff-source-change", "participant-b", sourceChangedAssignment.Token)
	require.Equal(t, http.StatusAccepted, code, sourceStillPending)
	require.Equal(t, "assignment_unknown", sourceStillPending["phase"])
	pcMu.Lock()
	require.Equal(t, "external-owner", pcTasks["task-source-change"]["assigneeAgentId"])
	require.Zero(t, patchesByTask["task-source-change"], "handoff must not overwrite a new external assignee")
	pcMu.Unlock()

	// Revoking/changing the target grant during an unknown stop likewise blocks
	// finalization, even though the durable Pi receipt has since appeared.
	targetChangedAssignment, err := h.sharedAssignmentDTO(t.Context(), sharedSpace{ID: "space", DeploymentID: "deployment-acme", CompanyID: "company", ProjectID: "project", Role: "owner"}, pcTasks["task-target-change"], identity)
	require.NoError(t, err)
	code, targetPending := handoff("task-target-change", "handoff-target-change", "participant-b", targetChangedAssignment.Token)
	require.Equal(t, http.StatusAccepted, code, targetPending)
	_, err = db.Exec(`UPDATE shared_space_participants SET version=version+1 WHERE id='participant-b'`)
	require.NoError(t, err)
	code, targetStillPending := handoff("task-target-change", "handoff-target-change", "participant-b", targetChangedAssignment.Token)
	require.Equal(t, http.StatusAccepted, code, targetStillPending)
	pcMu.Lock()
	require.Equal(t, "agent-a", pcTasks["task-target-change"]["assigneeAgentId"])
	require.Zero(t, patchesByTask["task-target-change"], "a revoked/version-changed grant cannot receive the task")
	pcMu.Unlock()
	var oldState, outboxState string
	require.NoError(t, db.QueryRow(`SELECT binding_state FROM shared_space_runtime_bindings WHERE id='binding-a'`).Scan(&oldState))
	require.NoError(t, db.QueryRow(`SELECT state FROM shared_space_runtime_outbox WHERE command_id='late-submit'`).Scan(&outboxState))
	require.Equal(t, "retired", oldState)
	require.Equal(t, "blocked", outboxState)
	var wsA dev.PersonalWorkspace
	require.NoError(t, h.k8s.Get(t.Context(), types.NamespacedName{Namespace: h.cfg.namespace, Name: "ws-a"}, &wsA))
	oldBinding := runtimeBinding{ID: "binding-a", SpaceID: "space", TaskID: "task", Workspace: "ws-a", WorkspaceUID: "uid-ws-a", Owner: "u-x", Companion: "assistant", ParticipantID: "participant-a", ParticipantVersion: 1, BindingState: oldState, AssignmentEpoch: 9}
	var stale *sharedError
	require.ErrorAs(t, h.runtimeAuthorize(t.Context(), oldBinding, &wsA, identity), &stale)
	require.Equal(t, http.StatusConflict, stale.code, "late callback from the retired binding must be rejected by the task epoch fence")

	// A→B→A keeps the retired binding as history and creates a new handoff
	// epoch; the old source conversation and binding identity remain untouched.
	assignment, err = h.sharedAssignmentDTO(t.Context(), sharedSpace{ID: "space", DeploymentID: "deployment-acme", CompanyID: "company", ProjectID: "project", Role: "owner"}, pcTasks["task"], identity)
	require.NoError(t, err)
	code, back := handoff("task", "handoff-b", "participant-a", assignment.Token)
	require.Equal(t, http.StatusOK, code, back)
	require.Equal(t, "applied", back["state"])
	pcMu.Lock()
	require.Equal(t, "agent-a", pcTasks["task"]["assigneeAgentId"])
	require.Equal(t, 2, patches)
	pcMu.Unlock()
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM shared_space_handoffs WHERE space_id='space' AND task_id='task'`).Scan(&count))
	require.Equal(t, 2, count)
	var sourceConversation int64
	require.NoError(t, db.QueryRow(`SELECT source_conversation_id FROM shared_space_runtime_bindings WHERE id='binding-a'`).Scan(&sourceConversation))
	require.EqualValues(t, 731, sourceConversation, "handoff must not rewrite the source conversation of the retired binding")
	var resultOrigin string
	require.NoError(t, db.QueryRow(`SELECT binding_id FROM shared_space_result_bindings WHERE result_id='result-from-a'`).Scan(&resultOrigin))
	require.Equal(t, "binding-a", resultOrigin, "a result remains routed to the execution that created it after handoff")

	// For a direct assignment without a running Pi binding, a lost Paperclip
	// acknowledgement is recovered by GET only. The pending patch is never
	// replayed; once an external readback confirms it, the request completes.
	unknownAssignment, err := h.sharedAssignmentDTO(t.Context(), sharedSpace{ID: "space", DeploymentID: "deployment-acme", CompanyID: "company", ProjectID: "project", Role: "owner"}, pcTasks["task-unknown"], identity)
	require.NoError(t, err)
	code, unknown := handoff("task-unknown", "handoff-patch-unknown", "participant-b", unknownAssignment.Token)
	require.Equal(t, http.StatusAccepted, code, unknown)
	require.Equal(t, "assignment_unknown", unknown["phase"])
	pcMu.Lock()
	require.Equal(t, 1, patchesByTask["task-unknown"])
	pcMu.Unlock()

	releaseAssignment, err := h.sharedAssignmentDTO(t.Context(), sharedSpace{ID: "space", DeploymentID: "deployment-acme", CompanyID: "company", ProjectID: "project", Role: "owner"}, pcTasks["task-release"], identity)
	require.NoError(t, err)
	code, released := handoff("task-release", "handoff-release", "", releaseAssignment.Token)
	require.Equal(t, http.StatusOK, code, released)
	require.Equal(t, "applied", released["state"])
	pcMu.Lock()
	require.Nil(t, pcTasks["task-release"]["assigneeAgentId"], "handoff without a target returns the task to the unassigned queue")
	pcMu.Unlock()
	code, stillUnknown := handoff("task-unknown", "handoff-patch-unknown", "participant-b", unknownAssignment.Token)
	require.Equal(t, http.StatusAccepted, code, stillUnknown)
	pcMu.Lock()
	require.Equal(t, 1, patchesByTask["task-unknown"], "unknown assignment must be read back, never patched a second time")
	pcTasks["task-unknown"]["assigneeAgentId"] = "agent-b"
	pcTasks["task-unknown"]["status"] = "todo"
	pcTasks["task-unknown"]["updatedAt"] = "eventually-confirmed"
	pcMu.Unlock()
	// Let the durable background recovery worker observe the assignment readback.
	// Other intentionally blocked handoffs remain pending in this fixture, so
	// schedule only this request for processing and exercise the worker's normal
	// selection/order path without making the test timing-dependent.
	_, err = db.Exec(`UPDATE shared_space_handoffs SET next_attempt_at=now()+interval '1 hour' WHERE phase<>'applied'`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE shared_space_handoffs SET next_attempt_at=now() WHERE space_id='space' AND request_id='handoff-patch-unknown'`)
	require.NoError(t, err)
	h.processSharedHandoffs(t.Context())
	pending, err := h.sharedHandoffDTO(t.Context(), "space", "task-unknown")
	require.NoError(t, err)
	require.Nil(t, pending, "worker recovery should finalize the durable handoff")
	history, err := h.sharedHandoffHistory(t.Context(), "space", "task-unknown")
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, "applied", history[0].Phase)
	pcMu.Lock()
	require.Equal(t, 1, patchesByTask["task-unknown"])
	pcMu.Unlock()
	h.processSharedHandoffs(t.Context()) // An empty worker queue is a no-op.

	// A concurrent handoff and claim serialize on the same space lock. The
	// winner advances the assignment token; the other operation must be stale
	// and cannot create a second native assignment.
	raceAssignment, err := h.sharedAssignmentDTO(t.Context(), sharedSpace{ID: "space", DeploymentID: "deployment-acme", CompanyID: "company", ProjectID: "project", Role: "owner"}, pcTasks["task-race"], identity)
	require.NoError(t, err)
	start := make(chan struct{})
	type outcome struct {
		code int
		err  error
	}
	handoffDone, claimDone := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		<-start
		code, _ := handoff("task-race", "race-handoff", "participant-b", raceAssignment.Token)
		handoffDone <- outcome{code: code}
	}()
	go func() {
		<-start
		conn, release, lockErr := h.sharedLock(t.Context(), "space")
		if lockErr != nil {
			claimDone <- outcome{err: lockErr}
			return
		}
		defer release()
		space, loadErr := h.sharedLoad(t.Context(), conn, "space", identity)
		if loadErr == nil {
			_, loadErr = h.sharedAssignmentLocked(t.Context(), conn, space, "task-race", identity, sharedAssignmentInput{RequestID: "race-claim", ExpectedAssignmentToken: raceAssignment.Token, ParticipantID: "participant-b"}, true)
		}
		claimDone <- outcome{err: loadErr}
	}()
	close(start)
	handoffOutcome, claimOutcome := <-handoffDone, <-claimDone
	claimCode := http.StatusOK
	if claimOutcome.err != nil {
		var sharedErr *sharedError
		require.ErrorAs(t, claimOutcome.err, &sharedErr)
		claimCode = sharedErr.code
	}
	winners := 0
	if handoffOutcome.code == http.StatusOK {
		winners++
	} else {
		require.Equal(t, http.StatusConflict, handoffOutcome.code)
	}
	if claimCode == http.StatusOK {
		winners++
	} else {
		require.Equal(t, http.StatusConflict, claimCode)
	}
	require.Equal(t, 1, winners)
	pcMu.Lock()
	require.Equal(t, 1, patchesByTask["task-race"]+checkouts)
	require.Equal(t, "agent-b", pcTasks["task-race"]["assigneeAgentId"])
	pcMu.Unlock()
}

func partsID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] != "issues" {
		return ""
	}
	if len(parts) >= 3 && parts[len(parts)-1] == "checkout" {
		return parts[len(parts)-2]
	}
	if len(parts) >= 2 {
		return parts[len(parts)-1]
	}
	return ""
}
