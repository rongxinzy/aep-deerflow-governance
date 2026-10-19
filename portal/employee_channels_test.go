package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func channelRequest(method, provider, action, body string) *http.Request {
	r := httptest.NewRequest(method, "/api/v1/employees/hr-bot/channels", strings.NewReader(body))
	r.SetPathValue("name", "hr-bot")
	r.SetPathValue("provider", provider)
	r.SetPathValue("action", action)
	return r
}

func channelPortal(t *testing.T, handler http.HandlerFunc) *portal {
	t.Helper()
	employee := readyEmployee("hr-bot", "owner")
	employee.Spec.Channels = &dev1alpha1.ChannelsSpec{Feishu: true, Wecom: true, Wechat: true}
	p, _, _ := chatTestPortal(t, employee, memkeySecret("hr-bot"))
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	p.chatBaseOverride = upstream.URL
	return p
}

func TestChannelReadRedactsCredentialsAndUsesWorkerState(t *testing.T) {
	p := channelPortal(t, func(w http.ResponseWriter, r *http.Request) {
		require.Empty(t, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Cookie"))
		require.Equal(t, "tok-123", r.Header.Get("X-DeerFlow-Internal-Token"))
		mac := hmac.New(sha256.New, []byte("tok-123"))
		_, _ = mac.Write([]byte("zhiyuan-channel-admin-v1\nGET\n" + r.URL.Path + "\nadmin-user"))
		require.Equal(t, hex.EncodeToString(mac.Sum(nil)), r.Header.Get(channelAdminHeader))
		if r.URL.Path == "/api/channels/providers" {
			writeJSON(w, 200, map[string]any{"enabled": true, "providers": []map[string]any{{"provider": "feishu", "enabled": true, "configured": true, "credential_values": map[string]string{"app_id": "app-test", "app_secret": "secret-never-return", "bot_token": "other-secret"}}}})
		} else {
			writeJSON(w, 200, map[string]any{"service_running": true, "channels": map[string]any{"feishu": map[string]any{"running": true, "debug_secret": "secret-never-return"}}})
		}
	})
	w := httptest.NewRecorder()
	p.handleEmployeeChannels(w, channelRequest("GET", "", "", ""), identity{UserID: "admin-user"})
	require.Equal(t, 200, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.Contains(t, w.Body.String(), `"workerState":"running"`)
	require.Contains(t, w.Body.String(), `"hasSecret":true`)
	require.Contains(t, w.Body.String(), "app-test")
	require.NotContains(t, w.Body.String(), "secret-never-return")
	require.NotContains(t, w.Body.String(), "other-secret")
	require.NotContains(t, w.Body.String(), "connection_status")
}

func TestChannelReadDoesNotInventHealth(t *testing.T) {
	for _, phase := range []dev1alpha1.DigitalEmployeePhase{dev1alpha1.PhaseSleeping, dev1alpha1.PhaseReady} {
		t.Run(string(phase), func(t *testing.T) {
			calls := 0
			p := channelPortal(t, func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(503) })
			var employee dev1alpha1.DigitalEmployee
			require.NoError(t, p.k8s.Get(t.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: "hr-bot"}, &employee))
			employee.Spec.Hibernation = &dev1alpha1.HibernationSpec{Enabled: new(true)}
			require.NoError(t, p.k8s.Update(t.Context(), &employee))
			employee.Status.Phase = phase
			require.NoError(t, p.k8s.Status().Update(t.Context(), &employee))
			w := httptest.NewRecorder()
			p.handleEmployeeChannels(w, channelRequest("GET", "", "", ""), identity{UserID: "admin-user"})
			require.Equal(t, 200, w.Code)
			require.Contains(t, w.Body.String(), `"runtimeAvailable":false`)
			require.Contains(t, w.Body.String(), `"hibernationEnabled":true`)
			require.NotContains(t, w.Body.String(), `"workerState":"running"`)
			if phase == dev1alpha1.PhaseSleeping {
				require.Zero(t, calls)
			}
		})
	}
}

