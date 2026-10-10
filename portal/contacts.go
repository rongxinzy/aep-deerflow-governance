package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// handleContacts lists the company-wide human account directory for the
// workbench messenger roster (通讯录). Any authenticated employee may read it:
// the surface is the same directory an IM client shows — display names,
// usernames, and team memberships for the org-structure view (组织架构).
// AEP's admin user listing is the only user-enumeration API, so the portal
// calls it with its cached admin token server-side and projects the result
// down to {userId, username, displayName, teams}, filtering to active human
// accounts and dropping the requesting user themselves (a roster never lists
// "you"). Team ids resolve to names via the admin teams listing; a team the
// listing cannot resolve is dropped rather than shown as a bare id.
func (p *portal) handleContacts(w http.ResponseWriter, r *http.Request, ident identity) {
	adminToken := p.adminToken(r.Context())
	if adminToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "admin token unavailable"})
		return
	}

	teamNames, teamErr := p.fetchTeamNames(r.Context(), adminToken)
	if teamErr != nil {
		// Fail closed, same as a failed users page: a silent partial org
		// structure would surface as departments flickering in and out.
		// The upstream detail stays in the log; employees get the same
		// sanitized wording as the users-page failure path.
		if p.logger != nil {
			p.logger.Warn("contacts: team listing failed", "error", teamErr.Error())
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "aep returned an error listing teams"})
		return
	}

	type aepUser struct {
		ID          string   `json:"id"`
		Username    string   `json:"username"`
		DisplayName string   `json:"displayName"`
		Kind        string   `json:"kind"`
		Status      string   `json:"status"`
		TeamIDs     []string `json:"teamIds"`
	}

	type contactTeam struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	contacts := make([]map[string]any, 0, 64)
	cursor := ""
	// Guard: AEP pages at most 200 per call; 10 pages bounds a directory at
	// 2000 accounts, which is far past anything this deployment size needs —
	// beyond that we serve a partial directory rather than loop on a broken
	// cursor.
	for page := 0; page < 10; page++ {
		query := url.Values{"limit": {"200"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
			p.cfg.aepBaseURL+"/aep/v1/admin/users?"+query.Encode(), nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("X-AEP-Protocol-Version", "1.0")
		resp, err := serviceProbeClient.Do(req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		// Fail closed: AEP errors are RFC 9457 problem+json — valid JSON that
		// would decode with empty items and surface as a 200 "no colleagues"
		// (or a silent partial directory if a later page fails). Any non-200
		// page rejects the whole listing.
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if p.logger != nil {
				p.logger.Warn("contacts: aep listing failed", "status", resp.StatusCode, "body", string(raw))
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("aep returned %d", resp.StatusCode)})
			return
		}
		var page struct {
			Items []aepUser `json:"items"`
			Next  *string   `json:"nextCursor"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&page)
		_ = resp.Body.Close()
		if decodeErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "bad upstream"})
			return
		}
		for _, u := range page.Items {
			// The listing contract returns human accounts only, but stay
			// defensive: never surface agent accounts or disabled ones, and
			// a roster does not contain the requester.
			if u.Kind != "" && u.Kind != "human" {
				continue
			}
			if u.Status != "" && u.Status != "active" {
				continue
			}
			if u.ID == ident.UserID || u.ID == "" || u.Username == "" {
				continue
			}
			teams := make([]contactTeam, 0, len(u.TeamIDs))
			for _, id := range u.TeamIDs {
				if name, ok := teamNames[id]; ok {
					teams = append(teams, contactTeam{ID: id, Name: name})
				}
			}
			contacts = append(contacts, map[string]any{
				"userId": u.ID, "username": u.Username, "displayName": u.DisplayName, "teams": teams,
			})
		}
		if page.Next == nil || *page.Next == "" {
			break
		}
		cursor = *page.Next
	}
	writeJSON(w, http.StatusOK, map[string]any{"contacts": contacts})
}

// fetchTeamNames resolves AEP team ids to display names for the contacts org
// structure. Follows nextCursor with the same 10-page guard as the users loop
// (AEP pages teams for real; stopping at page one would silently drop every
// member past the 100th team into 未分组); any non-200 page fails the whole
// map so the caller can fail closed instead of half-grouping the directory.
// Upstream bodies stay in the error for the server log — the employee-facing
// response gets the sanitized message only (same discipline as the users loop).
func (p *portal) fetchTeamNames(ctx context.Context, adminToken string) (map[string]string, error) {
	names := make(map[string]string, 16)
	cursor := ""
	for page := 0; page < 10; page++ {
		query := url.Values{"limit": {"100"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			p.cfg.aepBaseURL+"/aep/v1/admin/teams?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("X-AEP-Protocol-Version", "1.0")
		resp, err := serviceProbeClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("teams listing returned %d: %s", resp.StatusCode, string(raw))
		}
		var result struct {
			Teams []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"teams"`
			Next *string `json:"nextCursor"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("bad teams listing: %w", err)
		}
		_ = resp.Body.Close()
		for _, t := range result.Teams {
			if t.ID != "" && t.Name != "" {
				names[t.ID] = t.Name
			}
		}
		if result.Next == nil || *result.Next == "" {
			return names, nil
		}
		cursor = *result.Next
	}
	return names, nil
}
