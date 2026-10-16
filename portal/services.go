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

// 出站统一走 tracedTransport：AEP 控制面调用自动携带 traceparent，
// 逐调用点零改动（63 处 NewRequestWithContext 一处包圆）。
var serviceProbeClient = &http.Client{Timeout: 5 * time.Second, Transport: tracedTransport{}}

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
	// The internal OpenViking URL is cluster-only; show health + account
	// without the address (browsers cannot resolve cluster DNS).
	out := map[string]any{
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
				defer func() { _ = response.Body.Close() }()
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
	defer func() { _ = response.Body.Close() }()
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

// memoryRecallFilter names one part of the runtime recall contract and says
// whether this administrator diagnostic reproduces it, so a caller can tell
// "an admin looked at this employee's memory" apart from "the runtime would
// recall this for a requester".
type memoryRecallFilter struct {
	Name    string `json:"name"`
	Applied bool   `json:"applied"`
	Reason  string `json:"reason"`
}

// memoryRecallRetrieval mirrors the retrieval tuning the operator renders from
// spec.memory into the runtime config (operator employee_reconcile.go: top_k /
// score_threshold / max_injection_chars). A zero field is unset in the CR and
// takes the runtime default.
type memoryRecallRetrieval struct {
	TopK              int32   `json:"topK,omitempty"`
	ScoreThreshold    float64 `json:"scoreThreshold,omitempty"`
	MaxInjectionChars int32   `json:"maxInjectionChars,omitempty"`
}

// memoryRecallDiagnostic is returned with every successful admin memory search.
// The console's test box calls OpenViking directly, so its hit list is not the
// runtime's recall list; the annotation states which contract parts are missing
// instead of letting the console imply equivalence.
type memoryRecallDiagnostic struct {
	Mode         string                 `json:"mode"` // admin-diagnostic, never a requester recall
	Source       string                 `json:"source"`
	RequestedBy  string                 `json:"requestedBy"`
	Credential   string                 `json:"credential"`
	Employee     string                 `json:"employee"`
	MemoryUser   string                 `json:"memoryUser,omitempty"`
	MemoryScope  string                 `json:"memoryScope"`
	SharedPeerID string                 `json:"sharedPeerID,omitempty"`
	Retrieval    *memoryRecallRetrieval `json:"retrieval,omitempty"`
	Filters      []memoryRecallFilter   `json:"filters"`
}

const (
	memoryDiagnosticMode   = "admin-diagnostic"
	memoryDiagnosticSource = "POST /api/v1/search/find"
	memoryDiagnosticKey    = "employee-user-key"
	memoryScopeUnknown     = "unknown"
	memoryScopeInstance    = "instance"
	memoryScopeRequester   = "requester"
)

// memoryRuntimeContract resolves the employee's runtime memory scope straight
// off the DigitalEmployee CR — the same fields the operator renders into the
// runtime config, so the diagnostic cannot drift onto a second source of truth.
// An unresolvable CR reports "unknown" rather than a guess.
func (p *portal) memoryRuntimeContract(ctx context.Context, employee string) (scope, memoryUser, sharedPeerID string, retrieval *memoryRecallRetrieval) {
	scope = memoryScopeUnknown
	if p.k8s == nil {
		return scope, "", "", nil
	}
	var item dev1alpha1.DigitalEmployee
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: employee}, &item); err != nil {
		return scope, "", "", nil
	}
	memoryUser = item.Status.MemoryUser
	// The CRD defaults memoryScope to "instance"; an empty value means the
	// runtime partition is the single instance space.
	scope = memoryScopeInstance
	if memory := item.Spec.Memory; memory != nil {
		if memory.MemoryScope == memoryScopeRequester {
			scope = memoryScopeRequester
			// shared_peer_id is valid in requester scope only, matching the
			// operator's render condition.
			sharedPeerID = memory.SharedPeerID
		}
		if memory.TopK > 0 || memory.ScoreThreshold > 0 || memory.MaxInjectionChars > 0 {
			retrieval = &memoryRecallRetrieval{
				TopK: memory.TopK, ScoreThreshold: memory.ScoreThreshold, MaxInjectionChars: memory.MaxInjectionChars,
			}
		}
	}
	return scope, memoryUser, sharedPeerID, retrieval
}

// memoryRecallFilters states which parts of the runtime recall contract this
// diagnostic does not reproduce. A hit list here may therefore legitimately
// differ from what chat recall surfaces for the same employee.
func memoryRecallFilters(scope string) []memoryRecallFilter {
	scopeApplied := false
	scopeReason := "spec.memory.memoryScope could not be resolved for this employee; the runtime partition is unknown, treat this as an instance-space read."
	switch scope {
	case memoryScopeInstance:
		scopeApplied = true
		scopeReason = "memoryScope=instance: the owner key reads the same single space the runtime recalls from for every requester."
	case memoryScopeRequester:
		scopeReason = "memoryScope=requester: the runtime partitions memory per requesting user, while this owner key reads only the employee instance space — no requester's partition is exercised here."
	}
	return []memoryRecallFilter{
		{
			Name: "existence", Applied: false,
			Reason: "the runtime drops memory entries whose source no longer exists (deer-flow#18/#19) before injecting context; this box calls OpenViking search/find directly, so hits the chat would never recall can still appear.",
		},
		{Name: "scope", Applied: scopeApplied, Reason: scopeReason},
		{
			Name: "retrieval", Applied: false,
			Reason: "this box sends the query only; the runtime additionally applies the employee's retrieval tuning (top_k/score_threshold/max_injection_chars, unset knobs follow the runtime defaults).",
		},
	}
}

// handleMemorySearch runs an OpenViking search/find with the named
// employee's user key — the console's retrieval test box. It stays an
// administrator diagnostic: the result is annotated with the scope it ran in
// and the runtime filters it skips, and it never reads another user's memory.
func (p *portal) handleMemorySearch(w http.ResponseWriter, r *http.Request, ident identity) {
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
	scope, memoryUser, sharedPeerID, retrieval := p.memoryRuntimeContract(r.Context(), body.Employee)
	diagnostic := memoryRecallDiagnostic{
		Mode: memoryDiagnosticMode, Source: memoryDiagnosticSource, RequestedBy: ident.UserID,
		Credential: memoryDiagnosticKey, Employee: body.Employee, MemoryUser: memoryUser,
		MemoryScope: scope, SharedPeerID: sharedPeerID, Retrieval: retrieval,
		Filters: memoryRecallFilters(scope),
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
	defer func() { _ = response.Body.Close() }()
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
	writeJSON(w, http.StatusOK, map[string]any{"diagnostic": diagnostic, "memories": memories})
}

// handleKnowledgeStatus reports WeKnora reachability through its configured
// MCP bridge URL. An HTTP error status (404/405 from a plain GET against an
// MCP endpoint) still proves the service is up; only transport failures and
// 5xx count as unhealthy.
func (p *portal) handleKnowledgeStatus(w http.ResponseWriter, r *http.Request, _ identity) {
	// The internal MCP URL is cluster-only (browsers cannot reach it); the
	// console shows health + configured, never the internal address. The
	// browser-facing entry is the console's WeKnora proxy, derived from the
	// request host so it matches whichever hostname the user typed.
	out := map[string]any{
		"configured": p.cfg.knowledgeMCPURL != "",
		"uiURL":      p.knowledgeUIURL(r),
	}
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