func TestChannelMalformedRuntimeResponseIsUnknown(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"enabled":true,"providers":null}`} {
		p := channelPortal(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
		w := httptest.NewRecorder()
		p.handleEmployeeChannels(w, channelRequest("GET", "", "", ""), identity{UserID: "admin-user"})
		require.Contains(t, w.Body.String(), `"runtimeAvailable":false`)
		require.NotContains(t, w.Body.String(), `"workerState":"connected"`)
	}
}

func TestChannelReportsOnlyObservedPlatformConnection(t *testing.T) {
	p := channelPortal(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "providers") {
			_, _ = w.Write([]byte(`{"enabled":true,"providers":[]}`))
		} else {
			_, _ = w.Write([]byte(`{"service_running":true,"channels":{"wecom":{"running":true,"connected":true},"feishu":{"running":true,"connected":false}}}`))
		}
	})
	w := httptest.NewRecorder()
	p.handleEmployeeChannels(w, channelRequest("GET", "", "", ""), identity{UserID: "admin-user"})
	var result struct {
		Providers []employeeChannel `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Equal(t, "running", result.Providers[0].WorkerState)
	require.Equal(t, "connected", result.Providers[1].WorkerState)
	require.Equal(t, "unknown", result.Providers[2].WorkerState)
}

func TestChannelManagementRejectsNonAdminAndUnreadyWake(t *testing.T) {
	p := channelPortal(t, func(_ http.ResponseWriter, _ *http.Request) { t.Error("unauthorized runtime call") })
	w := httptest.NewRecorder()
	p.handleEmployeeChannels(w, channelRequest("GET", "", "", ""), identity{UserID: "owner"})
	require.Equal(t, 403, w.Code, "employee ownership is not channel admin permission")
	var employee dev1alpha1.DigitalEmployee
	require.NoError(t, p.k8s.Get(t.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: "hr-bot"}, &employee))
	employee.Status.Phase = dev1alpha1.PhasePending
	require.NoError(t, p.k8s.Status().Update(t.Context(), &employee))
	w = httptest.NewRecorder()
	p.handleWakeEmployeeChannels(w, channelRequest("POST", "", "", ""), identity{UserID: "admin-user"})
	require.Equal(t, 409, w.Code)
}

func TestChannelActivationPreservesOtherProviders(t *testing.T) {
	enableCalls := 0
	p := channelPortal(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "DELETE":
			require.Equal(t, "/api/channels/wechat/runtime-config", r.URL.Path)
			writeJSON(w, 200, map[string]bool{"enabled": false})
		case "PUT":
			enableCalls++
			require.Equal(t, "/api/channels/wechat/enabled", r.URL.Path)
			var body map[string]bool
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, map[string]bool{"enabled": true}, body)
			writeJSON(w, 200, map[string]any{"enabled": true, "configured": true})
		default:
			t.Errorf("unexpected runtime call %s %s", r.Method, r.URL.Path)
		}
	})
	w := httptest.NewRecorder()
	p.handleActivateEmployeeChannel(w, channelRequest("PUT", "wechat", "", `{"enabled":false}`), identity{UserID: "admin-user"})
	require.Equal(t, 202, w.Code, w.Body.String())
	var employee dev1alpha1.DigitalEmployee
	require.NoError(t, p.k8s.Get(t.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: "hr-bot"}, &employee))
	require.True(t, employee.Spec.Channels.Feishu)
	require.True(t, employee.Spec.Channels.Wecom)
	require.False(t, employee.Spec.Channels.Wechat)
	require.Zero(t, enableCalls, "disabling must not call the hot-apply endpoint")
	w = httptest.NewRecorder()
	p.handleActivateEmployeeChannel(w, channelRequest("PUT", "wechat", "", `{"enabled":true}`), identity{UserID: "admin-user"})
	require.Equal(t, 202, w.Code)
	require.Equal(t, 1, enableCalls, "enabling must hot-apply through the runtime")
}

func TestChannelEnableHotApplyFailureSurfacesAndKeepsDeclaredIntent(t *testing.T) {
	for _, status := range []int{400, 403, 404, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			p := channelPortal(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "PUT", r.Method)
				require.Equal(t, "/api/channels/wecom/enabled", r.URL.Path)
				var body map[string]bool
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, map[string]bool{"enabled": true}, body)
				http.Error(w, "credential=never-echo", status)
			})
			w := httptest.NewRecorder()
			p.handleActivateEmployeeChannel(w, channelRequest("PUT", "wecom", "", `{"enabled":true}`), identity{UserID: "admin-user"})
			require.Equal(t, status, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), "never-echo")
			var employee dev1alpha1.DigitalEmployee
			require.NoError(t, p.k8s.Get(t.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: "hr-bot"}, &employee))
			require.True(t, employee.Spec.Channels.Wecom, "the CR stays the declared intent so the operator can retry")
			require.True(t, employee.Spec.Channels.Feishu)
		})
	}
}

