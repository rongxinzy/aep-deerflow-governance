package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Department orchestration: one API call provisions a department across
// AEP (identity), OpenViking (memory isolation), and optionally WeKnora
// (knowledge isolation). Admin-only.

type departmentRequest struct {
	Name     string   `json:"name"`     // e.g. "研发部"; the AEP team id is server-generated from it
	AdminIDs []string `json:"adminIds"` // AEP user IDs who manage this department
	// ParentID makes the new department a child of an existing one (二级部门
	// and below). nil/absent = root. Immutable per the AEP contract (path and
	// depth are fixed at creation); reparenting is not offered.
	ParentID *string `json:"parentId"`
}

type departmentStatus struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	AepTeamID     string `json:"aepTeamId"`
	OvAccountID   string `json:"ovAccountId"`
	WeKnoraTenant string `json:"weknoraTenant,omitempty"`
	Phase         string `json:"phase"` // provisioning | ready | partial
	Message       string `json:"message,omitempty"`
}

func (p *portal) handleCreateDepartment(w http.ResponseWriter, r *http.Request, ident identity) {
	if !ident.isAdmin(p.cfg) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin required"})
		return
	}
	var req departmentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	// 1. AEP team (identity) — the id is server-generated from the name and
	// keys the downstream OpenViking/WeKnora provisioning as well.
	teamID, aepStatus, err := p.createAepTeam(r, req)
	if err != nil {
		// An AEP 4xx (unknown parent, name policy) is the caller's mistake,
		// not an upstream outage: surface it as 400 with the detail.
		code := http.StatusBadGateway
		if aepStatus >= 400 && aepStatus < 500 {
			code = http.StatusBadRequest
		}
		writeJSON(w, code, map[string]string{"error": fmt.Sprintf("aep-team: %v", err)})
		return
	}
	status := departmentStatus{ID: teamID, Name: req.Name, Phase: "provisioning", AepTeamID: teamID}
	var partials []string

	// 2. OpenViking account (memory isolation boundary)
	ovAccount, err := p.createOpenVikingAccount(r, teamID)
	if err != nil {
		partials = append(partials, fmt.Sprintf("openviking: %v", err))
	} else {
		status.OvAccountID = ovAccount
	}

	// 3. WeKnora tenant (knowledge isolation, optional — skip if not deployed)
	if p.cfg.knowledgeAPIURL != "" {
		tenant, err := p.createWeKnoraTenant(r, req)
		if err != nil {
			partials = append(partials, fmt.Sprintf("weknora: %v", err))
		} else {
			status.WeKnoraTenant = tenant
		}
	}

	if len(partials) == 0 {
		status.Phase = "ready"
	} else {
		status.Phase = "partial"
		status.Message = fmt.Sprintf("partially provisioned: %v", partials)
	}
	// Record the outcome: without it a partial provision disappears with the
	// request, nothing shows it in the listing, and the missing pieces can
	// never be retried (the department then stays half-provisioned for good).
	if err := p.storeDepartmentProvision(r.Context(), provisioningRecordFromStatus(status, partials)); err != nil {
		if p.logger != nil {
			p.logger.Warn("department provisioning outcome not recorded", "id", status.ID, "error", err.Error())
		}
	}
	p.logger.Info("department provisioned", "id", status.ID, "phase", status.Phase, "partials", partials)
	writeJSON(w, http.StatusOK, status)
}

func (p *portal) createAepTeam(r *http.Request, req departmentRequest) (string, int, error) {
	payload := map[string]any{"name": req.Name}
	if req.ParentID != nil {
		payload["parentId"] = *req.ParentID
	}
	body, _ := json.Marshal(payload)
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		p.cfg.aepBaseURL+"/aep/v1/admin/teams", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-AEP-Protocol-Version", "1.0")
	request.Header.Set("Authorization", "Bearer "+p.adminToken(r.Context()))
	resp, err := serviceProbeClient.Do(request)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", resp.StatusCode, fmt.Errorf("aep returned %d: %s", resp.StatusCode, string(raw))
	}
	var result struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &result)
	return result.ID, resp.StatusCode, nil
}

