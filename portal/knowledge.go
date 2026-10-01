package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)


// handleKnowledgeProxy forwards WeKnora knowledge-base API requests with
// server-side authentication. The browser never sees WeKnora credentials.
// Routes: /api/v1/knowledge/{path...} → WeKnora /api/v1/{path...}
func (p *portal) handleKnowledgeProxy(w http.ResponseWriter, r *http.Request, ident identity) {
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
	loginReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
		p.cfg.knowledgeAPIURL+"/api/v1/auth/login", bytes.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := serviceProbeClient.Do(loginReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "weknora unreachable: " + err.Error()})
		return
	}
	defer loginResp.Body.Close()
	var login struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(loginResp.Body).Decode(&login) != nil || login.Token == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "weknora auth failed"})
		return
	}

	// Forward the request
	target := p.cfg.knowledgeAPIURL + "/api/v1/" + rest
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	proxyReq.Header.Set("Authorization", "Bearer "+login.Token)
	proxyReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	if r.ContentLength > 0 {
		proxyReq.ContentLength = r.ContentLength
	}

	resp, err := serviceProbeClient.Do(proxyReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "weknora forward failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

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
	io.Copy(w, resp.Body)
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
	defer loginResp.Body.Close()
	var login struct {
		Token string `json:"token"`
	}
	json.NewDecoder(loginResp.Body).Decode(&login)
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
	defer listResp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(listResp.Body, 1<<20))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(listResp.StatusCode)
	w.Write(body)
}
