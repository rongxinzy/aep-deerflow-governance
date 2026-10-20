package main

import (
	"fmt"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSharedSpacesPostgresMetadata(t *testing.T) {
	f := newDependencyPGFixture(t)
	owner := identity{UserID: "u-x", DeploymentID: "deployment-acme"}
	status, out := f.call(t, f.h.sharedCreateTask, "POST", `{"requestId":"create","title":"initial","description":"draft","priority":"medium"}`, "", "", owner)
	require.Equal(t, 201, status, out)
	detail := func() map[string]any {
		status, out := f.call(t, f.h.sharedTaskDetail, "GET", "", "created", "", owner)
		require.Equal(t, 200, status, out)
		return out
	}
	first := detail()
	require.True(t, first["capabilities"].(map[string]any)["editMetadata"].(bool))
	input := func(request string, token any, title string) string {
		return fmt.Sprintf(`{"requestId":%q,"expectedMetadataToken":%q,"title":%q,"description":"updated body","priority":"high"}`, request, token, title)
	}
	raw := input("edit", first["metadataToken"], "updated")
	status, out = f.call(t, f.h.sharedSetMetadata, "PATCH", raw, "created", "", owner)
	require.Equal(t, 200, status, out)
	require.Equal(t, "applied", out["state"])
	second := detail()
	require.Equal(t, "updated", second["task"].(map[string]any)["title"])
	require.Equal(t, out["metadataToken"], second["metadataToken"])
	status, out = f.call(t, f.h.sharedTasks, "GET", "", "", "", owner)
	require.Equal(t, 200, status, out)
	found := false
	for _, v := range out["tasks"].([]any) {
		task := v.(map[string]any)
		if task["id"] == "created" {
			found = true
			require.Equal(t, "updated body", task["description"])
			require.Equal(t, "high", task["priority"])
		}
	}
	require.True(t, found)
	status, out = f.call(t, f.h.sharedSetMetadata, "PATCH", input("stale", first["metadataToken"], "stale"), "created", "", owner)
	require.Equal(t, 409, status, out)
	require.NoError(t, f.db.Close())
	f.db = stdlib.OpenDB(*f.cfg)
	f.h.db = f.db
	t.Cleanup(func() { _ = f.db.Close() })
	status, out = f.call(t, f.h.sharedSetMetadata, "PATCH", raw, "created", "", owner)
	require.Equal(t, 200, status, out)
	require.Equal(t, "updated", out["task"].(map[string]any)["title"])
	_, err := f.db.Exec(`INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES('space','viewer','viewer',1)`)
	require.NoError(t, err)
	status, out = f.call(t, f.h.sharedSetMetadata, "PATCH", input("viewer", second["metadataToken"], "no"), "created", "", identity{UserID: "viewer", DeploymentID: "deployment-acme"})
	require.Equal(t, 403, status, out)
	f.mu.Lock()
	f.tasks["created"]["assigneeAgentId"] = "agent"
	f.loseMetadata = true
	f.mu.Unlock()
	raw = input("unknown", second["metadataToken"], "recovered")
	status, out = f.call(t, f.h.sharedSetMetadata, "PATCH", raw, "created", "", owner)
	require.Equal(t, 423, status, out)
	status, out = f.call(t, f.h.sharedSetMetadata, "PATCH", raw, "created", "", owner)
	require.Equal(t, 200, status, out)
	f.mu.Lock()
	require.Equal(t, 2, f.metadataPatches)
	f.tasks["created"]["status"] = "in_progress"
	f.mu.Unlock()
	third := detail()
	require.False(t, third["capabilities"].(map[string]any)["editMetadata"].(bool))
	status, out = f.call(t, f.h.sharedSetMetadata, "PATCH", input("running", third["metadataToken"], "no"), "created", "", owner)
	require.Equal(t, 409, status, out)
	f.mu.Lock()
	f.tasks["created"]["status"] = "todo"
	f.mu.Unlock()
	_, err = f.db.Exec(`INSERT INTO shared_space_runtime_bindings(id,space_id,task_id,workspace_name,workspace_uid,owner_id,companion,source_conversation_id,generation,execution_id,expected_version,bind_hash,assignment_hash,participant_id,participant_version,binding_state,assignment_epoch) VALUES('metadata-bound','space','created','ws-source','runtime-test-uid','u-x','assistant',42,1,'execution-metadata',0,'metadata','metadata','participant',1,'active',0)`)
	require.NoError(t, err)
	bound := detail()
	require.False(t, bound["capabilities"].(map[string]any)["editMetadata"].(bool))
	status, out = f.call(t, f.h.sharedSetMetadata, "PATCH", input("bound", bound["metadataToken"], "no"), "created", "", owner)
	require.Equal(t, 409, status, out)
	var epoch int
	require.NoError(t, f.db.QueryRow(`SELECT assignment_epoch FROM shared_space_runtime_bindings WHERE id='metadata-bound'`).Scan(&epoch))
	require.Zero(t, epoch)
	var count int
	require.NoError(t, f.db.QueryRow(`SELECT count(*) FROM shared_space_commands WHERE action='task_metadata'`).Scan(&count))
	require.Equal(t, 2, count)
}
