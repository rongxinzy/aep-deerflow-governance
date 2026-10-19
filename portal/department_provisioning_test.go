package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Provisioning a department touches three systems, and a partial outcome used
// to vanish with the request: the caller saw 200 phase=partial, the console had
// no way to show it, and nothing could be retried — which is how a department
// stays without its memory account indefinitely.

func decodeDepartmentStatus(t *testing.T, w *httptest.ResponseRecorder) departmentStatus {
	t.Helper()
	var status departmentStatus
	require.NoError(t, jsonUnmarshal(w.Body.String(), &status), w.Body.String())
	return status
}

func listDepartments(t *testing.T, h *apiTestPortal) []map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	h.handleListDepartments(w, httptest.NewRequest(http.MethodGet, "/api/v1/departments", nil), adminIdentity())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Departments []map[string]any `json:"departments"`
	}
	require.NoError(t, jsonUnmarshal(w.Body.String(), &body), w.Body.String())
	return body.Departments
}

func departmentEntry(t *testing.T, h *apiTestPortal, id string) map[string]any {
	t.Helper()
	for _, entry := range listDepartments(t, h) {
		if entry["id"] == id {
			return entry
		}
	}
	t.Fatalf("department %s is not in the listing", id)
	return nil
}

func failingWeKnora(stubs *departmentStubUpstreams) {
	stubs.weknora.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
}

// provisionRequest builds the retry request with the route value set: the
// handler is called directly, so no mux would populate it.
func provisionRequest(id string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/departments/"+id+"/provision", nil)
	req.SetPathValue("id", id)
	return req
}

func createDepartment(t *testing.T, h *apiTestPortal) departmentStatus {
	t.Helper()
	w := httptest.NewRecorder()
	h.handleCreateDepartment(w,
		httptest.NewRequest(http.MethodPost, "/api/v1/departments", strings.NewReader(`{"name":"研发部"}`)),
		adminIdentity())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return decodeDepartmentStatus(t, w)
}

