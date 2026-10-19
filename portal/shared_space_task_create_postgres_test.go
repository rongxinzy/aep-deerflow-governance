package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// Uses only a disposable local PostgreSQL schema and a synthetic task service.
// Reopening the pool checks that receipt recovery needs no process memory.
func TestSharedSpacesPostgresTaskCreation(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable localhost PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.True(t, cfg.Host == "localhost" || cfg.Host == "127.0.0.1" || cfg.Host == "::1", "test PostgreSQL must be reached through localhost")
	schema := fmt.Sprintf("shared_task_create_%d", time.Now().UnixNano())
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		require.NoError(t, cleanupErr)
	})
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(sharedSpaceSchema)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO shared_spaces(id,deployment_id,company_id,project_id,name) VALUES('space','deployment','company','project','空间'); INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES('space','editor','editor',1),('space','viewer','viewer',1),('space','revoked','revoked',1)`)
	require.NoError(t, err)
	var calls atomic.Int32
	pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/companies/company/issues", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "project", body["projectId"])
		require.Equal(t, "backlog", body["status"])
		require.Nil(t, body["assigneeUserId"])
		if body["title"] == "lost acknowledgement" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		body["id"] = fmt.Sprintf("task-%d", n)
		body["companyId"] = "company"
		writeJSON(w, 201, body)
	}))
	defer pc.Close()
	p := &portal{db: db}
	p.cfg.paperclipBaseURL = pc.URL
	p.cfg.paperclipToken = "synthetic-only"
	call := func(actor, method, request, title string) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(sharedTaskCreation{Title: title, Priority: "medium", RequestID: request})
		r := httptest.NewRequest(method, "/", strings.NewReader(string(raw)))
		r.SetPathValue("space", "space")
		r.SetPathValue("request", request)
		w := httptest.NewRecorder()
		ident := identity{UserID: actor, DeploymentID: "deployment"}
		var callErr error
		if method == http.MethodPost {
			callErr = p.sharedCreateTask(w, r, ident)
		} else {
			callErr = p.sharedTaskCommand(w, r, ident)
		}
		if callErr != nil {
			sharedWriteError(w, callErr)
		}
		var out map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		return w.Code, out
	}
	status, created := call("editor", "POST", "first", "new task")
	require.Equal(t, 201, status, created)
	require.Equal(t, "applied", created["state"])
	require.EqualValues(t, 1, calls.Load())
	status, replayed := call("editor", "POST", "first", "new task")
	require.Equal(t, 200, status, replayed)
	require.Equal(t, created, replayed)
	require.EqualValues(t, 1, calls.Load())
	status, _ = call("editor", "POST", "first", "changed body")
	require.Equal(t, 409, status)
	require.EqualValues(t, 1, calls.Load())
	require.NoError(t, db.Close())
	db = stdlib.OpenDB(*cfg)
	p = &portal{db: db, cfg: p.cfg}
	status, recovered := call("editor", "GET", "first", "")
	require.Equal(t, 200, status, recovered)
	require.Equal(t, created, recovered)
	require.EqualValues(t, 1, calls.Load())
	status, unknown := call("editor", "POST", "uncertain", "lost acknowledgement")
	require.Equal(t, 202, status, unknown)
	require.Equal(t, "unknown", unknown["state"])
	require.EqualValues(t, 2, calls.Load())
	status, unknown = call("editor", "POST", "uncertain", "lost acknowledgement")
	require.Equal(t, 202, status, unknown)
	require.EqualValues(t, 2, calls.Load())
	status, unknown = call("editor", "GET", "uncertain", "")
	require.Equal(t, 200, status, unknown)
	require.Equal(t, "unknown", unknown["state"])
	require.EqualValues(t, 2, calls.Load())
	status, second := call("editor", "POST", "independent", "another task")
	require.Equal(t, 201, status, second)
	require.EqualValues(t, 3, calls.Load())
	for _, actor := range []string{"viewer", "revoked"} {
		status, out := call(actor, "POST", "denied-"+actor, "denied")
		if actor == "viewer" {
			require.Equal(t, 403, status, out)
		} else {
			require.Equal(t, 404, status, out)
		}
	}
	require.EqualValues(t, 3, calls.Load())
	// Two independently pooled sessions contend for the same space lock. Exactly
	// one dispatch is allowed even when both callers initially lack a receipt.
	type outcome struct {
		status int
		body   map[string]any
	}
	concurrent := make(chan outcome, 2)
	for range 2 {
		go func() {
			var result outcome
			defer func() { concurrent <- result }()
			result.status, result.body = call("editor", "POST", "concurrent", "concurrent task")
		}()
	}
	one, two := <-concurrent, <-concurrent
	require.ElementsMatch(t, []int{200, 201}, []int{one.status, two.status})
	require.Equal(t, one.body, two.body)
	require.EqualValues(t, 4, calls.Load())
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM shared_space_commands WHERE action='create_task'`).Scan(&count))
	require.Equal(t, 4, count)
	var state string
	require.NoError(t, db.QueryRow(`SELECT state FROM shared_space_commands WHERE request_id='uncertain'`).Scan(&state))
	require.Equal(t, "unknown", state)
}
