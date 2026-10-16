package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// This acceptance uses a disposable PostgreSQL schema and synthetic AEP and
// Paperclip HTTP services. The real isolated services are exercised by the
// separately named HTTP acceptance runner.
func TestSharedSpacesPostgresWorkflow(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable localhost PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.True(t, cfg.Host == "localhost" || cfg.Host == "127.0.0.1" || cfg.Host == "::1", "test PostgreSQL must be reached through localhost")
	schema := fmt.Sprintf("shared_spaces_%d", time.Now().UnixNano())
	adminDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = adminDB.Close() })
	_, err = adminDB.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = adminDB.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(sharedSpaceSchema)
	require.NoError(t, err)

	aep := newSharedSpaceAEP(t)
	defer aep.Close()
	pc := newSharedSpacePaperclip(t)
	defer pc.server.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, schemeAddK8s(scheme))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "portal-test-credentials", Namespace: "digital-employees"}, Data: map[string][]byte{
		"AEP_ADMIN_USER": []byte("synthetic-directory-admin"), "AEP_ADMIN_PASSWORD": []byte("synthetic-only"),
	}}
	h := newAPIHarness(t, secret)
	h.db = db
	h.cfg.credentialsSecret = secret.Name
	h.cfg.aepDeploymentID = "deployment-shared-space-test"
	h.cfg.adminUsers = map[string]bool{}
	h.cfg.adminRoles = map[string]bool{"platform-admin": true}
	h.cfg.aepBaseURL = aep.URL
	h.cfg.paperclipBaseURL = pc.server.URL + "/api"
	h.cfg.paperclipToken = "synthetic-paperclip-service-token"
	h.aep = &aepAuthenticator{baseURL: aep.URL, http: aep.Client()}
	h.k8s = fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(secret).Build()
	clearSharedSpaceAdminTokenCache(t)
	routes := h.routes()
	call := func(token, method, route string, body any) (int, map[string]any) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			b, marshalErr := json.Marshal(body)
			require.NoError(t, marshalErr)
			reader = strings.NewReader(string(b))
		}
		req := httptest.NewRequest(method, route, reader)
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		var out map[string]any
		if w.Body.Len() > 0 {
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
		}
		return w.Code, out
	}
	spaceID := ""
	status, out := call("token-admin-owner", http.MethodPost, "/api/v1/shared-spaces", map[string]any{
		"companyId": "company-synthetic", "projectId": "project-synthetic", "ownerId": "owner-synthetic", "requestId": "bind-space-1",
	})
	require.Equal(t, http.StatusCreated, status, out)
	spaceID = out["space"].(map[string]any)["id"].(string)
	base := "/api/v1/shared-spaces/" + url.PathEscape(spaceID)

	status, out = call("token-admin-owner", http.MethodPut, base+"/members/editor-synthetic", map[string]any{"role": "editor", "expectedVersion": 0, "requestId": "member-editor-1"})
	require.Equal(t, http.StatusOK, status, out)
	status, out = call("token-admin-owner", http.MethodPut, base+"/members/viewer-synthetic", map[string]any{"role": "viewer", "expectedVersion": 0, "requestId": "member-viewer-1"})
	require.Equal(t, http.StatusOK, status, out)
	status, out = call("token-outsider", http.MethodGet, base, nil)
	require.Equal(t, http.StatusNotFound, status, out)
	status, out = call("token-viewer", http.MethodGet, base+"/tasks", nil)
	require.Equal(t, http.StatusOK, status, out)
	status, out = call("token-editor", http.MethodGet, base+"/tasks", nil)
	require.Equal(t, http.StatusOK, status, out)
	status, out = call("token-outsider", http.MethodPost, base+"/tasks/task-main/results", map[string]any{"requestId": "outsider-submit", "expectedVersion": 0, "title": "x", "body": "x"})
	require.Equal(t, http.StatusNotFound, status, out)
	status, out = call("token-viewer", http.MethodPost, base+"/tasks/task-main/results", map[string]any{"requestId": "viewer-submit", "expectedVersion": 0, "title": "x", "body": "x"})
	require.Equal(t, http.StatusForbidden, status, out)
	status, out = call("token-editor", http.MethodGet, base+"/tasks/task-foreign", nil)
	require.Equal(t, http.StatusNotFound, status, out)
	status, out = call("token-editor", http.MethodGet, base+"/tasks/task-main/results/not-a-result", nil)
	require.Equal(t, http.StatusNotFound, status, out)

	first := map[string]any{"requestId": "submit-v1", "expectedVersion": 0, "title": "合成成果", "body": "第一版正文"}
	status, out = call("token-editor", http.MethodPost, base+"/tasks/task-main/results", first)
	require.Equal(t, http.StatusCreated, status, out)
	result1 := out["result"].(map[string]any)
	r1 := result1["id"].(string)
	revision1 := result1["revisionId"].(string)
	status, out = call("token-viewer", http.MethodGet, base+"/tasks/task-foreign/results/"+url.PathEscape(r1), nil)
	require.Equal(t, http.StatusNotFound, status, out, "a result cannot be read through a task from another project")
	status, replay := call("token-editor", http.MethodPost, base+"/tasks/task-main/results", first)
	require.Equal(t, http.StatusOK, status, replay)
	require.Equal(t, r1, replay["result"].(map[string]any)["id"])
	conflict := map[string]any{"requestId": "submit-v1", "expectedVersion": 0, "title": "冲突标题", "body": "第一版正文"}
	status, _ = call("token-editor", http.MethodPost, base+"/tasks/task-main/results", conflict)
	require.Equal(t, http.StatusConflict, status)
	status, _ = call("token-owner", http.MethodPost, base+"/tasks/task-main/results/"+url.PathEscape(r1)+"/review", map[string]any{"requestId": "stale-v1", "expectedVersion": 0, "decision": "accept"})
	require.Equal(t, http.StatusConflict, status)
	status, _ = call("token-owner", http.MethodPost, base+"/tasks/task-main/results/"+url.PathEscape(r1)+"/review", map[string]any{"requestId": "missing-reason", "expectedVersion": 1, "decision": "request_changes"})
	require.Equal(t, http.StatusBadRequest, status)
	status, out = call("token-owner", http.MethodPost, base+"/tasks/task-main/results/"+url.PathEscape(r1)+"/review", map[string]any{"requestId": "return-v1", "expectedVersion": 1, "decision": "request_changes", "reason": "请补充可核对的依据"})
	require.Equal(t, http.StatusOK, status, out)
	require.Equal(t, "in_progress", pc.status("task-main"))
	status, oldBody := call("token-viewer", http.MethodGet, base+"/tasks/task-main/results/"+url.PathEscape(r1), nil)
	require.Equal(t, http.StatusOK, status, oldBody)
	require.Equal(t, "第一版正文", oldBody["body"])

	second := map[string]any{"requestId": "submit-v2", "expectedVersion": 1, "title": "合成成果修订", "body": "第二版正文"}
	status, out = call("token-editor", http.MethodPost, base+"/tasks/task-main/results", second)
	require.Equal(t, http.StatusCreated, status, out)
	result2 := out["result"].(map[string]any)
	r2 := result2["id"].(string)
	require.NotEqual(t, revision1, result2["revisionId"])
	status, oldBody = call("token-viewer", http.MethodGet, base+"/tasks/task-main/results/"+url.PathEscape(r1), nil)
	require.Equal(t, http.StatusOK, status, oldBody)
	require.Equal(t, "第一版正文", oldBody["body"])
	status, _ = call("token-owner", http.MethodPost, base+"/tasks/task-main/results/"+url.PathEscape(r1)+"/review", map[string]any{"requestId": "accept-old-v1", "expectedVersion": 2, "decision": "accept"})
	require.Equal(t, http.StatusConflict, status)
	status, _ = call("token-owner", http.MethodPost, base+"/tasks/task-main/results/"+url.PathEscape(r2)+"/review", map[string]any{"requestId": "accept-stale-v2", "expectedVersion": 1, "decision": "accept"})
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "in_progress", pc.status("task-main"), "Paperclip must remain open until owner acceptance")
	status, out = call("token-owner", http.MethodPost, base+"/tasks/task-main/results/"+url.PathEscape(r2)+"/review", map[string]any{"requestId": "accept-v2", "expectedVersion": 2, "decision": "accept"})
	require.Equal(t, http.StatusOK, status, out)
	require.Equal(t, "accepted", out["result"].(map[string]any)["state"])
	require.Equal(t, "done", pc.status("task-main"))

	// A space owner may submit, but cannot accept their own result.
	status, out = call("token-owner", http.MethodPost, base+"/tasks/task-owner-self/results", map[string]any{"requestId": "owner-submit", "expectedVersion": 0, "title": "负责人自提交", "body": "自审检查"})
	require.Equal(t, http.StatusCreated, status, out)
	ownerResult := out["result"].(map[string]any)["id"].(string)
	status, _ = call("token-owner", http.MethodPost, base+"/tasks/task-owner-self/results/"+url.PathEscape(ownerResult)+"/review", map[string]any{"requestId": "owner-self-review", "expectedVersion": 1, "decision": "accept"})
	require.Equal(t, http.StatusForbidden, status)

	// The fake upstream commits the document but drops the acknowledgement.
	pc.failNextDocumentAck()
	status, _ = call("token-editor", http.MethodPost, base+"/tasks/task-unknown/results", map[string]any{"requestId": "unknown-submit", "expectedVersion": 0, "title": "未知回执", "body": "只写一次"})
	require.Equal(t, http.StatusLocked, status)
	status, _ = call("token-editor", http.MethodPost, base+"/tasks/task-unknown/results", map[string]any{"requestId": "unknown-submit", "expectedVersion": 0, "title": "未知回执", "body": "只写一次"})
	require.Equal(t, http.StatusLocked, status)
	require.Equal(t, 1, pc.documentWrites("task-unknown"), "unknown outcome must never be blindly resent")

	// Reopening the database simulates a Portal restart and proves durable state.
	restartedDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = restartedDB.Close() })
	restarted := *h.portal
	restarted.db = restartedDB
	restartedRoutes := restarted.routes()
	routeCall := func(token, method, route string, body any) (int, map[string]any) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			b, e := json.Marshal(body)
			require.NoError(t, e)
			reader = strings.NewReader(string(b))
		}
		req := httptest.NewRequest(method, route, reader)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		restartedRoutes.ServeHTTP(w, req)
		var out map[string]any
		if w.Body.Len() > 0 {
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
		}
		return w.Code, out
	}
	status, out = routeCall("token-viewer", http.MethodGet, base+"/tasks/task-main", nil)
	require.Equal(t, http.StatusOK, status, out)
	require.Len(t, out["results"], 2)
	status, out = routeCall("token-editor", http.MethodGet, base+"/tasks/task-unknown", nil)
	require.Equal(t, http.StatusOK, status, out)
	require.Equal(t, "unknown", out["commands"].([]any)[0].(map[string]any)["state"])
	status, replay = routeCall("token-owner", http.MethodPost, base+"/tasks/task-main/results/"+url.PathEscape(r2)+"/review", map[string]any{"requestId": "accept-v2", "expectedVersion": 2, "decision": "accept"})
	require.Equal(t, http.StatusOK, status, replay)
	require.Equal(t, 2, pc.patchWriteCount("task-main"), "the return and acceptance each write once; replay adds no third write")

	status, out = routeCall("token-owner", http.MethodPut, base+"/members/editor-synthetic", map[string]any{"role": "revoked", "expectedVersion": 1, "requestId": "member-editor-revoke"})
	require.Equal(t, http.StatusOK, status, out)
	status, _ = routeCall("token-editor", http.MethodGet, base, nil)
	require.Equal(t, http.StatusNotFound, status)
	status, _ = routeCall("token-editor", http.MethodPost, base+"/tasks/task-main/results", map[string]any{"requestId": "revoked-submit", "expectedVersion": 2, "title": "x", "body": "x"})
	require.Equal(t, http.StatusNotFound, status)
}

