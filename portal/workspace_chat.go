// Workspace chat proxy: the browser authenticates to the portal with an AEP
// session; the portal forwards to the workspace gateway (Service pw-<name>,
// port 80) under the per-companion internal token (WORKSPACE_INTERNAL_TOKEN
// from the pw-<ws>-<companion>-memkey Secret), routing to the companion's
// prefix on the gateway. Workspaces never hibernate, so there is no wake
// step; the phase gate is Running only.
package main

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

const workspaceOwnerHeaderName = "X-Workspace-Owner-User-Id"
const workspaceTokenHeader = "X-Workspace-Internal-Token"

// workspaceChatBaseOverride is test-only: replaces the in-cluster gateway.
func (p *portal) workspaceChatBaseURL(item *dev1alpha1.PersonalWorkspace) string {
	if p.workspaceChatOverride != "" {
		return p.workspaceChatOverride
	}
	if item.Status.GatewayEndpoint != "" {
		return "http://" + item.Status.GatewayEndpoint
	}
	return "http://pw-" + item.Name + "." + p.cfg.namespace + ".svc.cluster.local"
}

func workspaceMemkeySecretName(workspace, companion string) string {
	return "pw-" + workspace + "-" + companion + "-memkey"
}

// authorizeWorkspaceChat resolves the CR (owner/admin only), requires the
// Running phase, validates the companion against the roster, and returns the
// companion's internal token.
func (p *portal) authorizeWorkspaceChat(ctx context.Context, name, companion string, ident identity) (string, int, string) {
	var item dev1alpha1.PersonalWorkspace
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: name}, &item); err != nil {
		return "", statusOf(err), err.Error()
	}
	if item.Spec.Owner != ident.UserID && !ident.isAdmin(p.cfg) {
		return "", http.StatusForbidden, "not authorized for this workspace"
	}
	if item.Status.Phase != dev1alpha1.WorkspacePhaseRunning {
		return "", http.StatusConflict, "workspace is not Running (phase " + string(item.Status.Phase) + ")"
	}
	found := false
	for i := range item.Spec.Companions {
		if item.Spec.Companions[i].Name == companion {
			found = true
			break
		}
	}
	if !found {
		return "", http.StatusNotFound, "unknown companion " + companion
	}
	var secret corev1.Secret
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: workspaceMemkeySecretName(name, companion)}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", http.StatusServiceUnavailable, "companion credentials not provisioned yet"
		}
		return "", http.StatusInternalServerError, err.Error()
	}
	token := strings.TrimSpace(string(secret.Data["WORKSPACE_INTERNAL_TOKEN"]))
	if token == "" {
		return "", http.StatusServiceUnavailable, "workspace internal token missing from credentials secret"
	}
	return token, 0, ""
}

// handleWorkspaceChat proxies one HTTP request to a workspace companion.
// Route: /api/v1/workspaces/{name}/{companion}/chat/{rest...}
//
//	-> gateway /{companion}/api/{rest...}  (gateway strips the prefix and
//	   forwards to the companion's server.mjs process)
func (p *portal) handleWorkspaceChat(w http.ResponseWriter, r *http.Request, ident identity) {
	name := r.PathValue("name")
	companion := r.PathValue("companion")
	rest := r.PathValue("rest")
	token, code, errMsg := p.authorizeWorkspaceChat(r.Context(), name, companion, ident)
	if code != 0 {
		writeJSON(w, code, map[string]string{"error": errMsg})
		return
	}
	var item dev1alpha1.PersonalWorkspace
	if err := p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: name}, &item); err != nil {
		writeJSON(w, statusOf(err), map[string]string{"error": "workspace is unavailable"})
		return
	}
	target, err := url.Parse(p.workspaceChatBaseURL(&item))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	proxy := &httputil.ReverseProxy{
		// Flush immediately: conversation streams are SSE.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			stripped := strings.TrimPrefix(rest, "api/")
			pr.Out.URL.Path = "/" + companion + "/api/" + stripped
			pr.Out.Header.Set(workspaceTokenHeader, token)
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
