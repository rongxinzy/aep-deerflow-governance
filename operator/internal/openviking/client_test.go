package openviking

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newOVStub(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return New(server.URL, "root-key")
}

func TestRegisterUserFoundAndCreated(t *testing.T) {
	t.Run("new user is created and returns the key", func(t *testing.T) {
		client := newOVStub(t, func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "root-key", r.Header.Get("X-API-Key"))
			_, _ = w.Write([]byte(`{"status":"ok","result":{"user_key":"fresh-key"}}`))
		})
		key, found, err := client.RegisterUser(context.Background(), "acc", "user-2")
		require.NoError(t, err)
		assert.False(t, found)
		assert.Equal(t, "fresh-key", key)
	})

	t.Run("already-existing user reports found without a key", func(t *testing.T) {
		client := newOVStub(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"status":"error","error":{"message":"user ALREADY_EXISTS"}}`))
		})
		_, found, err := client.RegisterUser(context.Background(), "acc", "user-1")
		require.NoError(t, err)
		assert.True(t, found)
	})
}

func TestRegenerateAndDelete(t *testing.T) {
	t.Run("regenerate returns the rotated key", func(t *testing.T) {
		client := newOVStub(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/admin/accounts/acc/users/u/key", r.URL.Path)
			_, _ = w.Write([]byte(`{"status":"ok","result":{"user_key":"rotated"}}`))
		})
		key, err := client.RegenerateKey(context.Background(), "acc", "u")
		require.NoError(t, err)
		assert.Equal(t, "rotated", key)
	})

	t.Run("delete surfaces upstream errors", func(t *testing.T) {
		client := newOVStub(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		require.Error(t, client.DeleteUser(context.Background(), "acc", "u"))
	})
}

func TestPostTransportFailures(t *testing.T) {
	// Point at a dead port: the transport error path must be reported.
	client := New("http://127.0.0.1:1", "k")
	_, _, err := client.RegisterUser(context.Background(), "a", "u")
	require.Error(t, err)
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "abc", truncate([]byte("abcdef"), 3))
	assert.Equal(t, "abcdef", truncate([]byte("abcdef"), 10))
}

func TestRegisterUserEnsuringAccount(t *testing.T) {
	t.Run("a missing account is created before the register is retried", func(t *testing.T) {
		var calls []string
		client := newOVStub(t, func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, r.Method+" "+r.URL.Path)
			switch r.URL.Path {
			case "/api/v1/admin/accounts":
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(t, "tenant-all-users", body["account_id"])
				assert.Equal(t, "portal", body["admin_user_id"], "same admin contract as the portal's department provisioning")
				w.WriteHeader(http.StatusCreated)
			case "/api/v1/admin/accounts/tenant-all-users/users":
				if len(calls) == 1 {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte("Account not found: tenant-all-users"))
					return
				}
				_, _ = w.Write([]byte(`{"result":{"user_key":"minted"}}`))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		})

		key, found, err := client.RegisterUserEnsuringAccount(context.Background(), "tenant-all-users", "whz-test")
		require.NoError(t, err)
		assert.False(t, found)
		assert.Equal(t, "minted", key)
		assert.Equal(t, []string{
			"POST /api/v1/admin/accounts/tenant-all-users/users",
			"POST /api/v1/admin/accounts",
			"POST /api/v1/admin/accounts/tenant-all-users/users",
		}, calls)
	})

	t.Run("an account that already exists is not an error", func(t *testing.T) {
		client := newOVStub(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/admin/accounts" {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":{"code":"ALREADY_EXISTS","message":"account exists"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":{"user_key":"minted"}}`))
		})
		_, _, err := client.RegisterUserEnsuringAccount(context.Background(), "acc", "u")
		require.NoError(t, err)
	})

	t.Run("another failure is never retried as a missing account", func(t *testing.T) {
		calls := 0
		client := newOVStub(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		})
		_, _, err := client.RegisterUserEnsuringAccount(context.Background(), "acc", "u")
		require.Error(t, err)
		assert.Equal(t, 1, calls, "only an Account-not-found answer triggers account creation")
	})

	t.Run("a failed account creation keeps the original error", func(t *testing.T) {
		client := newOVStub(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/admin/accounts" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("nope"))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("Account not found: acc"))
		})
		key, found, err := client.RegisterUserEnsuringAccount(context.Background(), "acc", "u")
		require.Error(t, err)
		assert.Empty(t, key)
		assert.False(t, found)
		assert.Contains(t, err.Error(), "Account not found")
		assert.Contains(t, err.Error(), `creating OpenViking account "acc" failed`)
	})
}
