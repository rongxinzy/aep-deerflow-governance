// Owner-scoped rooms proxy: /api/v1/rooms/{rest...} -> the caller's workspace
// gateway /api/rooms/{rest...}. Rooms live at the gateway level (the room
// orchestrator fans out across companions), and workspaces are one-per-user by
// product invariant, so the workspace is resolved from the session identity
// instead of the URL — that also keeps this route disjoint from the
// {companion}/chat wildcard tree. The room SSE stream rides the same proxy
// with immediate flush.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// Room rosters are an authorization surface, not a UI convenience (FR-CP-14
// 跨域不提权): an agent member is a digital employee the gateway will call
// over A2A under the owner's act-as identity. The gateway cannot see
// employee CRs, so the portal re-checks the roster at the two dispatch
// entry points — create (where the roster is fixed) and send (the only
// dispatch trigger; also covers rooms created before this gate and
// visibility narrowed after creation). Human members resolve against the
// owner's own workspace companions inside the gateway — no cross-identity
// surface — so only kind=="agent" entries are checked, with the exact
// string match the gateway itself uses (anything else is coerced to human
// and rejected there unless it names a real companion).

const (
	// roomBodyLimit bounds the create body the portal inspects; past it the
	// request is abusive and failing closed is the right answer.
	roomBodyLimit = 1 << 20
	// roomRosterLimit bounds the roster read-back on the send path.
	roomRosterLimit = 4 << 20
)

// roomMemberObject mirrors the object form of one create-body member; the
// string form (human companion) never reaches this struct.
type roomMemberObject struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// roomAgentMembers extracts the digital-employee names from a create body.
// ok=false means the body could not be inspected — fail closed rather than
// forward what might hide an agent member.
func roomAgentMembers(raw []byte) ([]string, bool) {
	var body struct {
		Members []json.RawMessage `json:"members"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, false
	}
	var agents []string
	for _, item := range body.Members {
		var name string
		if json.Unmarshal(item, &name) == nil {
			continue // plain string: human companion
		}
		var member roomMemberObject
		if json.Unmarshal(item, &member) != nil {
			return nil, false
		}
		if member.Kind == "agent" {
			agents = append(agents, member.Name)
		}
	}
	return agents, true
}

// authorizeRoomAgentMembers applies the one-to-one chat rule (authorizeChat)
// to every agent member of a roster: the identity must be entitled to the
// employee and the employee must be dispatchable (Ready or Sleeping — the
// A2A adapter deployment stays resident while the runtime sleeps).
func (p *portal) authorizeRoomAgentMembers(ctx context.Context, names []string, ident identity) (int, string) {
	for _, name := range names {
		var employee dev1alpha1.DigitalEmployee
		if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: name}, &employee); err != nil {
			if apierrors.IsNotFound(err) {
				return http.StatusNotFound, fmt.Sprintf("agent member %q: digital employee not found", name)
			}
			return http.StatusInternalServerError, fmt.Sprintf("agent member %q: %v", name, err)
		}
		if employee.Spec.A2A == nil {
			return http.StatusBadRequest, fmt.Sprintf("agent member %q: A2A is not enabled for this digital employee", name)
		}
		if !ident.canAccessEmployee(p.cfg, employee.Spec.Team, employee.Spec.Owner, employee.Spec.Visibility) {
			return http.StatusForbidden, fmt.Sprintf("agent member %q: not authorized for this digital employee (owner, team member, or admin required)", name)
		}
		if employee.Status.Phase != dev1alpha1.PhaseReady && employee.Status.Phase != dev1alpha1.PhaseSleeping {
			return http.StatusConflict, fmt.Sprintf("agent member %q: digital employee is not Ready (phase %s)", name, employee.Status.Phase)
		}
	}
	return 0, ""
}

// authorizeRoomCreate validates the create body's agent members, then
// restores the body byte-identical for the proxy to forward.
func (p *portal) authorizeRoomCreate(w http.ResponseWriter, r *http.Request, ident identity) (int, string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, roomBodyLimit+1))
	r.Body = io.NopCloser(bytes.NewReader(raw)) // unconditional: the proxy re-reads it
	if err != nil {
		return http.StatusBadRequest, "could not read the room create body"
	}
	if len(raw) > roomBodyLimit {
		return http.StatusRequestEntityTooLarge, "room create body too large"
	}
	agents, ok := roomAgentMembers(raw)
	if !ok {
		return http.StatusBadRequest, "invalid room create body"
	}
	return p.authorizeRoomAgentMembers(r.Context(), agents, ident)
}

// authorizeRoomSend re-checks the room's persisted roster before a message
// dispatches (the only dispatch trigger). The roster is read back from the
// gateway under the same owner header the proxy itself stamps.
func (p *portal) authorizeRoomSend(ctx context.Context, target *url.URL, ident identity, roomID string) (int, string) {
	fetch, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String()+"/api/rooms/"+url.PathEscape(roomID), nil)
	if err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	fetch.Header.Set(workspaceOwnerHeaderName, ident.UserID)
	resp, err := serviceProbeClient.Do(fetch)
	if err != nil {
		return http.StatusBadGateway, "workspace gateway unreachable: " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return 0, "" // no room: forward and let the gateway answer the 404 itself
	}
	if resp.StatusCode != http.StatusOK {
		return http.StatusBadGateway, fmt.Sprintf("room roster unavailable (gateway %d): refusing to dispatch", resp.StatusCode)
	}
	var payload struct {
		Room struct {
			MemberEntries []roomMemberObject `json:"memberEntries"`
		} `json:"room"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, roomRosterLimit)).Decode(&payload); err != nil {
		return http.StatusBadGateway, "room roster unreadable: refusing to dispatch"
	}
	var agents []string
	for _, entry := range payload.Room.MemberEntries {
		if entry.Kind == "agent" {
			agents = append(agents, entry.Name)
		}
	}
	return p.authorizeRoomAgentMembers(ctx, agents, ident)
}

