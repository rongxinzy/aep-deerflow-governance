// Workspace rooms proxy specs: the room orchestrator lives at the gateway
// level, so the proxy rewrites /rooms/{rest...} to gateway /api/rooms/{rest...}
// with the session identity stamped as owner; unlike chat there is no
// companion segment and no memkey lookup.
package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func workspaceRoomsFixture(t *testing.T, ws *dev1alpha1.PersonalWorkspace) (*apiTestPortal, *[]string) {
	t.Helper()
	var mu sync.Mutex
	paths := &[]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*paths = append(*paths, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" owner="+r.Header.Get(workspaceOwnerHeaderName))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)

	h := newWorkspaceHarness(t, ws)
	h.workspaceChatOverride = upstream.URL
	return h, paths
}

func TestOwnerScopedRoomsProxy(t *testing.T) {
	ws := runningWorkspace("ws-mine", "u-owner", "assistant", "researcher")

	t.Run("rewrites to the gateway rooms path with the owner identity", func(t *testing.T) {
		h, paths := workspaceRoomsFixture(t, ws)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/rooms/room-1/stream", nil)
		req.SetPathValue("rest", "room-1/stream")
		w := httptest.NewRecorder()
		h.handleRooms(w, req, ownerIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		require.Len(t, *paths, 1)
		assert.Contains(t, (*paths)[0], "GET /api/rooms/room-1/stream")
		assert.Contains(t, (*paths)[0], "owner=u-owner")
	})

	t.Run("list hits the gateway rooms root (empty rest)", func(t *testing.T) {
		h, paths := workspaceRoomsFixture(t, ws)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/rooms", nil)
		req.SetPathValue("rest", "")
		w := httptest.NewRecorder()
		h.handleRooms(w, req, ownerIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		// "? " 紧跟根路径：证明无尾斜杠（弱 contains 旧实现 /api/rooms/ 也过）
		assert.Contains(t, (*paths)[0], "GET /api/rooms? owner")
	})

	t.Run("query strings survive the rewrite", func(t *testing.T) {
		h, paths := workspaceRoomsFixture(t, ws)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/rooms/room-1/messages?x=1", nil)
		req.SetPathValue("rest", "room-1/messages")
		w := httptest.NewRecorder()
		h.handleRooms(w, req, ownerIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, (*paths)[0], "?x=1")
	})

	t.Run("a stranger has no rooms surface (no workspace resolved)", func(t *testing.T) {
		h, paths := workspaceRoomsFixture(t, ws)
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.SetPathValue("rest", "")
		w := httptest.NewRecorder()
		h.handleRooms(w, req, identity{UserID: "u-stranger"})
		// Owner-scoped resolution: a user without a workspace gets 404 — no
		// cross-user access and no workspace-existence leak.
		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.Empty(t, *paths)
	})

	t.Run("requires the Running phase", func(t *testing.T) {
		pending := runningWorkspace("ws-mine", "u-owner", "assistant")
		pending.Status.Phase = dev1alpha1.WorkspacePhasePending
		h, paths := workspaceRoomsFixture(t, pending)
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.SetPathValue("rest", "")
		w := httptest.NewRecorder()
		h.handleRooms(w, req, ownerIdentity())
		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Empty(t, *paths)
	})
}
