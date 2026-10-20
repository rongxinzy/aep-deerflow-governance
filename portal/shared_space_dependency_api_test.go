package main

import (
	"database/sql"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDependencyListRequestsNativeRelationProjection(t *testing.T) {
	p, m := sharedNativePortal(t, "viewer", false)
	m.ExpectQuery("SELECT DISTINCT ON").WillReturnRows(sqlmock.NewRows([]string{"task", "state"}))
	m.ExpectQuery("SELECT r.id,r.task_id").WillReturnRows(dependencyResultRows(""))
	sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/companies/company/issues":
			require.Equal(t, "true", r.URL.Query().Get("includeBlockedBy"))
			task := map[string]any{"id": "b", "companyId": "company", "projectId": "project", "status": "todo"}
			if r.URL.Query().Get("includeBlockedBy") == "true" {
				task["blockedBy"] = []any{map[string]any{"id": "a"}}
			}
			writeJSON(w, 200, []any{task})
		case "/issues/a":
			writeJSON(w, 200, map[string]any{"id": "a", "companyId": "company", "projectId": "project", "title": "A", "status": "todo"})
		default:
			writeJSON(w, 200, map[string]any{"issue": map[string]any{"id": "b"}, "readiness": map[string]any{"isDependencyReady": false}, "blockers": []any{map[string]any{"id": "a", "isDependencyReady": false}}})
		}
	})
	w := httptest.NewRecorder()
	require.NoError(t, p.sharedTasks(w, sharedNativeRequest("GET", "/", ""), sharedNativeIdentity))
	require.Contains(t, w.Body.String(), `"pendingCount":1`)
	require.Contains(t, w.Body.String(), `"ready":false`)
}
func TestDependencyEditsRespectRoleBindingAndVersion(t *testing.T) {
	for _, mode := range []string{"viewer", "not-in-space", "bound", "stale", "self"} {
		t.Run(mode, func(t *testing.T) {
			role := "editor"
			if mode == "viewer" {
				role = "viewer"
			}
			p, m := sharedNativePortal(t, role, true)
			if mode != "viewer" {
				m.ExpectQuery("SELECT actor_id,action,payload_hash,state,receipt").WillReturnError(sql.ErrNoRows)
			}
			if mode != "viewer" && mode != "not-in-space" {
				m.ExpectQuery("SELECT EXISTS.*shared_space_runtime_bindings").WillReturnRows(sqlmock.NewRows([]string{"bound"}).AddRow(mode == "bound"))
			}
			sharedNativeUnlock(m)
			calls := 0
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "GET", r.Method)
				calls++
				project := "project"
				if mode == "not-in-space" {
					project = "foreign"
				}
				writeJSON(w, 200, map[string]any{"id": "task", "companyId": "company", "projectId": project, "status": "todo", "updatedAt": "v1"})
			})
			token := sharedHash([]any{"task", "v1", []string{}})
			if mode == "stale" {
				token = "stale"
			}
			source := "a"
			if mode == "self" {
				source = "task"
			}
			body := fmt.Sprintf(`{"requestId":"edit","expectedDependencyToken":%q,"blockedByIssueIds":[%q]}`, token, source)
			w := httptest.NewRecorder()
			err := p.sharedSetDependencies(w, sharedNativeRequest("PUT", "/", body), sharedNativeIdentity)
			require.Error(t, err)
			sharedWriteError(w, err)
			want := 409
			switch mode {
			case "viewer":
				want = 403
			case "not-in-space":
				want = 404
			case "self":
				want = 400
			}
			require.Equal(t, want, w.Code, w.Body.String())
			if mode == "viewer" {
				require.Zero(t, calls)
			}
		})
	}
}