func (p *portal) createOpenVikingAccount(r *http.Request, accountID string) (string, error) {
	rootKey := p.platformSecretKeyOrEmpty(r.Context(), "OPENVIKING_ROOT_API_KEY")
	if rootKey == "" {
		return "", fmt.Errorf("root key not configured")
	}
	body, _ := json.Marshal(map[string]any{
		"account_id":    "tenant-" + accountID,
		"admin_user_id": "portal",
	})
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		p.cfg.memoryServer+"/api/v1/admin/accounts", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", rootKey)
	resp, err := serviceProbeClient.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("openviking returned %d: %s", resp.StatusCode, string(raw))
	}
	return "tenant-" + accountID, nil
}

func (p *portal) createWeKnoraTenant(r *http.Request, req departmentRequest) (string, error) {
	// WeKnora admin auth: use the admin credentials from the platform secret
	// to obtain a JWT, then create a tenant.
	adminUser, adminPass := p.weknoraAdminCreds(r.Context())
	if adminUser == "" {
		return "", fmt.Errorf("weknora admin credentials not configured")
	}
	// Login
	body, _ := json.Marshal(map[string]string{"email": adminUser, "password": adminPass})
	loginReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
		p.cfg.knowledgeAPIURL+"/api/v1/auth/login", bytes.NewReader(body))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := serviceProbeClient.Do(loginReq)
	if err != nil {
		return "", fmt.Errorf("weknora login: %w", err)
	}
	defer func() { _ = loginResp.Body.Close() }()
	var loginResult struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(loginResp.Body).Decode(&loginResult)
	if loginResult.Token == "" {
		return "", fmt.Errorf("weknora login returned no token")
	}

	// Create tenant
	body, _ = json.Marshal(map[string]any{
		"name":        req.Name,
		"description": "Department: " + req.Name,
	})
	tenantReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost,
		p.cfg.knowledgeAPIURL+"/api/v1/tenants", bytes.NewReader(body))
	tenantReq.Header.Set("Content-Type", "application/json")
	tenantReq.Header.Set("Authorization", "Bearer "+loginResult.Token)
	resp, err := serviceProbeClient.Do(tenantReq)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("weknora returned %d: %s", resp.StatusCode, string(raw))
	}
	var tenant struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &tenant)
	return tenant.ID, nil
}

// platformSecretKeyOrEmpty reads a key from the platform Secret without erroring.
func (p *portal) platformSecretKeyOrEmpty(ctx context.Context, key string) string {
	// Simplified: reuse the existing platformSecretKey but return empty on error
	val, err := p.platformSecretKey(ctx, key)
	if err != nil {
		return ""
	}
	return val
}

// adminToken logs into AEP with the bootstrap admin credentials from the
// platform secret. For the private-deployment single-instance model this
// is acceptable; a dedicated service account replaces it in public mode.
var adminTokenCache struct {
	mu      sync.Mutex
	token   string
	expires time.Time
}

