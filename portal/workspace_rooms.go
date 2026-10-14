// Owner-scoped rooms proxy: /api/v1/rooms/{rest...} -> the caller's workspace
// gateway /api/rooms/{rest...}. Rooms live at the gateway level (the room
// orchestrator fans out across companions), and workspaces are one-per-user by
// product invariant, so the workspace is resolved from the session identity
// instead of the URL — that also keeps this route disjoint from the
// {companion}/chat wildcard tree. The room SSE stream rides the same proxy
// with immediate flush.
package main

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

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
	gatewayPath := "/api/rooms"
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
			// Room owner scoping: the portal session identity is the source of
			// truth — the gateway never sees the browser's cookies.
			pr.Out.Header.Set(workspaceOwnerHeaderName, ident.UserID)
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
