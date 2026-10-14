// GET /api/v1/organization specs: the workbench org tree needs one
// authoritative flattened department listing — parent departments with no
// direct members never appear in me.teams or any contact's teams, so
// assembling the tree client-side can only render a broken forest.
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// organizationStubAEP serves a three-level tree across two cursor pages:
// 研发中心 (root, no members) → 研发部 → 前端组, plus a root 销售部.
func organizationStubAEP(t *testing.T) *httptest.Server {
	t.Helper()
	pages := map[string]string{
		"": `{"teams":[
			{"id":"eng","name":"研发中心"},
			{"id":"rd","name":"研发部","parentId":"eng"}
		],"nextCursor":"page2"}`,
		"page2": `{"teams":[
			{"id":"fe","name":"前端组","parentId":"rd"},
			{"id":"sales","name":"销售部"}
		],"nextCursor":null}`,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/aep/v1/auth/password/login":
			_, _ = w.Write([]byte(`{"accessToken":"admin-token"}`))
		case "/aep/v1/admin/teams":
			body, ok := pages[r.URL.Query().Get("cursor")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func organizationFixture(t *testing.T, aep *httptest.Server) (*apiTestPortal, *httptest.ResponseRecorder) {
	t.Helper()
	resetAdminTokenCache(t)
	stubs := &departmentStubUpstreams{
		aep:     aep,
		ov:      httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
		weknora: httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })),
	}
	h := newDepartmentHarness(t, stubs)
	w := httptest.NewRecorder()
	h.handleOrganization(w, httptest.NewRequest(http.MethodGet, "/api/v1/organization", nil), identity{UserID: "u-plain", Username: "employee"})
	return h, w
}

func TestHandleOrganization(t *testing.T) {
	t.Run("any employee gets the full tree, including empty intermediate parents", func(t *testing.T) {
		_, w := organizationFixture(t, organizationStubAEP(t))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var got struct {
			Departments []struct {
				ID       string  `json:"id"`
				Name     string  `json:"name"`
				ParentID *string `json:"parentId"`
			} `json:"departments"`
		}
		require.NoError(t, jsonUnmarshal(w.Body.String(), &got))
		require.Len(t, got.Departments, 4, "both cursor pages must be followed")
		byID := map[string]struct {
			ID       string  `json:"id"`
			Name     string  `json:"name"`
			ParentID *string `json:"parentId"`
		}{}
		for _, d := range got.Departments {
			byID[d.ID] = d
		}
		// 三级链完整：研发中心(根) → 研发部 → 前端组；根部门无 parentId 键
		assert.Nil(t, byID["eng"].ParentID)
		require.NotNil(t, byID["rd"].ParentID)
		assert.Equal(t, "eng", *byID["rd"].ParentID)
		require.NotNil(t, byID["fe"].ParentID)
		assert.Equal(t, "rd", *byID["fe"].ParentID)
		assert.Nil(t, byID["sales"].ParentID)
	})

	t.Run("team listing failure fails closed", func(t *testing.T) {
		// 部分树会让组织视图里部门时有时无地漂移——与通讯录同纪律整体拒绝。
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/aep/v1/auth/password/login" {
				_, _ = w.Write([]byte(`{"accessToken":"admin-token"}`))
				return
			}
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"title":"Internal Server Error","status":500}`))
		}))
		_, w := organizationFixture(t, stub)
		assert.Equal(t, http.StatusBadGateway, w.Code)
		assert.Contains(t, w.Body.String(), "listing teams", "employee-facing wording stays sanitized")
	})
}
