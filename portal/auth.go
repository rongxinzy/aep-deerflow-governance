package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// identity is the authenticated caller resolved from an AEP access token.
type identity struct {
	Token          string    `json:"-"`
	UserID         string    `json:"id"`
	Username       string    `json:"-"`
	DisplayName    string    `json:"displayName"`
	Kind           string    `json:"kind"`
	Roles          []string  `json:"roles"`
	Teams          []string  `json:"-"` // AEP team ids; used for department scoping
	TeamsFetchedAt time.Time `json:"-"` // when Teams was last refreshed
	DeploymentID   string    `json:"deploymentId,omitempty"`
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

// canAccessEmployee applies the open-scope rule: admins bypass everything
// (the "大老板" requirement) and owners always pass. An explicit
// spec.visibility then decides — mode "all" opens the employee to every
// user, mode "restricted" admits members of the listed teams and the
// listed users, and any unknown mode fails closed. A nil visibility keeps
// the legacy department rule: same-team members pass, an empty (unscoped)
// team means owner/admin only.
func (i identity) canAccessEmployee(cfg config, team, ownerID string, visibility *dev1alpha1.VisibilitySpec) bool {
	if i.isAdmin(cfg) {
		return true
	}
	if ownerID != "" && i.UserID == ownerID {
		return true
	}
	if visibility != nil {
		switch visibility.Mode {
		case "all":
			return true
		case "restricted":
			for _, entry := range visibility.Teams {
				if entry.ID != "" && i.inTeam(entry.ID) {
					return true
				}
			}
			for _, entry := range visibility.Users {
				if entry.ID != "" && entry.ID == i.UserID {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return i.inTeam(team)
}

// employeeAccessReason is the JSON shape for the workbench roster's
// "why can I use this employee" column.
type employeeAccessReason struct {
	Kind     string `json:"kind"`
	TeamID   string `json:"teamId,omitempty"`
	TeamName string `json:"teamName,omitempty"`
	UserID   string `json:"userId,omitempty"`
}

// employeeAccessReason reports why the caller can see one employee, in
// machine-readable form for the workbench roster ("权限来源"). Kind is one
// of owner | all | team | user | legacy-team | admin; team/user carry the
// granting subject, and team carries the spec's name snapshot when present
// (the client resolves legacy-team ids against /api/v1/me teams). The
// admin bypass is deliberately NOT checked first, so admins get the
// informative reason too; "admin" only falls out for rows the caller sees
// purely through the administrator bypass (any other caller would have
// been filtered by canAccessEmployee before reaching this).
func (i identity) accessReason(team, ownerID string, visibility *dev1alpha1.VisibilitySpec) employeeAccessReason {
	if ownerID != "" && i.UserID == ownerID {
		return employeeAccessReason{Kind: "owner"}
	}
	if visibility != nil {
		switch visibility.Mode {
		case "all":
			return employeeAccessReason{Kind: "all"}
		case "restricted":
			for _, entry := range visibility.Teams {
				if entry.ID != "" && i.inTeam(entry.ID) {
					return employeeAccessReason{Kind: "team", TeamID: entry.ID, TeamName: entry.Name}
				}
			}
			for _, entry := range visibility.Users {
				if entry.ID != "" && entry.ID == i.UserID {
					return employeeAccessReason{Kind: "user", UserID: entry.ID}
				}
			}
			return employeeAccessReason{Kind: "admin"}
		default:
			return employeeAccessReason{Kind: "admin"}
		}
	}
	if i.inTeam(team) {
		return employeeAccessReason{Kind: "legacy-team", TeamID: team}
	}
	return employeeAccessReason{Kind: "admin"}
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
	defer func() { _ = response.Body.Close() }()
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
		Roles        []string `json:"roles"`
		DeploymentID string   `json:"deploymentId"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return identity{}, err
	}
	if body.User.ID == "" {
		return identity{}, fmt.Errorf("aep verify returned no user")
	}
	ident := identity{
		Token:        token,
		UserID:       body.User.ID,
		DisplayName:  body.User.DisplayName,
		Kind:         body.User.Kind,
		Roles:        body.Roles,
		DeploymentID: body.DeploymentID,
		Teams:        body.User.TeamIDs,
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