// authorizeRoomRequest gates the two dispatch entry points (create, send);
// every other rooms route is read-only or owner-scoped dissolve.
func (p *portal) authorizeRoomRequest(w http.ResponseWriter, r *http.Request, target *url.URL, rest string, ident identity) (int, string) {
	segments := strings.Split(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodPost:
		return p.authorizeRoomCreate(w, r, ident)
	case len(segments) == 2 && segments[1] == "messages" && r.Method == http.MethodPost:
		return p.authorizeRoomSend(r.Context(), target, ident, segments[0])
	}
	return 0, ""
}

// ownerWorkspace resolves the caller's PersonalWorkspace (first match; the
// self-service flow enforces one per user). Admins without their own
// workspace get a 404 here — cross-user room access is out of scope for now.
func (p *portal) ownerWorkspace(ctx context.Context, ident identity) (*dev1alpha1.PersonalWorkspace, int, string) {
	var list dev1alpha1.PersonalWorkspaceList
	if err := p.k8s.List(ctx, &list, client.InNamespace(p.cfg.namespace)); err != nil {
		return nil, statusOf(err), err.Error()
	}
	for i := range list.Items {
		if list.Items[i].Spec.Owner == ident.UserID {
			return &list.Items[i], 0, ""
		}
	}
	return nil, http.StatusNotFound, "no workspace provisioned for this user"
}

// handleRooms proxies one HTTP request to the owner's workspace gateway room
// orchestrator (companion/rooms.mjs): list/create rooms, @-routed messages,
// and the room SSE stream.
func (p *portal) handleRooms(w http.ResponseWriter, r *http.Request, ident identity) {
	p.handleOwnerWorkspaceGateway(w, r, ident, "/api/rooms")
}

const workspaceSessionHeader = "X-Workspace-Session-Id"

// Called only behind p.auth: derive an opaque binding from the credential that
// authentication actually selected, without forwarding that credential.
func workspaceSessionID(r *http.Request) string {
	credential := ""
	if token := bearer(r); token != "" {
		credential = "bearer:" + token
	} else if cookie, err := r.Cookie(portalSessionCookie); err == nil && cookie.Value != "" {
		credential = "session:" + cookie.Value
	}
	if credential == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte("workspace-control:"+credential)))
}

// handleWorkplace exposes shared files and available environment capabilities from the
// caller's own gateway. It never accepts a workspace owner from the browser.
func (p *portal) handleWorkplace(w http.ResponseWriter, r *http.Request, ident identity) {
	p.handleOwnerWorkspaceGateway(w, r, ident, "/api/workplace")
}

func (p *portal) handleOwnerWorkspaceGateway(w http.ResponseWriter, r *http.Request, ident identity, gatewayRoot string) {
	item, code, errMsg := p.ownerWorkspace(r.Context(), ident)
	if code != 0 {
		writeJSON(w, code, map[string]string{"error": errMsg})
		return
	}
	if item.Status.Phase != dev1alpha1.WorkspacePhaseRunning {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "workspace is not Running (phase " + string(item.Status.Phase) + ")"})
		return
	}
	target, err := url.Parse(p.workspaceChatBaseURL(item))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	rest := strings.TrimPrefix(r.PathValue("rest"), "/")
	// PathValue is already URL-decoded. Reject traversal before the Node
	// gateway normalizes dot segments into a different, separately authorized API.
	for _, segment := range strings.Split(strings.ReplaceAll(rest, "\\", "/"), "/") {
		if segment == "." || segment == ".." {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid gateway path"})
			return
		}
	}
	// Room roster authorization applies only to room dispatches.
	if gatewayRoot == "/api/rooms" {
		if code, errMsg := p.authorizeRoomRequest(w, r, target, rest, ident); code != 0 {
			writeJSON(w, code, map[string]string{"error": errMsg})
			return
		}
	}
	gatewayPath := gatewayRoot
	if rest != "" {
		gatewayPath += "/" + rest
	}
	proxy := &httputil.ReverseProxy{
		// Flush immediately: the room feed is SSE.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Empty rest must NOT grow a trailing slash: the gateway's SPA
			// fallback would swallow /api/rooms/ (both sides tolerate it now,
			// but keep the canonical form).
			pr.Out.URL.Path = gatewayPath
			pr.Out.URL.RawPath = ""
			// Room owner scoping: the portal session identity is the source of
			// truth — the gateway never sees the browser's cookies, and any
			// caller-supplied owner header (the gateway also honors
			// X-BotUI-User) is dropped before the stamp below.
			pr.Out.Header.Del("X-BotUI-User")
			pr.Out.Header.Del(workspaceSessionHeader)
			pr.Out.Header.Del("X-Workspace-Agent-Token")
			if gatewayRoot == "/api/workplace" {
				pr.Out.Header.Set(workspaceSessionHeader, workspaceSessionID(pr.In))
			}
			pr.Out.Header.Set(workspaceOwnerHeaderName, ident.UserID)
			pr.Out.Header.Del(workspaceTokenHeader)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("X-CSRF-Token")
		},
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "workspace gateway unreachable: " + err.Error()})
	}
	proxy.ServeHTTP(w, r)
}
