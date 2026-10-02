package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Department orchestration: one API call provisions a department across
// AEP (identity), OpenViking (memory isolation), and optionally WeKnora
// (knowledge isolation). Admin-only.

type departmentRequest struct {
	ID       string   `json:"id"`       // e.g. "rd-dept" (URL-safe)
	Name     string   `json:"name"`     // e.g. "研发部"
	AdminIDs []string `json:"adminIds"` // AEP user IDs who manage this department
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
	if req.ID == "" || req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id and name are required"})
		return
	}

	status := departmentStatus{ID: req.ID, Name: req.Name, Phase: "provisioning"}
	var partials []string

	// 1. AEP team (identity)
	teamID, err := p.createAepTeam(r, req)
	if err != nil {
		partials = append(partials, fmt.Sprintf("aep-team: %v", err))
	} else {
		status.AepTeamID = teamID
	}

	// 2. OpenViking account (memory isolation boundary)
	ovAccount, err := p.createOpenVikingAccount(r, req.ID)
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
	p.logger.Info("department provisioned", "id", req.ID, "phase", status.Phase, "partials", partials)
	writeJSON(w, http.StatusOK, status)
}

func (p *portal) createAepTeam(r *http.Request, req departmentRequest) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"id":   req.ID,
		"name": req.Name,
	})
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		p.cfg.aepBaseURL+"/aep/v1/admin/teams", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-AEP-Protocol-Version", "1.0")
	request.Header.Set("Authorization", "Bearer "+p.adminToken(r.Context()))
	resp, err := serviceProbeClient.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("aep returned %d: %s", resp.StatusCode, string(raw))
	}
	var result struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &result)
	if result.ID == "" {
		result.ID = req.ID // AEP may echo the team in a different shape
	}
	return result.ID, nil
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
	return req.ID, nil
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
	token   string
	expires time.Time
}

func (p *portal) adminToken(ctx context.Context) string {
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
	adminTokenCache.expires = time.Now().Add(20 * time.Minute)
	return result.AccessToken
}

// weknoraAdminCreds reads WeKnora admin credentials from the platform secret.
func (p *portal) weknoraAdminCreds(ctx context.Context) (string, string) {
	user := p.platformSecretKeyOrEmpty(ctx, "WEKNORA_ADMIN_USER")
	pass := p.platformSecretKeyOrEmpty(ctx, "WEKNORA_ADMIN_PASSWORD")
	return user, pass
}

// enrichTeams fetches the user's AEP team memberships via the admin API
// and stores them on the identity. Called once at login, cached in the
// portal session — /user/me doesn't include teamIds.
func (p *portal) enrichTeams(ctx context.Context, ident *identity) {
	if len(ident.Teams) > 0 && !ident.needsTeamRefresh() {
		return // fresh enough
	}
	if ident.Token == "" {
		return // no token to verify against
	}
	// Use the user's own token against /user/me — the response now
	// includes teamIds (AEP contract enhancement). This gives real-time
	// team data with zero admin credential usage: "authenticate once,
	// authorize per-request".
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.cfg.aepBaseURL+"/aep/v1/user/me", nil)
	if err != nil {
		return
	}
	request.Header.Set("Authorization", "Bearer "+ident.Token)
	request.Header.Set("X-AEP-Protocol-Version", "1.0")
	resp, err := serviceProbeClient.Do(request)
	if err != nil {
		p.logger.Warn("enrichTeams: /user/me unreachable", "error", err)
		return // keep cached teams; next request retries
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		p.logger.Warn("enrichTeams: /user/me returned non-OK", "status", resp.StatusCode)
		return
	}
	var result struct {
		User struct {
			TeamIDs []string `json:"teamIds"`
		} `json:"user"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		return
	}
	ident.Teams = result.User.TeamIDs
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
// provisioning status, for the admin console's team selector.
func (p *portal) handleListDepartments(w http.ResponseWriter, r *http.Request, ident identity) {
	adminToken := p.adminToken(r.Context())
	if adminToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "admin token unavailable"})
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		p.cfg.aepBaseURL+"/aep/v1/admin/teams?limit=100", nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("X-AEP-Protocol-Version", "1.0")
	resp, err := serviceProbeClient.Do(request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var result struct {
		Teams []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"teams"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "bad upstream response"})
		return
	}
	out := make([]map[string]any, 0, len(result.Teams))
	for _, team := range result.Teams {
		out = append(out, map[string]any{"id": team.ID, "name": team.Name})
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
