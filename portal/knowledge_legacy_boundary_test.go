package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLegacyKnowledgeProxyIsGoneForManagedTenant(t *testing.T) {
	h := newAPIHarness(t)
	h.cfg.managedKnowledge.tenantID = "tenant-1"
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	h.cfg.knowledgeAPIURL = upstream.URL

	sessionToken, csrf := h.sessions.issue(identity{
		Token:          "cached-admin-token",
		UserID:         "admin-user",
		Username:       "Admin",
		DeploymentID:   "demo",
		Roles:          []string{"platform-admin"},
		TeamsFetchedAt: time.Now(),
	})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/knowledge/proxy/knowledge-bases/kb-1", strings.NewReader(`{"name":"changed"}`))
	request.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: sessionToken})
	request.Header.Set("X-CSRF-Token", csrf)
	response := httptest.NewRecorder()
	h.routes().ServeHTTP(response, request)

	require.Equal(t, http.StatusGone, response.Code, response.Body.String())
	var gone managedError
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &gone))
	require.Equal(t, "KNOWLEDGE_LEGACY_PROXY_DISABLED", gone.Error.Code)
	require.Equal(t, "旧版知识库代理已停用，请改用托管知识库接口", gone.Error.Message)
	require.Zero(t, upstreamCalls, "legacy proxy must not authenticate or forward requests for a managed tenant")
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}
