package main

import (
	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkplaceAuthenticatedProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "user-1", r.Header.Get(workspaceOwnerHeaderName))
		assert.Len(t, r.Header.Get(workspaceSessionHeader), 64)
		assert.NotEqual(t, "browser-forged-session", r.Header.Get(workspaceSessionHeader))
		assert.Empty(t, r.Header.Get("X-Workspace-Agent-Token"))
		for _, header := range []string{"Cookie", "Authorization", "X-CSRF-Token", workspaceTokenHeader} {
			assert.Empty(t, r.Header.Get(header))
		}
		switch r.URL.Path {
		case "/api/workplace/desktop/screenshot":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("png-frame"))
		case "/api/workplace/desktop/input":
			body, _ := io.ReadAll(r.Body)
			assert.JSONEq(t, `{"leaseId":"lease-from-runtime","type":"click","x":20,"y":40}`, string(body))
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/workplace/files/content":
			assert.Equal(t, "reports/week.txt", r.URL.Query().Get("path"))
			assert.Equal(t, "1", r.URL.Query().Get("download"))
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Disposition", `attachment; filename="week.txt"`)
			_, _ = w.Write([]byte("actual file bytes"))
		case "/api/workplace/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {}\n\n"))
			w.(http.Flusher).Flush()
		default:
			assert.Equal(t, "/api/workplace", r.URL.Path)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer upstream.Close()
	h := newWorkspaceHarness(t, runningWorkspace("ws-mine", "user-1", "assistant"))
	aep := fakeAEP(t)
	h.aep = &aepAuthenticator{baseURL: aep.URL, http: aep.Client()}
	h.cfg.aepDeploymentID = "demo"
	h.workspaceChatOverride = upstream.URL
	server := httptest.NewServer(h.routes())
	defer server.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	_, loginBody := postJSON(t, client, server.URL+"/api/v1/session", `{"username":"user1","password":"pw-one"}`)
	require.Contains(t, loginBody, "user-1")
	for _, path := range []string{"", "/files/content?path=reports%2Fweek.txt&download=1", "/stream", "/desktop/screenshot"} {
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/workplace"+path, nil)
		request.Header.Set(workspaceOwnerHeaderName, "forged-owner")
		request.Header.Set(workspaceTokenHeader, "forged-token")
		request.Header.Set(workspaceSessionHeader, "browser-forged-session")
		request.Header.Set("X-Workspace-Agent-Token", "browser-forged-agent")
		response, err := client.Do(request)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode, string(body))
		if strings.Contains(path, "content?") {
			assert.Equal(t, `attachment; filename="week.txt"`, response.Header.Get("Content-Disposition"))
			assert.Equal(t, "actual file bytes", string(body))
		}
		if path == "/desktop/screenshot" {
			assert.Equal(t, "image/png", response.Header.Get("Content-Type"))
			assert.Equal(t, "png-frame", string(body))
		}
		if path == "/stream" {
			assert.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
			assert.Contains(t, string(body), "data:")
		}
	}
	// The actual mux decodes wildcard values: these must never become
	// /api/rooms in the Node gateway and bypass room roster authorization.
	for _, suffix := range []string{"/%2e%2e/rooms", "/%2e%2e%2frooms", "/.%2e/rooms", "/%2e%2e%5crooms"} {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/workplace"+suffix, strings.NewReader(`{"members":[]}`))
		request.Header.Set("X-CSRF-Token", csrfFrom(t, jar, server.URL))
		response, err := client.Do(request)
		require.NoError(t, err)
		_ = response.Body.Close()
		assert.Equal(t, http.StatusBadRequest, response.StatusCode, suffix)
	}

	for _, withCSRF := range []bool{false, true} {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/workplace/desktop/input", strings.NewReader(`{"leaseId":"lease-from-runtime","type":"click","x":20,"y":40}`))
		if withCSRF {
			request.Header.Set("X-CSRF-Token", csrfFrom(t, jar, server.URL))
		}
		response, err := client.Do(request)
		require.NoError(t, err)
		_ = response.Body.Close()
		if withCSRF {
			assert.Equal(t, http.StatusOK, response.StatusCode)
		} else {
			assert.Equal(t, http.StatusForbidden, response.StatusCode)
		}
	}
}

func TestWorkplaceOwnerAndPhase(t *testing.T) {
	ws := runningWorkspace("ws-mine", "u-owner", "assistant")
	h, paths := workspaceRoomsFixture(t, ws)
	for _, ident := range []identity{{UserID: "other"}, adminIdentity()} {
		response := httptest.NewRecorder()
		h.handleWorkplace(response, httptest.NewRequest(http.MethodGet, "/api/v1/workplace", nil), ident)
		assert.Equal(t, http.StatusNotFound, response.Code)
	}
	assert.Empty(t, *paths)
	ws.Status.Phase = dev1alpha1.WorkspacePhasePending
	h, paths = workspaceRoomsFixture(t, ws)
	response := httptest.NewRecorder()
	h.handleWorkplace(response, httptest.NewRequest(http.MethodGet, "/api/v1/workplace", nil), ownerIdentity())
	assert.Equal(t, http.StatusConflict, response.Code)
	assert.Empty(t, *paths)
}

func TestWorkspaceSummaryRuntimeResources(t *testing.T) {
	ws := runningWorkspace("ws-mine", "u-owner", "assistant")
	ws.Spec.Runtime = "microvm"
	ws.Spec.Resources = dev1alpha1.WorkspaceResources{Memory: "8Gi", Disk: "40Gi", VCPUs: 4}
	summary := summarizeWorkspace(ws)
	assert.Equal(t, "microvm", summary["runtime"])
	assert.Equal(t, ws.Spec.Resources, summary["resources"])
	ws.Spec.Runtime = ""
	assert.Equal(t, "pod", summarizeWorkspace(ws)["runtime"])
}

func TestWorkspaceSessionBinding(t *testing.T) {
	first := httptest.NewRequest(http.MethodPost, "/api/v1/workplace/control", nil)
	first.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: "first-session"})
	first.Header.Set(workspaceSessionHeader, "spoofed")
	firstID := workspaceSessionID(first)
	require.Len(t, firstID, 64)
	assert.NotContains(t, firstID, "first-session")
	assert.Equal(t, firstID, workspaceSessionID(first))
	second := httptest.NewRequest(http.MethodPost, "/api/v1/workplace/control", nil)
	second.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: "second-session"})
	assert.NotEqual(t, firstID, workspaceSessionID(second))
	// authenticate() selects Bearer before cookies; the binding must do so too.
	first.Header.Set("Authorization", "Bearer verified-token")
	second.Header.Set("Authorization", "Bearer verified-token")
	assert.Equal(t, workspaceSessionID(first), workspaceSessionID(second))
	assert.NotEqual(t, firstID, workspaceSessionID(first))
}
