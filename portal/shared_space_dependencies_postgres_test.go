package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

func TestSharedSpacesPostgresDependenciesResumeAndReturn(t *testing.T) {
	f := newDependencyPGFixture(t)
	owner := identity{UserID: "u-x", DeploymentID: "deployment-acme"}
	reviewer := identity{UserID: "u-owner", DeploymentID: "deployment-acme"}
	status, out := f.call(t, f.h.sharedTaskDependencies, "GET", "", "b", "", owner)
	require.Equal(t, 200, status, out)
	token := out["dependencies"].(map[string]any)["token"]
	input := fmt.Sprintf(`{"requestId":"dependencies-b","expectedDependencyToken":%q,"blockedByIssueIds":["a"]}`, token)
	status, out = f.call(t, f.h.sharedSetDependencies, "PUT", input, "b", "", owner)
	require.Equal(t, 200, status, out)
	require.False(t, out["dependencies"].(map[string]any)["ready"].(bool))
	status, out = f.call(t, f.h.sharedTasks, "GET", "", "", "", owner)
	require.Equal(t, 200, status, out)
	for _, item := range out["tasks"].([]any) {
		task := item.(map[string]any)
		if task["id"] == "b" {
			require.EqualValues(t, 1, task["dependencyState"].(map[string]any)["pendingCount"])
		}
	}
	ws := runtimeWorkspace(t, f.h)
	runtimeCall := func(action, binding string) func(http.ResponseWriter, *http.Request, identity) error {
		return func(w http.ResponseWriter, r *http.Request, i identity) error {
			r.SetPathValue("action", action)
			r.URL.RawQuery = url.Values{"bindingId": {binding}}.Encode()
			return f.h.sharedRuntimeAgent(w, r, i, ws, "assistant")
		}
	}
	status, out = f.call(t, runtimeCall("space-bind", ""), "POST", `{"spaceId":"space","taskId":"b","sourceConversationId":42,"requestId":"bind-b","title":"执行B","instruction":"利用前置A的共享成果完成B"}`, "b", "", owner)
	require.Equal(t, 202, status, out)
	require.True(t, out["waitingDependencies"].(bool))
	var binding runtimeBinding
	raw, _ := json.Marshal(out)
	require.NoError(t, json.Unmarshal(raw, &binding))
	// The first waiting row is deferred, allowing another ready start to run.
	f.mu.Lock()
	f.tasks["c"]["status"] = "in_progress"
	f.tasks["c"]["assigneeAgentId"] = "agent"
	f.mu.Unlock()
	_, err := f.db.Exec(`INSERT INTO shared_space_runtime_bindings(id,space_id,task_id,workspace_name,workspace_uid,owner_id,companion,source_conversation_id,generation,execution_id,expected_version,bind_hash,assignment_hash,participant_id,participant_version,binding_state,assignment_epoch) VALUES('ready-c','space','c','ws-source','runtime-test-uid','u-x','assistant',42,1,'execution-c',0,'c',$1,'participant',1,'active',0)`, runtimeAssignment(map[string]any{"assigneeAgentId": "agent"}))
	require.NoError(t, err)
	tx, err := f.db.Begin()
	require.NoError(t, err)
	require.NoError(t, runtimeEnqueue(t.Context(), tx, runtimeBinding{ID: "ready-c"}, "start:ready-c", "start", map[string]any{"bindingId": "ready-c", "spaceId": "space", "taskId": "c", "sourceConversationId": 42, "generation": 1, "executionId": "execution-c", "expectedVersion": 0, "assignmentEpoch": 0, "title": "执行C", "instruction": "独立就绪任务"}))
	require.NoError(t, tx.Commit())
	f.h.processSharedRuntime(t.Context())
	f.h.processSharedRuntime(t.Context())
	f.mu.Lock()
	require.Zero(t, f.starts["b"])
	require.Equal(t, 1, f.starts["c"])
	require.Zero(t, f.checkoutPosts)
	f.mu.Unlock()
	status, out = f.call(t, f.h.sharedSubmit, "POST", `{"requestId":"result-a","expectedVersion":0,"title":"A成果","body":"前置共享正文，只有接受后才能使用"}`, "a", "", owner)
	require.Equal(t, 201, status, out)
	resultA := out["result"].(map[string]any)["id"].(string)
	f.tick(t)
	f.mu.Lock()
	require.Zero(t, f.starts["b"])
	require.Zero(t, f.checkoutPosts)
	f.mu.Unlock()
	// Even an upstream done written before the local review transaction commits
	// cannot release the dependent while its latest shared result is submitted.
	f.mu.Lock()
	f.tasks["a"]["status"] = "done"
	f.mu.Unlock()
	f.tick(t)
	f.mu.Lock()
	require.Zero(t, f.starts["b"])
	f.tasks["a"]["status"] = "todo"
	f.mu.Unlock()
	status, out = f.call(t, f.h.sharedReviewResult, "POST", `{"requestId":"accept-a","expectedVersion":1,"decision":"accept"}`, "a", resultA, reviewer)
	require.Equal(t, 200, status, out)
	f.mu.Lock()
	f.finalize = true
	f.mu.Unlock()
	f.tick(t)
	f.mu.Lock()
	require.Zero(t, f.starts["b"])
	f.mu.Unlock()
	status, out = f.call(t, runtimeCall("space-status", binding.ID), "GET", "", "b", "", owner)
	require.Equal(t, 200, status, out)
	require.True(t, out["waitingDependencies"].(bool))
	require.EqualValues(t, 1, out["dependencies"].(map[string]any)["blockedBy"].([]any)[0].(map[string]any)["acceptedResult"].(map[string]any)["version"])
	f.mu.Lock()
	f.finalize = false
	f.mu.Unlock()
	// Native checkout commits but drops the response. No model call is made.
	f.tick(t)
	f.mu.Lock()
	require.Equal(t, 1, f.checkoutPosts)
	require.Zero(t, f.starts["b"])
	f.mu.Unlock()
	// Read-back resolves only that checkout intent; start then loses its own ack.
	f.tick(t)
	f.mu.Lock()
	require.Equal(t, 1, f.checkoutPosts)
	require.Equal(t, 1, f.starts["b"])
	f.mu.Unlock()
	var frozen []byte
	var outboxState string
	require.NoError(t, f.db.QueryRow(`SELECT payload,state FROM shared_space_runtime_outbox WHERE command_id=$1`, "start:"+binding.ID).Scan(&frozen, &outboxState))
	require.Equal(t, "unknown", outboxState)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(frozen, &payload))
	context := payload["dependencyContext"].([]any)[0].(map[string]any)
	require.Equal(t, "a", context["taskId"])
	require.Equal(t, "前置共享正文，只有接受后才能使用", context["acceptedResult"].(map[string]any)["body"])
	require.NotContains(t, string(frozen), "sourceConversationId\":99")
	require.NoError(t, f.db.Close())
	f.db = stdlib.OpenDB(*f.cfg)
	t.Cleanup(func() { _ = f.db.Close() })
	restarted := *f.h.portal
	restarted.db = f.db
	f.h.portal = &restarted
	f.tick(t)
	f.tick(t)
	f.mu.Lock()
	require.Equal(t, 1, f.starts["b"])
	require.Equal(t, 1, f.checkoutPosts)
	f.mu.Unlock()
	current, err := runtimeLoad(t.Context(), f.db, binding.ID)
	require.NoError(t, err)
	require.Equal(t, "pi-b", current.PiTaskID)
	require.Equal(t, binding.AssignmentEpoch, current.AssignmentEpoch)
	status, out = f.call(t, runtimeCall("space-status", binding.ID), "GET", "", "b", "", owner)
	require.Equal(t, 200, status, out)
	require.False(t, out["waitingDependencies"].(bool))
	submit := fmt.Sprintf(`{"bindingId":%q,"piTaskId":"pi-b","generation":1,"executionId":%q,"expectedVersion":0,"artifactId":"artifact-b","title":"B成果","body":"基于A共享成果形成的B成果","requestId":"submit-b"}`, binding.ID, binding.ExecutionID)
	status, out = f.call(t, runtimeCall("space-submit", binding.ID), "POST", submit, "b", "", owner)
	require.Equal(t, 201, status, out)
	resultB := out["receipt"].(map[string]any)["result"].(map[string]any)["id"].(string)
	f.tick(t)
	status, out = f.call(t, f.h.sharedReviewResult, "POST", `{"requestId":"accept-b","expectedVersion":1,"decision":"accept"}`, "b", resultB, reviewer)
	require.Equal(t, 200, status, out)
	f.tick(t)
	f.tick(t)
	f.mu.Lock()
	require.Len(t, f.returns, 2)
	require.Equal(t, "submitted", f.returns[0]["stage"])
	require.Equal(t, "accept", f.returns[1]["stage"])
	require.EqualValues(t, 42, f.returns[1]["conversationId"])
	require.Equal(t, 1, f.starts["b"])
	f.mu.Unlock()
}

