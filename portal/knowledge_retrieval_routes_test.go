package main

import (
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmployeeKnowledgeRetrievalRoutesUseAuthenticatedNonAdminMux(t *testing.T) {
	aep := fakeAEP(t)
	server := sessionTestPortal(t, aep)

	routes := []struct {
		method string
		path   string
		body   string
		status int
	}{
		{http.MethodPost, "/api/v1/knowledge/employees/invalid_name/search", `{}`, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/knowledge/employees/invalid_name/documents/doc-1", "", http.StatusNotFound},
		{http.MethodGet, "/api/v1/knowledge/employees/invalid_name/documents/doc-1/chunks/chunk-1/images/0", "", http.StatusNotFound},
		{http.MethodGet, "/api/v1/knowledge/employees/invalid_name/documents/doc-1/download", "", http.StatusNotFound},
	}

	// Every path is registered behind auth rather than falling through to 404.
	for _, route := range routes {
		req, err := http.NewRequest(route.method, server.URL+route.path, strings.NewReader(route.body))
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s %s", route.method, route.path)
		_ = resp.Body.Close()
	}

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{Jar: jar}
	resp, _ := postJSON(t, client, server.URL+"/api/v1/session", `{"username":"user1","password":"pw-one"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	csrf := csrfFrom(t, jar, server.URL)

	// user1 has only the ordinary "sales" role. Invalid employee names are
	// rejected by each real handler after authentication, proving these routes
	// are not wrapped in adminOnly and do not invoke downstream data access.
	for _, route := range routes {
		req, err := http.NewRequest(route.method, server.URL+route.path, strings.NewReader(route.body))
		require.NoError(t, err)
		if route.method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-CSRF-Token", csrf)
		}
		response, err := client.Do(req)
		require.NoError(t, err)
		require.Equal(t, route.status, response.StatusCode, "%s %s", route.method, route.path)
		_ = response.Body.Close()
	}
}
