package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// apiTestPortal wires a portal with a fake k8s client and a mocked DB.
type apiTestPortal struct {
	*portal
	dbmock sqlmock.Sqlmock
}

func newAPIHarness(t *testing.T, objects ...runtime.Object) *apiTestPortal {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, schemeAddK8s(scheme))
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	p := &portal{
		cfg: config{
			namespace:    "digital-employees",
			policyMode:   "self-service",
			adminUsers:   map[string]bool{"admin-user": true},
			quotaPerUser: 2,
		},
		db:       db,
		k8s:      fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).WithStatusSubresource(&dev1alpha1.DigitalEmployee{}).Build(),
		aep:      &aepAuthenticator{},
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: newSessionStore(time.Hour),
	}
	return &apiTestPortal{portal: p, dbmock: mock}
}

func readyCR(name, owner string) *dev1alpha1.DigitalEmployee {
	return &dev1alpha1.DigitalEmployee{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "digital-employees"},
		Spec:       dev1alpha1.DigitalEmployeeSpec{Owner: owner, DisplayName: name},
		Status:     dev1alpha1.DigitalEmployeeStatus{Phase: dev1alpha1.PhaseReady},
	}
}

func memkey(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "de-" + name + "-memkey", Namespace: "digital-employees"},
		Data:       map[string][]byte{"DEER_FLOW_INTERNAL_AUTH_TOKEN": []byte("tok")},
	}
}

func adminIdentity() identity { return identity{UserID: "admin-user", Roles: []string{"admin"}} }

