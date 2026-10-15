// Employee mailbox configuration specs: management authorization (admin or
// owner — chat visibility is NOT enough), validation mirrors (port
// allowlists, address shape), Secret write + CR wiring, masked reads.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func emailEmployeeFixture(name, owner string) *dev1alpha1.DigitalEmployee {
	return roomAgentEmployee(name, owner, nil, dev1alpha1.PhaseReady)
}

const goodMailboxes = `{"mailboxes":[{"label":"工作邮箱","address":"bot@corp.com","imap":{"host":"imap.corp.com","port":993,"tls":true},"smtp":{"host":"smtp.corp.com","port":465,"tls":true},"username":"bot","password":"app-pass"}]}`

func TestEmployeeEmailConfig(t *testing.T) {
	t.Run("owner configures: secret written, CR wired, credentials masked on read", func(t *testing.T) {
		h := newAPIHarness(t, emailEmployeeFixture("hr-bot", "u-owner"))
		put := httptest.NewRequest(http.MethodPut, "/api/v1/employees/hr-bot/email", strings.NewReader(goodMailboxes))
		put.SetPathValue("name", "hr-bot")
		w := httptest.NewRecorder()
		h.handlePutEmployeeEmail(w, put, identity{UserID: "u-owner"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		secret := &corev1.Secret{}
		require.NoError(t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: h.cfg.namespace, Name: "de-hr-bot-email"}, secret))
		var stored []map[string]any
		require.NoError(t, json.Unmarshal(secret.Data["EMAIL_MAILBOXES"], &stored))
		require.Len(t, stored, 1)
		assert.Equal(t, "bot@corp.com", stored[0]["address"])
		assert.Equal(t, "app-pass", stored[0]["password"], "Secret 里是完整凭证（运行时要用的就是它）")

		employee := &dev1alpha1.DigitalEmployee{}
		require.NoError(t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: h.cfg.namespace, Name: "hr-bot"}, employee))
		require.NotNil(t, employee.Spec.Email)
		assert.Equal(t, "de-hr-bot-email", employee.Spec.Email.SecretName)

		get := httptest.NewRequest(http.MethodGet, "/api/v1/employees/hr-bot/email", nil)
		get.SetPathValue("name", "hr-bot")
		w2 := httptest.NewRecorder()
		h.handleGetEmployeeEmail(w2, get, identity{UserID: "u-owner"})
		require.Equal(t, http.StatusOK, w2.Code)
		body := w2.Body.String()
		assert.Contains(t, body, `"hasPassword":true`)
		assert.NotContains(t, body, "app-pass", "读面掩码：凭证永不回显")
	})

	t.Run("validation mirrors the runtime rules", func(t *testing.T) {
		h := newAPIHarness(t, emailEmployeeFixture("hr-bot", "u-owner"))
		for _, tc := range []struct{ name, body string }{
			{"bad imap port", `{"mailboxes":[{"label":"a","address":"b@c.d","imap":{"host":"i","port":1430},"smtp":{"host":"s","port":465},"username":"u","password":"p"}]}`},
			{"bad smtp port", `{"mailboxes":[{"label":"a","address":"b@c.d","imap":{"host":"i","port":993},"smtp":{"host":"s","port":2525},"username":"u","password":"p"}]}`},
			{"bad address", `{"mailboxes":[{"label":"a","address":"not-an-address","imap":{"host":"i","port":993},"smtp":{"host":"s","port":465},"username":"u","password":"p"}]}`},
			{"missing password", `{"mailboxes":[{"label":"a","address":"b@c.d","imap":{"host":"i","port":993},"smtp":{"host":"s","port":465},"username":"u"}]}`},
			{"empty list", `{"mailboxes":[]}`},
			{"duplicate labels", `{"mailboxes":[` + strings.Repeat(`{"label":"a","address":"b@c.d","imap":{"host":"i","port":993},"smtp":{"host":"s","port":465},"username":"u","password":"p"},`, 2)[0:strings.Index(strings.Repeat(`x`, 0), "")] + `]}`},
		} {
			if tc.name == "duplicate labels" {
				tc.body = `{"mailboxes":[{"label":"a","address":"b@c.d","imap":{"host":"i","port":993},"smtp":{"host":"s","port":465},"username":"u","password":"p"},{"label":"a","address":"e@f.g","imap":{"host":"i","port":993},"smtp":{"host":"s","port":465},"username":"u","password":"p"}]}`
			}
			req := httptest.NewRequest(http.MethodPut, "/api/v1/employees/hr-bot/email", strings.NewReader(tc.body))
			req.SetPathValue("name", "hr-bot")
			w := httptest.NewRecorder()
			h.handlePutEmployeeEmail(w, req, identity{UserID: "u-owner"})
			assert.Equal(t, http.StatusBadRequest, w.Code, tc.name)
		}
	})

	t.Run("management is admin-or-owner: team visibility is not enough", func(t *testing.T) {
		employee := emailEmployeeFixture("hr-bot", "u-owner")
		employee.Spec.Team = "rd-dept" // team members may chat, not configure
		h := newAPIHarness(t, employee)
		req := httptest.NewRequest(http.MethodPut, "/api/v1/employees/hr-bot/email", strings.NewReader(goodMailboxes))
		req.SetPathValue("name", "hr-bot")
		w := httptest.NewRecorder()
		h.handlePutEmployeeEmail(w, req, identity{UserID: "u-teammate", Teams: []string{"rd-dept"}})
		assert.Equal(t, http.StatusForbidden, w.Code, "同组可见不授予邮箱配置权")

		w2 := httptest.NewRecorder()
		req2 := httptest.NewRequest(http.MethodGet, "/api/v1/employees/hr-bot/email", nil)
		req2.SetPathValue("name", "hr-bot")
		h.handleGetEmployeeEmail(w2, req2, adminIdentity())
		assert.Equal(t, http.StatusOK, w2.Code, "管理员可读")
	})

	t.Run("unknown employee is a 404", func(t *testing.T) {
		h := newAPIHarness(t)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/employees/ghost/email", nil)
		req.SetPathValue("name", "ghost")
		w := httptest.NewRecorder()
		h.handleGetEmployeeEmail(w, req, adminIdentity())
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}
