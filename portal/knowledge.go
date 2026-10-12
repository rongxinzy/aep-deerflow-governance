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
		writeJSON(w, http.StatusGone, map[string]string{
			"error": "legacy knowledge proxy is disabled; use the managed knowledge API",
		})
		return
	}
	if !ident.isAdmin(p.cfg) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin required"})
		return
	}
	rest := r.PathValue("rest")
	if rest == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path required"})
		return
	}

	// Authenticate to WeKnora
	user, pass := p.weknoraAdminCreds(r.Context())
	if user == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "weknora not configured"})
		return
	}
	loginBody, _ := json.Marshal(map[string]string{"email": user, "password": pass})
	loginReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		strings.TrimRight(p.cfg.knowledgeAPIURL, "/")+"/api/v1/auth/login", bytes.NewReader(loginBody))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge service authentication unavailable"})
		return
	}
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := knowledgeAuthClient.Do(loginReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge service authentication unavailable"})
		return
	}
	defer func() { _ = loginResp.Body.Close() }()
	var login struct {
		Token string `json:"token"`
	}
	if loginResp.StatusCode < http.StatusOK || loginResp.StatusCode >= http.StatusMultipleChoices ||
		json.NewDecoder(loginResp.Body).Decode(&login) != nil || login.Token == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "weknora auth failed"})
		return
	}

	// Forward the request
	target := strings.TrimRight(p.cfg.knowledgeAPIURL, "/") + "/api/v1/" + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid knowledge request"})
		return
	}
	proxyReq.Header.Set("Authorization", "Bearer "+login.Token)
	proxyReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	if r.ContentLength > 0 {
		proxyReq.ContentLength = r.ContentLength
	}

	resp, err := knowledgeTransferClient.Do(proxyReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge service request failed"})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge service redirect refused"})
		return
	}
	if resp.StatusCode >= http.StatusBadRequest {
		message := "knowledge service request rejected"
		if resp.StatusCode >= http.StatusInternalServerError {
			message = "knowledge service failed"
		}
		writeJSON(w, resp.StatusCode, map[string]string{"error": message})
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
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin required"})
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
