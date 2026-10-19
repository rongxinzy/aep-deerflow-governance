package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"testing"
	"time"
)

type runtimeReturnPayload struct{ stage string }

func (expected runtimeReturnPayload) Match(value driver.Value) bool {
	raw, ok := value.(string)
	if !ok {
		return false
	}
	var payload map[string]any
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return false
	}
	text, ok := payload["text"].(string)
	if !ok {
		return false
	}
	return strings.Contains(text, "『成果标题』第 1 版") && !strings.Contains(text, "result") && !strings.Contains(text, "revision") && payload["resultId"] == "result" && payload["revisionId"] == "revision" && payload["stage"] == expected.stage
}

func runtimeTestBinding() runtimeBinding {
	return runtimeBinding{BindingState: "active", AssignmentEpoch: 0, ParticipantID: "participant", ParticipantVersion: 1, ID: "binding", SpaceID: "space", TaskID: "task", Workspace: "ws-source", WorkspaceUID: "runtime-test-uid", Owner: "u-x", Companion: "assistant", SourceConversationID: 42, PiTaskID: "pi-task", Generation: 1, ExecutionID: "execution", ExpectedVersion: 0, BindHash: "bind-hash", AssignmentHash: runtimeAssignment(map[string]any{"assigneeAgentId": "agent"})}
}
func runtimeTestRows(b runtimeBinding) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "space_id", "task_id", "workspace_name", "workspace_uid", "owner_id", "companion", "source_conversation_id", "pi_task_id", "generation", "execution_id", "expected_version", "bind_hash", "assignment_hash", "participant_id", "participant_version", "binding_state", "assignment_epoch"}).AddRow(b.ID, b.SpaceID, b.TaskID, b.Workspace, b.WorkspaceUID, b.Owner, b.Companion, b.SourceConversationID, b.PiTaskID, b.Generation, b.ExecutionID, b.ExpectedVersion, b.BindHash, b.AssignmentHash, b.ParticipantID, b.ParticipantVersion, b.BindingState, b.AssignmentEpoch)
}
func runtimeExpectBinding(h *apiTestPortal, b runtimeBinding) {
	h.dbmock.ExpectQuery("SELECT id,space_id,task_id,workspace_name").WithArgs(b.ID).WillReturnRows(runtimeTestRows(b))
}
func runtimeExpectSpace(h *apiTestPortal, role string) {
	h.dbmock.ExpectQuery("SELECT id,deployment_id,company_id,project_id,name").WillReturnRows(sqlmock.NewRows([]string{"id", "deployment_id", "company_id", "project_id", "name"}).AddRow("space", h.cfg.aepDeploymentID, "company", "project", "空间"))
}
func runtimeExpectMemberSpace(h *apiTestPortal, role string) {
	h.dbmock.ExpectQuery("SELECT s.id,s.deployment_id").WillReturnRows(sqlmock.NewRows([]string{"id", "deployment_id", "company_id", "project_id", "name", "role"}).AddRow("space", h.cfg.aepDeploymentID, "company", "project", "空间", role))
}
func runtimeExpectParticipant(h *apiTestPortal, b runtimeBinding) {
	h.dbmock.ExpectQuery("SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants").WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "target", "name", "owner_id", "resource_uid", "role", "state", "version", "agent_id", "registration_state"}).AddRow(b.ParticipantID, "companion", b.Workspace+"/"+b.Companion, "assistant", b.Owner, b.WorkspaceUID, "editor", "active", b.ParticipantVersion, "agent", "ready"))
}
func runtimeWorkspace(t *testing.T, h *apiTestPortal) *dev.PersonalWorkspace {
	t.Helper()
	var ws dev.PersonalWorkspace
	require.NoError(t, h.k8s.Get(t.Context(), client.ObjectKey{Namespace: h.cfg.namespace, Name: "ws-source"}, &ws))
	if ws.UID == "" {
		ws.UID = "runtime-test-uid"
		require.NoError(t, h.k8s.Update(t.Context(), &ws))
	}
	return &ws
}
func runtimeExpectUnlock(h *apiTestPortal) {
	h.dbmock.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
}
func runtimeExpectTaskEpoch(h *apiTestPortal, space, task string, epoch int) {
	h.dbmock.ExpectQuery("SELECT epoch FROM shared_space_task_epochs WHERE space_id=\\$1 AND task_id=\\$2").WithArgs(space, task).WillReturnRows(sqlmock.NewRows([]string{"epoch"}).AddRow(epoch))
}

