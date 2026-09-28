package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Service-status endpoints backing the admin console's sidebar entries for
// the memory (OpenViking) and knowledge (WeKnora) stacks. The console is
// browser-only and must never hold the OpenViking root key, so the portal
// probes both services server-side and reports a summary.

var serviceProbeClient = &http.Client{Timeout: 5 * time.Second}

func (p *portal) platformSecretKey(ctx context.Context, key string) (string, error) {
	var secret corev1.Secret
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: p.cfg.credentialsSecret}, &secret); err != nil {
		return "", err
	}
	return string(secret.Data[key]), nil
}

// handleMemoryStatus reports OpenViking health plus its account inventory.
func (p *portal) handleMemoryStatus(w http.ResponseWriter, r *http.Request, _ identity) {
	out := map[string]any{
		"server":  p.cfg.memoryServer,
		"account": p.cfg.memoryAccount,
	}

	health, err := serviceProbeClient.Get(p.cfg.memoryServer + "/health")
	healthy := err == nil && health.StatusCode == http.StatusOK
	if health != nil {
		_ = health.Body.Close()
	}
	out["healthy"] = healthy

	rootKey, keyErr := p.platformSecretKey(r.Context(), "OPENVIKING_ROOT_API_KEY")
	var accounts []map[string]any
	if keyErr == nil && rootKey != "" {
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.cfg.memoryServer+"/api/v1/admin/accounts", nil)
		if err == nil {
			request.Header.Set("X-API-Key", rootKey)
			if response, err := serviceProbeClient.Do(request); err == nil {
				defer response.Body.Close()
				var payload struct {
					Accounts []struct {
						AccountID  string `json:"account_id"`
						AdminUser  string `json:"admin_user_id"`
						CreatedAt  string `json:"created_at"`
					} `json:"result"`
				}
				if err := json.NewDecoder(response.Body).Decode(&payload); err == nil {
					for _, account := range payload.Accounts {
						accounts = append(accounts, map[string]any{
							"accountID": account.AccountID, "adminUser": account.AdminUser, "createdAt": account.CreatedAt,
						})
					}
				}
			}
		}
	}
	if accounts == nil {
		accounts = []map[string]any{}
	}
	out["accounts"] = accounts
	writeJSON(w, http.StatusOK, out)
}

// handleKnowledgeStatus reports WeKnora reachability through its configured
// MCP bridge URL. An HTTP error status (404/405 from a plain GET against an
// MCP endpoint) still proves the service is up; only transport failures and
// 5xx count as unhealthy.
func (p *portal) handleKnowledgeStatus(w http.ResponseWriter, r *http.Request, _ identity) {
	out := map[string]any{"url": p.cfg.knowledgeMCPURL, "configured": p.cfg.knowledgeMCPURL != ""}
	healthy := false
	if p.cfg.knowledgeMCPURL != "" {
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.cfg.knowledgeMCPURL, nil)
		if err == nil {
			if response, err := serviceProbeClient.Do(request); err == nil {
				_ = response.Body.Close()
				healthy = response.StatusCode < 500
			}
		}
	}
	out["healthy"] = healthy
	writeJSON(w, http.StatusOK, out)
}
