package main

import (
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/url"
	"testing"
)

func TestSharedSpacesPostgresLabels(t *testing.T) {
	f := newDependencyPGFixture(t)
	f.labels = []sharedCompanyLabel{}
	owner := identity{UserID: "u-x", DeploymentID: "deployment-acme"}
	create := func(request, name, color string) (int, map[string]any) {
		return f.call(t, f.h.sharedCreateLabel, "POST", fmt.Sprintf(`{"requestId":%q,"name":%q,"color":%q}`, request, name, color), "", "", owner)
	}
	status, out := create("first", "研发", "#AABBCC")
	require.Equal(t, 200, status, out)
	require.False(t, out["reused"].(bool))
	first := out["label"].(map[string]any)["id"].(string)
	status, out = create("reuse", "研发", "#aabbcc")
	require.Equal(t, 200, status, out)
	require.True(t, out["reused"].(bool))
	status, out = create("different-color", "研发", "#112233")
	require.Equal(t, 409, status, out)
	status, out = create("second", "紧急", "#112233")
	require.Equal(t, 200, status, out)
	second := out["label"].(map[string]any)["id"].(string)
	detail := func() map[string]any {
		status, out := f.call(t, f.h.sharedTaskDetail, "GET", "", "a", "", owner)
		require.Equal(t, 200, status, out)
		return out
	}
	initial := detail()
	require.True(t, initial["capabilities"].(map[string]any)["editLabels"].(bool))
	set := func(request string, token any, ids []string, actor identity) (int, map[string]any) {
		raw, _ := json.Marshal(map[string]any{"requestId": request, "expectedLabelsToken": token, "labelIds": ids})
		return f.call(t, f.h.sharedSetTaskLabels, "PUT", string(raw), "a", "", actor)
	}
	status, out = set("add", initial["labelsToken"], []string{second, first, first}, owner)
	require.Equal(t, 200, status, out)
	token := out["labelsToken"]
	current := detail()
	require.Len(t, current["task"].(map[string]any)["labels"], 2)
	require.Equal(t, token, current["labelsToken"])
	status, out = set("stale", initial["labelsToken"], []string{}, owner)
	require.Equal(t, 409, status, out)
	_, err := f.db.Exec(`INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES('space','viewer','viewer',1)`)
	require.NoError(t, err)
	viewer := identity{UserID: "viewer", DeploymentID: "deployment-acme"}
	status, out = f.call(t, f.h.sharedLabels, "GET", "", "", "", viewer)
	require.Equal(t, 200, status, out)
	require.Equal(t, "company", out["scope"])
	require.False(t, out["capabilities"].(map[string]any)["createLabel"].(bool))
	require.NotContains(t, fmt.Sprint(out["labels"]), "companyId")
	status, out = f.call(t, f.h.sharedCreateLabel, "POST", `{"requestId":"viewer","name":"viewer","color":"#123456"}`, "", "", viewer)
	require.Equal(t, 403, status, out)
	status, out = set("viewer-set", token, []string{}, viewer)
	require.Equal(t, 403, status, out)
	status, out = set("foreign-label", token, []string{"foreign-company-label"}, owner)
	require.Equal(t, 400, status, out)
	query := url.Values{"labelId": {first}, "q": {"任务"}, "limit": {"1"}, "offset": {"1"}}
	f.mu.Lock()
	f.tasks["b"]["labelIds"] = []string{first}
	f.tasks["c"]["labelIds"] = []string{first}
	f.mu.Unlock()
	filtered := func(w http.ResponseWriter, r *http.Request, i identity) error {
		r.URL.RawQuery = query.Encode()
		return f.h.sharedTasks(w, r, i)
	}
	status, out = f.call(t, filtered, "GET", "", "", "", viewer)
	require.Equal(t, 200, status, out)
	require.True(t, out["hasMore"].(bool))
	require.Equal(t, "b", out["tasks"].([]any)[0].(map[string]any)["id"])
	f.mu.Lock()
	nativeQuery, err := url.ParseQuery(f.lastLabelQuery)
	f.mu.Unlock()
	require.NoError(t, err)
	require.Equal(t, "project", nativeQuery.Get("projectId"))
	require.Equal(t, "true", nativeQuery.Get("includeBlockedBy"))
	require.Equal(t, "任务", nativeQuery.Get("q"))
	require.Equal(t, "2", nativeQuery.Get("limit"))
	require.Equal(t, "1", nativeQuery.Get("offset"))
	status, out = set("clear", token, []string{}, owner)
	require.Equal(t, 200, status, out)
	token = out["labelsToken"]
	f.mu.Lock()
	f.tasks["a"]["status"] = "in_progress"
	f.tasks["a"]["assigneeAgentId"] = "agent"
	f.tasks["a"]["updatedAt"] = "heartbeat"
	f.mu.Unlock()
	_, err = f.db.Exec(`INSERT INTO shared_space_runtime_bindings(id,space_id,task_id,workspace_name,workspace_uid,owner_id,companion,source_conversation_id,generation,execution_id,expected_version,bind_hash,assignment_hash,participant_id,participant_version,binding_state,assignment_epoch) VALUES('labels-bound','space','a','ws-source','runtime-test-uid','u-x','assistant',42,1,'labels-execution',0,'labels','labels','participant',1,'active',7)`)
	require.NoError(t, err)
	running := detail()
	require.True(t, running["capabilities"].(map[string]any)["editLabels"].(bool))
	require.Equal(t, token, running["labelsToken"])
	f.mu.Lock()
	f.loseLabelPatch = true
	f.mu.Unlock()
	status, out = set("unknown", token, []string{second}, owner)
	require.Equal(t, 423, status, out)
	require.NoError(t, f.db.Close())
	f.db = stdlib.OpenDB(*f.cfg)
	f.h.db = f.db
	t.Cleanup(func() { _ = f.db.Close() })
	status, out = set("unknown", token, []string{second}, owner)
	require.Equal(t, 200, status, out)
	require.Equal(t, "in_progress", out["task"].(map[string]any)["status"])
	require.Equal(t, "agent", out["task"].(map[string]any)["assigneeAgentId"])
	var epoch int
	require.NoError(t, f.db.QueryRow(`SELECT assignment_epoch FROM shared_space_runtime_bindings WHERE id='labels-bound'`).Scan(&epoch))
	require.Equal(t, 7, epoch)
	f.mu.Lock()
	require.Equal(t, 3, f.labelPatches)
	f.loseLabelPost = true
	f.mu.Unlock()
	status, out = create("unknown-create", "后续", "#abcdef")
	require.Equal(t, 423, status, out)
	status, out = create("unknown-create", "后续", "#abcdef")
	require.Equal(t, 200, status, out)
	f.mu.Lock()
	require.Equal(t, 3, f.labelPosts)
	f.mu.Unlock()
}