func TestDepartmentProvisioningRecordAndRetry(t *testing.T) {
	t.Run("a partial create is stored and shown by the listing", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		failingWeKnora(stubs)
		h := newDepartmentHarness(t, stubs)

		status := createDepartment(t, h)
		require.Equal(t, "partial", status.Phase)

		records, err := h.provisioningStore.Load(context.Background())
		require.NoError(t, err)
		record, ok := records["rd-dept"]
		require.True(t, ok, "the outcome must survive the request")
		assert.Equal(t, "partial", record.Phase)
		assert.Equal(t, "tenant-rd-dept", record.OvAccountID)
		require.NotEmpty(t, record.Partials)
		assert.Contains(t, record.Partials[0], "weknora")
		assert.Equal(t, "研发部", record.Name, "the name is needed to retry later")

		entry := departmentEntry(t, h, "rd-dept")
		assert.Equal(t, "partial", entry["phase"])
		assert.Equal(t, "tenant-rd-dept", entry["ovAccountId"])
		assert.Contains(t, entry["message"], "weknora")
		assert.NotEmpty(t, entry["updatedAt"])
		assert.NotEmpty(t, entry["partials"])
	})

	t.Run("retry fills only the missing piece and the department becomes ready", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		failingWeKnora(stubs)
		h := newDepartmentHarness(t, stubs)
		require.Equal(t, "partial", createDepartment(t, h).Phase)

		ovCallsAfterCreate := stubs.ovCalls
		// The upstream recovers.
		failingWeKnoraRecover(stubs)

		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w,
			provisionRequest("rd-dept"),
			adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		status := decodeDepartmentStatus(t, w)
		assert.Equal(t, "ready", status.Phase)
		assert.Empty(t, status.Message)
		assert.Equal(t, "tenant-rd-dept", status.OvAccountID, "the account already existed and must not be created again")
		assert.Equal(t, ovCallsAfterCreate, stubs.ovCalls, "only the missing piece is retried")

		entry := departmentEntry(t, h, "rd-dept")
		assert.Equal(t, "ready", entry["phase"])
		assert.NotContains(t, entry, "partials")
	})

	t.Run("retry repairs a department that has no record at all", func(t *testing.T) {
		// The shape the real bug arrived in: a department provisioned before
		// this record existed, so its memory account was never created and
		// there is nothing stored to read.
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)

		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w,
			provisionRequest("rd-dept"),
			adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		status := decodeDepartmentStatus(t, w)
		assert.Equal(t, "ready", status.Phase)
		assert.Equal(t, "研发部", status.Name, "the name comes from the AEP team, not from an invented value")
		assert.Equal(t, "tenant-rd-dept", status.OvAccountID)
		assert.GreaterOrEqual(t, stubs.ovCalls, 1)
	})

	t.Run("retry is idempotent for a ready department", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		require.Equal(t, "ready", createDepartment(t, h).Phase)
		ovCalls, wkCalls, aepCalls := stubs.ovCalls, stubs.wkCalls, stubs.aepCalls

		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w,
			provisionRequest("rd-dept"),
			adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "ready", decodeDepartmentStatus(t, w).Phase)

		assert.Equal(t, ovCalls, stubs.ovCalls, "no OpenViking call for a department that already has its account")
		assert.Equal(t, wkCalls, stubs.wkCalls, "no WeKnora call either")
		assert.Equal(t, aepCalls, stubs.aepCalls, "the AEP team listing is not needed when the record is known")
	})

	t.Run("retry refuses when there is no store to read the outcome from", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		h.provisioningStore = nil

		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w,
			provisionRequest("rd-dept"),
			adminIdentity())
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Contains(t, w.Body.String(), "retry unavailable")
	})

	t.Run("retry requires an administrator", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)

		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w,
			provisionRequest("rd-dept"),
			identity{UserID: "user-x"})
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("retry reports partial again while the upstream stays down", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		failingWeKnora(stubs)
		h := newDepartmentHarness(t, stubs)
		require.Equal(t, "partial", createDepartment(t, h).Phase)

		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w,
			provisionRequest("rd-dept"),
			adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		status := decodeDepartmentStatus(t, w)
		assert.Equal(t, "partial", status.Phase)
		assert.Contains(t, status.Message, "weknora")

		record, err := h.provisioningStore.Load(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "partial", record["rd-dept"].Phase, "a still-failing retry keeps the record partial")
	})
}

// brokenProvisioningStore fails on demand: the retry must refuse rather than
// guess what is already provisioned when it cannot read the record.
type brokenProvisioningStore struct{}

func (brokenProvisioningStore) Save(context.Context, departmentProvision) error { return nil }

func (brokenProvisioningStore) Load(context.Context) (map[string]departmentProvision, error) {
	return nil, fmt.Errorf("database unreachable")
}

