package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// Runs in the CI PostgreSQL 16 fixture. Every mutation crosses the real portal
// service and durable command/epoch tables; only native board and Pi are stubs.
func TestSharedSpacesPostgresHumanHandoffResourcesAndReturn(t *testing.T) {
	f := newDependencyPGFixture(t)
	owner := identity{UserID: "u-x", DeploymentID: "deployment-acme", DisplayName: "用户甲"}
	human := identity{UserID: "u-owner", DeploymentID: "deployment-acme", DisplayName: "李四"}
	mapUser := func(user, native string) {
		fn := func(w http.ResponseWriter, r *http.Request, i identity) error {
			r.SetPathValue("user", user)
			return f.h.sharedHumanMapping(w, r, i)
		}
		status, out := f.call(t, fn, "PUT", fmt.Sprintf(`{"requestId":%q,"paperclipUserId":%q}`, "map-"+user, native), "b", "", owner)
		require.Equal(t, 200, status, out)
	}
	mapUser("u-x", "native-x")
	mapUser("u-owner", "native-owner")
	f.mu.Lock()
	f.tasks["b"]["assigneeAgentId"] = "agent"
	f.tasks["b"]["status"] = "in_progress"
	f.mu.Unlock()
	_, err := f.db.Exec(`INSERT INTO shared_space_runtime_bindings(id,space_id,task_id,workspace_name,workspace_uid,owner_id,companion,source_conversation_id,pi_task_id,generation,execution_id,expected_version,bind_hash,assignment_hash,participant_id,participant_version,binding_state,assignment_epoch) VALUES('source-human','space','b','ws-source','runtime-test-uid','u-x','assistant',42,'pi-b',1,'execution-b',0,'hash',$1,'participant',1,'active',0)`, runtimeAssignment(map[string]any{"assigneeAgentId": "agent"}))
	require.NoError(t, err)
	ws := runtimeWorkspace(t, f.h)
	runtimeCall := func(action string) func(http.ResponseWriter, *http.Request, identity) error {
		return func(w http.ResponseWriter, r *http.Request, i identity) error {
			r.SetPathValue("action", action)
			r.URL.RawQuery = url.Values{"bindingId": {"source-human"}, "piTaskId": {"pi-b"}, "generation": {"1"}, "executionId": {"execution-b"}, "key": {"zhiyuan-reference-guide"}, "attachmentId": {"file"}}.Encode()
			return f.h.sharedRuntimeAgent(w, r, i, ws, "assistant")
		}
	}
	status, out := f.call(t, runtimeCall("space-resources"), "GET", "", "b", "", owner)
	require.Equal(t, 200, status, out)
	require.Len(t, out["documents"], 1)
	require.Len(t, out["attachments"], 1)
	status, out = f.call(t, runtimeCall("space-document"), "GET", "", "b", "", owner)
	require.Equal(t, 200, status, out)
	require.Equal(t, "参考正文", out["document"].(map[string]any)["body"])
	req := sharedNativeRequest("GET", "/", "")
	w := httptest.NewRecorder()
	require.NoError(t, runtimeCall("space-attachment")(w, req, owner))
	require.Equal(t, []byte{0, 1, 2, 255}, w.Body.Bytes())
	assignmentToken := func() string {
		status, out = f.call(t, f.h.sharedTaskDetail, "GET", "", "b", "", owner)
		require.Equal(t, 200, status, out)
		return out["assignment"].(map[string]any)["token"].(string)
	}
	token := assignmentToken()
	status, out = f.call(t, f.h.sharedHandoffTask, "POST", fmt.Sprintf(`{"requestId":"handoff-person","expectedAssignmentToken":%q,"memberUserId":"u-owner","reason":"请同事接着完成"}`, token), "b", "", owner)
	require.Equal(t, 200, status, out)
	require.Equal(t, "applied", out["phase"])
	var state string
	require.NoError(t, f.db.QueryRow(`SELECT binding_state FROM shared_space_runtime_bindings WHERE id='source-human'`).Scan(&state))
	require.Equal(t, "retired", state)
	status, out = f.call(t, runtimeCall("space-resources"), "GET", "", "b", "", owner)
	require.Equal(t, 409, status, out)
	status, out = f.call(t, runtimeCall("space-submit"), "POST", `{"bindingId":"source-human","piTaskId":"pi-b","generation":1,"executionId":"execution-b","expectedVersion":0,"artifactId":"artifact-b","title":"旧成果","body":"旧执行不能提交","requestId":"stale-submit"}`, "b", "", owner)
	require.Equal(t, 409, status, out)
	token = assignmentToken()
	claim := func(w http.ResponseWriter, r *http.Request, i identity) error {
		r.SetPathValue("assignmentAction", "claim")
		return f.h.sharedAssignmentChange(w, r, i)
	}
	status, out = f.call(t, claim, "POST", fmt.Sprintf(`{"requestId":"claim-person","expectedAssignmentToken":%q,"memberUserId":"u-owner"}`, token), "b", "", human)
	require.Equal(t, 200, status, out)
	require.Equal(t, "in_progress", out["task"].(map[string]any)["status"])
	status, out = f.call(t, f.h.sharedAddComment, "POST", `{"clientRequestId":"d17c142e-65b8-4831-bc2a-075b55ad2d88","body":"已核对资料，正在整理交付"}`, "b", "", human)
	require.Equal(t, 201, status, out)
	status, out = f.call(t, f.h.sharedSubmit, "POST", `{"requestId":"human-result","expectedVersion":0,"title":"人工完成成果","body":"资料核对后的交付正文"}`, "b", "", human)
	require.Equal(t, 201, status, out)
	result := out["result"].(map[string]any)["id"].(string)
	f.tick(t)
	status, out = f.call(t, f.h.sharedReviewResult, "POST", `{"requestId":"human-accept","expectedVersion":1,"decision":"accept","reason":"核验通过"}`, "b", result, owner)
	require.Equal(t, 200, status, out)
	f.tick(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, "done", f.tasks["b"]["status"])
	stages := []string{}
	for _, returned := range f.returns {
		if returned["resultId"] == result {
			require.EqualValues(t, 42, returned["conversationId"])
			stages = append(stages, returned["stage"].(string))
		}
	}
	require.ElementsMatch(t, []string{"submitted", "accept"}, stages)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "native-owner")
}
