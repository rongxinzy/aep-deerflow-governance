package main

import (
	"database/sql"
	"encoding/json"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sharedNativePortal(t *testing.T, role string, lock bool) (*portal, sqlmock.Sqlmock) {
	t.Helper()
	db, m, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, m.ExpectationsWereMet()); _ = db.Close() })
	if lock {
		m.ExpectExec("SELECT pg_advisory_lock").WillReturnResult(sqlmock.NewResult(0, 1))
	}
	m.ExpectQuery("SELECT s.id,s.deployment_id").WillReturnRows(sqlmock.NewRows([]string{"id", "deployment", "company", "project", "name", "role"}).AddRow("space", "deployment", "company", "project", "空间", role))
	return &portal{db: db}, m
}
func sharedNativeUnlock(m sqlmock.Sqlmock) {
	m.ExpectQuery("SELECT pg_advisory_unlock").WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))
}
func sharedNativeRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.SetPathValue("space", "space")
	r.SetPathValue("task", "task")
	return r
}

var sharedNativeIdentity = identity{UserID: "person", DeploymentID: "deployment", DisplayName: "同事"}

func sharedNativeServer(t *testing.T, p *portal, fn http.HandlerFunc) {
	t.Helper()
	s := httptest.NewServer(fn)
	t.Cleanup(s.Close)
	p.cfg.paperclipBaseURL = s.URL
	p.cfg.paperclipToken = "synthetic"
}
func sharedNativeTask(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{"id": "task", "companyId": "company", "projectId": "project"})
}

func TestSharedNativeProject(t *testing.T) {
	for _, mode := range []string{"get", "patch", "viewer", "wrong-scope", "upstream"} {
		t.Run(mode, func(t *testing.T) {
			patch := mode != "get"
			role := "owner"
			if mode == "viewer" {
				role = "viewer"
			}
			p, m := sharedNativePortal(t, role, patch)
			if patch && mode != "viewer" && mode != "wrong-scope" && mode != "upstream" {
				m.ExpectExec("UPDATE shared_spaces SET name").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			if patch {
				sharedNativeUnlock(m)
			}
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/projects/project", r.URL.Path)
				if patch {
					require.Equal(t, "PATCH", r.Method)
					var b map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
					require.Equal(t, "新空间", b["name"])
				}
				if mode == "upstream" {
					w.WriteHeader(503)
					return
				}
				company := "company"
				if mode == "wrong-scope" {
					company = "other"
				}
				writeJSON(w, 200, map[string]any{"id": "project", "companyId": company, "name": "新空间", "env": "secret"})
			})
			w := httptest.NewRecorder()
			var err error
			if patch {
				err = p.sharedUpdateProject(w, sharedNativeRequest("PATCH", "/", `{"name":"新空间"}`), sharedNativeIdentity)
			} else {
				err = p.sharedProject(w, sharedNativeRequest("GET", "/", ""), sharedNativeIdentity)
			}
			if mode == "viewer" || mode == "wrong-scope" || mode == "upstream" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotContains(t, w.Body.String(), "secret")
			}
		})
	}
}
func TestSharedNativeFieldBoundaries(t *testing.T) {
	for _, input := range []map[string]any{{"name": nil}, {"name": " "}, {"status": true}, {"env": "secret"}, {"description": 2}, {}} {
		_, err := sharedProjectFields(input, true)
		require.Error(t, err)
	}
	fields, err := sharedProjectFields(map[string]any{"name": "空间", "description": nil, "status": "planned", "color": "blue", "icon": nil, "targetDate": nil}, true)
	require.NoError(t, err)
	require.Len(t, fields, 6)
	_, err = sharedProjectFields(map[string]any{"description": "内容"}, false)
	require.NoError(t, err)
	for _, key := range []string{"shared-space-result-1", "plan", "zhiyuan-reference-", "../zhiyuan-reference-a", strings.Repeat("x", 65)} {
		require.False(t, sharedValidReferenceKey(key))
	}
	require.True(t, sharedValidReferenceKey("zhiyuan-reference-guide"))
	require.True(t, sharedTaskStatus("in_review"))
	require.False(t, sharedTaskStatus("accepted"))
	require.Equal(t, "parent", sharedTaskDTO(map[string]any{"parentId": "parent", "executionConfig": "secret"})["parentId"])
	p := &portal{}
	require.Error(t, p.sharedCreateSpace(httptest.NewRecorder(), sharedNativeRequest("POST", "/", `{}`), sharedNativeIdentity))
	for _, body := range []string{`{}`, `{"body":"hello","clientRequestId":"no"}`, `{"body":" ","clientRequestId":"00000000-0000-4000-8000-000000000000"}`} {
		require.Error(t, p.sharedAddComment(httptest.NewRecorder(), sharedNativeRequest("POST", "/", body), sharedNativeIdentity))
	}
}
func TestSharedNativeDocuments(t *testing.T) {
	for _, mode := range []string{"list", "get", "revisions", "put", "viewer", "wrong-scope", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			put := mode == "put" || mode == "viewer" || mode == "conflict"
			role := "editor"
			if mode == "viewer" {
				role = "viewer"
			}
			p, m := sharedNativePortal(t, role, put)
			if put {
				sharedNativeUnlock(m)
			}
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/issues/task" {
					sharedNativeTask(w)
					return
				}
				if mode == "conflict" {
					w.WriteHeader(409)
					return
				}
				if put {
					var body map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					require.Equal(t, "markdown", body["format"])
					require.Equal(t, "revision-1", body["baseRevisionId"])
				}
				doc := map[string]any{"id": "doc", "companyId": "company", "issueId": "task", "key": "zhiyuan-reference-guide", "body": "资料正文", "latestRevisionId": "revision-2", "private": "secret"}
				if mode == "wrong-scope" {
					doc["companyId"] = "foreign"
				}
				switch mode {
				case "list":
					writeJSON(w, 200, []any{doc, map[string]any{"companyId": "company", "issueId": "task", "key": "shared-space-result-1"}})
				case "revisions":
					writeJSON(w, 200, []any{doc})
				default:
					writeJSON(w, 200, doc)
				}
			})
			r := sharedNativeRequest("GET", "/documents", "")
			if mode != "list" {
				r.SetPathValue("key", "zhiyuan-reference-guide")
			}
			if mode == "revisions" {
				r.URL.Path += "/zhiyuan-reference-guide/revisions"
			}
			w := httptest.NewRecorder()
			var err error
			if put {
				r = sharedNativeRequest("PUT", "/", `{"title":"资料","body":"正文","baseRevisionId":"revision-1"}`)
				r.SetPathValue("key", "zhiyuan-reference-guide")
				err = p.sharedPutDocument(w, r, sharedNativeIdentity)
			} else {
				err = p.sharedDocuments(w, r, sharedNativeIdentity)
			}
			if mode == "viewer" || mode == "wrong-scope" || mode == "conflict" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotContains(t, w.Body.String(), "secret")
				require.NotContains(t, w.Body.String(), "shared-space-result")
			}
		})
	}
}