func TestChannelEnableHotApplyTransportFailureIsRuntimeUnavailable(t *testing.T) {
	p := channelPortal(t, func(_ http.ResponseWriter, _ *http.Request) {})
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upstream.Close()
	p.chatBaseOverride = upstream.URL
	w := httptest.NewRecorder()
	p.handleActivateEmployeeChannel(w, channelRequest("PUT", "feishu", "", `{"enabled":true}`), identity{UserID: "admin-user"})
	require.Equal(t, 502, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "CHANNEL_RUNTIME_UNAVAILABLE")
}

func TestChannelActivationRejectsHibernationBeforeMutatingEmployee(t *testing.T) {
	for _, provider := range channelProviders {
		t.Run(provider, func(t *testing.T) {
			p := channelPortal(t, func(_ http.ResponseWriter, _ *http.Request) { t.Error("activation must not call runtime") })
			var employee dev1alpha1.DigitalEmployee
			key := client.ObjectKey{Namespace: p.cfg.namespace, Name: "hr-bot"}
			require.NoError(t, p.k8s.Get(t.Context(), key, &employee))
			employee.Spec.Channels = nil
			employee.Spec.Hibernation = &dev1alpha1.HibernationSpec{Enabled: new(true)}
			require.NoError(t, p.k8s.Update(t.Context(), &employee))
			w := httptest.NewRecorder()
			p.handleActivateEmployeeChannel(w, channelRequest("PUT", provider, "", `{"enabled":true}`), identity{UserID: "admin-user"})
			require.Equal(t, 409, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "CHANNEL_HIBERNATION_CONFLICT")
			require.NoError(t, p.k8s.Get(t.Context(), key, &employee))
			require.Nil(t, employee.Spec.Channels)
			require.True(t, *employee.Spec.Hibernation.Enabled, "activation must not silently increase resource residency")
		})
	}
}

func TestChannelDisableRecoversErrorEmployeeWithoutChangingHibernation(t *testing.T) {
	p := channelPortal(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "DELETE", r.Method)
		require.Equal(t, "/api/channels/wecom/runtime-config", r.URL.Path)
		writeJSON(w, 200, map[string]bool{"enabled": false})
	})
	var employee dev1alpha1.DigitalEmployee
	key := client.ObjectKey{Namespace: p.cfg.namespace, Name: "hr-bot"}
	require.NoError(t, p.k8s.Get(t.Context(), key, &employee))
	employee.Spec.Hibernation = &dev1alpha1.HibernationSpec{Enabled: new(true)}
	require.NoError(t, p.k8s.Update(t.Context(), &employee))
	employee.Status.Phase = dev1alpha1.PhaseError
	require.NoError(t, p.k8s.Status().Update(t.Context(), &employee))
	w := httptest.NewRecorder()
	p.handleActivateEmployeeChannel(w, channelRequest("PUT", "wecom", "", `{"enabled":false}`), identity{UserID: "admin-user"})
	require.Equal(t, 202, w.Code, w.Body.String())
	require.NoError(t, p.k8s.Get(t.Context(), key, &employee))
	require.False(t, employee.Spec.Channels.Wecom)
	require.True(t, employee.Spec.Channels.Feishu)
	require.True(t, *employee.Spec.Hibernation.Enabled)
}

func TestChannelDisableFailureKeepsGate(t *testing.T) {
	p := channelPortal(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	w := httptest.NewRecorder()
	p.handleActivateEmployeeChannel(w, channelRequest("PUT", "feishu", "", `{"enabled":false}`), identity{UserID: "admin-user"})
	require.Equal(t, 503, w.Code)
	var employee dev1alpha1.DigitalEmployee
	require.NoError(t, p.k8s.Get(t.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: "hr-bot"}, &employee))
	require.True(t, employee.Spec.Channels.Feishu)
}

