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

// Web-chat proxy: the browser talks AEP-session auth to the portal; the
// portal forwards to the employee runtime using the per-employee gateway
// internal token (DEER_FLOW_INTERNAL_AUTH_TOKEN from the de-<name>-memkey
// Secret) and stamps X-DeerFlow-Owner-User-Id with the caller's AEP user id.
// The runtime's internal-auth resolves that header into the effective
// runtime user, and the governance middleware validates it as an AEP user —
// so browser chat runs under the human's data scope, not the employee default.

const ownerHeaderName = "X-DeerFlow-Owner-User-Id"

// chatBaseOverride, when set (tests), replaces the in-cluster Service URL.
// It lives on the portal struct so real deployments never configure it.

func memkeySecretName(employee string) string { return "de-" + employee + "-memkey" }

// runtimeBaseURL resolves the runtime Service for one employee.
// A portal method (not a package var) so the namespace comes from config;
// tests substitute the upstream by pointing it at an httptest server via
// the portal's chatBaseOverride hook below.
func (p *portal) runtimeBaseURL(employee string) string {
	if p.chatBaseOverride != "" {
		return p.chatBaseOverride
	}
	return "http://de-" + employee + "." + p.cfg.namespace + ".svc.cluster.local:8001"
}

// authorizeChat resolves the employee CR, enforces owner/admin visibility,
// and returns the internal token from the memkey Secret.
func (p *portal) authorizeChat(ctx context.Context, name string, ident identity) (string, int, string) {
	var item dev1alpha1.DigitalEmployee
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: name}, &item); err != nil {
		return "", statusOf(err), err.Error()
	}
	if !ident.canAccessEmployee(p.cfg, item.Spec.Team, item.Spec.Owner, item.Spec.Visibility) {
		return "", http.StatusForbidden, "not authorized for this digital employee (owner, team member, or admin required)"
	}
	// Sleeping (KEDA scaled to zero) is fine to authorize: the memkey
	// Secret persists, and handleChat wakes the runtime before proxying.
	if item.Status.Phase != dev1alpha1.PhaseReady && item.Status.Phase != dev1alpha1.PhaseSleeping {
		return "", http.StatusConflict, "digital employee is not Ready (phase " + string(item.Status.Phase) + ")"
	}
	var secret corev1.Secret
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: memkeySecretName(name)}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", http.StatusServiceUnavailable, "runtime credentials not provisioned yet"
		}
		return "", http.StatusInternalServerError, err.Error()
	}
	token := strings.TrimSpace(string(secret.Data["DEER_FLOW_INTERNAL_AUTH_TOKEN"]))
	if token == "" {
		return "", http.StatusServiceUnavailable, "runtime internal token missing from credentials secret"
	}
	return token, 0, ""
}

// handleChat proxies one HTTP request to the employee runtime.
// Route: /api/v1/employees/{name}/chat/{rest...} -> runtime /api/{rest...}
// Only DeerFlow REST routes are reachable this way; the browser never sees
// the internal token, the model credential, or the cluster network.
func (p *portal) handleChat(w http.ResponseWriter, r *http.Request, ident identity) {
	name := r.PathValue("name")
	rest := r.PathValue("rest")
	token, code, errMsg := p.authorizeChat(r.Context(), name, ident)
	if code != 0 {
		writeJSON(w, code, map[string]string{"error": errMsg})
		return
	}
	// Hibernation: mark the conversation active (KEDA's idle signal) and, if
	// the runtime is scaled to zero, wake it and wait out the cold start.
	p.markActivity(name)
	if code, errMsg := p.wakeRuntime(r.Context(), name); code != 0 {
		writeJSON(w, code, map[string]string{"error": errMsg})
		return
	}

	target, err := url.Parse(p.runtimeBaseURL(name))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The reused DeerFlow frontend builds REST URLs as
			// ${prefix}/api/...; strip the redundant api/ so the runtime sees
			// its native path. LangGraph SDK paths (${prefix}/threads/...)
			// carry no api/ segment and pass through untouched.
			stripped := strings.TrimPrefix(rest, "api/")
			pr.Out.URL.Path = "/api/" + stripped
			pr.Out.Header.Set("X-DeerFlow-Internal-Token", token)
			pr.Out.Header.Set(ownerHeaderName, ident.UserID)
			pr.Out.Header.Del("Authorization")
			// The runtime needs neither the portal session nor its CSRF proof.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("X-CSRF-Token")
		},
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "runtime unreachable: " + err.Error()})
	}

	proxy.ServeHTTP(w, r)
}
