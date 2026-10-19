package main

import (
	"context"
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

// failingWeKnoraRecover puts the WeKnora stub back to its healthy behaviour.
func failingWeKnoraRecover(stubs *departmentStubUpstreams) {
	stubs.weknora.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/login"):
			_, _ = w.Write([]byte(`{"token":"wk-token"}`))
		case r.URL.Path == "/api/v1/tenants":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}
