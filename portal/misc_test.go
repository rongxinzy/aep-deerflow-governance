package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func TestConfigAndHelpers(t *testing.T) {
	t.Setenv("PORTAL_ADMIN_USERS", "alice, bob")
	t.Setenv("PORTAL_QUOTA_PER_USER", "5")
	t.Setenv("PORTAL_WAKE_TIMEOUT_SECONDS", "90")
	cfg := loadConfig()
	assert.Equal(t, map[string]bool{"alice": true, "bob": true}, cfg.adminUsers)
	assert.Equal(t, 5, cfg.quotaPerUser)
	assert.Equal(t, 90*1_000_000_000, int(cfg.wakeTimeout))

	assert.Equal(t, "fallback", env("PORTAL_MISSING_KEY", "fallback"))
	assert.Equal(t, 7, envInt("PORTAL_MISSING_INT", 7))
	assert.Equal(t, map[string]bool{"a": true, "b": true}, set("a,b,"))
}

func TestStatusOf(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, statusOf(apierrors.NewNotFound(schema.GroupResource{}, "x")))
	assert.Equal(t, http.StatusConflict, statusOf(apierrors.NewAlreadyExists(schema.GroupResource{}, "x")))
	assert.Equal(t, http.StatusInternalServerError, statusOf(assert.AnError))
}

func TestEvaluatePolicy(t *testing.T) {
	admin := identity{UserID: "admin-user", Roles: []string{"admin"}}
	human := identity{UserID: "u1", Kind: "human"}

	t.Run("admin-only mode", func(t *testing.T) {
		decision := evaluatePolicy("admin-only", admin.IsAdminForTest(), false, 0, 2)
		assert.True(t, decision.Create || decision.NeedsApproval)
		rejected := evaluatePolicy("admin-only", false, human.Kind == "human", 0, 2)
		assert.False(t, rejected.Create || rejected.NeedsApproval)
	})

	t.Run("approval mode queues humans and guards quota", func(t *testing.T) {
		within := evaluatePolicy("approval", false, true, 1, 2)
		assert.True(t, within.NeedsApproval, "humans park for approval")
		over := evaluatePolicy("approval", false, true, 2, 2)
		assert.False(t, over.Create || over.NeedsApproval, "quota blocks before parking")
		assert.Equal(t, http.StatusConflict, over.HTTPStatus)
		adminDirect := evaluatePolicy("approval", true, true, 0, 2)
		assert.True(t, adminDirect.Create, "admins skip approval")
		agents := evaluatePolicy("approval", false, false, 0, 2)
		assert.False(t, agents.Create || agents.NeedsApproval, "agents may not apply")
		assert.Equal(t, http.StatusForbidden, agents.HTTPStatus)
	})
}

func TestSummarizeCoversChannelShapes(t *testing.T) {
	item := readyCR("x", "u1")
	item.Spec.Channels = &dev1alpha1.ChannelsSpec{Wecom: true, WecomName: "数字员工测试"}
	out := summarize(item)
	assert.Equal(t, "x", out["name"])
	channels := out["channels"].(map[string]any)
	assert.Equal(t, true, channels["wecom"])
	assert.Equal(t, "数字员工测试", channels["wecomName"])
}

func TestAdminOnlyGate(t *testing.T) {
	h := newAPIHarness(t)
	reached := false
	gated := h.portal.adminOnly(func(_ http.ResponseWriter, _ *http.Request, _ identity) { reached = true })

	w := httptest.NewRecorder()
	gated(w, httptest.NewRequest(http.MethodPost, "/x", nil), identity{UserID: "user-x"})
	assert.False(t, reached)
	assert.Equal(t, http.StatusForbidden, w.Code)

	gated(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil), adminIdentity())
	assert.True(t, reached)
}

func TestChatEntryPage(t *testing.T) {
	h := newAPIHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/chat", nil)
	req.Host = "portal.example:30190"
	w := httptest.NewRecorder()
	h.portal.handleChatEntry(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, w.Body.String(), "30195")
}

func TestSessionLoginFlow(t *testing.T) {
	t.Run("bad credentials are rejected", func(t *testing.T) {
		h := newAPIHarness(t)
		// No AEP stub: verification fails closed.
		h.portal.aep = &aepAuthenticator{baseURL: "http://127.0.0.1:1", http: http.DefaultClient}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/session",
			strings.NewReader(`{"username":"u","password":"wrong"}`))
		w := httptest.NewRecorder()
		h.portal.handleSessionLogin(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("logout clears cookies", func(t *testing.T) {
		h := newAPIHarness(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/session/logout", nil)
		req.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: "tok"})
		w := httptest.NewRecorder()
		h.portal.handleSessionLogout(w, req)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Header().Get("Set-Cookie"), "Max-Age=0")
	})

	t.Run("whoami echoes the session identity", func(t *testing.T) {
		h := newAPIHarness(t)
		ident := adminIdentity()
		ident.Token = "tok"
		w := httptest.NewRecorder()
		h.portal.handleSessionWhoami(w, httptest.NewRequest(http.MethodGet, "/api/v1/session", nil), ident)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "admin-user")
	})
}

func TestDecideRequestApprovesAndCreates(t *testing.T) {
	h := newAPIHarness(t)
	rows := sqlmock.NewRows([]string{"employee_name", "spec_json"}).
		AddRow("parked-helper", `{"metadata":{"name":"parked-helper","namespace":"digital-employees"},"spec":{"displayName":"助手"}}`)
	h.dbmock.ExpectQuery(`SELECT employee_name, spec_json FROM requests`).WithArgs("req-9").WillReturnRows(rows)
	h.dbmock.ExpectExec(`UPDATE requests SET state=`).WillReturnResult(sqlmock.NewResult(0, 1))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/requests/req-9/approve", nil)
	req.SetPathValue("id", "req-9")
	w := httptest.NewRecorder()
	h.portal.decideRequest("approved")(w, req, adminIdentity())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var item dev1alpha1.DigitalEmployee
	require.NoError(t, h.k8s.Get(t.Context(), objectKey("parked-helper"), &item))
	assert.Equal(t, "助手", item.Spec.DisplayName)
}

func TestKnowledgeUIURLDerivation(t *testing.T) {
	h := newAPIHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "ops.example:30190"
	assert.Equal(t, "http://ops.example:30163", h.portal.knowledgeUIURL(req))
	req.Host = "ops.example:30196"
	assert.Equal(t, "http://ops.example:30163", h.portal.knowledgeUIURL(req))
	h.portal.cfg.knowledgeUIURL = "http://override"
	assert.Equal(t, "http://override", h.portal.knowledgeUIURL(req))
}

func TestMarkActivityAndHibernating(t *testing.T) {
	h := newAPIHarness(t)
	h.dbmock.ExpectExec(`INSERT INTO employee_activity`).WillReturnResult(sqlmock.NewResult(1, 1))
	h.portal.markActivity("sales-helper")
	assert.NoError(t, h.dbmock.ExpectationsWereMet())

	on := true
	item := readyCR("x", "u")
	item.Spec.Hibernation = &dev1alpha1.HibernationSpec{Enabled: &on}
	assert.True(t, hibernating(item))
	assert.False(t, hibernating(readyCR("y", "u")))
}
