package main

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
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

func TestSharedSpacesPostgresAttachments(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable localhost PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	require.Contains(t, []string{"localhost", "127.0.0.1", "::1"}, cfg.Host)
	schema := fmt.Sprintf("shared_attachments_%d", time.Now().UnixNano())
	admin := stdlib.OpenDB(*cfg)
	defer func() { _ = admin.Close() }()
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	defer func() { _, e := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); require.NoError(t, e) }()
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(sharedSpaceSchema)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO shared_spaces(id,deployment_id,company_id,project_id,name) VALUES('space','deployment','company','project','空间'); INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES('space','editor','editor',1),('space','viewer','viewer',1)`)
	require.NoError(t, err)
	var uploads atomic.Int32
	payload := []byte("原始字节\x00\xff")
	state := "done"
	conversation := false
	pc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/issues/task":
			body := map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": state}
			if conversation {
				body["conversationAgentId"] = "agent"
			}
			writeJSON(w, 200, body)
		case r.Method == "GET" && r.URL.Path == "/issues/task/attachments":
			writeJSON(w, 200, []any{map[string]any{"id": "attachment", "issueId": "task", "originalFilename": "中文.bin", "contentType": "application/octet-stream", "byteSize": len(payload), "provider": "secret", "objectKey": "secret"}})
		case r.Method == "POST":
			uploads.Add(1)
			require.NoError(t, r.ParseMultipartForm(1<<20))
			f, h, e := r.FormFile("file")
			require.NoError(t, e)
			defer func() { _ = f.Close() }()
			data, e := io.ReadAll(f)
			require.NoError(t, e)
			require.Equal(t, payload, data)
			require.Equal(t, "中文.bin", h.Filename)
			if uploads.Load() == 2 {
				w.WriteHeader(503)
				return
			}
			writeJSON(w, 201, map[string]any{"id": "attachment", "issueId": "task", "originalFilename": h.Filename, "byteSize": len(data)})
		case r.URL.Path == "/attachments/attachment/content":
			require.Equal(t, "true", r.URL.Query().Get("download"))
			w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''%E4%B8%AD%E6%96%87.bin")
			http.ServeContent(w, r, "中文.bin", time.Time{}, bytes.NewReader(payload))
		default:
			t.Errorf("unexpected native route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer pc.Close()
	p := &portal{db: db}
	p.cfg.paperclipBaseURL = pc.URL
	p.cfg.paperclipToken = "synthetic"
	call := func(actor, method, request, attachment, rangeHeader string) (*httptest.ResponseRecorder, error) {
		var body bytes.Buffer
		ct := ""
		if method == "POST" {
			mw := multipart.NewWriter(&body)
			require.NoError(t, mw.WriteField("requestId", request))
			f, e := mw.CreateFormFile("file", "中文.bin")
			require.NoError(t, e)
			_, e = f.Write(payload)
			require.NoError(t, e)
			require.NoError(t, mw.Close())
			ct = mw.FormDataContentType()
		}
		r := httptest.NewRequest(method, "/?download=1", &body)
		r.Header.Set("Content-Type", ct)
		r.Header.Set("Range", rangeHeader)
		r.SetPathValue("space", "space")
		r.SetPathValue("task", "task")
		r.SetPathValue("request", request)
		r.SetPathValue("attachment", attachment)
		w := httptest.NewRecorder()
		i := identity{UserID: actor, DeploymentID: "deployment"}
		var e error
		switch {
		case method == "POST":
			e = p.sharedUploadAttachment(w, r, i)
		case attachment != "":
			e = p.sharedAttachmentContent(w, r, i)
		case request != "":
			e = p.sharedAttachmentCommand(w, r, i)
		default:
			e = p.sharedAttachments(w, r, i)
		}
		if e != nil {
			sharedWriteError(w, e)
		}
		return w, e
	}
	w, _ := call("viewer", "POST", "denied", "", "")
	require.Equal(t, 403, w.Code)
	w, _ = call("absent", "GET", "", "", "")
	require.Equal(t, 404, w.Code)
	w, e := call("editor", "POST", "first", "", "")
	require.NoError(t, e)
	require.Equal(t, 201, w.Code)
	require.Contains(t, w.Body.String(), `"status":"applied"`)
	w, _ = call("editor", "POST", "first", "", "")
	require.Equal(t, 200, w.Code)
	require.EqualValues(t, 1, uploads.Load())
	state = "in_progress"
	w, _ = call("editor", "POST", "uncertain", "", "")
	require.Equal(t, 202, w.Code)
	w, _ = call("editor", "POST", "uncertain", "", "")
	require.Equal(t, 202, w.Code)
	require.EqualValues(t, 2, uploads.Load())
	w, _ = call("editor", "GET", "uncertain", "", "")
	require.Contains(t, w.Body.String(), `"status":"unknown"`)
	var blocked bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM shared_space_commands WHERE task_id='task' AND state<>'applied')`).Scan(&blocked))
	require.False(t, blocked)
	w, _ = call("editor", "POST", "after-unknown", "", "")
	require.Equal(t, 201, w.Code)
	w, _ = call("viewer", "GET", "", "", "")
	require.Equal(t, 200, w.Code)
	require.NotContains(t, w.Body.String(), "secret")
	require.Contains(t, w.Body.String(), `"uploadAttachment":false`)
	w, _ = call("viewer", "GET", "", "attachment", "")
	require.Equal(t, payload, w.Body.Bytes())
	w, _ = call("viewer", "GET", "", "attachment", "bytes=0-2")
	require.Equal(t, 206, w.Code)
	require.Equal(t, payload[:3], w.Body.Bytes())
	w, _ = call("viewer", "GET", "", "attachment", "bytes=999-1000")
	require.Equal(t, 416, w.Code)
	w, _ = call("viewer", "GET", "", "wrong", "")
	require.Equal(t, 404, w.Code)
	conversation = true
	w, _ = call("editor", "GET", "", "", "")
	require.Contains(t, w.Body.String(), `"uploadAttachment":false`)
	w, _ = call("editor", "POST", "conversation", "", "")
	require.Equal(t, 403, w.Code)
	require.True(t, strings.Contains(w.Body.String(), "会话任务"))
}