func TestChannelActionsFixedPathsAndQRRedaction(t *testing.T) {
	for _, tc := range []struct{ provider, action, body, method, path, result string }{
		{"feishu", "runtime-config", `{"values":{"app_id":"demo","app_secret":"disposable-test"}}`, "POST", "/api/channels/feishu/runtime-config", `{}`},
		{"wecom", "restart", "", "POST", "/api/channels/wecom/restart", `{"success":true}`},
		{"wechat", "qr-login", "", "POST", "/api/channels/wechat/qr-login", `{"id":"abcdefghijklmnop","status":"pending","qrcode_content":"test-qr","expires_in":180,"provider":{"credential_values":{"bot_token":"secret-test"}}}`},
		{"wechat", "qr-poll", `{"id":"abcdefghijklmnop","verifyCode":"123456"}`, "POST", "/api/channels/wechat/qr-login/abcdefghijklmnop/poll", `{"id":"abcdefghijklmnop","status":"confirmed","qrcode_content":"test-qr","expires_in":170,"provider":{"bot_token":"secret-test"}}`},
		{"wechat", "qr-cancel", `{"id":"abcdefghijklmnop"}`, "DELETE", "/api/channels/wechat/qr-login/abcdefghijklmnop", `{}`},
	} {
		t.Run(tc.action, func(t *testing.T) {
			p := channelPortal(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, tc.method, r.Method)
				require.Equal(t, tc.path, r.URL.Path)
				if tc.action == "qr-poll" {
					var body map[string]string
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					require.Equal(t, "123456", body["verify_code"])
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.result))
			})
			w := httptest.NewRecorder()
			p.handleEmployeeChannelAction(w, channelRequest("POST", tc.provider, tc.action, tc.body), identity{UserID: "admin-user"})
			require.Equal(t, 200, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), "secret-test")
		})
	}
}

func TestChannelQRPollOmitsBlankVerificationCode(t *testing.T) {
	p := channelPortal(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.NotContains(t, body, "verify_code", "the runtime rejects an empty code with 422")
		writeJSON(w, 200, map[string]any{"id": "abcdefghijklmnop", "status": "pending", "qrcode_content": "test-qr", "expires_in": 180})
	})
	w := httptest.NewRecorder()
	p.handleEmployeeChannelAction(w, channelRequest("POST", "wechat", "qr-poll", `{"id":"abcdefghijklmnop"}`), identity{UserID: "admin-user"})
	require.Equal(t, 200, w.Code, w.Body.String())
}

func TestChannelRejectsInvalidInputsBeforeRuntime(t *testing.T) {
	for _, tc := range []struct{ provider, action, body string }{
		{"unknown", "restart", ""}, {"feishu", "unknown", ""},
		{"feishu", "runtime-config", `{"values":{"domain":"http://attacker.invalid"}}`},
		{"wecom", "runtime-config", `{"values":{"bot_secret":"bad\nvalue"}}`},
		{"wechat", "qr-poll", `{"id":"../../outside"}`},
		{"wechat", "qr-poll", `{"id":"abcdefghijklmnop","verifyCode":"abc"}`},
		{"feishu", "qr-login", ""}, {"feishu", "runtime-config", `{"extra":true}`},
	} {
		t.Run(tc.provider+tc.action+tc.body, func(t *testing.T) {
			p := channelPortal(t, func(_ http.ResponseWriter, _ *http.Request) { t.Error("invalid request reached runtime") })
			w := httptest.NewRecorder()
			p.handleEmployeeChannelAction(w, channelRequest("POST", tc.provider, tc.action, tc.body), identity{UserID: "admin-user"})
			require.Contains(t, []int{400, 404}, w.Code)
		})
	}
}

func TestChannelRuntimeFailureDoesNotLeakSDKBody(t *testing.T) {
	for _, status := range []int{400, 403, 404, 503} {
		p := channelPortal(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "credential=never-echo", status) })
		w := httptest.NewRecorder()
		p.handleEmployeeChannelAction(w, channelRequest("POST", "wecom", "restart", ""), identity{UserID: "admin-user"})
		require.Equal(t, status, w.Code)
		require.NotContains(t, w.Body.String(), "never-echo")
	}
}

func TestChannelManagementRevalidatesAdminRole(t *testing.T) {
	p := channelPortal(t, func(_ http.ResponseWriter, _ *http.Request) { t.Error("unauthorized runtime call") })
	p.cfg.adminUsers = map[string]bool{}
	p.cfg.adminRoles = map[string]bool{"platform-admin": true}
	aep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"user": map[string]any{"id": "admin-user"}, "roles": []string{}})
	}))
	t.Cleanup(aep.Close)
	p.aep = &aepAuthenticator{baseURL: aep.URL, http: aep.Client()}
	w := httptest.NewRecorder()
	p.freshAdminOnly(p.handleEmployeeChannels)(w, channelRequest("GET", "", "", ""), identity{UserID: "admin-user", Token: "test-access-token", Roles: []string{"platform-admin"}})
	require.Equal(t, 403, w.Code)
	require.Contains(t, w.Body.String(), "ADMIN_REQUIRED")
}
