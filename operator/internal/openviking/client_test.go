package openviking

import (
	"context"
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