// Rejected input must stop before directory access, database mutation or an
// upstream request, including company spoofing and unsupported project states.
func TestSharedNativeProjectRejectsInvalidInputBeforeWriting(t *testing.T) {
	p := &portal{}
	p.cfg.paperclipCompanyID = "company"
	for _, body := range []string{
		`{`,
		`{"name":"空间"}`,
		`{"name":"空间","requestId":"create","companyId":"foreign"}`,
		`{"name":"空间","requestId":"create","status":"paused"}`,
	} {
		w := httptest.NewRecorder()
		sharedWriteError(w, p.sharedCreateSpace(w, sharedNativeRequest("POST", "/", body), sharedNativeIdentity))
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
	for _, body := range []string{`{`, `{}`, `{"status":"paused"}`, `{"status":""}`} {
		w := httptest.NewRecorder()
		sharedWriteError(w, p.sharedUpdateProject(w, sharedNativeRequest("PATCH", "/", body), sharedNativeIdentity))
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
}

// Revocation must cut off the additional project and task resource surfaces
// before any native service read or write occurs.
func TestSharedNativeRevokedMemberCannotAccessResources(t *testing.T) {
	for _, handler := range []struct {
		name   string
		method string
		body   string
		call   func(*portal, http.ResponseWriter, *http.Request, identity) error
	}{
		{"project", "GET", "", (*portal).sharedProject},
		{"comments", "GET", "", (*portal).sharedComments},
		{"documents", "GET", "", (*portal).sharedDocuments},
		{"edit-project", "PATCH", `{"name":"空间"}`, (*portal).sharedUpdateProject},
		{"post-comment", "POST", `{"body":"讨论","clientRequestId":"00000000-0000-4000-8000-000000000006"}`, (*portal).sharedAddComment},
		{"put-document", "PUT", `{"title":"资料","body":"正文"}`, (*portal).sharedPutDocument},
	} {
		t.Run(handler.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, mock.ExpectationsWereMet()); _ = db.Close() })
			write := handler.method != "GET"
			if write {
				mock.ExpectExec("SELECT pg_advisory_lock").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectQuery("SELECT s.id,s.deployment_id").WithArgs("space", "deployment", "person").WillReturnError(sql.ErrNoRows)
			if write {
				sharedNativeUnlock(mock)
			}
			p := &portal{db: db}
			calls := 0
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) })
			r := sharedNativeRequest(handler.method, "/", handler.body)
			if handler.name == "put-document" {
				r.SetPathValue("key", "zhiyuan-reference-guide")
			}
			w := httptest.NewRecorder()
			sharedWriteError(w, handler.call(p, w, r, sharedNativeIdentity))
			require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
			require.Zero(t, calls)
		})
	}
}
