package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

const maxKnowledgeRunBodyBytes = 8 << 20

// injectKnowledgeRunContext uses LangGraph's supported run `context` field.
// Custom HTTP headers are not part of the tool runtime context, so merely
// stamping one on the reverse proxy would not bind the actual MCP call.
func injectKnowledgeRunContext(r *http.Request, rest, handle, employee string) error {
	if handle == "" || r.Method != http.MethodPost || !isLangGraphRunPath(rest) {
		return nil
	}
	if r.Body == nil {
		return errors.New("knowledge run body is required")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxKnowledgeRunBodyBytes+1))
	_ = r.Body.Close()
	if err != nil || len(body) == 0 || len(body) > maxKnowledgeRunBodyBytes {
		return errors.New("invalid knowledge run body")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return errors.New("invalid knowledge run json")
	}
	contextValue, ok := payload["context"]
	var runContext map[string]any
	if ok {
		runContext, ok = contextValue.(map[string]any)
		if !ok {
			return errors.New("invalid knowledge run context")
		}
	} else {
		runContext = map[string]any{}
	}
	// Never trust a client-supplied handle with the same key.
	runContext["zhiyuan_knowledge_context"] = handle
	runContext["zhiyuan_employee_name"] = employee
	payload["context"] = runContext
	// Gateway's top-level body.context is intentionally allowlisted and drops
	// these application-specific fields. Its build_run_config path preserves
	// body.config.context for ToolRuntime.context, so bind the same server-owned
	// values there as well. Remove caller copies from configurable: it is not an
	// authorization channel and may be persisted in checkpoints.
	configValue, exists := payload["config"]
	var runConfig map[string]any
	if !exists || configValue == nil {
		runConfig = map[string]any{}
	} else {
		var ok bool
		runConfig, ok = configValue.(map[string]any)
		if !ok {
			return errors.New("invalid knowledge run config")
		}
	}
	configContext, exists := runConfig["context"]
	var runtimeContext map[string]any
	if !exists || configContext == nil {
		runtimeContext = map[string]any{}
	} else {
		var ok bool
		runtimeContext, ok = configContext.(map[string]any)
		if !ok {
			return errors.New("invalid knowledge run config context")
		}
	}
	runtimeContext["zhiyuan_knowledge_context"] = handle
	runtimeContext["zhiyuan_employee_name"] = employee
	runConfig["context"] = runtimeContext
	if configurable, ok := runConfig["configurable"].(map[string]any); ok {
		delete(configurable, "zhiyuan_knowledge_context")
		delete(configurable, "zhiyuan_employee_name")
	}
	payload["config"] = runConfig
	encoded, err := json.Marshal(payload)
	if err != nil {
		return errors.New("invalid knowledge run json")
	}
	r.Body = io.NopCloser(strings.NewReader(string(encoded)))
	r.ContentLength = int64(len(encoded))
	return nil
}

func isLangGraphRunPath(rest string) bool {
	clean := strings.Trim(rest, "/")
	clean = strings.TrimPrefix(clean, "api/")
	parts := strings.Split(clean, "/")
	if len(parts) == 2 && parts[0] == "runs" && (parts[1] == "wait" || parts[1] == "stream") {
		return true
	}
	if len(parts) < 3 || parts[0] != "threads" || parts[1] == "" {
		return false
	}
	return (len(parts) == 3 && parts[2] == "runs") ||
		(len(parts) == 4 && parts[2] == "runs" && (parts[3] == "wait" || parts[3] == "stream"))
}

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
// and returns the internal token from the memkey Secret plus the display
// name (spec.displayName, falling back to the technical name).
func (p *portal) authorizeChat(ctx context.Context, name string, ident identity) (string, string, int, string) {
	var item dev1alpha1.DigitalEmployee
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: name}, &item); err != nil {
		return "", "", statusOf(err), err.Error()
	}
	// The display name rides along for callers that surface the employee to
	// the user (the chat UI header via the session handler); the technical
	// name is the fallback so the UI always has something to show.
	display := strings.TrimSpace(item.Spec.DisplayName)
	if display == "" {
		display = name
	}
	if !ident.canAccessEmployee(p.cfg, item.Spec.Team, item.Spec.Owner, item.Spec.Visibility) {
		return "", "", http.StatusForbidden, "not authorized for this digital employee (owner, team member, or admin required)"
	}
	// Sleeping (KEDA scaled to zero) is fine to authorize: the memkey
	// Secret persists, and handleChat wakes the runtime before proxying.
	if item.Status.Phase != dev1alpha1.PhaseReady && item.Status.Phase != dev1alpha1.PhaseSleeping {
		return "", "", http.StatusConflict, "digital employee is not Ready (phase " + string(item.Status.Phase) + ")"
	}
	var secret corev1.Secret
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: memkeySecretName(name)}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", http.StatusServiceUnavailable, "runtime credentials not provisioned yet"
		}
		return "", "", http.StatusInternalServerError, err.Error()
	}
	token := strings.TrimSpace(string(secret.Data["DEER_FLOW_INTERNAL_AUTH_TOKEN"]))
	if token == "" {
		return "", "", http.StatusServiceUnavailable, "runtime internal token missing from credentials secret"
	}
	return token, display, 0, ""
}

// handleChat proxies one HTTP request to the employee runtime.
// Route: /api/v1/employees/{name}/chat/{rest...} -> runtime /api/{rest...}
// Only DeerFlow REST routes are reachable this way; the browser never sees
// the internal token, the model credential, or the cluster network.
func (p *portal) handleChat(w http.ResponseWriter, r *http.Request, ident identity) {
	name := r.PathValue("name")
	rest := r.PathValue("rest")
	token, _, code, errMsg := p.authorizeChat(r.Context(), name, ident)
	if code != 0 {
		writeJSON(w, code, map[string]string{"error": errMsg})
		return
	}
	// Bind knowledge-capable runtime requests to this authenticated cookie
	// session. Never forward a caller-supplied context handle. The MCP boundary
	// can exchange this short-lived handle with the Portal for a fresh grant
	// decision without receiving the user's AEP token.
	knowledgeContext := ""
	var employee dev1alpha1.DigitalEmployee
	if err := p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: name}, &employee); err != nil {
		writeJSON(w, statusOf(err), map[string]string{"error": "digital employee is unavailable"})
		return
	}
	if employee.Spec.Knowledge != nil {
		cookie, err := r.Cookie(portalSessionCookie)
		if err != nil || cookie.Value == "" {
			p.managedWriteError(w, http.StatusUnauthorized, "KNOWLEDGE_SESSION_UNAVAILABLE", "访问知识库需要登录门户会话", "")
			return
		}
		knowledgeContext, err = p.issueKnowledgeRuntimeContext(r.Context(), cookie.Value, name)
		if err != nil {
			p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_CONTEXT_UNAVAILABLE", "知识库运行上下文暂不可用", "")
			return
		}
		if err := injectKnowledgeRunContext(r, rest, knowledgeContext, name); err != nil {
			p.managedWriteError(w, http.StatusBadRequest, "KNOWLEDGE_CONTEXT_UNAVAILABLE", "无法注入知识库运行上下文", "")
			return
		}
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
			pr.Out.Header.Del(knowledgeContextHeader)
			if knowledgeContext != "" {
				pr.Out.Header.Set(knowledgeContextHeader, knowledgeContext)
			}
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
