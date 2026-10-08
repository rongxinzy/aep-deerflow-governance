package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// TestAccessReason pins the "why can I see this" derivation to the same
// identity/visibility inputs as TestCanAccessEmployeeVisibility, so the
// gate and the explanation can never drift apart.
func TestAccessReason(t *testing.T) {
	owner := identity{UserID: "owner-a"}
	member := identity{UserID: "stranger", Teams: []string{"rd-dept"}}
	stranger := identity{UserID: "stranger"}
	admin := identity{UserID: "admin-user"}
	friend := identity{UserID: "friend"}

	restricted := &dev1alpha1.VisibilitySpec{
		Mode:  "restricted",
		Teams: []dev1alpha1.SubjectRef{{ID: "rd-dept", Name: "研发部"}},
		Users: []dev1alpha1.SubjectRef{{ID: "friend", Name: "好友"}},
	}

	cases := []struct {
		name  string
		ident identity
		team  string
		owner string
		vis   *dev1alpha1.VisibilitySpec
		want  employeeAccessReason
	}{
		{"owner", owner, "", "owner-a", restricted, employeeAccessReason{Kind: "owner"}},
		{"explicit all-users", stranger, "", "owner-a", &dev1alpha1.VisibilitySpec{Mode: "all"}, employeeAccessReason{Kind: "all"}},
		{"restricted team member", member, "sales-dept", "owner-a", restricted, employeeAccessReason{Kind: "team", TeamID: "rd-dept", TeamName: "研发部"}},
		{"restricted listed user", friend, "sales-dept", "owner-a", restricted, employeeAccessReason{Kind: "user", UserID: "friend"}},
		{"legacy department rule", member, "rd-dept", "owner-a", nil, employeeAccessReason{Kind: "legacy-team", TeamID: "rd-dept"}},
		{"admin-only row falls back to admin", admin, "rd-dept", "owner-a", restricted, employeeAccessReason{Kind: "admin"}},
		{"unknown mode falls back to admin", admin, "", "owner-a", &dev1alpha1.VisibilitySpec{Mode: "everyone-trust-me"}, employeeAccessReason{Kind: "admin"}},
		{"unscoped legacy falls back to admin", admin, "", "owner-a", nil, employeeAccessReason{Kind: "admin"}},
	}
	cfg := config{adminUsers: map[string]bool{"admin-user": true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.ident.accessReason(tc.team, tc.owner, tc.vis)
			assert.Equal(t, tc.want, got)
			// The load-bearing invariant: any non-admin who can access the
			// row must get an informative reason — the admin fallback is
			// only reachable through the canAccessEmployee admin bypass.
			// This executes for the five non-admin callers above; if either
			// helper's branch order drifts, it fails here.
			if !tc.ident.isAdmin(cfg) && tc.ident.canAccessEmployee(cfg, tc.team, tc.owner, tc.vis) {
				assert.NotEqual(t, "admin", got.Kind, "an accessible row reported the admin fallback")
			}
		})
	}
}