func TestSharedRuntimeLoadAndCommandGate(t *testing.T) {
	h := newAPIHarness(t)
	b := runtimeTestBinding()
	runtimeExpectBinding(h, b)
	got, err := runtimeLoad(t.Context(), h.db, b.ID)
	require.NoError(t, err)
	require.Equal(t, b, got)
	h.dbmock.ExpectQuery("SELECT id,space_id").WillReturnError(sql.ErrNoRows)
	_, err = runtimeLoad(t.Context(), h.db, "missing")
	require.ErrorIs(t, err, sql.ErrNoRows)
	conn, err := h.db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	for _, blocked := range []bool{false, true} {
		h.dbmock.ExpectQuery("SELECT EXISTS").WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(blocked))
		err = runtimeCommandGate(t.Context(), conn, "space", "task")
		if blocked {
			require.ErrorContains(t, err, "待核验")
		} else {
			require.NoError(t, err)
		}
	}
	h.dbmock.ExpectQuery("SELECT EXISTS").WillReturnError(errors.New("temporary db"))
	require.ErrorContains(t, runtimeCommandGate(t.Context(), conn, "space", "task"), "temporary db")
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}
func TestSharedRuntimeEnqueuePreservesExactReviewAndSkipsManualResults(t *testing.T) {
	for _, stage := range []string{"unbound", "foreign", "manual", "submitted", "accept", "request_changes"} {
		t.Run(stage, func(t *testing.T) {
			h := newAPIHarness(t)
			b := runtimeTestBinding()
			h.dbmock.ExpectBegin()
			tx, err := h.db.Begin()
			require.NoError(t, err)
			result := sharedResult{ID: "result", Title: "成果标题", Version: 1, RevisionID: "revision", SubmittedBy: b.Owner}
			switch stage {
			case "unbound":
				h.dbmock.ExpectQuery("SELECT binding_id FROM shared_space_result_bindings WHERE result_id=\\$1").WithArgs("result").WillReturnError(sql.ErrNoRows)
			case "submitted":
				h.dbmock.ExpectQuery("SELECT id FROM shared_space_runtime_bindings").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(b.ID))
				h.dbmock.ExpectExec("INSERT INTO shared_space_result_bindings").WithArgs("result", b.ID).WillReturnResult(sqlmock.NewResult(0, 1))
			default:
				h.dbmock.ExpectQuery("SELECT binding_id FROM shared_space_result_bindings WHERE result_id=\\$1").WithArgs("result").WillReturnRows(sqlmock.NewRows([]string{"binding_id"}).AddRow(b.ID))
			}
			if stage != "unbound" {
				runtimeExpectBinding(h, b)
				if stage == "foreign" {
					result.SubmittedBy = "other"
				} else if stage != "submitted" {
					h.dbmock.ExpectQuery("SELECT EXISTS").WithArgs("return:result:submitted", b.ID).WillReturnRows(sqlmock.NewRows([]string{"linked"}).AddRow(stage != "manual"))
				}
				if stage == "request_changes" {
					h.dbmock.ExpectExec("UPDATE shared_space_runtime_bindings SET generation").WithArgs(b.ID, 2, collaborationID(b.ID, "2"), 1).WillReturnResult(sqlmock.NewResult(0, 1))
					h.dbmock.ExpectExec("INSERT INTO shared_space_runtime_outbox").WithArgs("revise:result", b.ID, "revise", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				}
				if stage == "submitted" || stage == "accept" || stage == "request_changes" {
					h.dbmock.ExpectExec("INSERT INTO shared_space_runtime_outbox").WithArgs("return:result:"+stage, b.ID, "return", runtimeReturnPayload{stage: stage}, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			require.NoError(t, h.sharedRuntimeEnqueue(t.Context(), tx, "space", "task", result, stage, "中文 <>& 修改意见"))
			h.dbmock.ExpectRollback()
			require.NoError(t, tx.Rollback())
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
	h := newAPIHarness(t)
	h.dbmock.ExpectBegin()
	tx, err := h.db.Begin()
	require.NoError(t, err)
	require.Error(t, runtimeEnqueue(t.Context(), tx, runtimeTestBinding(), "x", "start", map[string]any{"bad": make(chan int)}))
	h.dbmock.ExpectRollback()
	require.NoError(t, tx.Rollback())
}
func TestSharedRuntimeAuthorizationFences(t *testing.T) {
	for _, mode := range []string{"uid", "owner", "inactive", "missing-companion", "viewer", "revoked", "assignment", "temporary", "ok"} {
		t.Run(mode, func(t *testing.T) {
			h := collaborationHarness(t)
			ws := runtimeWorkspace(t, h)
			b := runtimeTestBinding()
			b.WorkspaceUID = string(ws.UID)
			switch mode {
			case "uid":
				b.WorkspaceUID = "old"
			case "owner":
				b.Owner = "other"
			case "inactive":
				b.Owner = "u-off"
				ws.Spec.Owner = "u-off"
			case "missing-companion":
				b.Companion = "removed"
			}
			runtimeExpectTaskEpoch(h, b.SpaceID, b.TaskID, b.AssignmentEpoch)
			if mode == "viewer" || mode == "assignment" || mode == "ok" || mode == "revoked" {
				role := "editor"
				if mode == "viewer" {
					role = "viewer"
				}
				runtimeExpectSpace(h, role)
			}
			if mode == "revoked" {
				h.dbmock.ExpectQuery("SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants").WillReturnError(sql.ErrNoRows)
			}
			if mode == "temporary" {
				h.dbmock.ExpectQuery("SELECT id,deployment_id,company_id,project_id,name").WillReturnError(errors.New("temporary db"))
			}
			if mode == "assignment" || mode == "ok" {
				runtimeExpectParticipant(h, b)
				pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, "/issues/task", r.URL.Path)
					body := map[string]any{"id": "task", "projectId": "project", "companyId": "company", "assigneeAgentId": "agent"}
					if mode == "assignment" {
						body["assigneeUserId"] = "changed"
					}
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer pc.Close()
				h.cfg.paperclipBaseURL = pc.URL
			}
			err := h.runtimeAuthorize(t.Context(), b, ws, identity{UserID: b.Owner, DeploymentID: h.cfg.aepDeploymentID})
			if mode == "ok" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			if mode == "temporary" {
				var rejected *sharedError
				require.False(t, errors.As(err, &rejected))
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
func TestSharedRuntimeHandlerValidationAndDiscovery(t *testing.T) {
	h := collaborationHarness(t)
	ws := runtimeWorkspace(t, h)
	for _, body := range []string{`{`, `{"ownerId":"forged"}`, `{}`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.SetPathValue("action", "space-bind")
		err := h.sharedRuntimeAgent(httptest.NewRecorder(), r, identity{UserID: "u-x"}, ws, "assistant")
		require.Error(t, err)
	}
	h.dbmock.ExpectQuery("SELECT s.id FROM shared_spaces").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("space"))
	runtimeExpectSpace(h, "editor")
	b := runtimeTestBinding()
	runtimeExpectParticipant(h, b)
	h.dbmock.ExpectQuery("SELECT count\\(\\*\\) FROM shared_space_commands").WithArgs("space", "allowed").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "allowed", "projectId": "project", "companyId": "company", "executionConfig": "PRIVATE"}, {"id": "foreign", "projectId": "other", "companyId": "company"}})
	}))
	defer pc.Close()
	h.cfg.paperclipBaseURL = pc.URL
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("action", "space-discover")
	w := httptest.NewRecorder()
	require.NoError(t, h.sharedRuntimeAgent(w, r, identity{UserID: "u-x"}, ws, "assistant"))
	require.Contains(t, w.Body.String(), "allowed")
	require.NotContains(t, w.Body.String(), "PRIVATE")
	require.NotContains(t, w.Body.String(), "foreign")
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}
func TestSharedRuntimeWorkerUnknownOnlyQueriesReceipt(t *testing.T) {
	for _, mode := range []string{"unknown", "wrong-hash", "applied", "stale", "temporary"} {
		t.Run(mode, func(t *testing.T) {
			h := collaborationHarness(t)
			_ = runtimeWorkspace(t, h)
			b := runtimeTestBinding()
			b.PiTaskID = ""
			payload := map[string]any{"executionId": b.ExecutionID}
			raw, _ := json.Marshal(payload)
			hash := sharedHash(payload)
			state := "pending"
			if mode == "unknown" {
				state = "unknown"
			}
			h.dbmock.ExpectQuery("SELECT command_id,binding_id,action,state,payload,payload_hash").WillReturnRows(sqlmock.NewRows([]string{"command_id", "binding_id", "action", "state", "payload", "payload_hash"}).AddRow("command", b.ID, "start", state, raw, hash))
			runtimeExpectBinding(h, b)
			h.dbmock.ExpectExec("SELECT pg_advisory_lock").WillReturnResult(sqlmock.NewResult(0, 1))
			current := b
			if mode == "stale" {
				current.ExecutionID = "new-execution"
			}
			runtimeExpectBinding(h, current)
			h.dbmock.ExpectExec("UPDATE shared_space_runtime_outbox SET next_attempt_at").WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectQuery("SELECT state FROM shared_space_runtime_outbox").WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow(state))
			if mode == "temporary" {
				runtimeExpectTaskEpoch(h, b.SpaceID, b.TaskID, b.AssignmentEpoch)
				h.dbmock.ExpectQuery("SELECT id,deployment_id,company_id,project_id,name").WillReturnError(errors.New("temporary db"))
			} else {
				runtimeExpectTaskEpoch(h, b.SpaceID, b.TaskID, b.AssignmentEpoch)
				runtimeExpectSpace(h, "editor")
				runtimeExpectParticipant(h, current)
			}
			calls := []string{}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				receiptHash := hash
				if mode == "wrong-hash" {
					receiptHash = "wrong"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"commandId": "command", "payloadHash": receiptHash, "state": "applied", "task": map[string]any{"id": "created-task", "executionId": b.ExecutionID, "generation": 1}})
			}))
			defer up.Close()
			h.workspaceChatOverride = up.URL
			pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "task", "projectId": "project", "companyId": "company", "status": "todo", "assigneeAgentId": "agent"})
			}))
			defer pc.Close()
			h.cfg.paperclipBaseURL = pc.URL
			if mode == "stale" {
				h.dbmock.ExpectExec("UPDATE shared_space_runtime_outbox SET state='blocked'").WillReturnResult(sqlmock.NewResult(0, 1))
			} else if mode != "temporary" {
				runtimeExpectSpace(h, "editor")
				h.dbmock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
				if state == "pending" {
					h.dbmock.ExpectExec("UPDATE shared_space_runtime_outbox SET state='unknown'").WillReturnResult(sqlmock.NewResult(0, 1))
				}
				if mode != "wrong-hash" {
					h.dbmock.ExpectBegin()
					h.dbmock.ExpectExec("UPDATE shared_space_runtime_bindings SET pi_task_id").WithArgs(b.ID, "created-task").WillReturnResult(sqlmock.NewResult(0, 1))
					h.dbmock.ExpectExec("UPDATE shared_space_runtime_outbox SET state='applied'").WillReturnResult(sqlmock.NewResult(0, 1))
					h.dbmock.ExpectCommit()
				}
			}
			runtimeExpectUnlock(h)
			h.processSharedRuntime(t.Context())
			require.NoError(t, h.dbmock.ExpectationsWereMet())
			if mode == "unknown" {
				require.Equal(t, []string{"GET /assistant/internal/shared-space/receipt"}, calls)
			}
			if mode == "stale" || mode == "temporary" {
				require.Empty(t, calls)
			}
		})
	}
	p := &portal{}
	p.processSharedRuntime(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.runSharedRuntime(ctx)
	time.Sleep(time.Millisecond)
}

