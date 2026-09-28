package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
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

// handleMemoryStatus reports OpenViking health plus its account inventory
// and per-employee memory activity (sessions count + last write, queried
// with each employee's own OpenViking user key — the root key is denied
// tenant-scoped data by design).
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
						AccountID string `json:"account_id"`
						UserCount int    `json:"user_count"`
						CreatedAt string `json:"created_at"`
					} `json:"result"`
				}
				if err := json.NewDecoder(response.Body).Decode(&payload); err == nil {
					for _, account := range payload.Accounts {
						accounts = append(accounts, map[string]any{
							"accountID": account.AccountID, "createdAt": account.CreatedAt, "userCount": account.UserCount,
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

	// Per-employee activity from the user key the operator provisions.
	var employees []map[string]any
	var list dev1alpha1.DigitalEmployeeList
	if err := p.k8s.List(r.Context(), &list, client.InNamespace(p.cfg.namespace)); err == nil {
		for i := range list.Items {
			item := &list.Items[i]
			if item.Status.MemoryUser == "" {
				continue
			}
			entry := map[string]any{"name": item.Name, "memoryUser": item.Status.MemoryUser}
			if userKey := p.memkeyUserKey(r.Context(), item.Name); userKey != "" {
				if stats := p.openvikingSessionStats(r.Context(), userKey); stats != nil {
					entry["sessions"] = stats["sessions"]
					entry["lastActive"] = stats["lastActive"]
				}
			}
			employees = append(employees, entry)
		}
	}
	if employees == nil {
		employees = []map[string]any{}
	}
	out["employees"] = employees
	writeJSON(w, http.StatusOK, out)
}

// memkeyUserKey reads the employee's OpenViking user key from the memkey
// Secret the operator provisions (empty string when absent).
func (p *portal) memkeyUserKey(ctx context.Context, employee string) string {
	var secret corev1.Secret
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: memkeySecretName(employee)}, &secret); err != nil {
		return ""
	}
	return string(secret.Data["OPENVIKING_API_KEY"])
}

func (p *portal) openvikingSessionStats(ctx context.Context, userKey string) map[string]any {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.memoryServer+"/api/v1/sessions", nil)
	if err != nil {
		return nil
	}
	request.Header.Set("X-API-Key", userKey)
	response, err := serviceProbeClient.Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	var payload struct {
		Sessions []struct {
			ModTime string `json:"mod_time"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil
	}
	last := ""
	for _, session := range payload.Sessions {
		if session.ModTime > last {
			last = session.ModTime
		}
	}
	return map[string]any{"sessions": len(payload.Sessions), "lastActive": last}
}

// handleMemorySearch runs an OpenViking search/find with the named
// employee's user key — the console's retrieval test box.
func (p *portal) handleMemorySearch(w http.ResponseWriter, r *http.Request, _ identity) {
	var body struct {
		Employee string `json:"employee"`
		Query    string `json:"query"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil ||
		body.Employee == "" || body.Query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "employee and query are required"})
		return
	}
	userKey := p.memkeyUserKey(r.Context(), body.Employee)
	if userKey == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no memory key for employee"})
		return
	}
	payload, _ := json.Marshal(map[string]string{"query": body.Query})
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.cfg.memoryServer+"/api/v1/search/find", bytes.NewReader(payload))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	request.Header.Set("X-API-Key", userKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := serviceProbeClient.Do(request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer response.Body.Close()
	var result struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			Memories []struct {
				URI      string  `json:"uri"`
				Score    float64 `json:"score"`
				Abstract string  `json:"abstract"`
			} `json:"memories"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "bad upstream response"})
		return
	}
	if result.Status != "ok" && result.Error != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": result.Error.Message})
		return
	}
	memories := make([]map[string]any, 0, len(result.Result.Memories))
	for _, memory := range result.Result.Memories {
		memories = append(memories, map[string]any{"uri": memory.URI, "score": memory.Score, "abstract": memory.Abstract})
	}
	writeJSON(w, http.StatusOK, map[string]any{"memories": memories})
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

	// Knowledge-base inventory through the WeKnora tenant API, using the
	// retrieve-scoped key from the platform Secret (server-side only;
	// skipped silently when the key is not configured yet).
	var knowledgeBases []map[string]any
	if key, err := p.platformSecretKey(r.Context(), "WEKNORA_API_KEY"); err == nil && key != "" && p.cfg.knowledgeAPIURL != "" {
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.cfg.knowledgeAPIURL+"/api/v1/knowledge-bases", nil)
		if err == nil {
			request.Header.Set("X-API-Key", key)
			if response, err := serviceProbeClient.Do(request); err == nil {
				var payload struct {
					Data []struct {
						ID            string `json:"id"`
						Name          string `json:"name"`
						Description   string `json:"description"`
						DocumentCount int    `json:"document_count"`
						CreatedAt     string `json:"created_at"`
					} `json:"data"`
				}
				if json.NewDecoder(response.Body).Decode(&payload) == nil {
					for _, kb := range payload.Data {
						knowledgeBases = append(knowledgeBases, map[string]any{
							"id": kb.ID, "name": kb.Name, "description": kb.Description,
							"documentCount": kb.DocumentCount, "createdAt": kb.CreatedAt,
						})
					}
				}
			}
		}
	}
	if knowledgeBases == nil {
		knowledgeBases = []map[string]any{}
	}
	out["knowledgeBases"] = knowledgeBases
	writeJSON(w, http.StatusOK, out)
}
