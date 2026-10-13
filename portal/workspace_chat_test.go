// Workspace chat proxy specs: an httptest upstream records the rewritten
// request (path + stamped headers) the way chat_test.go does for employees.
package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func workspaceMemkey(workspace, companion string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceMemkeySecretName(workspace, companion), Namespace: "digital-employees"},
		Data:       map[string][]byte{"WORKSPACE_INTERNAL_TOKEN": []byte("wstok")},
	}
}

func workspaceChatFixture(t *testing.T, ws *dev1alpha1.PersonalWorkspace, secrets ...*corev1.Secret) (*apiTestPortal, *httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	paths := &[]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*paths = append(*paths, r.Method+" "+r.URL.Path+" tok="+r.Header.Get(workspaceTokenHeader)+" owner="+r.Header.Get(workspaceOwnerHeaderName))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)

	h := newWorkspaceHarness(t, ws)
	for _, s := range secrets {
		require.NoError(t, h.k8s.Create(t.Context(), s))
	}
	h.workspaceChatOverride = upstream.URL
	return h, upstream, paths
}

func TestWorkspaceChatProxy(t *testing.T) {
	ws := runningWorkspace("ws-mine", "u-owner", "assistant", "researcher")

	t.Run("rewrites to the companion prefix with the internal token", func(t *testing.T) {
		h, _, paths := workspaceChatFixture(t, ws, workspaceMemkey("ws-mine", "assistant"))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-mine/assistant/chat/api/conversations/7", nil)
		req.SetPathValue("name", "ws-mine")
		req.SetPathValue("companion", "assistant")
		req.SetPathValue("rest", "api/conversations/7")
		w := httptest.NewRecorder()
		h.handleWorkspaceChat(w, req, ownerIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		require.Len(t, *paths, 1)
		assert.Contains(t, (*paths)[0], "GET /assistant/api/conversations/7")
		assert.Contains(t, (*paths)[0], "tok=wstok")
		assert.Contains(t, (*paths)[0], "owner=u-owner")
	})

	t.Run("rejects non-owner", func(t *testing.T) {
		h, _, paths := workspaceChatFixture(t, ws, workspaceMemkey("ws-mine", "assistant"))
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.SetPathValue("name", "ws-mine")
		req.SetPathValue("companion", "assistant")
		req.SetPathValue("rest", "api/healthz")
		w := httptest.NewRecorder()
		h.handleWorkspaceChat(w, req, identity{UserID: "u-stranger"})
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Empty(t, *paths)
	})

	t.Run("requires the Running phase", func(t *testing.T) {
		pending := runningWorkspace("ws-mine", "u-owner", "assistant")
		pending.Status.Phase = dev1alpha1.WorkspacePhasePending
		h, _, paths := workspaceChatFixture(t, pending, workspaceMemkey("ws-mine", "assistant"))
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.SetPathValue("name", "ws-mine")
		req.SetPathValue("companion", "assistant")
		req.SetPathValue("rest", "api/healthz")
		w := httptest.NewRecorder()
		h.handleWorkspaceChat(w, req, ownerIdentity())
		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Empty(t, *paths)
	})

	t.Run("rejects companions outside the roster", func(t *testing.T) {
		h, _, paths := workspaceChatFixture(t, ws, workspaceMemkey("ws-mine", "assistant"))
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.SetPathValue("name", "ws-mine")
		req.SetPathValue("companion", "ghost")
		req.SetPathValue("rest", "api/healthz")
		w := httptest.NewRecorder()
		h.handleWorkspaceChat(w, req, ownerIdentity())
		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.Empty(t, *paths)
	})

	t.Run("fails closed when the memkey secret is missing", func(t *testing.T) {
		h, _, paths := workspaceChatFixture(t, ws)
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.SetPathValue("name", "ws-mine")
		req.SetPathValue("companion", "assistant")
		req.SetPathValue("rest", "api/healthz")
		w := httptest.NewRecorder()
		h.handleWorkspaceChat(w, req, ownerIdentity())
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Empty(t, *paths)
	})
}