func TestListEmployeesAccessReason(t *testing.T) {
	visible := readyCR("sales-helper", "owner-a")
	visible.Spec.Description = "整理销售资料并辅助生成周报。"
	visible.Spec.Visibility = &dev1alpha1.VisibilitySpec{
		Mode:  "restricted",
		Teams: []dev1alpha1.SubjectRef{{ID: "rd-dept", Name: "研发部"}},
	}
	hidden := readyCR("hidden-helper", "owner-b")

	h := newAPIHarness(t, visible, hidden)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/employees", nil)
	w := httptest.NewRecorder()
	h.listEmployees(w, req, identity{UserID: "stranger", Teams: []string{"rd-dept"}})
	require.Equal(t, http.StatusOK, w.Code)

	var out struct {
		Employees []struct {
			Name         string               `json:"name"`
			Description  string               `json:"description"`
			AccessReason employeeAccessReason `json:"accessReason"`
		} `json:"employees"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out.Employees, 1, "rows the caller cannot access stay hidden")
	assert.Equal(t, "sales-helper", out.Employees[0].Name)
	assert.Equal(t, "整理销售资料并辅助生成周报。", out.Employees[0].Description)
	assert.Equal(t, employeeAccessReason{Kind: "team", TeamID: "rd-dept", TeamName: "研发部"}, out.Employees[0].AccessReason)
}

func TestMe(t *testing.T) {
	h := newApplyHarness(t)
	// One owned CR and one parked request: used = owned + pending = 2.
	require.NoError(t, h.k8s.Create(context.Background(), readyCR("owned-helper", "user-1")))
	rows := sqlmock.NewRows([]string{"count"}).AddRow(1)
	h.dbmock.ExpectQuery(`FROM requests WHERE owner_id=\$1 AND state='pending'`).WithArgs("user-1").WillReturnRows(rows)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	w := httptest.NewRecorder()
	h.handleMe(w, req, identity{UserID: "user-1", DisplayName: "张三", Kind: "human", Teams: []string{"sales-dept", "ghost"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var out struct {
		User struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
			Kind        string `json:"kind"`
		} `json:"user"`
		Teams []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"teams"`
		Quota struct {
			Limit   int `json:"limit"`
			Used    int `json:"used"`
			Owned   int `json:"owned"`
			Pending int `json:"pending"`
		} `json:"quota"`
		PolicyMode   string `json:"policyMode"`
		DefaultModel string `json:"defaultModel"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "张三", out.User.DisplayName)
	// Team names resolve through the admin directory; a missing team
	// degrades to an empty name instead of failing the whole response.
	assert.Equal(t, []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{{ID: "sales-dept", Name: "销售部"}, {ID: "ghost", Name: ""}}, out.Teams)
	assert.Equal(t, 2, out.Quota.Limit)
	assert.Equal(t, 2, out.Quota.Used, "parked requests count against the quota")
	assert.Equal(t, 1, out.Quota.Owned)
	assert.Equal(t, 1, out.Quota.Pending)
	assert.Equal(t, "self-service", out.PolicyMode)
	assert.Equal(t, "bench-glm", out.DefaultModel)

	t.Run("a team lookup outage degrades instead of failing", func(t *testing.T) {
		h := newApplyHarness(t)
		rows := sqlmock.NewRows([]string{"count"}).AddRow(0)
		h.dbmock.ExpectQuery(`FROM requests WHERE owner_id=\$1 AND state='pending'`).WithArgs("user-1").WillReturnRows(rows)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		w := httptest.NewRecorder()
		h.handleMe(w, req, identity{UserID: "user-1", Teams: []string{"boom"}})
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `{"id":"boom","name":""}`)
	})
}

func TestMyRequests(t *testing.T) {
	cols := []string{"id", "employee_name", "owner_id", "owner_name", "display_name", "state", "decision_reason", "created_at"}

	t.Run("lists every state for the caller with deploy legs", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("sales-helper", "user-1"))
		rows := sqlmock.NewRows(cols).
			AddRow("req-3", "sales-helper", "user-1", "张三", "销售数据助理", "approved", "", time.Now()).
			AddRow("req-2", "parked-helper", "user-1", "张三", "待批助理", "pending", "", time.Now()).
			AddRow("req-1", "gone-helper", "user-1", "张三", "已删助理", "deployed", "", time.Now())
		h.dbmock.ExpectQuery(`FROM requests WHERE owner_id=\$1 ORDER BY created_at DESC LIMIT 100`).WithArgs("user-1").WillReturnRows(rows)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/requests/mine", nil)
		w := httptest.NewRecorder()
		h.handleMyRequests(w, req, identity{UserID: "user-1"})
		require.Equal(t, http.StatusOK, w.Code)

		var out struct {
			Requests []struct {
				ID     string `json:"id"`
				State  string `json:"state"`
				Deploy struct {
					Exists bool   `json:"exists"`
					Phase  string `json:"phase"`
				} `json:"deploy"`
			} `json:"requests"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		require.Len(t, out.Requests, 3)
		// Approved and running.
		assert.Equal(t, "req-3", out.Requests[0].ID)
		assert.True(t, out.Requests[0].Deploy.Exists)
		assert.Equal(t, "Ready", out.Requests[0].Deploy.Phase)
		// Pending never probes the cluster.
		assert.False(t, out.Requests[1].Deploy.Exists)
		// Deployed record whose employee is gone reports exists=false.
		assert.Equal(t, "req-1", out.Requests[2].ID)
		assert.False(t, out.Requests[2].Deploy.Exists)
	})

	t.Run("state filter narrows the SQL", func(t *testing.T) {
		h := newAPIHarness(t)
		rows := sqlmock.NewRows(cols).
			AddRow("req-2", "parked-helper", "user-1", "张三", "待批助理", "pending", "", time.Now())
		h.dbmock.ExpectQuery(`FROM requests WHERE owner_id=\$1 AND state=\$2`).WithArgs("user-1", "pending").WillReturnRows(rows)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/requests/mine?state=pending", nil)
		w := httptest.NewRecorder()
		h.handleMyRequests(w, req, identity{UserID: "user-1"})
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("unknown state filter is rejected", func(t *testing.T) {
		h := newAPIHarness(t)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/requests/mine?state=weird", nil)
		w := httptest.NewRecorder()
		h.handleMyRequests(w, req, identity{UserID: "user-1"})
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})
}

func TestRequestDetail(t *testing.T) {
	cols := []string{"employee_name", "owner_id", "owner_name", "display_name", "spec_json", "state", "decision_reason", "decided_by", "created_at", "decided_at"}
	spec := `{"metadata":{"name":"sales-helper","annotations":{"digitalemployee.zhiyuan.io/apply-note":"仅使用销售团队可见资料。"}},"spec":{"description":"整理团队销售数据，辅助制作周报。","team":"sales-dept","runtime":{"model":"bench-glm"}}}`

	seed := func(t *testing.T, h *apiTestPortal, state, decidedBy string, decidedAt any) {
		t.Helper()
		rows := sqlmock.NewRows(cols).
			AddRow("sales-helper", "user-1", "张三", "销售数据助理", spec, state, "", decidedBy, time.Now(), decidedAt)
		h.dbmock.ExpectQuery(`FROM requests WHERE id=\$1`).WithArgs("req-1").WillReturnRows(rows)
	}

	detail := func(t *testing.T, h *apiTestPortal, ident identity) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/requests/req-1", nil)
		req.SetPathValue("id", "req-1")
		w := httptest.NewRecorder()
		h.handleRequestDetail(w, req, ident)
		return w
	}

	t.Run("owner sees the decoded spec, note and deployment leg", func(t *testing.T) {
		h := newApplyHarness(t) // fake AEP resolves the team name
		require.NoError(t, h.k8s.Create(context.Background(), readyCR("sales-helper", "user-1")))
		seed(t, h, "approved", "u-owner", time.Now())
		w := detail(t, h, identity{UserID: "user-1"})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var out struct {
			Request struct {
				State         string `json:"state"`
				Description   string `json:"description"`
				TeamID        string `json:"teamId"`
				TeamName      string `json:"teamName"`
				Note          string `json:"note"`
				Model         string `json:"model"`
				DecidedBy     string `json:"decidedBy"`
				DecidedByName string `json:"decidedByName"`
				DecidedAt     any    `json:"decidedAt"`
				Deploy        struct {
					Exists bool   `json:"exists"`
					Phase  string `json:"phase"`
				} `json:"deploy"`
			} `json:"request"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		assert.Equal(t, "整理团队销售数据，辅助制作周报。", out.Request.Description)
		assert.Equal(t, "sales-dept", out.Request.TeamID)
		assert.Equal(t, "销售部", out.Request.TeamName)
		assert.Equal(t, "仅使用销售团队可见资料。", out.Request.Note)
		assert.Equal(t, "bench-glm", out.Request.Model)
		assert.Equal(t, "u-owner", out.Request.DecidedBy)
		assert.Equal(t, "李四", out.Request.DecidedByName, "decided_by resolves through the admin directory")
		assert.NotNil(t, out.Request.DecidedAt)
		assert.True(t, out.Request.Deploy.Exists)
		assert.Equal(t, "Ready", out.Request.Deploy.Phase)
	})

	t.Run("pending requests show no decision and no deployment", func(t *testing.T) {
		h := newApplyHarness(t)
		seed(t, h, "pending", "", nil)
		w := detail(t, h, identity{UserID: "user-1"})
		require.Equal(t, http.StatusOK, w.Code)
		var out struct {
			Request struct {
				DecidedAt any `json:"decidedAt"`
				Deploy    struct {
					Exists bool `json:"exists"`
				} `json:"deploy"`
			} `json:"request"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		assert.Nil(t, out.Request.DecidedAt)
		assert.False(t, out.Request.Deploy.Exists)
	})

	t.Run("approved but deleted employee reports a missing deploy record", func(t *testing.T) {
		h := newApplyHarness(t) // no CR seeded for sales-helper
		seed(t, h, "approved", "", nil)
		w := detail(t, h, identity{UserID: "user-1"})
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"deploy":{"exists":false`)
	})

	t.Run("strangers get 404 (not 403) so ids do not leak existence", func(t *testing.T) {
		h := newApplyHarness(t)
		seed(t, h, "pending", "", nil)
		w := detail(t, h, identity{UserID: "someone-else"})
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("administrators may read any request", func(t *testing.T) {
		h := newApplyHarness(t)
		seed(t, h, "pending", "", nil)
		w := detail(t, h, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
	})
}

func TestApplyNoteHandling(t *testing.T) {
	t.Run("renderSpec stores the trimmed note as a CR annotation", func(t *testing.T) {
		p := &portal{cfg: config{namespace: "digital-employees"}}
		spec := p.renderSpec(applyRequest{Name: "note-helper", Note: "  仅使用销售团队可见资料。  "}, applyContext{}, nil)
		assert.Equal(t, "仅使用销售团队可见资料。", spec.Annotations[applyNoteAnnotation])

		blank := p.renderSpec(applyRequest{Name: "note-helper", Note: "   "}, applyContext{}, nil)
		assert.Nil(t, blank.Annotations, "a blank note leaves ObjectMeta untouched")
	})

	t.Run("an over-long note is a field-anchored violation", func(t *testing.T) {
		h := newApplyHarness(t)
		long := strings.Repeat("补", 501)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
			strings.NewReader(`{"name":"note-helper","description":"整理资料","team":"rd-dept","note":"`+long+`","visibility":{"mode":"restricted","teams":["rd-dept"]}}`))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, adminIdentity())
		require.Equal(t, http.StatusBadRequest, w.Code)
		var out struct {
			Violations []violation `json:"violations"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		require.NotEmpty(t, out.Violations)
		assert.Equal(t, "note", out.Violations[0].Field)
	})

	t.Run("whitespace padding does not reject an in-bounds note", func(t *testing.T) {
		h := newApplyHarness(t)
		h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		note := strings.Repeat(" ", 301) + strings.Repeat("补", 200) // 501 raw runes, 200 stored
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
			strings.NewReader(`{"name":"padded-helper","description":"整理资料","team":"rd-dept","note":"`+note+`","visibility":{"mode":"restricted","teams":["rd-dept"]}}`))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, adminIdentity())
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		var item dev1alpha1.DigitalEmployee
		require.NoError(t, h.k8s.Get(context.Background(), objectKey("padded-helper"), &item))
		assert.Equal(t, strings.Repeat("补", 200), item.Annotations[applyNoteAnnotation])
	})

	t.Run("a parked request's spec_json carries the note to the approved CR", func(t *testing.T) {
		h := newAPIHarness(t)
		rows := sqlmock.NewRows([]string{"employee_name", "spec_json"}).
			AddRow("note-helper", `{"metadata":{"name":"note-helper","namespace":"digital-employees","annotations":{"digitalemployee.zhiyuan.io/apply-note":"仅使用销售团队可见资料。"}},"spec":{"owner":"user-1"}}`)
		h.dbmock.ExpectQuery(`SELECT employee_name, spec_json FROM requests`).WithArgs("req-note").WillReturnRows(rows)
		h.dbmock.ExpectExec(`UPDATE requests SET state=`).WillReturnResult(sqlmock.NewResult(0, 1))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/requests/req-note/approve", nil)
		req.SetPathValue("id", "req-note")
		w := httptest.NewRecorder()
		h.decideRequest("approved")(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var item dev1alpha1.DigitalEmployee
		require.NoError(t, h.k8s.Get(context.Background(), objectKey("note-helper"), &item))
		assert.Equal(t, "仅使用销售团队可见资料。", item.Annotations[applyNoteAnnotation])
	})
}

func TestApplyUsesPlatformDefaultModel(t *testing.T) {
	payload := `{"name":"default-helper","displayName":"默认模型员工","description":"整理资料","team":"rd-dept","visibility":{"mode":"restricted","teams":["rd-dept"]}}`

	t.Run("applies without a model field ride the validated platform default", func(t *testing.T) {
		h := newApplyHarness(t)
		h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees", strings.NewReader(payload))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, adminIdentity())
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		var item dev1alpha1.DigitalEmployee
		require.NoError(t, h.k8s.Get(context.Background(), objectKey("default-helper"), &item))
		assert.Equal(t, "bench-glm", item.Spec.Runtime.Model)
		assert.Empty(t, item.Spec.Runtime.Models)
	})

	t.Run("a stale default fails the apply instead of creating a mute employee", func(t *testing.T) {
		h := newApplyHarness(t)
		h.cfg.model = "ghost-model"
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees", strings.NewReader(payload))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, adminIdentity())
		require.Equal(t, http.StatusBadRequest, w.Code)

		var out struct {
			Violations []violation `json:"violations"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		require.Len(t, out.Violations, 1)
		assert.Equal(t, "models", out.Violations[0].Field)
		assert.Contains(t, out.Violations[0].Message, "ghost-model")

		var item dev1alpha1.DigitalEmployee
		err := h.k8s.Get(context.Background(), objectKey("default-helper"), &item)
		assert.Error(t, err, "nothing was created")
	})
}

// TestLeakRoutesRequireAdmin exercises the real mux so the adminOnly gates
// cannot be lost in a routes() refactor: platform-wide probe routes must
// answer 403 to a regular session, and the whole workbench surface must be
// reachable without them.
func TestLeakRoutesRequireAdmin(t *testing.T) {
	aep := fakeAEP(t)
	server := sessionTestPortal(t, aep)
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	client := &http.Client{Jar: jar}
	resp, _ := postJSON(t, client, server.URL+"/api/v1/session", `{"username":"user1","password":"pw-one"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	csrf := csrfFrom(t, jar, server.URL)
	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/departments", ""},
		{http.MethodGet, "/api/v1/memory/status", ""},
		{http.MethodPost, "/api/v1/memory/search", `{"employee":"x"}`},
		{http.MethodGet, "/api/v1/knowledge/status", ""},
		{http.MethodGet, "/api/v1/knowledge/bases", ""},
	}
	for _, route := range routes {
		req, err := http.NewRequest(route.method, server.URL+route.path, strings.NewReader(route.body))
		require.NoError(t, err)
		req.Header.Set("X-CSRF-Token", csrf)
		response, err := client.Do(req)
		require.NoError(t, err)
		got := response.Status
		_ = response.Body.Close()
		assert.Equal(t, http.StatusForbidden, response.StatusCode, "%s %s → %s", route.method, route.path, got)
	}

	t.Run("administrators pass the gate", func(t *testing.T) {
		p := &portal{
			cfg:      config{namespace: "digital-employees", adminUsers: map[string]bool{"user-1": true}},
			k8s:      fake.NewClientBuilder().WithScheme(chatTestScheme(t)).Build(),
			aep:      &aepAuthenticator{baseURL: aep.URL, http: aep.Client()},
			sessions: newSessionStore(time.Hour),
			logger:   slog.New(slog.DiscardHandler),
		}
		server := httptest.NewServer(p.routes())
		t.Cleanup(server.Close)
		jar, err := cookiejar.New(nil)
		require.NoError(t, err)
		client := &http.Client{Jar: jar}
		resp, _ := postJSON(t, client, server.URL+"/api/v1/session", `{"username":"user1","password":"pw-one"}`)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		response, err := client.Get(server.URL + "/api/v1/departments")
		require.NoError(t, err)
		status := response.StatusCode
		_ = response.Body.Close()
		assert.NotEqual(t, http.StatusForbidden, status, "admin identity must clear the gate")
	})

	t.Run("workbench routes are reachable through the real mux", func(t *testing.T) {
		// Handler-level tests bypass mux registration, so a routes()
		// regression (segment precedence, method typo) would surface as
		// 404/405 in production with every handler test still green.
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		mock.MatchExpectationsInOrder(false) // session persistence shares the mock
		p := &portal{
			cfg:      config{namespace: "digital-employees", adminUsers: map[string]bool{"admin-user": true}},
			db:       db,
			k8s:      fake.NewClientBuilder().WithScheme(chatTestScheme(t)).Build(),
			aep:      &aepAuthenticator{baseURL: aep.URL, http: aep.Client()},
			sessions: newSessionStore(time.Hour),
			logger:   slog.New(slog.DiscardHandler),
		}
		server := httptest.NewServer(p.routes())
		t.Cleanup(server.Close)
		jar, err := cookiejar.New(nil)
		require.NoError(t, err)
		client := &http.Client{Jar: jar}
		resp, _ := postJSON(t, client, server.URL+"/api/v1/session", `{"username":"user1","password":"pw-one"}`)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		mock.ExpectQuery(`FROM requests WHERE owner_id=\$1 AND state='pending'`).WithArgs("user-1").
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		response, err := client.Get(server.URL + "/api/v1/me")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, response.StatusCode, "GET /api/v1/me must be registered")
		_ = response.Body.Close()

		mock.ExpectQuery(`FROM requests WHERE owner_id=\$1 ORDER BY created_at DESC LIMIT 100`).WithArgs("user-1").
			WillReturnRows(sqlmock.NewRows([]string{"id", "employee_name", "owner_id", "owner_name", "display_name", "state", "decision_reason", "created_at"}))
		response, err = client.Get(server.URL + "/api/v1/requests/mine")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, response.StatusCode, "GET /api/v1/requests/mine must win over the {id} wildcard")
		_ = response.Body.Close()

		mock.ExpectQuery(`FROM requests WHERE id=\$1`).WithArgs("req-missing").
			WillReturnRows(sqlmock.NewRows([]string{"employee_name", "owner_id", "owner_name", "display_name", "spec_json", "state", "decision_reason", "decided_by", "created_at", "decided_at"}))
		response, err = client.Get(server.URL + "/api/v1/requests/req-missing")
		require.NoError(t, err)
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		assert.Equal(t, http.StatusNotFound, response.StatusCode)
		// A missing id and a foreign id share one shape (existence must
		// not leak); the body only echoes the caller-supplied id.
		assert.JSONEq(t, `{"error":"request req-missing not found"}`, string(body))
	})
}
