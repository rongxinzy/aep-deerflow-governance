package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

type dependencyPGFixture struct {
	h                 *apiTestPortal
	db                *sql.DB
	cfg               *pgx.ConnConfig
	mu                sync.Mutex
	tasks             map[string]map[string]any
	ids               map[string][]string
	docs              map[string]map[string]any
	receipts          map[string]map[string]any
	starts            map[string]int
	returns           []map[string]any
	checkoutPosts     int
	loseCheckout      bool
	loseStart         bool
	finalize          bool
	rejectCycle       bool
	loseDependency    bool
	dependencyPatches int
}

func newDependencyPGFixture(t *testing.T) *dependencyPGFixture {
	t.Helper()
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable localhost PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"localhost", "127.0.0.1", "::1"}, cfg.Host)
	schema := fmt.Sprintf("shared_dependencies_%d", time.Now().UnixNano())
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
	h := collaborationHarness(t)
	_ = runtimeWorkspace(t, h)
	h.db = db
	_, err = db.Exec(`INSERT INTO shared_spaces(id,deployment_id,company_id,project_id,name) VALUES('space','deployment-acme','company','project','空间');
INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES('space','u-x','owner',1),('space','u-owner','owner',1);
INSERT INTO shared_space_participants(id,space_id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state) VALUES('participant','space','companion','ws-source/assistant','搭档','u-x','runtime-test-uid','editor','active',1,'agent','ready')`)
	require.NoError(t, err)
	f := &dependencyPGFixture{h: h, db: db, cfg: cfg, tasks: map[string]map[string]any{}, ids: map[string][]string{}, docs: map[string]map[string]any{}, receipts: map[string]map[string]any{}, starts: map[string]int{}, loseCheckout: true, loseStart: true}
	for _, id := range []string{"a", "b", "c", "foreign"} {
		f.tasks[id] = map[string]any{"id": id, "companyId": "company", "projectId": "project", "title": "任务" + id, "status": "todo", "updatedAt": "initial"}
	}
	f.tasks["foreign"]["projectId"] = "another-project"
	pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.paperclip(t, w, r) }))
	t.Cleanup(pc.Close)
	h.cfg.paperclipBaseURL = pc.URL
	h.cfg.paperclipToken = "synthetic"
	pi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.pi(t, w, r) }))
	t.Cleanup(pi.Close)
	h.workspaceChatOverride = pi.URL
	return f
}
func (f *dependencyPGFixture) task(id string, relations bool) map[string]any {
	out := map[string]any{}
	for k, v := range f.tasks[id] {
		out[k] = v
	}
	if relations {
		refs := []any{}
		for _, source := range f.ids[id] {
			refs = append(refs, map[string]any{"id": source, "title": f.tasks[source]["title"], "status": f.tasks[source]["status"]})
		}
		out["blockedBy"] = refs
	}
	return out
}
func (f *dependencyPGFixture) paperclip(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.URL.Path == "/companies/company/issues" {
		require.Equal(t, "true", r.URL.Query().Get("includeBlockedBy"))
		out := []map[string]any{}
		for id, task := range f.tasks {
			if task["projectId"] == r.URL.Query().Get("projectId") {
				out = append(out, f.task(id, r.URL.Query().Get("includeBlockedBy") == "true"))
			}
		}
		writeJSON(w, 200, out)
		return
	}
	if len(parts) < 2 || parts[0] != "issues" {
		t.Errorf("unexpected native path %s", r.URL.Path)
		w.WriteHeader(404)
		return
	}
	id := parts[1]
	if len(parts) == 2 {
		if r.Method == "PATCH" {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if refs, ok := body["blockedByIssueIds"].([]any); ok {
				f.dependencyPatches++
				if f.rejectCycle {
					w.WriteHeader(422)
					return
				}
				f.ids[id] = []string{}
				for _, v := range refs {
					f.ids[id] = append(f.ids[id], v.(string))
				}
				delete(body, "blockedByIssueIds")
				if f.loseDependency {
					f.loseDependency = false
					f.tasks[id]["updatedAt"] = "dependencies-changed"
					w.WriteHeader(503)
					return
				}
			}
			for k, v := range body {
				f.tasks[id][k] = v
			}
			f.tasks[id]["updatedAt"] = fmt.Sprint(time.Now().UnixNano())
		}
		writeJSON(w, 200, f.task(id, true))
		return
	}
	switch parts[2] {
	case "diagnostics":
		ready := !f.finalize
		blockers := []any{}
		for _, source := range f.ids[id] {
			done := f.tasks[source]["status"] == "done"
			ready = ready && done
			blockers = append(blockers, map[string]any{"id": source, "isDependencyReady": done && !f.finalize, "isPendingFinalize": done && f.finalize})
		}
		writeJSON(w, 200, map[string]any{"issue": map[string]any{"id": id}, "readiness": map[string]any{"isDependencyReady": ready}, "blockers": blockers})
	case "checkout":
		f.checkoutPosts++
		f.tasks[id]["status"] = "in_progress"
		if f.loseCheckout {
			f.loseCheckout = false
			w.WriteHeader(503)
			return
		}
		writeJSON(w, 200, f.task(id, true))
	case "documents":
		if r.Method == "PUT" {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			f.docs[id] = map[string]any{"id": "doc-" + id, "latestRevisionId": "revision-" + id, "body": body["body"]}
		}
		writeJSON(w, 200, f.docs[id])
	default:
		t.Errorf("unexpected native request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}
}
func (f *dependencyPGFixture) pi(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/assistant/api/conversations/42":
		writeJSON(w, 200, map[string]any{"conversationId": 42})
	case "/assistant/internal/shared-space/receipt":
		receipt, ok := f.receipts[r.URL.Query().Get("commandId")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		writeJSON(w, 200, receipt)
	case "/assistant/internal/shared-space/start", "/assistant/internal/shared-space/return":
		var in struct {
			CommandID string         `json:"commandId"`
			Hash      string         `json:"payloadHash"`
			Payload   map[string]any `json:"payload"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.Equal(t, in.Hash, sharedHash(in.Payload))
		receipt := map[string]any{"commandId": in.CommandID, "payloadHash": in.Hash, "state": "applied"}
		if strings.HasSuffix(r.URL.Path, "/start") {
			id := in.Payload["taskId"].(string)
			f.starts[id]++
			receipt["task"] = map[string]any{"id": "pi-" + id, "executionId": in.Payload["executionId"], "generation": in.Payload["generation"]}
			f.receipts[in.CommandID] = receipt
			if id == "b" && f.loseStart {
				f.loseStart = false
				w.WriteHeader(503)
				return
			}
		} else {
			require.EqualValues(t, 42, in.Payload["conversationId"])
			f.returns = append(f.returns, in.Payload)
			f.receipts[in.CommandID] = receipt
		}
		writeJSON(w, 200, receipt)
	case "/assistant/api/tasks/pi-b":
		var binding, execution string
		require.NoError(t, f.db.QueryRow(`SELECT id,execution_id FROM shared_space_runtime_bindings WHERE task_id='b' AND binding_state='active'`).Scan(&binding, &execution))
		writeJSON(w, 200, map[string]any{"task": map[string]any{"id": "pi-b", "status": "completed", "executionId": execution, "generation": 1, "selectedArtifactId": "artifact-b", "sharedRuntime": map[string]any{"bindingId": binding}}})
	case "/assistant/internal/shared-space/artifact":
		writeJSON(w, 200, map[string]any{"taskId": "pi-b", "title": "B成果", "body": "基于A共享成果形成的B成果"})
	default:
		t.Errorf("unexpected companion request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}
}
func (f *dependencyPGFixture) call(t *testing.T, fn func(http.ResponseWriter, *http.Request, identity) error, method, body, task, result string, actor identity) (int, map[string]any) {
	t.Helper()
	r := sharedNativeRequest(method, "/", body)
	r.SetPathValue("task", task)
	r.SetPathValue("result", result)
	w := httptest.NewRecorder()
	if err := fn(w, r, actor); err != nil {
		sharedWriteError(w, err)
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
	return w.Code, out
}
func (f *dependencyPGFixture) tick(t *testing.T) {
	t.Helper()
	_, err := f.db.Exec(`UPDATE shared_space_runtime_outbox SET next_attempt_at='1970-01-01' WHERE state IN('pending','unknown')`)
	require.NoError(t, err)
	f.h.processSharedRuntime(t.Context())
}
