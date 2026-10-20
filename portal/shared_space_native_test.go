package main

import (
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
				if mode == "list" {
					writeJSON(w, 200, []any{doc, map[string]any{"companyId": "company", "issueId": "task", "key": "shared-space-result-1"}})
				} else if mode == "revisions" {
					writeJSON(w, 200, []any{doc})
				} else {
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
