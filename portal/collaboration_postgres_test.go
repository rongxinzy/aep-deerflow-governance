package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// An isolated schema exercises real PostgreSQL claims and durable return receipts.
// Runtime model/tool isolation and SQLite restart are tested in pi-durable-bench.
func TestCollaborationPostgresRelayRoundTrip(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("collaboration_%d", time.Now().UnixNano())
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(collaborationSchema)
	require.NoError(t, err)
	h := collaborationHarness(t)
	h.db = db
	var mu sync.Mutex
	deliveries := map[string]map[string]any{}
	calls := 0
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		require.Equal(t, "wstok", r.Header.Get(collaborationTokenHeader))
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/assistant/internal/collaboration/respond" {
			calls++
			writeJSON(w, 200, map[string]string{"reply": "正在请本人确认。", "ownerQuestion": "周三下午可以吗？"})
			return
		}
		require.Equal(t, "/assistant/internal/collaboration/return", r.URL.Path)
		require.Equal(t, float64(23), input["conversationId"])
		deliveries[input["requestId"].(string)] = input
		writeJSON(w, 200, map[string]bool{"returned": true})
	}))
	defer runtime.Close()
	h.workspaceChatOverride = runtime.URL
	send := func() string {
		w := httptest.NewRecorder()
		h.sendCollaboration(w, collaborationRequest("POST", "/", `{"targetId":"ws-target/assistant","message":"帮我问周三评审","requestId":"stable-command","sourceConversationId":23}`), identity{UserID: "u-x"}, &collaborationSource{Workspace: "ws-source", Companion: "assistant"})
		require.Equal(t, 202, w.Code, w.Body.String())
		var result map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		return result["id"].(string)
	}
	id := send()
	require.Equal(t, id, send())
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM companion_collaboration_messages`).Scan(&count))
	require.Equal(t, 1, count)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.processCollaboration(context.Background()) }()
	}
	wg.Wait()
	mu.Lock()
	require.Equal(t, 1, calls)
	mu.Unlock()
	mu.Lock()
	require.Len(t, deliveries, 1)
	mu.Unlock()
	messages, err := h.collaborationMessages(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, "waiting_owner", messages[0].State)
	req := collaborationRequest("POST", "/", fmt.Sprintf(`{"messageId":%q,"text":"周三下午可以"}`, messages[0].ID))
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	h.replyCollaboration(w, req, ownerIdentity())
	require.Equal(t, 200, w.Code, w.Body.String())
	// A newly opened pool/portal observes the answer and returns it once.
	restarted := stdlib.OpenDB(*cfg)
	defer func() { require.NoError(t, restarted.Close()) }()
	h.db = restarted
	require.True(t, h.processCollaboration(t.Context()))
	require.False(t, h.processCollaboration(t.Context()))
	mu.Lock()
	require.Len(t, deliveries, 2)
	mu.Unlock()
	mu.Lock()
	require.Equal(t, 1, calls)
	mu.Unlock()
	messages, err = h.collaborationMessages(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "owner", messages[0].Returned)
	require.Equal(t, "周三下午可以", messages[0].OwnerReply)
	_, err = h.loadCollaboration(t.Context(), id, "unrelated-user")
	require.Error(t, err)
	// Simulated portal failure after delivery, before marking returned: same receipt id.
	_, err = db.Exec(`UPDATE companion_collaboration_messages SET returned='',attempt_at=NULL WHERE thread_id=$1`, id)
	require.NoError(t, err)
	require.True(t, h.processCollaboration(t.Context()))
	mu.Lock()
	require.Len(t, deliveries, 2)
	mu.Unlock()
}