func TestDepartmentProvisioningStoreFailures(t *testing.T) {
	t.Run("retry refuses when the record cannot be read", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		h.provisioningStore = brokenProvisioningStore{}

		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w, provisionRequest("rd-dept"), adminIdentity())
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), "retry later")
	})

	t.Run("retry reports an unknown department as not found", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)

		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w, provisionRequest("ghost"), adminIdentity())
		assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	})

	t.Run("retry reports a failing AEP listing instead of guessing the name", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		original := stubs.aep.Config.Handler
		stubs.aep.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/aep/v1/admin/teams" && r.Method == http.MethodGet {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			original.ServeHTTP(w, r)
		})
		h := newDepartmentHarness(t, stubs)

		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w, provisionRequest("ghost"), adminIdentity())
		assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	})

	t.Run("creating without a store still provisions and lists", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)
		h.provisioningStore = nil

		status := createDepartment(t, h)
		assert.Equal(t, "ready", status.Phase)

		// The listing keeps working with no records to merge in.
		w := httptest.NewRecorder()
		h.handleListDepartments(w, httptest.NewRequest(http.MethodGet, "/api/v1/departments", nil), adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.NotContains(t, w.Body.String(), "\"phase\"", "no record, no invented phase")
	})
}
func failingWeKnoraRecover(stubs *departmentStubUpstreams) {
	stubs.weknora.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/login"):
			_, _ = w.Write([]byte(`{"token":"wk-token"}`))
		case r.URL.Path == "/api/v1/tenants" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"success":true,"data":{"items":[]}}`))
		case r.URL.Path == "/api/v1/tenants" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":7,"name":"研发部"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// WeKnora answers {"success":true,"data":{"id":<number>,"name":…}} and lists
// tenants under {"success":…,"data":{"items":[…]}}; the portal used to decode
// the create answer into a top-level string field, so a provisioned department
// was recorded without its tenant and a retry created a second one.
func TestWeKnoraTenantProvisioning(t *testing.T) {
	t.Run("a provisioned department records the tenant id WeKnora returns", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		h := newDepartmentHarness(t, stubs)

		status := createDepartment(t, h)

		assert.Equal(t, "ready", status.Phase)
		assert.Equal(t, "7", status.WeKnoraTenant, "the id is nested under data and numeric")
	})

	t.Run("retrying reuses the tenant the department already owns", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		creates := 0
		stubs.weknora.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/auth/login"):
				_, _ = w.Write([]byte(`{"token":"wk-token"}`))
			case r.URL.Path == "/api/v1/tenants" && r.Method == http.MethodGet:
				_, _ = w.Write([]byte(`{"success":true,"data":{"items":[{"id":41,"name":"研发部"}]}}`))
			case r.URL.Path == "/api/v1/tenants" && r.Method == http.MethodPost:
				creates++
				w.WriteHeader(http.StatusInternalServerError)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		h := newDepartmentHarness(t, stubs)
		// The shape the old decode produced: the step is missing and its tenant
		// id is empty, while the tenant exists upstream.
		require.NoError(t, h.provisioningStore.Save(context.Background(), departmentProvision{
			ID: "rd-dept", Name: "研发部", Phase: "partial",
			Partials: []string{"weknora: weknora returned 500: unavailable"},
		}))

		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w, provisionRequest("rd-dept"), adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		status := decodeDepartmentStatus(t, w)
		assert.Equal(t, "ready", status.Phase)
		assert.Equal(t, "41", status.WeKnoraTenant, "the tenant this department owns is reused")
		assert.Equal(t, 0, creates, "a tenant that exists must never be created again")
	})
}

// The retry endpoint must not turn "the account already existed" into a
// permanent partial (it would POST on every retry), and it must not re-create
// a step that already succeeded just because that upstream reported no id.
func TestDepartmentProvisioningRetryOutcomes(t *testing.T) {
	t.Run("an account that already exists is recorded as the deterministic id", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		stubs.ov.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/admin/accounts" {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":{"code":"ALREADY_EXISTS","message":"account exists"}}`))
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		h := newDepartmentHarness(t, stubs)
		// The shape the console arrives at for a department provisioned before
		// the record existed: the account is upstream, the row's id is empty.
		require.NoError(t, h.provisioningStore.Save(context.Background(), departmentProvision{
			ID: "rd-dept", Name: "研发部", Phase: "partial",
			Partials: []string{"openviking: openviking returned 409: ALREADY_EXISTS"},
		}))

		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w, provisionRequest("rd-dept"), adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		status := decodeDepartmentStatus(t, w)
		assert.Equal(t, "ready", status.Phase, "an existing account is the same answer as a created one")
		assert.Empty(t, status.Message)
		assert.Equal(t, "tenant-rd-dept", status.OvAccountID)

		records, err := h.provisioningStore.Load(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "tenant-rd-dept", records["rd-dept"].OvAccountID)
	})

	t.Run("a step that already succeeded is not retried into a second tenant", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		stubs.ov.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		h := newDepartmentHarness(t, stubs)
		// WeKnora answered once (its own response carries no id), so the record
		// is partial only because of OpenViking.
		require.NoError(t, h.provisioningStore.Save(context.Background(), departmentProvision{
			ID: "rd-dept", Name: "研发部", Phase: "partial",
			Partials: []string{"openviking: openviking returned 500: unavailable"},
		}))

		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w, provisionRequest("rd-dept"), adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "partial", decodeDepartmentStatus(t, w).Phase)
		assert.Equal(t, 0, stubs.wkCalls, "a succeeded step must not be created again")
	})
}
