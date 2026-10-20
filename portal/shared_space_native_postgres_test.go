package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// Exercises the complete native workflow with real PostgreSQL persistence and
// a synthetic HTTP service matching Paperclip v2026.1012.0 response fields.
func TestSharedSpacesPostgresNativeWorkflow(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable localhost PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"localhost", "127.0.0.1", "::1"}, cfg.Host)
	schema := fmt.Sprintf("shared_native_%d", time.Now().UnixNano())
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, e := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); require.NoError(t, e) })
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(sharedSpaceSchema + sharedRuntimeSchema)
	require.NoError(t, err)
	clearSharedSpaceAdminTokenCache(t)
	adminTokenCache.mu.Lock()
	adminTokenCache.token = "synthetic"
	adminTokenCache.expires = time.Now().Add(time.Minute)
	adminTokenCache.mu.Unlock()
	p := &portal{db: db}
	p.cfg.paperclipCompanyID = "company"
	project := map[string]any{"id": "project", "companyId": "company", "name": "空间", "status": "backlog"}
	tasks := map[string]map[string]any{}
	comments := []map[string]any{}
	docs := map[string]map[string]any{}
	revisions := map[string][]map[string]any{}
	creates := 0
	posts := 0
	sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
		path := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case r.URL.Path == "/aep/v1/admin/users/person":
			writeJSON(w, 200, map[string]any{"id": "person", "status": "active", "kind": "human"})
		case r.Method == "POST" && r.URL.Path == "/companies/company/projects":
			creates++
			var b map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
			require.NotEmpty(t, b["idempotencyKey"])
			for _, k := range []string{"name", "description", "status", "targetDate", "color", "icon"} {
				if v, ok := b[k]; ok {
					project[k] = v
				}
			}
			writeJSON(w, 201, project)
		case r.URL.Path == "/projects/project":
			if r.Method == "PATCH" {
				var b map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
				for k, v := range b {
					project[k] = v
				}
			}
			writeJSON(w, 200, project)
		case r.URL.Path == "/companies/company/issues":
			if r.Method == "POST" {
				var b map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
				id := fmt.Sprintf("task-%d", len(tasks)+1)
				b["id"] = id
				b["companyId"] = "company"
				tasks[id] = b
				writeJSON(w, 201, b)
			} else {
				out := []map[string]any{}
				for _, task := range tasks {
					if parent := r.URL.Query().Get("parentId"); parent != "" && task["parentId"] != parent {
						continue
					}
					out = append(out, task)
				}
				writeJSON(w, 200, out)
			}
		case len(path) == 2 && path[0] == "issues":
			writeJSON(w, 200, tasks[path[1]])
		case len(path) == 3 && path[2] == "comments":
			if r.Method == "GET" {
				writeJSON(w, 200, comments)
				return
			}
			posts++
			var b map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
			b["id"] = fmt.Sprintf("comment-%d", posts)
			b["issueId"] = path[1]
			comments = append(comments, b)
			if strings.HasPrefix(b["body"].(string), "lost acknowledgement") {
				w.WriteHeader(503)
				return
			}
			writeJSON(w, 201, b)
		case len(path) >= 3 && path[2] == "documents":
			if len(path) == 3 {
				out := []map[string]any{}
				for _, d := range docs {
					out = append(out, d)
				}
				writeJSON(w, 200, out)
				return
			}
			key := path[3]
			if len(path) == 5 {
				writeJSON(w, 200, revisions[key])
				return
			}
			if r.Method == "GET" {
				writeJSON(w, 200, docs[key])
				return
			}
			var b map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
			if old := docs[key]; old != nil && b["baseRevisionId"] != old["latestRevisionId"] {
				w.WriteHeader(409)
				return
			}
			n := len(revisions[key]) + 1
			id := "doc-" + key
			revision := fmt.Sprintf("revision-%s-%d", key, n)
			d := map[string]any{"id": id, "companyId": "company", "issueId": path[1], "key": key, "title": b["title"], "format": b["format"], "body": b["body"], "latestRevisionId": revision, "latestRevisionNumber": n}
			docs[key] = d
			revisions[key] = append(revisions[key], map[string]any{"id": revision, "documentId": id, "companyId": "company", "issueId": path[1], "key": key, "title": b["title"], "format": b["format"], "body": b["body"], "revisionNumber": n, "changeSummary": b["changeSummary"]})
			writeJSON(w, 200, d)
		default:
			t.Errorf("unexpected native request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	p.cfg.aepBaseURL = p.cfg.paperclipBaseURL
	call := func(handler func(http.ResponseWriter, *http.Request, identity) error, method, body, space, task, key, path string) (int, map[string]any) {
		t.Helper()
		r := sharedNativeRequest(method, path, body)
		r.SetPathValue("space", space)
		r.SetPathValue("task", task)
		r.SetPathValue("key", key)
		w := httptest.NewRecorder()
		if e := handler(w, r, sharedNativeIdentity); e != nil {
			sharedWriteError(w, e)
		}
		var out map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
		return w.Code, out
	}
	createBody := `{"requestId":"native-space","name":"空间","description":"目标和成功条件"}`
	status, out := call(p.sharedCreateSpace, "POST", createBody, "", "", "", "/")
	require.Equal(t, 201, status, out)
	space := out["space"].(map[string]any)["id"].(string)
	require.Equal(t, "owner", out["space"].(map[string]any)["role"])
	status, out = call(p.sharedCreateSpace, "POST", createBody, "", "", "", "/")
	require.Equal(t, 200, status, out)
	require.Equal(t, 1, creates)
	status, out = call(p.sharedUpdateProject, "PATCH", `{"name":"协作空间","status":"planned","description":"更新后的目标"}`, space, "", "", "/")
	require.Equal(t, 200, status, out)
	status, out = call(p.sharedProject, "GET", "", space, "", "", "/")
	require.Equal(t, 200, status, out)
	require.Equal(t, "planned", out["project"].(map[string]any)["status"])
	status, out = call(p.sharedCreateTask, "POST", `{"title":"父任务","requestId":"parent"}`, space, "", "", "/")
	require.Equal(t, 201, status, out)
	parent := out["task"].(map[string]any)["id"].(string)
	status, out = call(p.sharedCreateTask, "POST", fmt.Sprintf(`{"title":"子任务","requestId":"child","parentId":%q}`, parent), space, "", "", "/")
	require.Equal(t, 201, status, out)
	child := out["task"].(map[string]any)["id"].(string)
	require.Equal(t, parent, out["task"].(map[string]any)["parentId"])
	status, out = call(p.sharedTasks, "GET", "", space, "", "", "/?parentId="+parent)
	require.Equal(t, 200, status, out)
	require.Len(t, out["tasks"], 1)
	commentBody := `{"body":"讨论内容","clientRequestId":"00000000-0000-4000-8000-000000000001"}`
	status, out = call(p.sharedAddComment, "POST", commentBody, space, child, "", "/")
	require.Equal(t, 201, status, out)
	status, out = call(p.sharedAddComment, "POST", commentBody, space, child, "", "/")
	require.Equal(t, 200, status, out)
	require.Equal(t, 1, posts)
	status, out = call(p.sharedComments, "GET", "", space, child, "", "/")
	require.Equal(t, 200, status, out)
	require.Equal(t, "person", out["comments"].([]any)[0].(map[string]any)["author"].(map[string]any)["userId"])
	key := "zhiyuan-reference-guide"
	status, out = call(p.sharedPutDocument, "PUT", `{"title":"资料","body":"第一版","baseRevisionId":null}`, space, child, key, "/")
	require.Equal(t, 200, status, out)
	revision := out["document"].(map[string]any)["latestRevisionId"].(string)
	status, out = call(p.sharedPutDocument, "PUT", fmt.Sprintf(`{"title":"资料","body":"第二版","baseRevisionId":%q}`, revision), space, child, key, "/")
	require.Equal(t, 200, status, out)
	status, out = call(p.sharedPutDocument, "PUT", fmt.Sprintf(`{"title":"资料","body":"旧版覆盖","baseRevisionId":%q}`, revision), space, child, key, "/")
	require.Equal(t, 409, status, out)
	status, out = call(p.sharedDocuments, "GET", "", space, child, key, "/documents/"+key+"/revisions")
	require.Equal(t, 200, status, out)
	require.Len(t, out["revisions"], 2)
	status, out = call(p.sharedPutDocument, "PUT", `{"title":"绕过","body":"禁止"}`, space, child, "shared-space-result", "/")
	require.Equal(t, 400, status, out)
	lost := `{"body":"lost acknowledgement","clientRequestId":"00000000-0000-4000-8000-000000000002"}`
	status, out = call(p.sharedAddComment, "POST", lost, space, child, "", "/")
	require.Equal(t, 423, status, out)
	status, out = call(p.sharedAddComment, "POST", lost, space, child, "", "/")
	require.Equal(t, 423, status, out)
	require.Equal(t, 2, posts)
	// Unknown discussion evidence cannot block the task's existing result flow.
	status, out = call(p.sharedSubmit, "POST", `{"requestId":"result-after-comment","expectedVersion":0,"title":"成果","body":"可以提交"}`, space, child, "", "/")
	require.Equal(t, 201, status, out)
	status, out = call(p.sharedDocuments, "GET", "", space, child, "", "/documents")
	require.Equal(t, 200, status, out)
	require.Len(t, out["documents"], 1, "result remains behind the existing review surface")
}
