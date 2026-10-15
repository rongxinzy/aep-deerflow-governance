package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceDesktopConfiguration(t *testing.T) {
	h := newWorkspaceHarness(t)
	response := postWorkspace(t, h, `{"name":"ws-cloud","runtime":"microvm","desktop":{"enabled":true},"templateTag":"desktop-v1","companions":[{"name":"assistant"}]}`, ownerIdentity())
	require.Equal(t, 201, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), `"memory":"4Gi"`)
	require.Contains(t, response.Body.String(), `"vcpus":2`)
	for _, body := range []string{
		`{"resources":{"memory":"1Gi"}}`, `{"resources":{"disk":"40Gi"}}`,
		`{"templateTag":"../bad"}`, `{"resources":{"vcpus":9}}`,
	} {
		req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body))
		req.SetPathValue("name", "ws-cloud")
		w := httptest.NewRecorder()
		h.updateWorkspace(w, req, ownerIdentity())
		require.Equal(t, 400, w.Code, body)
	}
	req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"resources":{"vcpus":4,"memory":"8Gi"},"templateTag":"desktop-v2"}`))
	req.SetPathValue("name", "ws-cloud")
	w := httptest.NewRecorder()
	h.updateWorkspace(w, req, ownerIdentity())
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"disk":"20Gi"`)
	require.Contains(t, w.Body.String(), `"templateTag":"desktop-v2"`)
	req = httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(`{"desktop":{"enabled":false}}`))
	req.SetPathValue("name", "ws-cloud")
	w = httptest.NewRecorder()
	h.updateWorkspace(w, req, identity{UserID: "other"})
	require.Equal(t, 403, w.Code)
}

func TestWorkspaceConfigurationValidation(t *testing.T) {
	for _, body := range []string{
		`{"runtime":"unknown"}`, `{"desktop":{"enabled":true}}`, `{"templateTag":"desktop-v1"}`,
		`{"resources":{"memory":"oops"}}`, `{"resources":{"memory":"64Gi"}}`,
		`{"runtime":"microvm","desktop":{"enabled":true},"resources":{"vcpus":1}}`,
	} {
		h := newWorkspaceHarness(t)
		body = strings.TrimSuffix(body, "}") + `,"companions":[{"name":"assistant"}]}`
		w := postWorkspace(t, h, body, ownerIdentity())
		require.Equal(t, 400, w.Code, w.Body.String())
	}
	spec := dev1alpha1.PersonalWorkspaceSpec{}
	require.NoError(t, applyWorkspaceConfiguration(&spec, workspaceConfiguration{}, false))
	require.Equal(t, "pod", spec.Runtime)
}
