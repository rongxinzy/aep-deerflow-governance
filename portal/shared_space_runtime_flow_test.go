package main

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Match the business reference and revision instructions, rather than relying on
// JSON key order or accepting any payload at the transaction boundary.
type runtimeFlowPayload struct {
	bindingID, resultID, revisionID, stage, reason string
	generation, expectedVersion                    int
}

func (want runtimeFlowPayload) Match(value driver.Value) bool {
	raw, ok := value.(string)
	if !ok {
		return false
	}
	var got map[string]any
	if json.Unmarshal([]byte(raw), &got) != nil {
		return false
	}
	if got["bindingId"] != want.bindingID || got["resultId"] != want.resultID || got["revisionId"] != want.revisionID || got["generation"] != float64(want.generation) {
		return false
	}
	if want.stage == "revise" {
		return got["reason"] == want.reason && got["expectedVersion"] == float64(want.expectedVersion) && got["piTaskId"] == "pi-task" && got["executionId"] == collaborationID(want.bindingID, "2")
	}
	text, ok := got["text"].(string)
	return ok && got["stage"] == want.stage && got["conversationId"] == float64(42) && strings.Contains(text, "『成果标题』第 1 版") && !strings.Contains(text, want.resultID) && !strings.Contains(text, want.revisionID)
}

