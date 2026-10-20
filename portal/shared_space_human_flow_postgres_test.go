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
	mapUser("u-owner", "native-owner") // Same explicit association is safe to repeat.
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
	// A human can pass work to another colleague and receive it back without
	// losing the stopped Agent's original conversation provenance.
	for _, turn := range []struct {
		request, target string
		actor           identity
	}{
		{"human-to-colleague", "u-x", human}, {"colleague-back", "u-owner", owner},
	} {
		token = assignmentToken()
		status, out = f.call(t, f.h.sharedHandoffTask, "POST", fmt.Sprintf(`{"requestId":%q,"expectedAssignmentToken":%q,"memberUserId":%q,"reason":"接续协作资料"}`, turn.request, token, turn.target), "b", "", turn.actor)
		require.Equal(t, 200, status, out)
		require.Equal(t, "applied", out["phase"])
		var inherited string
		require.NoError(t, f.db.QueryRow(`SELECT source_binding_id FROM shared_space_handoffs WHERE request_id=$1`, turn.request).Scan(&inherited))
		require.Equal(t, "source-human", inherited)
	}
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
	status, out = f.call(t, f.h.sharedReviewResult, "POST", `{"requestId":"human-feedback","expectedVersion":1,"decision":"request_changes","reason":"请补充核验依据"}`, "b", result, owner)
	require.Equal(t, 200, status, out)
	f.tick(t)
	var sourceGeneration int
	require.NoError(t, f.db.QueryRow(`SELECT generation FROM shared_space_runtime_bindings WHERE id='source-human'`).Scan(&sourceGeneration))
	require.Equal(t, 1, sourceGeneration, "human feedback must not restart the retired Agent")
	firstResult := result
	status, out = f.call(t, f.h.sharedSubmit, "POST", `{"requestId":"human-result-revised","expectedVersion":1,"title":"人工完成成果修订","body":"补齐核验依据的交付正文"}`, "b", "", human)
	require.Equal(t, 201, status, out)
	result = out["result"].(map[string]any)["id"].(string)
	f.tick(t)
	status, out = f.call(t, f.h.sharedReviewResult, "POST", `{"requestId":"human-accept","expectedVersion":2,"decision":"accept","reason":"核验通过"}`, "b", result, owner)
	require.Equal(t, 200, status, out)
	f.tick(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, "done", f.tasks["b"]["status"])
	stages := []string{}
	firstStages := []string{}
	for _, returned := range f.returns {
		if returned["resultId"] == firstResult {
			firstStages = append(firstStages, returned["stage"].(string))
		}
		if returned["resultId"] == result {
			require.EqualValues(t, 42, returned["conversationId"])
			stages = append(stages, returned["stage"].(string))
		}
	}
	require.ElementsMatch(t, []string{"submitted", "accept"}, stages)
	require.ElementsMatch(t, []string{"submitted", "request_changes"}, firstStages)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "native-owner")
}

func TestSharedSpacesPostgresColleagueStartsAndReturnsTaskToAgent(t *testing.T) {
	f := newDependencyPGFixture(t)
	owner := identity{UserID: "u-x", DeploymentID: "deployment-acme"}
	colleague := identity{UserID: "u-owner", DeploymentID: "deployment-acme"}
	mapping := func(w http.ResponseWriter, r *http.Request, i identity) error {
		r.SetPathValue("user", "u-owner")
		return f.h.sharedHumanMapping(w, r, i)
	}
	status, out := f.call(t, mapping, "PUT", `{"requestId":"link-colleague","paperclipUserId":"native-owner"}`, "a", "", owner)
	require.Equal(t, 200, status, out)
	f.h.cfg.adminUsers[owner.UserID] = true
	status, out = f.call(t, f.h.sharedHumanAssignees, "GET", "", "a", "", owner)
	delete(f.h.cfg.adminUsers, owner.UserID)
	require.Equal(t, 200, status, out)
	require.Len(t, out["members"], 2)
	require.Len(t, out["nativeUsers"], 2)
	token := func() string {
		status, out = f.call(t, f.h.sharedTaskDetail, "GET", "", "a", "", owner)
		require.Equal(t, 200, status, out)
		return out["assignment"].(map[string]any)["token"].(string)
	}
	assign := func(action string) func(http.ResponseWriter, *http.Request, identity) error {
		return func(w http.ResponseWriter, r *http.Request, i identity) error {
			r.SetPathValue("assignmentAction", action)
			return f.h.sharedAssignmentChange(w, r, i)
		}
	}
	status, out = f.call(t, assign("assignment"), "POST", fmt.Sprintf(`{"requestId":"give-colleague","expectedAssignmentToken":%q,"memberUserId":"u-owner"}`, token()), "a", "", owner)
	require.Equal(t, 200, status, out)
	require.Equal(t, "todo", out["task"].(map[string]any)["status"])
	status, out = f.call(t, assign("claim"), "POST", fmt.Sprintf(`{"requestId":"colleague-starts","expectedAssignmentToken":%q,"memberUserId":"u-owner"}`, token()), "a", "", colleague)
	require.Equal(t, 200, status, out)
	require.Equal(t, "in_progress", out["task"].(map[string]any)["status"])
	status, out = f.call(t, f.h.sharedHandoffTask, "POST", fmt.Sprintf(`{"requestId":"human-to-agent","expectedAssignmentToken":%q,"participantId":"participant","reason":"请搭档继续整理"}`, token()), "a", "", colleague)
	require.Equal(t, 200, status, out)
	require.Equal(t, "applied", out["phase"])
	ws := runtimeWorkspace(t, f.h)
	bind := func(w http.ResponseWriter, r *http.Request, i identity) error {
		r.SetPathValue("action", "space-bind")
		return f.h.sharedRuntimeAgent(w, r, i, ws, "assistant")
	}
	status, out = f.call(t, bind, "POST", `{"spaceId":"space","taskId":"a","sourceConversationId":42,"requestId":"resume-agent","title":"整理资料","instruction":"继续同事交接的任务"}`, "a", "", owner)
	require.Equal(t, 202, status, out)
	f.tick(t)
	var binding runtimeBinding
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &binding))
	require.NoError(t, f.db.QueryRow(`SELECT pi_task_id,binding_state FROM shared_space_runtime_bindings WHERE id=$1`, binding.ID).Scan(&binding.PiTaskID, &binding.BindingState))
	require.Equal(t, "pi-a", binding.PiTaskID)
	require.Equal(t, "active", binding.BindingState)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Equal(t, 1, f.starts["a"])
	require.Equal(t, "agent", f.tasks["a"]["assigneeAgentId"])
	require.Equal(t, "in_progress", f.tasks["a"]["status"])
}
