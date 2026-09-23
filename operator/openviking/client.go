// Package openviking is a minimal admin client for the operator: it
// provisions one user (and its API key) per DigitalEmployee.
package openviking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	rootKey string
	http    *http.Client
}

func New(baseURL, rootKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		rootKey: rootKey,
		http:    &http.Client{Timeout: 20 * time.Second},
	}
}

// RegisterUser creates account/accountID user userID (idempotent: an
// ALREADY_EXISTS style error returns found=true) and returns the user key.
func (c *Client) RegisterUser(ctx context.Context, accountID, userID string) (key string, found bool, err error) {
	body, _ := json.Marshal(map[string]any{"user_id": userID, "role": "user"})
	key, err = c.post(ctx, fmt.Sprintf("/api/v1/admin/accounts/%s/users", accountID), body)
	if err == nil {
		return key, false, nil
	}
	if strings.Contains(err.Error(), "ALREADY_EXISTS") || strings.Contains(err.Error(), "already exists") {
		return "", true, nil
	}
	return "", false, err
}

// RegenerateKey mints a fresh key for an existing user (the create response
// is the only place a key is returned verbatim, so regeneration is how the
// operator recovers a key it failed to persist).
func (c *Client) RegenerateKey(ctx context.Context, accountID, userID string) (string, error) {
	body, _ := json.Marshal(map[string]any{})
	return c.post(ctx, fmt.Sprintf("/api/v1/admin/accounts/%s/users/%s/key", accountID, userID), body)
}

// DeleteUser removes the user on DigitalEmployee deletion (best effort).
func (c *Client) DeleteUser(ctx context.Context, accountID, userID string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+fmt.Sprintf("/api/v1/admin/accounts/%s/users/%s", accountID, userID), nil)
	if err != nil {
		return err
	}
	c.auth(request)
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 16<<10))
	if response.StatusCode >= 200 && response.StatusCode < 300 || response.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("delete user returned %d", response.StatusCode)
}

func (c *Client) post(ctx context.Context, path string, body []byte) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	c.auth(request)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("POST %s returned %d: %s", path, response.StatusCode, truncate(raw, 300))
	}
	var envelope struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", err
	}
	if envelope.Error != nil {
		return "", fmt.Errorf("POST %s: %s: %s", path, envelope.Error.Code, envelope.Error.Message)
	}
	payload := envelope.Result
	if len(payload) == 0 {
		payload = raw
	}
	var result struct {
		UserKey string `json:"user_key"`
	}
	if err := json.Unmarshal(payload, &result); err != nil || result.UserKey == "" {
		return "", fmt.Errorf("POST %s: response carries no user_key", path)
	}
	return result.UserKey, nil
}

func (c *Client) auth(request *http.Request) {
	request.Header.Set("X-API-Key", c.rootKey)
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n]
	}
	return s
}
