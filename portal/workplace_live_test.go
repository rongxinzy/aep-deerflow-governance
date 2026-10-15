package main

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

// Opt-in browser acceptance against an explicitly supplied disposable gateway.
// The real portal mux/session/CSRF/proxy and BFF/UI are exercised; only AEP and
// the Kubernetes client are fixtures. Never point this at a user's workspace.
func TestWorkplaceLiveBrowser(t *testing.T) {
	gateway, script := os.Getenv("WORKPLACE_ACCEPTANCE_GATEWAY"), os.Getenv("WORKPLACE_ACCEPTANCE_SCRIPT")
	if gateway == "" || script == "" {
		t.Skip("requires disposable gateway and browser acceptance script")
	}
	ws := runningWorkspace("ws-cloud-qa", "user-1", "assistant")
	ws.Spec.Runtime = "microvm"
	ws.Spec.Desktop.Enabled = true
	ws.Spec.Resources = dev1alpha1.WorkspaceResources{VCPUs: 2, Memory: "4Gi", Disk: "2Gi"}
	ws.Spec.TemplateTag = "desktop-qa"
	ws.Status.VM = &dev1alpha1.WorkspaceVMStatus{TemplateTag: "desktop-qa", DesktopEnabled: true, Resources: ws.Spec.Resources}
	h := newWorkspaceHarness(t, ws)
	aep := fakeAEP(t)
	h.aep = &aepAuthenticator{baseURL: aep.URL, http: aep.Client()}
	h.cfg.aepDeploymentID = "demo"
	h.workspaceChatOverride = gateway
	server := httptest.NewServer(h.routes())
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", script)
	command.Env = append(os.Environ(), "PORTAL_BASE_URL="+server.URL, "AEP_BASE_URL="+aep.URL)
	output, err := command.CombinedOutput()
	t.Log(string(output))
	require.NoError(t, err)
}
