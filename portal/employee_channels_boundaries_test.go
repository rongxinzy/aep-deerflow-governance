package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func TestChannelIdentityAndEmployeeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, employee, deployment string
		status                     int
	}{
		{"invalid name", "../other", "", 400},
		{"missing employee", "missing-bot", "", 404},
		{"wrong enterprise", "hr-bot", "another-enterprise", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := channelPortal(t, func(http.ResponseWriter, *http.Request) { t.Error("rejected input reached runtime") })
			ident := identity{UserID: "admin-user"}
			if tc.deployment != "" {
				p.cfg.aepDeploymentID = "this-enterprise"
				ident.DeploymentID = tc.deployment
			}
			r := channelRequest("GET", "", "", "")
			r.SetPathValue("name", tc.employee)
			w := httptest.NewRecorder()
			p.handleEmployeeChannels(w, r, ident)
			require.Equal(t, tc.status, w.Code)
		})
	}
}

func TestChannelRuntimeTransportRejectsRedirectsAndOversizedResponses(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status, size int
	}{
		{"redirect", 302, 0}, {"oversized", 200, (1 << 20) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := channelPortal(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirect-target" {
					t.Error("privileged request followed redirect")
				}
				w.Header().Set("Location", "/redirect-target")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(strings.Repeat("x", tc.size)))
			})
			w := httptest.NewRecorder()
			p.handleEmployeeChannelAction(w, channelRequest("POST", "feishu", "restart", ""), identity{UserID: "admin-user"})
			require.Equal(t, 502, w.Code)
			require.Contains(t, w.Body.String(), "CHANNEL_RUNTIME_UNAVAILABLE")
		})
	}
}

func TestChannelMissingRuntimeCredentialFailsBeforeCallingUpstream(t *testing.T) {
	for _, remove := range []bool{false, true} {
		p := channelPortal(t, func(http.ResponseWriter, *http.Request) { t.Error("missing credential reached runtime") })
		var secret corev1.Secret
		require.NoError(t, p.k8s.Get(t.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: memkeySecretName("hr-bot")}, &secret))
		if remove {
			require.NoError(t, p.k8s.Delete(t.Context(), &secret))
		} else {
			secret.Data = nil
			require.NoError(t, p.k8s.Update(t.Context(), &secret))
		}
		_, status, err := p.runtimeChannelRequest(context.Background(), "hr-bot", "admin-user", "GET", "/api/channels/providers", nil)
		require.Error(t, err)
		require.Equal(t, 503, status)
	}
}

func TestChannelRejectsMalformedSuccessAndDisabledActions(t *testing.T) {
	for _, tc := range []struct{ action, result string }{
		{"qr-login", `{"id":"short","status":"pending"}`},
		{"qr-login", `{"id":"abcdefghijklmnop","status":"unknown"}`},
		{"qr-login", `not-json`}, {"restart", `{"success":false}`}, {"restart", `not-json`},
	} {
		p := channelPortal(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tc.result)) })
		w := httptest.NewRecorder()
		p.handleEmployeeChannelAction(w, channelRequest("POST", "wechat", tc.action, ""), identity{UserID: "admin-user"})
		require.Equal(t, 502, w.Code)
	}
	for _, phase := range []dev1alpha1.DigitalEmployeePhase{dev1alpha1.PhaseReady, dev1alpha1.PhaseSleeping} {
		p := channelPortal(t, func(http.ResponseWriter, *http.Request) { t.Error("disabled channel reached runtime") })
		var employee dev1alpha1.DigitalEmployee
		require.NoError(t, p.k8s.Get(t.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: "hr-bot"}, &employee))
		employee.Spec.Channels = nil
		require.NoError(t, p.k8s.Update(t.Context(), &employee))
		employee.Status.Phase = phase
		require.NoError(t, p.k8s.Status().Update(t.Context(), &employee))
		w := httptest.NewRecorder()
		p.handleEmployeeChannelAction(w, channelRequest("POST", "feishu", "restart", ""), identity{UserID: "admin-user"})
		require.Equal(t, 409, w.Code)
		require.Contains(t, w.Body.String(), "CHANNEL_NOT_ENABLED")
		w = httptest.NewRecorder()
		p.handleWakeEmployeeChannels(w, channelRequest("POST", "", "", ""), identity{UserID: "admin-user"})
		require.Equal(t, 202, w.Code)
	}
}