func (p *portal) adminToken(ctx context.Context) string {
	adminTokenCache.mu.Lock()
	defer adminTokenCache.mu.Unlock()
	if adminTokenCache.token != "" && time.Now().Before(adminTokenCache.expires) {
		return adminTokenCache.token
	}
	user := p.platformSecretKeyOrEmpty(ctx, "AEP_ADMIN_USER")
	pass := p.platformSecretKeyOrEmpty(ctx, "AEP_ADMIN_PASSWORD")
	if user == "" || pass == "" {
		p.logger.Error("department: AEP admin credentials not configured")
		return ""
	}
	body, _ := json.Marshal(map[string]string{
		"deploymentId": p.cfg.aepDeploymentID,
		"sessionId":    fmt.Sprintf("portal-dept-%d", time.Now().UnixNano()),
		"username":     user,
		"password":     pass,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		p.cfg.aepBaseURL+"/aep/v1/auth/password/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AEP-Protocol-Version", "1.0")
	resp, err := serviceProbeClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		p.logger.Error("department: AEP admin login failed", "error", err)
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var result struct {
		AccessToken string `json:"accessToken"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&result)
	adminTokenCache.token = result.AccessToken
	// Stay comfortably below the AEP access-token lifetime (AEP_ACCESS_TTL,
	// default 15 minutes): a longer cache reuses a token the control plane
	// has already expired and every consumer 401s until the cache expires
	// (observed live: a five-minute outage window per cycle on the failover
	// watcher). Re-login is cheap and the mutex serializes it.
	adminTokenCache.expires = time.Now().Add(10 * time.Minute)
	return result.AccessToken
}

// weknoraAdminCreds reads WeKnora admin credentials from the platform secret.
func (p *portal) weknoraAdminCreds(ctx context.Context) (string, string) {
	user := p.platformSecretKeyOrEmpty(ctx, "WEKNORA_ADMIN_USER")
	pass := p.platformSecretKeyOrEmpty(ctx, "WEKNORA_ADMIN_PASSWORD")
	return user, pass
}

// enrichTeams fetches the user's AEP team memberships and stores them on
// the identity. Called once at login and on stale-cache refresh — /user/me's
// browser contract doesn't include teamIds, but the token-authenticated call
// does (AEP contract enhancement): real-time team data with zero admin
// credential usage. When the user token is absent or rejected (the norm for
// sessions restored after a rolling restart — their mint-time AEP token has
// long expired), the admin user listing is the fallback so department scope
// self-heals instead of staying collapsed until the next login.
func (p *portal) enrichTeams(ctx context.Context, ident *identity) {
	if len(ident.Teams) > 0 && !ident.needsTeamRefresh() {
		return // fresh enough
	}
	if p.enrichTeamsWithToken(ctx, ident) {
		return
	}
	p.enrichTeamsFromAdmin(ctx, ident)
}

// enrichTeamsWithToken tries the user's own token against /user/me.
// Returns false when the token is missing or rejected (caller falls back).
func (p *portal) enrichTeamsWithToken(ctx context.Context, ident *identity) bool {
	if ident.Token == "" {
		return false // no token to verify against
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.cfg.aepBaseURL+"/aep/v1/user/me", nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+ident.Token)
	request.Header.Set("X-AEP-Protocol-Version", "1.0")
	resp, err := serviceProbeClient.Do(request)
	if err != nil {
		p.logger.Warn("enrichTeams: /user/me unreachable", "error", err)
		return false // caller falls back to the admin listing
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Expired/rejected tokens are the steady state for restored
		// sessions (the fallback below is their designed path) — keep those
		// quiet; upstream outages deserve a warning.
		if resp.StatusCode >= 500 {
			p.logger.Warn("enrichTeams: /user/me upstream error", "status", resp.StatusCode)
		} else {
			p.logger.Info("enrichTeams: /user/me rejected, falling back to admin lookup", "status", resp.StatusCode)
		}
		return false
	}
	var result struct {
		User struct {
			TeamIDs []string `json:"teamIds"`
		} `json:"user"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return false
	}
	ident.Teams = result.User.TeamIDs
	if ident.Teams == nil {
		ident.Teams = []string{}
	}
	ident.TeamsFetchedAt = time.Now()
	return true
}

// enrichTeamsFromAdmin recovers team memberships through the admin
// single-user lookup (one exact request, no directory scan). Only reached
// when the user-token path is unavailable — the cost is bounded by the
// staleness window: every failure exit stamps TeamsFetchedAt so a broken
// upstream is retried at most once per window per session instead of on
// every request (which would stack two probe timeouts onto the hot path).
func (p *portal) enrichTeamsFromAdmin(ctx context.Context, ident *identity) {
	giveUp := func(stage string, detail string) {
		if p.logger != nil {
			p.logger.Warn("enrichTeams: admin fallback failed", "stage", stage, "detail", detail, "userId", ident.UserID)
		}
		// Negative cache for one staleness window: keeps whatever Teams the
		// session already carries (possibly empty) rather than hammering a
		// broken upstream on every request.
		ident.TeamsFetchedAt = time.Now()
	}
	adminToken := p.adminToken(ctx)
	if adminToken == "" {
		giveUp("admin-token", "unavailable")
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.cfg.aepBaseURL+"/aep/v1/admin/users/"+url.PathEscape(ident.UserID), nil)
	if err != nil {
		giveUp("build-request", err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("X-AEP-Protocol-Version", "1.0")
	resp, err := serviceProbeClient.Do(req)
	if err != nil {
		giveUp("do", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		giveUp("status", fmt.Sprintf("aep returned %d: %s", resp.StatusCode, string(raw)))
		return
	}
	var user struct {
		TeamIDs []string `json:"teamIds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		giveUp("decode", err.Error())
		return
	}
	ident.Teams = user.TeamIDs
	if ident.Teams == nil {
		ident.Teams = []string{}
	}
	ident.TeamsFetchedAt = time.Now()
}

// refreshSessionTeams re-fetches team memberships from AEP and updates
// the session in place. Called in the background by authenticate() when
// the cached teams are stale; the current request proceeds with the old
// values (within the 5-minute window) and the next request sees fresh ones.
func (p *portal) refreshSessionTeams(sessionToken string, ident identity) {
	ident.Teams = nil // force re-fetch in enrichTeams
	p.enrichTeams(context.Background(), &ident)
	if len(ident.Teams) == 0 {
		return // nothing changed (or fetch failed; retry on next stale check)
	}
	ident.TeamsFetchedAt = time.Now()
	p.sessions.updateIdentity(sessionToken, ident)
	p.logger.Info("refreshed session teams", "userId", ident.UserID, "teams", ident.Teams)
}

// handleListDepartments returns all AEP teams (departments) with their
// provisioning status, for the admin console's team selector. parentId rides
// along so hierarchical selectors (二级部门 and below) can group correctly.
func (p *portal) handleListDepartments(w http.ResponseWriter, r *http.Request, ident identity) {
	adminToken := p.adminToken(r.Context())
	if adminToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "admin token unavailable"})
		return
	}
	// Paged fetch (same 10-page guard as contacts): a single limit=100 page
	// silently truncated every department past the 100th.
	teams, err := p.fetchTeams(r.Context(), adminToken)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("departments: team listing failed", "error", err.Error())
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "aep returned an error listing teams"})
		return
	}
	// Upstream listing order: selector endpoints must not shuffle between
	// calls the way a map iteration would.
	records := p.loadDepartmentProvisioning(r.Context())
	out := make([]map[string]any, 0, len(teams))
	for _, team := range teams {
		entry := map[string]any{"id": team.ID, "name": team.Name}
		if team.ParentID != nil {
			entry["parentId"] = *team.ParentID
		}
		// Provisioning fields are only emitted when the portal recorded an
		// outcome: a department provisioned outside it (or before this record
		// existed) has no phase to claim, rather than a made-up one.
		if record, ok := records[team.ID]; ok {
			entry["phase"] = record.Phase
			if len(record.Partials) > 0 {
				entry["partials"] = record.Partials
			}
			if record.OvAccountID != "" {
				entry["ovAccountId"] = record.OvAccountID
			}
			if record.WeKnoraTenant != "" {
				entry["weknoraTenant"] = record.WeKnoraTenant
			}
			if record.Message != "" {
				entry["message"] = record.Message
			}
			entry["updatedAt"] = record.UpdatedAt.Format(time.RFC3339)
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"departments": out})
}

// handleUpdateDepartment renames a department (AEP team name; the OV/WeKnora
// mappings keep their original IDs). Admin only.
func (p *portal) handleUpdateDepartment(w http.ResponseWriter, r *http.Request, ident identity) {
	if !ident.isAdmin(p.cfg) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin required"})
		return
	}
	deptID := r.PathValue("id")
	var body struct {
		Name *string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if body.Name == nil || *body.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	// Update the AEP team name
	adminToken := p.adminToken(r.Context())
	if adminToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "admin token unavailable"})
		return
	}
	reqBody, _ := json.Marshal(map[string]string{"name": *body.Name})
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPatch,
		p.cfg.aepBaseURL+"/aep/v1/admin/teams/"+deptID, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("X-AEP-Protocol-Version", "1.0")
	resp, err := serviceProbeClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeJSON(w, resp.StatusCode, map[string]string{"error": fmt.Sprintf("aep: %s", string(raw))})
		return
	}
	p.logger.Info("department renamed", "id", deptID, "name", *body.Name)
	writeJSON(w, http.StatusOK, map[string]any{"id": deptID, "name": *body.Name})
}

// handleDeleteDepartment removes a department's AEP team. OV/WeKnora
// artifacts are left in place (they contain user data). Admin only.
func (p *portal) handleDeleteDepartment(w http.ResponseWriter, r *http.Request, ident identity) {
	if !ident.isAdmin(p.cfg) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin required"})
		return
	}
	deptID := r.PathValue("id")
	adminToken := p.adminToken(r.Context())
	if adminToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "admin token unavailable"})
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodDelete,
		p.cfg.aepBaseURL+"/aep/v1/admin/teams/"+deptID, nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("X-AEP-Protocol-Version", "1.0")
	resp, err := serviceProbeClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeJSON(w, resp.StatusCode, map[string]string{"error": fmt.Sprintf("aep: %s", string(raw))})
		return
	}
	p.logger.Info("department deleted", "id", deptID)
	writeJSON(w, http.StatusOK, map[string]any{"id": deptID, "deleted": true})
}

// handleDepartmentMembers lists users in a department and supports adding
// /removing members via PUT (mirrors AEP's user RBAC replace).
func (p *portal) handleDepartmentMembers(w http.ResponseWriter, r *http.Request, ident identity) {
	if !ident.isAdmin(p.cfg) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin required"})
		return
	}
	deptID := r.PathValue("id")
	adminToken := p.adminToken(r.Context())
	if adminToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "admin token unavailable"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		// List members: fetch all users, filter by team
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet,
			p.cfg.aepBaseURL+"/aep/v1/admin/users?limit=200", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("X-AEP-Protocol-Version", "1.0")
		resp, err := serviceProbeClient.Do(req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var result struct {
			Items []struct {
				ID          string   `json:"id"`
				Username    string   `json:"username"`
				DisplayName string   `json:"displayName"`
				TeamIDs     []string `json:"teamIds"`
			} `json:"items"`
		}
		if json.NewDecoder(resp.Body).Decode(&result) != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "bad upstream"})
			return
		}
		members := make([]map[string]any, 0)
		for _, u := range result.Items {
			for _, t := range u.TeamIDs {
				if t == deptID {
					members = append(members, map[string]any{
						"userId": u.ID, "username": u.Username, "displayName": u.DisplayName,
					})
					break
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"members": members})

	case http.MethodPut:
		// Replace member list: set team membership for listed users,
		// remove for others currently in the team.
		var body struct {
			UserIDs []string `json:"userIds"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		// Fetch current members to compute adds/removes
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet,
			p.cfg.aepBaseURL+"/aep/v1/admin/users?limit=200", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("X-AEP-Protocol-Version", "1.0")
		resp, err := serviceProbeClient.Do(req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		var result struct {
			Items []struct {
				ID      string   `json:"id"`
				TeamIDs []string `json:"teamIds"`
				RoleIDs []string `json:"roleIds"`
			} `json:"items"`
		}
		if json.NewDecoder(resp.Body).Decode(&result) != nil {
			_ = resp.Body.Close()
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "bad upstream"})
			return
		}
		_ = resp.Body.Close()

		desired := make(map[string]bool)
		for _, id := range body.UserIDs {
			desired[id] = true
		}
		var added, removed int
		for _, u := range result.Items {
			currentlyIn := false
			for _, t := range u.TeamIDs {
				if t == deptID {
					currentlyIn = true
					break
				}
			}
			should := desired[u.ID]
			if currentlyIn == should {
				continue
			}
			// Compute new team list
			newTeams := make([]string, 0)
			for _, t := range u.TeamIDs {
				if t != deptID {
					newTeams = append(newTeams, t)
				}
			}
			if should {
				newTeams = append(newTeams, deptID)
			}
			// Preserve existing roles
			newRoles := u.RoleIDs
			if newRoles == nil {
				newRoles = []string{}
			}
			rb, _ := json.Marshal(map[string][]string{"roleIds": newRoles, "teamIds": newTeams})
			putReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPut,
				p.cfg.aepBaseURL+"/aep/v1/admin/users/"+u.ID+"/rbac", bytes.NewReader(rb))
			putReq.Header.Set("Content-Type", "application/json")
			putReq.Header.Set("Authorization", "Bearer "+adminToken)
			putReq.Header.Set("X-AEP-Protocol-Version", "1.0")
			putResp, err := serviceProbeClient.Do(putReq)
			if err != nil {
				p.logger.Warn("department member update failed", "userId", u.ID, "error", err)
				continue
			}
			_ = putResp.Body.Close()
			if should {
				added++
			} else {
				removed++
			}
		}
		p.logger.Info("department members updated", "id", deptID, "added", added, "removed", removed)
		writeJSON(w, http.StatusOK, map[string]any{"id": deptID, "added": added, "removed": removed})
	}
}
