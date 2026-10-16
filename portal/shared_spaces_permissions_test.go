package main

import (
	"bytes"
	"context"
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

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestSharedSpacesOwnerPermissions exercises ownership transitions against
// PostgreSQL because the last-owner check and membership versions are
// transactional invariants.
func TestSharedSpacesOwnerPermissions(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable localhost PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.True(t, cfg.Host == "localhost" || cfg.Host == "127.0.0.1" || cfg.Host == "::1", "test PostgreSQL must be reached through localhost")
	schema := fmt.Sprintf("shared_space_permissions_%d", time.Now().UnixNano())
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
	_, err = db.Exec(sharedRuntimeSchema)
	require.NoError(t, err)

	aep := newSharedSpaceAEP(t)
	defer aep.Close()
	pc := newSharedSpacePaperclip(t)
	defer pc.server.Close()
	pc.tasks["task-main"]["assigneeAgentId"] = "assigned-agent-synthetic"
	pc.tasks["task-unassigned"] = map[string]any{"id": "task-unassigned", "companyId": "company-synthetic", "projectId": "project-synthetic", "status": "todo"}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "portal-test-credentials", Namespace: "digital-employees"}, Data: map[string][]byte{
		"AEP_ADMIN_USER": []byte("synthetic-directory-admin"), "AEP_ADMIN_PASSWORD": []byte("synthetic-only"),
	}}
	h := newAPIHarness(t, secret)
	h.db = db
	h.cfg.credentialsSecret = secret.Name
	h.cfg.aepDeploymentID = "deployment-shared-space-test"
	h.cfg.adminRoles = map[string]bool{"platform-admin": true}
	h.cfg.aepBaseURL = aep.URL
	h.cfg.paperclipBaseURL = pc.server.URL + "/api"
	h.cfg.paperclipToken = "synthetic-paperclip-service-token"
	h.aep = &aepAuthenticator{baseURL: aep.URL, http: aep.Client()}
	scheme := runtime.NewScheme()
	require.NoError(t, schemeAddK8s(scheme))
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

	status, out := call("token-admin-owner", http.MethodPost, "/api/v1/shared-spaces", map[string]any{
		"companyId": "company-synthetic", "projectId": "project-synthetic", "ownerId": "owner-synthetic", "requestId": "bind-permissions-1",
	})
	require.Equal(t, http.StatusCreated, status, out)
	spaceID := out["space"].(map[string]any)["id"].(string)
	base := "/api/v1/shared-spaces/" + spaceID
	status, out = call("token-owner", http.MethodGet, "/api/v1/shared-spaces", nil)
	require.Equal(t, http.StatusOK, status, out)
	require.Len(t, out["spaces"], 1)
	status, out = call("token-outsider", http.MethodGet, "/api/v1/shared-spaces", nil)
	require.Equal(t, http.StatusOK, status, out)
	require.Empty(t, out["spaces"], "the directory lists only spaces the current person can access")
	bindBody := map[string]any{"companyId": "company-synthetic", "projectId": "project-synthetic", "ownerId": "owner-synthetic", "requestId": "bind-permissions-1"}
	status, out = call("token-admin-owner", http.MethodPost, "/api/v1/shared-spaces", bindBody)
	require.Equal(t, http.StatusOK, status, out, "an identical bootstrap request replays its receipt")
	changedBind := map[string]any{"companyId": "company-synthetic", "projectId": "project-second", "ownerId": "owner-synthetic", "requestId": "bind-permissions-1"}
	status, _ = call("token-admin-owner", http.MethodPost, "/api/v1/shared-spaces", changedBind)
	require.Equal(t, http.StatusConflict, status, "a request id cannot be reused with a different project")
	status, _ = call("token-editor", http.MethodPost, "/api/v1/shared-spaces", map[string]any{"companyId": "company-synthetic", "projectId": "project-second", "ownerId": "owner-synthetic", "requestId": "nonadmin-bind"})
	require.Equal(t, http.StatusForbidden, status)
	status, _ = call("token-admin-owner", http.MethodPost, "/api/v1/shared-spaces", map[string]any{"companyId": "company-other", "projectId": "project-second", "ownerId": "owner-synthetic", "requestId": "wrong-company-bind"})
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = call("token-admin-owner", http.MethodPost, "/api/v1/shared-spaces", map[string]any{"companyId": "company-synthetic", "projectId": "project-second", "ownerId": "owner-synthetic"})
	require.Equal(t, http.StatusBadRequest, status, "bootstrap requires a stable request id")
	status, _ = call("token-admin-owner", http.MethodPost, "/api/v1/shared-spaces", map[string]any{"companyId": "company-synthetic", "projectId": "project-synthetic", "ownerId": "owner-synthetic", "requestId": "bind-duplicate-project"})
	require.Equal(t, http.StatusConflict, status, "a project can be bound to only one shared space")
	status, _ = call("token-admin-owner", http.MethodPost, "/api/v1/shared-spaces", map[string]any{"companyId": "company-synthetic", "projectId": "project-second", "ownerId": "missing-directory-user", "requestId": "bind-unknown-owner"})
	require.Equal(t, http.StatusBadRequest, status, "only active AEP users may own a shared space")
	status, _ = call("token-outsider-unrecognized", http.MethodGet, base, nil)
	require.Equal(t, http.StatusUnauthorized, status)

	// Malformed or forward-unknown request fields fail closed at the API edge.
	status, _ = call("token-owner", http.MethodPut, base+"/members/viewer-synthetic", map[string]any{"role": "viewer", "expectedVersion": 0, "requestId": "strict-body", "unexpected": true})
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = call("token-owner", http.MethodPut, base+"/members/missing-directory-user", map[string]any{"role": "viewer", "expectedVersion": 0, "requestId": "unknown-member"})
	require.Equal(t, http.StatusBadRequest, status, "member changes require an active AEP directory user")
	status, _ = call("token-owner", http.MethodPut, base+"/members/viewer-synthetic", map[string]any{"role": "viewer", "expectedVersion": 0})
	require.Equal(t, http.StatusBadRequest, status, "membership commands require a stable request id")
	status, _ = call("token-owner", http.MethodPut, base+"/members/viewer-synthetic", map[string]any{"role": "unexpected-role", "expectedVersion": 0, "requestId": "invalid-member-role"})
	require.Equal(t, http.StatusBadRequest, status)
	status, out = call("token-owner", http.MethodPut, base+"/members/viewer-synthetic", map[string]any{"role": "editor", "expectedVersion": 0, "requestId": "add-viewer-as-editor"})
	require.Equal(t, http.StatusOK, status, out)
	status, _ = call("token-owner", http.MethodPut, base+"/members/viewer-synthetic", map[string]any{"role": "viewer", "expectedVersion": 0, "requestId": "stale-member-version"})
	require.Equal(t, http.StatusConflict, status, "membership updates use optimistic versions")
	status, _ = call("token-owner", http.MethodGet, base+"/tasks?limit=0", nil)
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = call("token-owner", http.MethodGet, base+"/tasks?offset=-1", nil)
	require.Equal(t, http.StatusBadRequest, status)

	// The Paperclip response can grow new private fields; pagination must still
	// return only the browser DTO and report that another page exists.
	pc.tasks["task-main"]["providerConfig"] = map[string]any{"token": "must-not-leak"}
	status, out = call("token-owner", http.MethodGet, base+"/tasks?limit=1&offset=0&q=synthetic", nil)
	require.Equal(t, http.StatusOK, status, out)
	require.True(t, out["hasMore"].(bool))
	require.Len(t, out["tasks"], 1)
	for _, task := range out["tasks"].([]any) {
		require.NotContains(t, task.(map[string]any), "providerConfig")
	}

	// A platform administrator still needs explicit space membership.
	status, _ = call("directory-admin-token", http.MethodGet, base, nil)
	require.Equal(t, http.StatusNotFound, status)

	status, out = call("token-owner", http.MethodPut, base+"/members/editor-synthetic", map[string]any{"role": "owner", "expectedVersion": 0, "requestId": "promote-second-owner"})
	require.Equal(t, http.StatusOK, status, out)
	status, out = call("token-editor", http.MethodPost, base+"/tasks/task-unassigned/results", map[string]any{"requestId": "submit-unassigned-task", "expectedVersion": 0, "title": "未分配任务成果", "body": "请求修改后应维持待办"})
	require.Equal(t, http.StatusCreated, status, out)
	unassignedResultID := out["result"].(map[string]any)["id"].(string)
	status, out = call("token-owner", http.MethodPost, base+"/tasks/task-unassigned/results/"+url.PathEscape(unassignedResultID)+"/review", map[string]any{"requestId": "return-unassigned-task", "expectedVersion": 1, "decision": "request_changes", "reason": "补充交付依据"})
	require.Equal(t, http.StatusOK, status, out)
	require.Equal(t, "todo", pc.status("task-unassigned"), "an unassigned task returns to todo")

	// The first owner may hand off ownership while another owner remains.
	status, out = call("token-owner", http.MethodPut, base+"/members/owner-synthetic", map[string]any{"role": "revoked", "expectedVersion": 1, "requestId": "revoke-first-owner"})
	require.Equal(t, http.StatusOK, status, out)
	status, _ = call("token-owner", http.MethodGet, base, nil)
	require.Equal(t, http.StatusNotFound, status, "the former owner loses access immediately")
	status, out = call("token-editor", http.MethodGet, base, nil)
	require.Equal(t, http.StatusOK, status, out, "the new owner retains access")

	// Concurrent retries with the same command id write one document and replay
	// the stored receipt to every caller.
	parallelCall := func(token, route string, body func(int) any, count int) []int {
		t.Helper()
		start := make(chan struct{})
		codes := make(chan int, count)
		var wg sync.WaitGroup
		for index := range count {
			wg.Add(1)
			go func() {
				defer wg.Done()
				encoded, marshalErr := json.Marshal(body(index))
				if marshalErr != nil {
					codes <- http.StatusInternalServerError
					return
				}
				<-start
				req := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(encoded))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				routes.ServeHTTP(w, req)
				codes <- w.Code
			}()
		}
		close(start)
		wg.Wait()
		close(codes)
		got := make([]int, 0, count)
		for code := range codes {
			got = append(got, code)
		}
		return got
	}
	sameKeyCodes := parallelCall("token-editor", base+"/tasks/task-main/results", func(int) any {
		return map[string]any{"requestId": "concurrent-same-key", "expectedVersion": 0, "title": "一次写入", "body": "相同请求并发重试"}
	}, 4)
	for _, code := range sameKeyCodes {
		require.Contains(t, []int{http.StatusCreated, http.StatusOK}, code)
	}
	require.Equal(t, 1, pc.documentWrites("task-main"))

	// Different command ids still race on the task's expected version; exactly
	// one version-zero submission may reach Paperclip.
	differentKeyCodes := parallelCall("token-editor", base+"/tasks/task-second/results", func(index int) any {
		return map[string]any{"requestId": fmt.Sprintf("concurrent-version-race-%d", index), "expectedVersion": 0, "title": "竞争提交", "body": "版本冲突验证"}
	}, 2)
	created, conflicted := 0, 0
	for _, code := range differentKeyCodes {
		if code == http.StatusCreated {
			created++
		}
		if code == http.StatusConflict {
			conflicted++
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, 1, conflicted)
	require.Equal(t, 1, pc.documentWrites("task-second"))

	// A stale owner cannot remove the last active owner after a handoff.
	status, _ = call("token-editor", http.MethodPut, base+"/members/editor-synthetic", map[string]any{"role": "revoked", "expectedVersion": 1, "requestId": "remove-last-owner"})
	require.Equal(t, http.StatusConflict, status)
	status, out = call("token-editor", http.MethodGet, base, nil)
	require.Equal(t, http.StatusOK, status, out)
	require.Equal(t, "owner", out["space"].(map[string]any)["role"])

	// Restore the former owner while retaining the promoted owner, then accept
	// the competing submission and verify terminal tasks lose write capability.
	status, out = call("token-editor", http.MethodPut, base+"/members/owner-synthetic", map[string]any{"role": "owner", "expectedVersion": 2, "requestId": "restore-owner"})
	require.Equal(t, http.StatusOK, status, out)
	status, out = call("token-editor", http.MethodGet, base+"/tasks/task-second", nil)
	require.Equal(t, http.StatusOK, status, out)
	resultID := out["results"].([]any)[0].(map[string]any)["id"].(string)
	status, _ = call("token-viewer", http.MethodPost, base+"/tasks/task-second/results/"+url.PathEscape(resultID)+"/review", map[string]any{"requestId": "non-owner-review", "expectedVersion": 1, "decision": "accept"})
	require.Equal(t, http.StatusForbidden, status, "editors cannot review results")
	status, _ = call("token-owner", http.MethodPost, base+"/tasks/task-second/results/not-the-current-result/review", map[string]any{"requestId": "review-stale-id", "expectedVersion": 1, "decision": "request_changes", "reason": "审阅边界"})
	require.Equal(t, http.StatusConflict, status)
	status, _ = call("token-owner", http.MethodPost, base+"/tasks/task-second/results/any-result/review", map[string]any{"requestId": "review-invalid-decision", "expectedVersion": 1, "decision": "approve"})
	require.Equal(t, http.StatusBadRequest, status)
	pc.mu.Lock()
	docKey := "task-second/shared-space-result"
	doc := pc.docs[docKey]
	doc.history[0].body = "被篡改的历史版本"
	pc.docs[docKey] = doc
	pc.mu.Unlock()
	status, _ = call("token-editor", http.MethodGet, base+"/tasks/task-second/results/"+url.PathEscape(resultID), nil)
	require.Equal(t, http.StatusConflict, status, "a revision body that no longer matches its stored hash must not be served")
	pc.mu.Lock()
	doc = pc.docs[docKey]
	doc.history[0].body = "版本冲突验证"
	pc.docs[docKey] = doc
	pc.mu.Unlock()
	status, out = call("token-owner", http.MethodPost, base+"/tasks/task-second/results/"+url.PathEscape(resultID)+"/review", map[string]any{"requestId": "accept-raced-result", "expectedVersion": 1, "decision": "accept"})
	require.Equal(t, http.StatusOK, status, out)
	status, out = call("token-editor", http.MethodGet, base+"/tasks/task-second", nil)
	require.Equal(t, http.StatusOK, status, out)
	require.Equal(t, false, out["capabilities"].(map[string]any)["submit"])
	status, out = call("token-owner", http.MethodPost, base+"/tasks/task-main/results/"+url.PathEscape(collaborationID(spaceID, "concurrent-same-key"))+"/review", map[string]any{"requestId": "return-assigned-task", "expectedVersion": 1, "decision": "request_changes", "reason": "补充内容"})
	require.Equal(t, http.StatusOK, status, out)
	require.Equal(t, "in_progress", pc.status("task-main"), "an assigned task remains in progress after changes are requested")
	status, _ = call("token-editor", http.MethodPost, base+"/tasks/task-second/results", map[string]any{"requestId": "submit-after-terminal", "expectedVersion": 1, "title": "已结束", "body": "不可更改"})
	require.Equal(t, http.StatusConflict, status)
	status, _ = call("token-editor", http.MethodPost, base+"/tasks/task-second/results", map[string]any{"requestId": "submit-empty-body", "expectedVersion": 1, "title": "标题", "body": "  "})
	require.Equal(t, http.StatusBadRequest, status)

	// A deployment change invalidates an otherwise valid fresh identity.
	h.cfg.aepDeploymentID = "different-deployment"
	status, _ = call("token-editor", http.MethodGet, base, nil)
	require.Equal(t, http.StatusUnauthorized, status)
	h.cfg.aepDeploymentID = "deployment-shared-space-test"

	// Missing upstream configuration is rejected before any Paperclip call.
	h.cfg.paperclipToken = ""
	status, _ = call("token-editor", http.MethodGet, base, nil)
	require.Equal(t, http.StatusServiceUnavailable, status)
	h.cfg.paperclipToken = "synthetic-paperclip-service-token"
	h.cfg.paperclipBaseURL = "://malformed-upstream-url"
	status, _ = call("token-editor", http.MethodGet, base+"/tasks", nil)
	require.Equal(t, http.StatusServiceUnavailable, status, "an invalid upstream URL fails closed")
	h.cfg.paperclipBaseURL = pc.server.URL + "/api"

	// A task returned outside the bound project is rejected by both detail and
	// list APIs instead of leaking cross-project data.
	pc.mu.Lock()
	originalProject := pc.tasks["task-main"]["projectId"]
	pc.tasks["task-main"]["projectId"] = "other-project"
	pc.mu.Unlock()
	status, _ = call("token-editor", http.MethodGet, base+"/tasks/task-main", nil)
	require.Equal(t, http.StatusNotFound, status)
	status, out = call("token-editor", http.MethodGet, base+"/tasks", nil)
	require.Equal(t, http.StatusOK, status)
	for _, task := range out["tasks"].([]any) {
		require.NotEqual(t, "task-main", task.(map[string]any)["id"])
	}
	pc.mu.Lock()
	pc.tasks["task-main"]["projectId"] = originalProject
	pc.mu.Unlock()

	// A live AEP identity is not enough to authorize membership changes when
	// the current deployment's directory cannot resolve the target user.
	directoryUnavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/aep/v1/auth/password/login":
			writeJSON(w, http.StatusOK, map[string]any{"accessToken": "directory-admin-token", "passwordChangeRequired": false})
		case r.Method == http.MethodGet && r.URL.Path == "/aep/v1/user/me":
			user := map[string]any{"id": "owner-synthetic", "displayName": "Synthetic owner", "kind": "human", "roles": []string{"platform-admin"}}
			writeJSON(w, http.StatusOK, map[string]any{"user": user, "roles": user["roles"], "deploymentId": "deployment-shared-space-test"})
		case strings.HasPrefix(r.URL.Path, "/aep/v1/admin/users"):
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "synthetic directory outage"})
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		}
	}))
	defer directoryUnavailable.Close()
	h.aep = &aepAuthenticator{baseURL: directoryUnavailable.URL, http: directoryUnavailable.Client()}
	h.cfg.aepBaseURL = directoryUnavailable.URL
	clearSharedSpaceAdminTokenCache(t)
	status, _ = call("token-admin-owner", http.MethodPut, base+"/members/directory-outage-user", map[string]any{"role": "editor", "expectedVersion": 0, "requestId": "directory-outage-member-change"})
	require.Equal(t, http.StatusServiceUnavailable, status, "membership changes fail closed during an AEP directory outage")
}

func TestSharedDiscardConnectionClosesBrokenConnection(t *testing.T) {
	require.Equal(t, "synthetic error", sharedFail(http.StatusBadRequest, "synthetic error").Error())
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)

	sharedDiscardConnection(conn)
	require.Error(t, conn.PingContext(context.Background()))
}