func clearSharedSpaceAdminTokenCache(t *testing.T) {
	t.Helper()
	adminTokenCache.mu.Lock()
	adminTokenCache.token = ""
	adminTokenCache.expires = time.Time{}
	adminTokenCache.mu.Unlock()
	t.Cleanup(func() {
		adminTokenCache.mu.Lock()
		adminTokenCache.token = ""
		adminTokenCache.expires = time.Time{}
		adminTokenCache.mu.Unlock()
	})
}

func newSharedSpaceAEP(t *testing.T) *httptest.Server {
	t.Helper()
	users := map[string]map[string]any{
		"token-admin-owner": {"id": "owner-synthetic", "displayName": "Synthetic owner", "kind": "human", "roles": []string{"platform-admin"}},
		"token-owner":       {"id": "owner-synthetic", "displayName": "Synthetic owner", "kind": "human", "roles": []string{"platform-admin"}},
		"token-editor":      {"id": "editor-synthetic", "displayName": "Synthetic editor", "kind": "human", "roles": []string{}},
		"token-viewer":      {"id": "viewer-synthetic", "displayName": "Synthetic viewer", "kind": "human", "roles": []string{}},
		"token-outsider":    {"id": "outsider-synthetic", "displayName": "Synthetic outsider", "kind": "human", "roles": []string{}},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/aep/v1/auth/password/login":
			writeJSON(w, 200, map[string]any{"accessToken": "directory-admin-token", "passwordChangeRequired": false})
		case r.Method == http.MethodGet && r.URL.Path == "/aep/v1/user/me":
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			user, ok := users[token]
			if !ok && token != "directory-admin-token" {
				writeJSON(w, 401, map[string]string{"error": "unauthorized"})
				return
			}
			if token == "directory-admin-token" {
				user = map[string]any{"id": "directory-admin", "displayName": "Directory admin", "kind": "human", "roles": []string{"platform-admin"}}
			}
			writeJSON(w, 200, map[string]any{"user": user, "roles": user["roles"], "deploymentId": "deployment-shared-space-test"})
		case r.Method == http.MethodGet && r.URL.Path == "/aep/v1/admin/users":
			items := []map[string]any{}
			for _, u := range users {
				items = append(items, map[string]any{"id": u["id"], "status": "active", "kind": "human"})
			}
			writeJSON(w, 200, map[string]any{"items": items})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/aep/v1/admin/users/"):
			if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != "directory-admin-token" {
				writeJSON(w, 401, map[string]string{"error": "unauthorized"})
				return
			}
			id := strings.TrimPrefix(r.URL.Path, "/aep/v1/admin/users/")
			for _, u := range users {
				if u["id"] == id {
					writeJSON(w, 200, map[string]any{"id": id, "username": id, "displayName": u["displayName"], "status": "active", "kind": "human"})
					return
				}
			}
			writeJSON(w, 404, map[string]string{"error": "not found"})
		default:
			writeJSON(w, 404, map[string]string{"error": "not found"})
		}
	}))
}