func TestSharedRuntimeBindRequiresActiveParticipant(t *testing.T) {
	h := collaborationHarness(t)
	ws := runtimeWorkspace(t, h)
	h.dbmock.ExpectExec("SELECT pg_advisory_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	runtimeExpectSpace(h, "editor")
	h.dbmock.ExpectQuery("SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants").WillReturnError(sql.ErrNoRows)
	runtimeExpectUnlock(h)
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"spaceId":"space","taskId":"task","sourceConversationId":42,"requestId":"origin-call","title":"交办","instruction":"真实任务"}`))
	r.SetPathValue("action", "space-bind")
	err := h.sharedRuntimeAgent(httptest.NewRecorder(), r, identity{UserID: "u-x"}, ws, "assistant")
	var rejected *sharedError
	require.ErrorAs(t, err, &rejected)
	require.Equal(t, 403, rejected.code)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}
func TestSharedRuntimeStatusReceiptAndSubmitFences(t *testing.T) {
	for _, mode := range []string{"missing", "companion", "status", "receipt", "receipt-absent", "unknown-action", "old-generation", "not-completed", "wrong-artifact"} {
		t.Run(mode, func(t *testing.T) {
			h := collaborationHarness(t)
			ws := runtimeWorkspace(t, h)
			b := runtimeTestBinding()
			if mode == "missing" {
				h.dbmock.ExpectQuery("SELECT id,space_id").WillReturnError(sql.ErrNoRows)
			} else {
				runtimeExpectBinding(h, b)
			}
			companion := "assistant"
			if mode == "companion" {
				companion = "forged"
			}
			if mode != "missing" && mode != "companion" {
				runtimeExpectTaskEpoch(h, b.SpaceID, b.TaskID, b.AssignmentEpoch)
				runtimeExpectSpace(h, "editor")
				runtimeExpectParticipant(h, b)
			}
			pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": "todo", "assigneeAgentId": "agent"})
			}))
			defer pc.Close()
			h.cfg.paperclipBaseURL = pc.URL
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/artifact") {
					_ = json.NewEncoder(w).Encode(map[string]any{"taskId": "pi-task", "title": "成果", "body": "tampered"})
					return
				}
				status := "completed"
				if mode == "not-completed" {
					status = "running"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{"id": "pi-task", "status": status, "executionId": "execution", "generation": 1, "selectedArtifactId": "artifact", "sharedRuntime": map[string]any{"bindingId": "binding"}}})
			}))
			defer up.Close()
			h.workspaceChatOverride = up.URL
			action := "space-submit"
			method := "POST"
			body := `{"bindingId":"binding","piTaskId":"pi-task","generation":1,"executionId":"execution","expectedVersion":0,"artifactId":"artifact","title":"成果","body":"中文 <>&","requestId":"submit-command"}`
			if mode == "old-generation" {
				body = strings.Replace(body, `"generation":1`, `"generation":0`, 1)
			}
			if mode == "status" || mode == "receipt" || mode == "receipt-absent" || mode == "unknown-action" {
				method = "GET"
				body = ""
				action = "space-status"
				if strings.HasPrefix(mode, "receipt") {
					action = "space-receipt"
					query := h.dbmock.ExpectQuery("SELECT payload_hash,state,receipt")
					if mode == "receipt-absent" {
						query.WillReturnError(sql.ErrNoRows)
					} else {
						query.WillReturnRows(sqlmock.NewRows([]string{"hash", "state", "receipt"}).AddRow("precise-hash", "applied", `{"result":{"id":"result"}}`))
					}
				}
				if mode == "unknown-action" {
					action = "space-nonexistent"
				}
			}
			r := httptest.NewRequest(method, "/?bindingId=binding&commandId=submit-command", strings.NewReader(body))
			r.SetPathValue("action", action)
			w := httptest.NewRecorder()
			err := h.sharedRuntimeAgent(w, r, identity{UserID: "u-x"}, ws, companion)
			if mode == "status" || mode == "receipt" {
				require.NoError(t, err)
				require.Equal(t, 200, w.Code)
				if mode == "receipt" {
					require.Contains(t, w.Body.String(), "precise-hash")
				}
			} else {
				require.Error(t, err)
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