func TestSharedRuntimeSubmitAndReviewTransactionFlow(t *testing.T) {
	for _, failRevisionOutbox := range []bool{false, true} {
		name := "submit-and-revise-commit"
		if failRevisionOutbox {
			name = "revision-outbox-failure-rolls-back"
		}
		t.Run(name, func(t *testing.T) {
			h := collaborationHarness(t)
			ws := runtimeWorkspace(t, h)
			binding := runtimeTestBinding()
			const title = "成果标题"
			const body = "中文成果 <>&，实际内容\n"
			const submitCommand = "submit:binding:1"
			resultID := collaborationID(binding.SpaceID, submitCommand)
			const documentID = "pc-document"
			const revisionID = "pc-revision"
			taskStatus := "todo"
			documentWrites, taskPatches := 0, 0
			pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/issues/task" && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": taskStatus, "assigneeAgentId": "agent"})
				case r.URL.Path == "/issues/task" && r.Method == http.MethodPatch:
					var change map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&change))
					taskStatus = change["status"].(string)
					taskPatches++
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": taskStatus})
				case r.URL.Path == "/issues/task/documents/shared-space-result":
					if r.Method == http.MethodPut {
						var document map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&document))
						require.Equal(t, body, document["body"])
						require.Equal(t, title, document["title"])
						documentWrites++
					}
					_ = json.NewEncoder(w).Encode(sharedDocument{ID: documentID, LatestRevisionID: revisionID, Body: body})
				default:
					t.Errorf("unexpected Paperclip call %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer pc.Close()
			h.cfg.paperclipBaseURL = pc.URL
			pi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/assistant/api/tasks/pi-task":
					_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{"id": "pi-task", "status": "completed", "generation": 1, "executionId": "execution", "selectedArtifactId": "selected-artifact", "sharedRuntime": map[string]any{"bindingId": "binding"}}})
				case "/assistant/internal/shared-space/artifact":
					require.Equal(t, "selected-artifact", r.URL.Query().Get("id"))
					_ = json.NewEncoder(w).Encode(map[string]any{"taskId": "pi-task", "title": title, "body": body})
				default:
					t.Errorf("unexpected Pi call %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer pi.Close()
			h.workspaceChatOverride = pi.URL

			// The authenticated handler must verify runtime provenance before entering
			// the shared submission service, then verify the binding again under its lock.
			runtimeExpectBinding(h, binding)
			runtimeExpectSpace(h, "editor")
			runtimeExpectParticipant(h, binding)
			h.dbmock.ExpectExec("SELECT pg_advisory_lock").WillReturnResult(sqlmock.NewResult(0, 1))
			runtimeExpectBinding(h, binding)
			runtimeExpectSpace(h, "editor")
			runtimeExpectParticipant(h, binding)
			runtimeExpectSpace(h, "editor")
			h.dbmock.ExpectQuery("SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants").WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "target", "name", "owner_id", "resource_uid", "role", "state", "version", "agent_id", "registration_state"}).AddRow(binding.ParticipantID, "companion", "ws-source/assistant", "assistant", binding.Owner, binding.WorkspaceUID, "editor", "active", binding.ParticipantVersion, "agent", "ready"))
			h.dbmock.ExpectQuery("SELECT actor_id,action,payload_hash,state,receipt").WithArgs("space", submitCommand).WillReturnError(sql.ErrNoRows)
			h.dbmock.ExpectQuery("SELECT r.id,r.task_id").WillReturnRows(sqlmock.NewRows([]string{"id"}))
			h.dbmock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
			h.dbmock.ExpectExec("INSERT INTO shared_space_commands").WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectBegin()
			h.dbmock.ExpectExec("INSERT INTO shared_space_results").WithArgs(resultID, "space", "task", 1, title, "shared-space-result", documentID, revisionID, sharedHash(body), "u-x", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectQuery("SELECT id FROM shared_space_runtime_bindings").WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(binding.ID))
			runtimeExpectBinding(h, binding)
			h.dbmock.ExpectExec("INSERT INTO shared_space_runtime_outbox").WithArgs("return:"+resultID+":submitted", binding.ID, "return", runtimeFlowPayload{bindingID: binding.ID, resultID: resultID, revisionID: revisionID, stage: "submitted", generation: 1}, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectExec("UPDATE shared_space_commands SET state='applied'").WithArgs("space", submitCommand, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectCommit()
			runtimeExpectUnlock(h)
			requestBody, _ := json.Marshal(map[string]any{"bindingId": binding.ID, "piTaskId": binding.PiTaskID, "generation": 1, "executionId": binding.ExecutionID, "expectedVersion": 0, "artifactId": "selected-artifact", "title": title, "body": body, "requestId": submitCommand})
			req := httptest.NewRequest("POST", "/", strings.NewReader(string(requestBody)))
			req.SetPathValue("action", "space-submit")
			response := httptest.NewRecorder()
			require.NoError(t, h.sharedRuntimeAgent(response, req, identity{UserID: "u-x"}, ws, "assistant"))
			require.Equal(t, 201, response.Code, response.Body.String())
			var submissionReceipt struct {
				CommandID   string `json:"commandId"`
				PayloadHash string `json:"payloadHash"`
				State       string `json:"state"`
				Receipt     struct {
					Result sharedResult `json:"result"`
				} `json:"receipt"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &submissionReceipt))
			require.Equal(t, "applied", submissionReceipt.State)
			require.Equal(t, submitCommand, submissionReceipt.CommandID)
			require.Equal(t, sharedHash([]any{"task", sharedSubmission{RequestID: submitCommand, ExpectedVersion: 0, Title: title, Body: body}}), submissionReceipt.PayloadHash)
			submitted := submissionReceipt.Receipt.Result
			require.Equal(t, resultID, submitted.ID)
			require.Equal(t, revisionID, submitted.RevisionID)
			require.Equal(t, 1, documentWrites)
			require.NoError(t, h.dbmock.ExpectationsWereMet())

			// A different owner reviews the exact submitted document revision. The
			// review, new execution fence, and both delivery intents form one transaction.
			const reason = "请补充验收结论 <>&"
			h.dbmock.ExpectExec("SELECT pg_advisory_lock").WillReturnResult(sqlmock.NewResult(0, 1))
			runtimeExpectMemberSpace(h, "owner")
			h.dbmock.ExpectQuery("SELECT actor_id,action,payload_hash,state,receipt").WithArgs("space", "review-command").WillReturnError(sql.ErrNoRows)
			h.dbmock.ExpectQuery("SELECT r.id,r.task_id").WillReturnRows(sqlmock.NewRows([]string{"id", "task_id", "version", "title", "document_key", "document_id", "revision_id", "body_hash", "submitted_by", "state", "created_at"}).AddRow(resultID, "task", 1, title, "shared-space-result", documentID, revisionID, sharedHash(body), "u-x", "submitted", time.Now()))
			h.dbmock.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(false))
			h.dbmock.ExpectExec("INSERT INTO shared_space_commands").WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectBegin()
			h.dbmock.ExpectExec("INSERT INTO shared_space_reviews").WithArgs(collaborationID("space", "review-command"), resultID, "request_changes", reason, "u-owner").WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectQuery("SELECT id FROM shared_space_runtime_bindings").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(binding.ID))
			runtimeExpectBinding(h, binding)
			h.dbmock.ExpectQuery("SELECT EXISTS").WithArgs("return:"+resultID+":submitted", binding.ID).WillReturnRows(sqlmock.NewRows([]string{"linked"}).AddRow(true))
			h.dbmock.ExpectExec("UPDATE shared_space_runtime_bindings SET generation").WithArgs(binding.ID, 2, collaborationID(binding.ID, "2"), 1).WillReturnResult(sqlmock.NewResult(0, 1))
			revise := h.dbmock.ExpectExec("INSERT INTO shared_space_runtime_outbox").WithArgs("revise:"+resultID, binding.ID, "revise", runtimeFlowPayload{bindingID: binding.ID, resultID: resultID, revisionID: revisionID, stage: "revise", reason: reason, generation: 2, expectedVersion: 1}, sqlmock.AnyArg())
			if failRevisionOutbox {
				revise.WillReturnError(errors.New("durable outbox unavailable"))
				h.dbmock.ExpectRollback()
			} else {
				revise.WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectExec("INSERT INTO shared_space_runtime_outbox").WithArgs("return:"+resultID+":request_changes", binding.ID, "return", runtimeFlowPayload{bindingID: binding.ID, resultID: resultID, revisionID: revisionID, stage: "request_changes", generation: 2}, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectExec("UPDATE shared_space_commands SET state='applied'").WithArgs("space", "review-command", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectCommit()
			}
			runtimeExpectUnlock(h)
			reviewJSON, _ := json.Marshal(map[string]any{"requestId": "review-command", "expectedVersion": 1, "decision": "request_changes", "reason": reason})
			reviewReq := httptest.NewRequest("POST", "/", strings.NewReader(string(reviewJSON)))
			reviewReq.SetPathValue("space", "space")
			reviewReq.SetPathValue("task", "task")
			reviewReq.SetPathValue("result", resultID)
			reviewResponse := httptest.NewRecorder()
			err := h.sharedReviewResult(reviewResponse, reviewReq, identity{UserID: "u-owner", DeploymentID: h.cfg.aepDeploymentID})
			if failRevisionOutbox {
				require.ErrorContains(t, err, "durable outbox unavailable")
			} else {
				require.NoError(t, err)
				require.Equal(t, 200, reviewResponse.Code)
				require.Contains(t, reviewResponse.Body.String(), "changes_requested")
			}
			require.Equal(t, 1, taskPatches)
			require.Equal(t, 1, documentWrites)
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
