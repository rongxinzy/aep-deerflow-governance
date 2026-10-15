package main

import (
	"fmt"
	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"testing"
)

func TestAddWorkspaceCompanion(t *testing.T) {
	ws := runningWorkspace("ws-owned", "u-owner", "assistant")
	ws.Spec.Companions[0].Persona = "existing private persona"
	ws.Spec.Companions[0].Memory = &dev.CompanionMemory{UserKeySecret: "preserve-me"}
	h := newWorkspaceHarness(t, ws)
	call := func(body string, who identity) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/workspaces/ws-owned/companions", strings.NewReader(body))
		req.SetPathValue("name", "ws-owned")
		w := httptest.NewRecorder()
		h.addWorkspaceCompanion(w, req, who)
		return w
	}
	body := `{"name":"researcher","displayName":"研究搭档","persona":"每天整理资料"}`
	require.Equal(t, 403, call(body, identity{UserID: "other"}).Code)
	require.Equal(t, 403, call(body, adminIdentity()).Code)
	require.Equal(t, 400, call(`{`, ownerIdentity()).Code)
	require.Equal(t, 400, call(`{"name":"bad_name","displayName":"搭档"}`, ownerIdentity()).Code)
	require.Equal(t, 400, call(`{"name":"a","displayName":" "}`, ownerIdentity()).Code)
	require.Equal(t, 201, call(body, ownerIdentity()).Code)
	require.Equal(t, 200, call(body, ownerIdentity()).Code)
	require.Equal(t, 409, call(strings.Replace(body, "每天整理资料", "different", 1), ownerIdentity()).Code)
	var stored dev.PersonalWorkspace
	key := client.ObjectKey{Namespace: h.cfg.namespace, Name: ws.Name}
	require.NoError(t, h.k8s.Get(t.Context(), key, &stored))
	require.Len(t, stored.Spec.Companions, 2)
	require.Equal(t, ws.Spec.Companions[0], stored.Spec.Companions[0])
	require.Equal(t, "organization", contactSettings(&stored)["researcher"].Mode)
	for i := 2; i < 10; i++ {
		stored.Spec.Companions = append(stored.Spec.Companions, dev.CompanionSpec{Name: fmt.Sprintf("other-%d", i), Model: "test"})
	}
	require.NoError(t, h.k8s.Update(t.Context(), &stored))
	require.Equal(t, 409, call(`{"name":"overflow","displayName":"太多"}`, ownerIdentity()).Code)
	require.Equal(t, 200, call(body, ownerIdentity()).Code)
}
func TestWorkspaceCompanionReady(t *testing.T) {
	h := collaborationHarness(t)
	call := func(name string, who identity) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/ready", nil)
		req.SetPathValue("name", "ws-source")
		req.SetPathValue("companion", name)
		w := httptest.NewRecorder()
		h.workspaceCompanionReady(w, req, who)
		return w
	}
	who := identity{UserID: "u-x"}
	require.Equal(t, 403, call("assistant", identity{UserID: "stranger"}).Code)
	require.Equal(t, 404, call("missing", who).Code)
	roster := `{"companions":[{"name":"assistant"}]}`
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(roster)) }))
	defer gateway.Close()
	h.workspaceChatOverride = gateway.URL
	require.JSONEq(t, `{"ready":true}`, call("assistant", who).Body.String())
	roster = `{"companions":[]}`
	require.JSONEq(t, `{"ready":false}`, call("assistant", who).Body.String())
	roster = `not json`
	require.JSONEq(t, `{"ready":false}`, call("assistant", who).Body.String())
	gateway.Close()
	require.JSONEq(t, `{"ready":false}`, call("assistant", who).Body.String())
}
