package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// identity is the authenticated caller resolved from an AEP access token.
type identity struct {
	Token       string   `json:"-"`
	UserID      string   `json:"id"`
	Username    string   `json:"-"`
	DisplayName string   `json:"displayName"`
	Kind        string   `json:"kind"`
	Roles       []string `json:"roles"`
	Teams       []string `json:"-"` // AEP team ids; used for department scoping
	TeamsFetchedAt time.Time `json:"-"` // when Teams was last refreshed
}

// teamsStaleAt is how long cached team memberships remain trusted before
// the portal re-fetches them from AEP. Department changes (UI-driven)
// take effect within this window without requiring re-login.
const teamsStaleAt = 5 * time.Minute

// needsTeamRefresh reports whether the cached team memberships are due
// for refresh. Admin check is separate (Roles, also cached) but role
// changes are rarer and the session TTL bounds them.
func (i *identity) needsTeamRefresh() bool {
	return time.Since(i.TeamsFetchedAt) > teamsStaleAt
}

// inTeam reports whether the caller belongs to the given AEP team.
// An empty team means the employee is not scoped to any department:
// only the owner and admins can access it (strict by default — set
// spec.team to grant department-level access).
func (i identity) inTeam(team string) bool {
	if team == "" {
		return false // unscoped: owner/admin only, not open to everyone
	}
	for _, t := range i.Teams {
		if t == team {
			return true
		}
	}
	return false
}

// canAccessEmployee applies the department-scoping rule: admins bypass all
// team scoping (the "大老板" requirement); owners always can; same-team
// members can; everyone else cannot.
func (i identity) canAccessEmployee(cfg config, team, ownerID string) bool {
	if i.isAdmin(cfg) {
		return true
	}
	if ownerID != "" && i.UserID == ownerID {
		return true
	}
	return i.inTeam(team)
}

func (i identity) isAdmin(cfg config) bool {
	if cfg.adminUsers[i.UserID] || cfg.adminUsers[strings.ToLower(i.Username)] {
		return true
	}
	for _, role := range i.Roles {
		if cfg.adminRoles[role] {
			return true
		}
	}
	return false
}

type aepAuthenticator struct {
	baseURL string
	http    *http.Client
}

func (a *aepAuthenticator) verify(ctx context.Context, token string) (identity, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/aep/v1/user/me", nil)
	if err != nil {
		return identity{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-AEP-Protocol-Version", "1.0")
	response, err := a.http.Do(request)
	if err != nil {
		return identity{}, fmt.Errorf("aep verify: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return identity{}, fmt.Errorf("aep verify returned %d", response.StatusCode)
	}
	var body struct {
		User struct {
			ID          string   `json:"id"`
			DisplayName string   `json:"displayName"`
			Email       *string  `json:"email"`
			Kind        string   `json:"kind"`
			TeamIDs     []string `json:"teamIds"`
		} `json:"user"`
		Roles []string `json:"roles"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return identity{}, err
	}
	if body.User.ID == "" {
		return identity{}, fmt.Errorf("aep verify returned no user")
	}
	ident := identity{
		Token:       token,
		UserID:      body.User.ID,
		DisplayName: body.User.DisplayName,
		Kind:        body.User.Kind,
		Roles:       body.Roles,
		Teams:       body.User.TeamIDs,
	}
	if ident.Teams == nil {
		ident.Teams = []string{}
	}
	// Username: derive from displayName when AEP does not expose it on /me.
	ident.Username = body.User.DisplayName
	if ident.Username == "" {
		ident.Username = ident.UserID
	}
	return ident, nil
}

// bearer extracts the Bearer token from a request.
func bearer(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}