func TestSharedSpacesPostgresDependencyEditReadback(t *testing.T) {
	f := newDependencyPGFixture(t)
	owner := identity{UserID: "u-x", DeploymentID: "deployment-acme"}
	status, out := f.call(t, f.h.sharedTaskDependencies, "GET", "", "b", "", owner)
	require.Equal(t, 200, status, out)
	token := out["dependencies"].(map[string]any)["token"]
	input := fmt.Sprintf(`{"requestId":"set-with-lost-ack","expectedDependencyToken":%q,"blockedByIssueIds":["a"]}`, token)
	f.mu.Lock()
	f.loseDependency = true
	f.mu.Unlock()
	status, out = f.call(t, f.h.sharedSetDependencies, "PUT", input, "b", "", owner)
	require.Equal(t, 423, status, out)
	status, out = f.call(t, f.h.sharedSetDependencies, "PUT", input, "b", "", owner)
	require.Equal(t, 200, status, out)
	f.mu.Lock()
	require.Equal(t, 1, f.dependencyPatches)
	f.mu.Unlock()
	status, out = f.call(t, f.h.sharedTaskDependencies, "GET", "", "b", "", owner)
	require.Equal(t, 200, status, out)
	token = out["dependencies"].(map[string]any)["token"]
	cross := fmt.Sprintf(`{"requestId":"cross-space","expectedDependencyToken":%q,"blockedByIssueIds":["foreign"]}`, token)
	status, out = f.call(t, f.h.sharedSetDependencies, "PUT", cross, "b", "", owner)
	require.Equal(t, 404, status, out)
	f.mu.Lock()
	f.rejectCycle = true
	f.mu.Unlock()
	cycle := fmt.Sprintf(`{"requestId":"cycle","expectedDependencyToken":%q,"blockedByIssueIds":["a"]}`, token)
	status, out = f.call(t, f.h.sharedSetDependencies, "PUT", cycle, "b", "", owner)
	require.Equal(t, 400, status, out)
	status, out = f.call(t, f.h.sharedSetDependencies, "PUT", cycle, "b", "", owner)
	require.Equal(t, 400, status, out)
	f.mu.Lock()
	require.Equal(t, 2, f.dependencyPatches)
	f.mu.Unlock()
}
