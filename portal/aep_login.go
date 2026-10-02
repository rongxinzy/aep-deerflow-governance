package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// passwordLogin exchanges AEP credentials for an access token — the same
// contract the governance extension uses (POST /aep/v1/auth/password/login,
// X-AEP-Protocol-Version pinned). Only the portal's /chat entry page calls
// it; the token never leaves the portal process (the session cookie is the
// browser-side credential).
func (a *aepAuthenticator) passwordLogin(ctx context.Context, deploymentID, username, password string) (string, error) {
	payload, _ := json.Marshal(map[string]string{
		"deploymentId": deploymentID,
		"sessionId":    fmt.Sprintf("de-portal-%d", time.Now().UnixNano()),
		"username":     username,
		"password":     password,
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/aep/v1/auth/password/login", strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-AEP-Protocol-Version", "1.0")
	response, err := a.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("aep login: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("aep login returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("aep login returned no accessToken")
	}
	return parsed.AccessToken, nil
}