func TestAPICRUDHandlers(t *testing.T) {
	t.Run("get employee returns the summary for the owner", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("sales-helper", "admin-user"))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/employees/sales-helper", nil)
		req.SetPathValue("name", "sales-helper")
		w := httptest.NewRecorder()
		h.getEmployee(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		var out struct {
			Employee map[string]any `json:"employee"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		assert.Equal(t, "sales-helper", out.Employee["name"])
	})

	t.Run("get employee refuses non-owner", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("sales-helper", "someone-else"))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/employees/sales-helper", nil)
		req.SetPathValue("name", "sales-helper")
		w := httptest.NewRecorder()
		h.getEmployee(w, req, adminIdentity())
		// admin bypasses; use a plain identity instead
		plain := identity{UserID: "user-x"}
		w = httptest.NewRecorder()
		h.getEmployee(w, req, plain)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("update employee patches displayName and team", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("sales-helper", "admin-user"))
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/employees/sales-helper",
			strings.NewReader(`{"displayName":"销售专员","team":"rd-dept"}`))
		req.SetPathValue("name", "sales-helper")
		w := httptest.NewRecorder()
		h.updateEmployee(w, req, adminIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		var item dev1alpha1.DigitalEmployee
		require.NoError(t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: "digital-employees", Name: "sales-helper"}, &item))
		assert.Equal(t, "销售专员", item.Spec.DisplayName)
		assert.Equal(t, "rd-dept", item.Spec.Team)
	})

	t.Run("delete employee removes the CR", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("old-helper", "admin-user"))
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/employees/old-helper", nil)
		req.SetPathValue("name", "old-helper")
		w := httptest.NewRecorder()
		h.deleteEmployee(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var item dev1alpha1.DigitalEmployee
		err := h.k8s.Get(context.Background(), client.ObjectKey{Namespace: "digital-employees", Name: "old-helper"}, &item)
		assert.True(t, strings.Contains(err.Error(), "not found"))
	})

	t.Run("apply creates directly in direct mode", func(t *testing.T) {
		h := newAPIHarness(t)
		h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
			strings.NewReader(`{"name":"new-helper","displayName":"新员工","team":"rd-dept"}`))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, adminIdentity())
		assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		var item dev1alpha1.DigitalEmployee
		require.NoError(t, h.k8s.Get(context.Background(), client.ObjectKey{Namespace: "digital-employees", Name: "new-helper"}, &item))
		assert.Equal(t, "rd-dept", item.Spec.Team)
	})

	t.Run("apply rejects malformed names", func(t *testing.T) {
		h := newAPIHarness(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
			strings.NewReader(`{"name":"Bad Name!"}`))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, adminIdentity())
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("apply conflicts with an existing employee", func(t *testing.T) {
		h := newAPIHarness(t, readyCR("dup-helper", "admin-user"))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
			strings.NewReader(`{"name":"dup-helper"}`))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, adminIdentity())
		assert.Equal(t, http.StatusConflict, w.Code)
	})

	t.Run("apply parks a request in approval mode", func(t *testing.T) {
		h := newAPIHarness(t)
		h.cfg.policyMode = "approval"
		h.dbmock.ExpectQuery(`SELECT id FROM requests WHERE employee_name=`).
			WithArgs("parked-helper").WillReturnError(errNoRowsForMock)
		h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/employees",
			strings.NewReader(`{"name":"parked-helper"}`))
		w := httptest.NewRecorder()
		h.applyForEmployee(w, req, identity{UserID: "user-x", Kind: "human"})
		assert.Equal(t, http.StatusAccepted, w.Code)
	})

	t.Run("list employees filters by team for non-admins", func(t *testing.T) {
		inTeam := readyCR("in-helper", "owner-a")
		inTeam.Spec.Team = "rd-dept"
		outTeam := readyCR("out-helper", "owner-b")
		outTeam.Spec.Team = "sales-dept"
		h := newAPIHarness(t, inTeam, outTeam)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/employees?team=rd-dept", nil)
		w := httptest.NewRecorder()
		member := identity{UserID: "user-x", Teams: []string{"rd-dept"}}
		h.listEmployees(w, req, member)
		assert.Equal(t, http.StatusOK, w.Code)
		body := w.Body.String()
		assert.Contains(t, body, "in-helper")
		assert.NotContains(t, body, "out-helper")
	})

	t.Run("decideRequest rejects with reason", func(t *testing.T) {
		h := newAPIHarness(t)
		rows := sqlmock.NewRows([]string{"employee_name", "spec_json"}).AddRow("parked-helper", `{"metadata":{"name":"parked-helper"}}`)
		h.dbmock.ExpectQuery(`SELECT employee_name, spec_json FROM requests`).WithArgs("req-1").WillReturnRows(rows)
		h.dbmock.ExpectExec(`UPDATE requests SET state=`).WillReturnResult(sqlmock.NewResult(0, 1))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/requests/req-1/reject",
			strings.NewReader(`{"reason":"容量不足"}`))
		req.SetPathValue("id", "req-1")
		w := httptest.NewRecorder()
		h.decideRequest("rejected")(w, req, adminIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("listRequests scopes non-admins to their own rows", func(t *testing.T) {
		h := newAPIHarness(t)
		rows := sqlmock.NewRows([]string{"id", "employee_name", "owner_id", "owner_name", "display_name", "state", "decision_reason", "created_at"}).
			AddRow("req-1", "helper", "user-x", "User X", "助手", "pending", "", time.Now())
		h.dbmock.ExpectQuery(`SELECT id, employee_name`).WithArgs("pending").WillReturnRows(rows)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/requests", nil)
		w := httptest.NewRecorder()
		h.listRequests(w, req, identity{UserID: "user-x"})
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "helper")
	})
}

var errNoRowsForMock = errMockNoRows{}

type errMockNoRows struct{}

func (errMockNoRows) Error() string { return "sql: no rows in result set" }

func schemeAddK8s(scheme *runtime.Scheme) error {
	return k8sscheme.AddToScheme(scheme)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func jsonUnmarshal(raw string, out any) error { return json.Unmarshal([]byte(raw), out) }

func objectKey(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: "digital-employees", Name: name}
}

// IsAdminForTest adapts identity.isAdmin to the boolean-only evaluatePolicy
// signature without changing production code.
func (i identity) IsAdminForTest() bool {
	return i.isAdmin(config{adminUsers: map[string]bool{"admin-user": true}, adminRoles: map[string]bool{"admin": true}})
}
