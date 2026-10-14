package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Knowledge auth and transfer requests can outlive short service health probes.
// Keep these clients separate so document uploads/downloads do not change the
// timeout behavior of unrelated readiness checks.
var (
	knowledgeAuthClient = &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: refuseKnowledgeRedirect,
	}
	knowledgeTransferClient = &http.Client{
		Timeout:       2 * time.Minute,
		CheckRedirect: refuseKnowledgeRedirect,
	}
)

func refuseKnowledgeRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

// handleKnowledgeProxy forwards WeKnora knowledge-base API requests with
// server-side authentication. The browser never sees WeKnora credentials.
// Routes: /api/v1/knowledge/{path...} → WeKnora /api/v1/{path...}
func (p *portal) handleKnowledgeProxy(w http.ResponseWriter, r *http.Request, ident identity) {
	if p.cfg.managedKnowledge.tenantID != "" {
		p.managedWriteError(w, http.StatusGone, "KNOWLEDGE_LEGACY_PROXY_DISABLED", "旧版知识库代理已停用，请改用托管知识库接口", "")
		return
	}
	if !ident.isAdmin(p.cfg) {
		p.managedWriteError(w, http.StatusForbidden, "ADMIN_REQUIRED", "需要管理员权限", "")
		return
	}
	rest := r.PathValue("rest")
	if rest == "" {
		p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "缺少知识库接口路径", "")
		return
	}

	// Authenticate to WeKnora
	user, pass := p.weknoraAdminCreds(r.Context())
	if user == "" {
		p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_NOT_CONFIGURED", "知识库服务尚未配置管理员凭据", "")
		return
	}
	loginBody, _ := json.Marshal(map[string]string{"email": user, "password": pass})
	loginReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		strings.TrimRight(p.cfg.knowledgeAPIURL, "/")+"/api/v1/auth/login", bytes.NewReader(loginBody))
	if err != nil {
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库服务认证暂不可用", "")
		return
	}
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := knowledgeAuthClient.Do(loginReq)
	if err != nil {
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库服务认证暂不可用", "")
		return
	}
	defer func() { _ = loginResp.Body.Close() }()
	var login struct {
		Token string `json:"token"`
	}
	if loginResp.StatusCode < http.StatusOK || loginResp.StatusCode >= http.StatusMultipleChoices ||
		json.NewDecoder(loginResp.Body).Decode(&login) != nil || login.Token == "" {
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_REJECTED", "知识库服务拒绝了管理员凭据", "")
		return
	}

	// Forward the request
	target := strings.TrimRight(p.cfg.knowledgeAPIURL, "/") + "/api/v1/" + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "知识库请求无效", "")
		return
	}
	proxyReq.Header.Set("Authorization", "Bearer "+login.Token)
	proxyReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	if r.ContentLength > 0 {
		proxyReq.ContentLength = r.ContentLength
	}

	resp, err := knowledgeTransferClient.Do(proxyReq)
	if err != nil {
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库服务请求失败", "")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_INVALID", "知识库服务返回了不受信任的重定向", "")
		return
	}
	if resp.StatusCode >= http.StatusBadRequest {
		code, message := "KNOWLEDGE_UPSTREAM_REJECTED", "知识库服务拒绝了请求"
		if resp.StatusCode >= http.StatusInternalServerError {
			code, message = "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库服务调用失败"
		}
		p.managedWriteError(w, resp.StatusCode, code, message, "")
		return
	}

	// Copy response
	for k, v := range map[string]string{
		"Content-Type":        resp.Header.Get("Content-Type"),
		"Content-Disposition": resp.Header.Get("Content-Disposition"),
	} {
		if v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleKnowledgeStatusEnhanced returns per-tenant knowledge base inventory
// with document counts, going beyond the simple health check.
func (p *portal) handleKnowledgeListBases(w http.ResponseWriter, r *http.Request, ident identity) {
	if !ident.isAdmin(p.cfg) {
		p.managedWriteError(w, http.StatusForbidden, "ADMIN_REQUIRED", "需要管理员权限", "")
		return
	}
	user, pass := p.weknoraAdminCreds(r.Context())
	if user == "" {
		writeJSON(w, http.StatusOK, map[string]any{"knowledgeBases": []any{}})
		return
	}

	loginBody, _ := json.Marshal(map[string]string{"email": user, "password": pass})
	loginReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
		p.cfg.knowledgeAPIURL+"/api/v1/auth/login", bytes.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := serviceProbeClient.Do(loginReq)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"knowledgeBases": []any{}, "error": err.Error()})
		return
	}
	defer func() { _ = loginResp.Body.Close() }()
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(loginResp.Body).Decode(&login)
	if login.Token == "" {
		writeJSON(w, http.StatusOK, map[string]any{"knowledgeBases": []any{}, "error": "auth failed"})
		return
	}

	// List knowledge bases
	listReq, _ := http.NewRequestWithContext(r.Context(), http.MethodGet,
		p.cfg.knowledgeAPIURL+"/api/v1/knowledge-bases", nil)
	listReq.Header.Set("Authorization", "Bearer "+login.Token)
	listResp, err := serviceProbeClient.Do(listReq)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"knowledgeBases": []any{}, "error": err.Error()})
		return
	}
	defer func() { _ = listResp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(listResp.Body, 1<<20))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(listResp.StatusCode)
	_, _ = w.Write(body)
}
