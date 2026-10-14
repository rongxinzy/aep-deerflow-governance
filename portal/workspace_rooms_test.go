// Workspace rooms proxy specs: the room orchestrator lives at the gateway
// level, so the proxy rewrites /rooms/{rest...} to gateway /api/rooms/{rest...}
// with the session identity stamped as owner; unlike chat there is no
// companion segment and no memkey lookup. The roster-authorization specs
// below pin FR-CP-14: agent members are digital employees the gateway
// dispatches A2A to under the owner's act-as identity.
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func workspaceRoomsFixture(t *testing.T, ws *dev1alpha1.PersonalWorkspace, objects ...runtime.Object) (*apiTestPortal, *[]string) {
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

	args := append([]runtime.Object{ws}, objects...)
	h := newWorkspaceHarness(t, args...)
	h.workspaceChatOverride = upstream.URL
	return h, paths
}

// roomAgentEmployee builds a digital-employee CR for the roster gate specs.
func roomAgentEmployee(name, owner string, visibility *dev1alpha1.VisibilitySpec, phase dev1alpha1.DigitalEmployeePhase) *dev1alpha1.DigitalEmployee {
	return &dev1alpha1.DigitalEmployee{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "digital-employees"},
		Spec: dev1alpha1.DigitalEmployeeSpec{
			Owner: owner, Visibility: visibility,
			A2A: &dev1alpha1.A2ASpec{Image: "a2a-adapter:0.2.1"},
		},
		Status: dev1alpha1.DigitalEmployeeStatus{Phase: phase},
	}
}

func postRooms(t *testing.T, h *apiTestPortal, rest, body string, ident identity) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/rooms/"+rest, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetPathValue("rest", rest)
	w := httptest.NewRecorder()
	h.handleRooms(w, req, ident)
	return w
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
		// The send path reads the roster back first (send gate), then the
		// forwarded POST must still carry the query string.
		require.Len(t, *paths, 2)
		assert.Contains(t, (*paths)[0], "GET /api/rooms/room-1")
		assert.Contains(t, (*paths)[1], "POST /api/rooms/room-1/messages?x=1")
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

// roomsAuthFixture records "METHOD path body owner" per upstream request and
// lets each subtest script the gateway's responses (the send gate reads the
// roster back from the gateway before allowing a dispatch).
func roomsAuthFixture(t *testing.T, ws *dev1alpha1.PersonalWorkspace, objects []runtime.Object, handler http.HandlerFunc) (*apiTestPortal, *[]string) {
	t.Helper()
	var mu sync.Mutex
	seen := &[]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*seen = append(*seen, r.Method+" "+r.URL.Path+" "+string(body)+" owner="+r.Header.Get(workspaceOwnerHeaderName))
		mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	h := newWorkspaceHarness(t, append([]runtime.Object{ws}, objects...)...)
	h.workspaceChatOverride = upstream.URL
	return h, seen
}

