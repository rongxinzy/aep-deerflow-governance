package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// identity is the authenticated caller resolved from an AEP access token.
type identity struct {
	Token       string   `json:"-"`
	UserID      string   `json:"id"`
	Username    string   `json:"-"`
	DisplayName string   `json:"displayName"`
	Kind        string   `json:"kind"`
	Roles       []string `json:"roles"`
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
			ID          string  `json:"id"`
			DisplayName string  `json:"displayName"`
			Email       *string `json:"email"`
			Kind        string  `json:"kind"`
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