type sharedSpaceFakeRevision struct{ id, body string }
type sharedSpaceFakeDoc struct {
	id, title, body, revision string
	history                   []sharedSpaceFakeRevision
}
type sharedSpaceFakePaperclip struct {
	server       *httptest.Server
	mu           sync.Mutex
	docs         map[string]sharedSpaceFakeDoc
	tasks        map[string]map[string]any
	docWrites    map[string]int
	patchCount   map[string]int
	failAck      bool
	nextRevision int
}

func newSharedSpacePaperclip(t *testing.T) *sharedSpaceFakePaperclip {
	t.Helper()
	f := &sharedSpaceFakePaperclip{docs: map[string]sharedSpaceFakeDoc{}, docWrites: map[string]int{}, patchCount: map[string]int{}, tasks: map[string]map[string]any{
		"task-main":       {"id": "task-main", "companyId": "company-synthetic", "projectId": "project-synthetic", "status": "in_progress", "assigneeAgentId": "synthetic-agent"},
		"task-second":     {"id": "task-second", "companyId": "company-synthetic", "projectId": "project-synthetic", "status": "in_progress"},
		"task-owner-self": {"id": "task-owner-self", "companyId": "company-synthetic", "projectId": "project-synthetic", "status": "in_progress"},
		"task-unknown":    {"id": "task-unknown", "companyId": "company-synthetic", "projectId": "project-synthetic", "status": "in_progress"},
		"task-foreign":    {"id": "task-foreign", "companyId": "company-synthetic", "projectId": "other-project", "status": "in_progress"},
	}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	return f
}
func (f *sharedSpaceFakePaperclip) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer synthetic-paperclip-service-token" {
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "api" && parts[1] == "projects" {
		writeJSON(w, 200, map[string]any{"id": parts[2], "companyId": "company-synthetic", "name": "Synthetic project"})
		return
	}
	if r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "api" && parts[1] == "issues" {
		task, ok := f.tasks[parts[2]]
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, 200, task)
		return
	}
	if r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "api" && parts[1] == "companies" && parts[3] == "issues" {
		out := []map[string]any{}
		for _, task := range f.tasks {
			if task["companyId"] == parts[2] && task["projectId"] == r.URL.Query().Get("projectId") {
				out = append(out, task)
			}
		}
		writeJSON(w, 200, out)
		return
	}
	if len(parts) >= 5 && parts[0] == "api" && parts[1] == "issues" && parts[3] == "documents" {
		taskID, key := parts[2], parts[4]
		if _, ok := f.tasks[taskID]; !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		lookup := taskID + "/" + key
		if r.Method == http.MethodGet && len(parts) == 6 && parts[5] == "revisions" {
			doc, ok := f.docs[lookup]
			if !ok {
				writeJSON(w, 404, map[string]string{"error": "not found"})
				return
			}
			revisions := make([]map[string]any, 0, len(doc.history))
			for _, revision := range doc.history {
				revisions = append(revisions, map[string]any{"id": revision.id, "documentId": doc.id, "issueId": taskID, "companyId": "company-synthetic", "body": revision.body})
			}
			writeJSON(w, 200, revisions)
			return
		}
		switch r.Method {
		case http.MethodPut:
			var body struct {
				Title          string `json:"title"`
				Body           string `json:"body"`
				BaseRevisionID string `json:"baseRevisionId"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, 400, map[string]string{"error": "bad request"})
				return
			}
			doc := f.docs[lookup]
			if doc.id != "" && body.BaseRevisionID != doc.revision {
				writeJSON(w, 409, map[string]string{"error": "revision conflict"})
				return
			}
			if doc.id == "" {
				if body.BaseRevisionID != "" {
					writeJSON(w, 409, map[string]string{"error": "base revision missing"})
					return
				}
				doc.id = "document-" + taskID
			}
			f.nextRevision++
			doc.title, doc.body, doc.revision = body.Title, body.Body, fmt.Sprintf("revision-%d", f.nextRevision)
			doc.history = append(doc.history, sharedSpaceFakeRevision{id: doc.revision, body: doc.body})
			f.docs[lookup] = doc
			f.docWrites[taskID]++
			if f.failAck {
				f.failAck = false
				writeJSON(w, 500, map[string]string{"error": "ack lost"})
				return
			}
			writeJSON(w, 200, map[string]any{"id": doc.id, "body": doc.body, "latestRevisionId": doc.revision})
			return
		case http.MethodGet:
			doc, ok := f.docs[lookup]
			if !ok {
				writeJSON(w, 404, map[string]string{"error": "not found"})
				return
			}
			writeJSON(w, 200, map[string]any{"id": doc.id, "body": doc.body, "latestRevisionId": doc.revision})
			return
		}
	}
	if r.Method == http.MethodPatch && len(parts) == 3 && parts[0] == "api" && parts[1] == "issues" {
		task, ok := f.tasks[parts[2]]
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "not found"})
			return
		}
		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad request"})
			return
		}
		for key, value := range patch {
			task[key] = value
		}
		f.patchCount[parts[2]]++
		writeJSON(w, 200, task)
		return
	}
	writeJSON(w, 404, map[string]string{"error": "not found"})
}
func (f *sharedSpaceFakePaperclip) failNextDocumentAck() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAck = true
}
func (f *sharedSpaceFakePaperclip) status(task string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks[task]["status"].(string)
}
func (f *sharedSpaceFakePaperclip) documentWrites(task string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.docWrites[task]
}
func (f *sharedSpaceFakePaperclip) patchWriteCount(task string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.patchCount[task]
}