func TestRoomCreateRosterGate(t *testing.T) {
	ws := runningWorkspace("ws-mine", "u-owner", "assistant")
	restricted := &dev1alpha1.VisibilitySpec{Mode: "restricted", Teams: []dev1alpha1.SubjectRef{{ID: "team-other", Name: "Other"}}}

	t.Run("agent member the owner is not entitled to is refused before dispatch", func(t *testing.T) {
		employee := roomAgentEmployee("d1-helper", "u-someone-else", restricted, dev1alpha1.PhaseReady)
		h, seen := roomsAuthFixture(t, ws, []runtime.Object{employee}, nil)
		w := postRooms(t, h, "", `{"title":"群","members":[{"kind":"agent","name":"d1-helper"}]}`, ownerIdentity())
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Contains(t, w.Body.String(), "not authorized for this digital employee")
		assert.Empty(t, *seen, "nothing may reach the gateway when the roster is refused")
	})

	t.Run("entitled agent member forwards the create byte-identical", func(t *testing.T) {
		employee := roomAgentEmployee("d1-helper", "u-owner", nil, dev1alpha1.PhaseReady)
		h, seen := roomsAuthFixture(t, ws, []runtime.Object{employee}, nil)
		body := `{"title":"群","members":["assistant",{"kind":"agent","name":"d1-helper"}]}`
		w := postRooms(t, h, "", body, ownerIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		require.Len(t, *seen, 1)
		assert.Contains(t, (*seen)[0], "POST /api/rooms "+body)
	})

	t.Run("agent member without A2A gets a clear 400", func(t *testing.T) {
		employee := roomAgentEmployee("d1-helper", "u-owner", &dev1alpha1.VisibilitySpec{Mode: "all"}, dev1alpha1.PhaseReady)
		employee.Spec.A2A = nil
		h, seen := roomsAuthFixture(t, ws, []runtime.Object{employee}, nil)
		w := postRooms(t, h, "", `{"title":"群","members":[{"kind":"agent","name":"d1-helper"}]}`, ownerIdentity())
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "A2A is not enabled")
		assert.Empty(t, *seen)
	})

	t.Run("unknown agent employee is a 404", func(t *testing.T) {
		h, seen := roomsAuthFixture(t, ws, nil, nil)
		w := postRooms(t, h, "", `{"title":"群","members":[{"kind":"agent","name":"ghost"}]}`, ownerIdentity())
		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.Contains(t, w.Body.String(), "digital employee not found")
		assert.Empty(t, *seen)
	})

	t.Run("kind match mirrors the gateway: only exact agent strings gate", func(t *testing.T) {
		// "Agent" is coerced to human by the gateway and dies there unless it
		// names a real companion — the portal must not treat it as an agent.
		h, seen := roomsAuthFixture(t, ws, nil, nil)
		w := postRooms(t, h, "", `{"title":"群","members":[{"kind":"Agent","name":"ghost"}]}`, ownerIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
		require.Len(t, *seen, 1)
	})

	t.Run("human-only members forward without employee lookups", func(t *testing.T) {
		h, seen := roomsAuthFixture(t, ws, nil, nil)
		w := postRooms(t, h, "", `{"title":"群","members":["assistant"]}`, ownerIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
		require.Len(t, *seen, 1)
	})

	t.Run("undecodable create body fails closed", func(t *testing.T) {
		h, seen := roomsAuthFixture(t, ws, nil, nil)
		w := postRooms(t, h, "", `{"title":`, ownerIdentity())
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Empty(t, *seen)
	})

	t.Run("pending-phase agent member is a 409", func(t *testing.T) {
		employee := roomAgentEmployee("d1-helper", "u-owner", nil, dev1alpha1.PhasePending)
		h, seen := roomsAuthFixture(t, ws, []runtime.Object{employee}, nil)
		w := postRooms(t, h, "", `{"title":"群","members":[{"kind":"agent","name":"d1-helper"}]}`, ownerIdentity())
		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Contains(t, w.Body.String(), "not Ready")
		assert.Empty(t, *seen)
	})
}

func TestRoomSendRosterGate(t *testing.T) {
	ws := runningWorkspace("ws-mine", "u-owner", "assistant")
	// Legacy-room shape: the roster lives in the gateway's sqlite, so the gate
	// reads it back per send — this CR exists but the identity has no entitlement.
	restricted := &dev1alpha1.VisibilitySpec{Mode: "restricted", Teams: []dev1alpha1.SubjectRef{{ID: "team-other"}}}
	unauthorized := roomAgentEmployee("d1-helper", "u-someone-else", restricted, dev1alpha1.PhaseReady)
	authorized := roomAgentEmployee("d2-sales", "u-owner", nil, dev1alpha1.PhaseSleeping)

	rosterHandler := func(entries string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/rooms/r-legacy") {
				_, _ = w.Write([]byte(`{"room":{"id":"r-legacy","memberEntries":` + entries + `}}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}

	t.Run("send re-checks the persisted roster and refuses cross-domain members", func(t *testing.T) {
		entries := `[{"name":"d1-helper","kind":"agent"}]`
		h, seen := roomsAuthFixture(t, ws, []runtime.Object{unauthorized}, rosterHandler(entries))
		w := postRooms(t, h, "r-legacy/messages", `{"text":"@d1-helper 干活"}`, ownerIdentity())
		assert.Equal(t, http.StatusForbidden, w.Code)
		require.Len(t, *seen, 1, "only the roster read-back; the dispatch POST must not be forwarded")
		assert.Contains(t, (*seen)[0], "GET /api/rooms/r-legacy")
	})

	t.Run("send with an entitled roster forwards (sleeping counts as dispatchable)", func(t *testing.T) {
		entries := `[{"name":"assistant","kind":"human"},{"name":"d2-sales","kind":"agent"}]`
		h, seen := roomsAuthFixture(t, ws, []runtime.Object{authorized}, rosterHandler(entries))
		w := postRooms(t, h, "r-legacy/messages", `{"text":"@d2-sales 干活"}`, ownerIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
		require.Len(t, *seen, 2)
		assert.Contains(t, (*seen)[0], "GET /api/rooms/r-legacy")
		assert.Contains(t, (*seen)[1], "POST /api/rooms/r-legacy/messages")
	})

	t.Run("roster read failure fails closed", func(t *testing.T) {
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
		h, seen := roomsAuthFixture(t, ws, nil, handler)
		w := postRooms(t, h, "r-legacy/messages", `{"text":"hi"}`, ownerIdentity())
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.Len(t, *seen, 1, "the dispatch POST must not be forwarded when the roster cannot be read")
	})

	t.Run("send to a missing room forwards (the gateway answers the 404)", func(t *testing.T) {
		handler := func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
		h, seen := roomsAuthFixture(t, ws, nil, handler)
		w := postRooms(t, h, "r-none/messages", `{"text":"hi"}`, ownerIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
		require.Len(t, *seen, 2)
		assert.Contains(t, (*seen)[1], "POST /api/rooms/r-none/messages")
	})
}

func TestRoomsOwnerHeaderSpoof(t *testing.T) {
	ws := runningWorkspace("ws-mine", "u-owner", "assistant")
	employee := roomAgentEmployee("d1-helper", "u-owner", nil, dev1alpha1.PhaseReady)

	// The gateway honors X-BotUI-User as an owner source too (BFF direct
	// mode); a browser-supplied one must never ride the portal proxy through
	// — the session stamp is the only owner the gateway may see.
	t.Run("caller-supplied X-BotUI-User is dropped at the proxy", func(t *testing.T) {
		var mu sync.Mutex
		forwarded := map[string]string{}
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			mu.Lock()
			forwarded["x-botui-user"] = r.Header.Get("X-BotUI-User")
			forwarded["owner"] = r.Header.Get(workspaceOwnerHeaderName)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(upstream.Close)
		h := newWorkspaceHarness(t, ws, employee)
		h.workspaceChatOverride = upstream.URL

		req := httptest.NewRequest(http.MethodPost, "/api/v1/rooms", strings.NewReader(`{"title":"群","members":["assistant"]}`))
		req.SetPathValue("rest", "")
		req.Header.Set("X-BotUI-User", "u-victim") // spoof attempt
		w := httptest.NewRecorder()
		h.handleRooms(w, req, ownerIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, forwarded["x-botui-user"], "spoofed owner header must not reach the gateway")
		assert.Equal(t, "u-owner", forwarded["owner"])
	})
}
