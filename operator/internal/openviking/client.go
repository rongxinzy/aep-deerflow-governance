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
	if isAlreadyExists(err) {
		return "", true, nil
	}
	return "", false, err
}

// EnsureAccount creates accountID when OpenViking does not have it yet
// (idempotent: an ALREADY_EXISTS style answer counts as success). It is the
// same admin contract the portal uses to provision a department account.
func (c *Client) EnsureAccount(ctx context.Context, accountID string) error {
	body, _ := json.Marshal(map[string]any{"account_id": accountID, "admin_user_id": "portal"})
	if _, err := c.postRaw(ctx, "/api/v1/admin/accounts", body); err != nil {
		if isAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

// RegisterUserEnsuringAccount registers userID under accountID, creating the
// account first when OpenViking reports that it does not exist.
//
// Department accounts are provisioned by the portal's department
// orchestration, but that step can be skipped or can fail partially. A bare
// RegisterUser then answers "Account not found" on every reconcile forever, so
// the employee never leaves Error and nothing retries the missing account.
func (c *Client) RegisterUserEnsuringAccount(ctx context.Context, accountID, userID string) (string, bool, error) {
	key, found, err := c.RegisterUser(ctx, accountID, userID)
	if err == nil || !isAccountMissing(err) {
		return key, found, err
	}
	if ensureErr := c.EnsureAccount(ctx, accountID); ensureErr != nil {
		return "", false, fmt.Errorf("%w (creating OpenViking account %q failed: %v)", err, accountID, ensureErr)
	}
	return c.RegisterUser(ctx, accountID, userID)
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
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 16<<10))
	if response.StatusCode >= 200 && response.StatusCode < 300 || response.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("delete user returned %d", response.StatusCode)
}

func (c *Client) post(ctx context.Context, path string, body []byte) (string, error) {
	payload, err := c.postRaw(ctx, path, body)
	if err != nil {
		return "", err
	}
	var result struct {
		UserKey string `json:"user_key"`
	}
	if err := json.Unmarshal(payload, &result); err != nil || result.UserKey == "" {
		return "", fmt.Errorf("POST %s: response carries no user_key", path)
	}
	return result.UserKey, nil
}

// postRaw is the shared admin transport: it posts body to path and returns the
// decoded "result" payload (or the raw body when the response carries no
// envelope). Admin calls that do not mint a user key use this directly.
func (c *Client) postRaw(ctx context.Context, path string, body []byte) (json.RawMessage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.auth(request)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("POST %s returned %d: %s", path, response.StatusCode, truncate(raw, 300))
	}
	// Some admin creates answer 2xx with no body at all: for those the status
	// is the whole answer, and the callers below decide whether they still need
	// something (post) or not (EnsureAccount).
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage(nil), nil
	}
	var envelope struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("POST %s: %s: %s", path, envelope.Error.Code, envelope.Error.Message)
	}
	if len(envelope.Result) == 0 {
		return json.RawMessage(raw), nil
	}
	return envelope.Result, nil
}

// isAlreadyExists reports the admin API's answer for "this object is already
// there", which every create call in this package treats as success.
func isAlreadyExists(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "ALREADY_EXISTS") || strings.Contains(err.Error(), "already exists"))
}

// isAccountMissing reports the answer for "the account does not exist": the
// 404 the portal's department provisioning is expected to have created first.
func isAccountMissing(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Account not found")
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
