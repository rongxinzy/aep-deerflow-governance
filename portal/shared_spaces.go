package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Paperclip remains the only task/document store. These tables contain access
// grants, immutable revision references, review decisions and delivery receipts.
const sharedSpaceSchema = `
CREATE TABLE IF NOT EXISTS shared_space_bootstrap_commands (
 deployment_id TEXT NOT NULL, request_id TEXT NOT NULL, actor_id TEXT NOT NULL, payload_hash TEXT NOT NULL, receipt JSONB NOT NULL,
 PRIMARY KEY(deployment_id,request_id)
);
CREATE TABLE IF NOT EXISTS shared_spaces (
 id TEXT PRIMARY KEY, deployment_id TEXT NOT NULL, company_id TEXT NOT NULL,
 project_id TEXT NOT NULL UNIQUE, name TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS shared_space_members (
 space_id TEXT NOT NULL REFERENCES shared_spaces(id), user_id TEXT NOT NULL,
 role TEXT NOT NULL CHECK(role IN ('owner','editor','viewer','revoked')), version INTEGER NOT NULL,
 PRIMARY KEY(space_id,user_id)
);
CREATE TABLE IF NOT EXISTS shared_space_commands (
 space_id TEXT NOT NULL REFERENCES shared_spaces(id), request_id TEXT NOT NULL, task_id TEXT NOT NULL,
 actor_id TEXT NOT NULL, action TEXT NOT NULL, payload_hash TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('sending','unknown','applied')), expected JSONB NOT NULL DEFAULT '{}', receipt JSONB,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), PRIMARY KEY(space_id,request_id)
);
CREATE INDEX IF NOT EXISTS shared_space_command_task ON shared_space_commands(space_id,task_id);
CREATE TABLE IF NOT EXISTS shared_space_results (
 id TEXT PRIMARY KEY, space_id TEXT NOT NULL REFERENCES shared_spaces(id), task_id TEXT NOT NULL,
 version INTEGER NOT NULL, title TEXT NOT NULL, document_key TEXT NOT NULL, document_id TEXT NOT NULL, revision_id TEXT NOT NULL,
 body_hash TEXT NOT NULL, submitted_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(space_id,task_id,version)
);
CREATE TABLE IF NOT EXISTS shared_space_reviews (
 id TEXT PRIMARY KEY, result_id TEXT NOT NULL REFERENCES shared_space_results(id),
 decision TEXT NOT NULL CHECK(decision IN ('request_changes','accept')), reason TEXT NOT NULL,
 actor_id TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), UNIQUE(result_id)
);
`

type sharedSpace struct {
	ID           string `json:"id"`
	DeploymentID string `json:"-"`
	CompanyID    string `json:"companyId"`
	ProjectID    string `json:"projectId"`
	Name         string `json:"name"`
	Role         string `json:"role"`
}
type sharedMember struct {
	UserID  string `json:"userId"`
	Role    string `json:"role"`
	Version int    `json:"version"`
}
type sharedResult struct {
	ID          string    `json:"id"`
	TaskID      string    `json:"taskId"`
	Version     int       `json:"version"`
	Title       string    `json:"title"`
	DocumentKey string    `json:"documentKey"`
	RevisionID  string    `json:"revisionId"`
	BodyHash    string    `json:"-"`
	SubmittedBy string    `json:"submittedBy"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"createdAt"`
	DocumentID  string    `json:"documentId"`
}
type sharedReview struct {
	ResultID  string    `json:"resultId"`
	Decision  string    `json:"decision"`
	Reason    string    `json:"reason"`
	ActorID   string    `json:"actorId"`
	CreatedAt time.Time `json:"createdAt"`
}
type sharedCommand struct {
	RequestID string `json:"requestId"`
	State     string `json:"state"`
	Action    string `json:"action"`
}
type sharedError struct {
	code    int
	message string
}

func (e *sharedError) Error() string            { return e.message }
func sharedFail(code int, message string) error { return &sharedError{code, message} }
func sharedWriteError(w http.ResponseWriter, err error) {
	var e *sharedError
	if errors.As(err, &e) {
		code := "request_rejected"
		switch e.code {
		case 423:
			code = "command_unknown"
		case 409:
			code = "conflict"
			if strings.Contains(e.message, "版本已变化") || strings.Contains(e.message, "成员已变化") {
				code = "stale_version"
			}
			if strings.Contains(e.message, "请求编号") {
				code = "idempotency_conflict"
			}
		}
		writeJSON(w, e.code, map[string]string{"error": e.message, "code": code})
		return
	}
	writeJSON(w, 503, map[string]string{"error": "协作空间暂不可用"})
}
func sharedHash(v any) string { b, _ := json.Marshal(v); return fmt.Sprintf("%x", sha256.Sum256(b)) }
func sharedDecode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return sharedFail(400, "请求内容不正确")
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return sharedFail(400, "请求内容不正确")
	}
	return nil
}
func sharedCaps(role string) map[string]bool {
	return map[string]bool{"createTask": role == "owner" || role == "editor", "manageMembers": role == "owner", "submit": role == "owner" || role == "editor", "review": role == "owner"}
}

func (p *portal) registerSharedSpaces(mux *http.ServeMux) {
	wrap := func(fn func(http.ResponseWriter, *http.Request, identity) error) http.HandlerFunc {
		return p.auth(func(w http.ResponseWriter, r *http.Request, i identity) {
			w.Header().Set("Cache-Control", "no-store")
			if p.db == nil || p.cfg.paperclipBaseURL == "" || p.cfg.paperclipToken == "" {
				sharedWriteError(w, sharedFail(503, "协作空间尚未配置"))
				return
			}
			if p.aep == nil || i.Token == "" {
				sharedWriteError(w, sharedFail(401, "请重新登录"))
				return
			}
			fresh, err := p.aep.verify(r.Context(), i.Token)
			if err != nil || fresh.UserID != i.UserID || fresh.DeploymentID == "" || fresh.DeploymentID != p.cfg.aepDeploymentID {
				sharedWriteError(w, sharedFail(401, "当前企业身份无法确认"))
				return
			}
			if err = fn(w, r, fresh); err != nil {
				sharedWriteError(w, err)
			}
		})
	}
	mux.HandleFunc("GET /api/v1/shared-spaces", wrap(p.sharedList))
	mux.HandleFunc("POST /api/v1/shared-spaces", wrap(p.sharedBind))
	mux.HandleFunc("GET /api/v1/shared-spaces/{space}", wrap(p.sharedDetail))
	mux.HandleFunc("PUT /api/v1/shared-spaces/{space}/members/{user}", wrap(p.sharedSetMember))
	mux.HandleFunc("GET /api/v1/shared-spaces/{space}/tasks", wrap(p.sharedTasks))
	mux.HandleFunc("POST /api/v1/shared-spaces/{space}/tasks", wrap(p.sharedCreateTask))
	mux.HandleFunc("GET /api/v1/shared-spaces/{space}/task-commands/{request}", wrap(p.sharedTaskCommand))
	mux.HandleFunc("GET /api/v1/shared-spaces/{space}/tasks/{task}", wrap(p.sharedTaskDetail))
	mux.HandleFunc("GET /api/v1/shared-spaces/{space}/tasks/{task}/results/{result}", wrap(p.sharedResultBody))
	mux.HandleFunc("POST /api/v1/shared-spaces/{space}/tasks/{task}/results", wrap(p.sharedSubmit))
	mux.HandleFunc("POST /api/v1/shared-spaces/{space}/tasks/{task}/results/{result}/review", wrap(p.sharedReviewResult))
}

// Disable redirects: service credentials never follow an upstream Location.
var sharedHTTP = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (p *portal) sharedPC(ctx context.Context, method, path string, body any, out any) error {
	var b io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		b = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.cfg.paperclipBaseURL+path, b)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.paperclipToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := sharedHTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return sharedFail(502, "任务服务未确认操作")
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(out)
}
func (p *portal) sharedLoad(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string, i identity) (sharedSpace, error) {
	var s sharedSpace
	err := q.QueryRowContext(ctx, `SELECT s.id,s.deployment_id,s.company_id,s.project_id,s.name,m.role FROM shared_spaces s JOIN shared_space_members m ON m.space_id=s.id WHERE s.id=$1 AND s.deployment_id=$2 AND m.user_id=$3 AND m.role<>'revoked'`, id, i.DeploymentID, i.UserID).Scan(&s.ID, &s.DeploymentID, &s.CompanyID, &s.ProjectID, &s.Name, &s.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return s, sharedFail(404, "空间不存在或无权访问")
	}
	return s, err
}

// Serializes ACL changes and commands across replicas. The lock is held across
// the upstream call; commands are committed BEFORE dispatch, never in an open
// transaction that could roll back the evidence of an unknown side effect.
func (p *portal) sharedLock(ctx context.Context, id string) (*sql.Conn, func(), error) {
	c, err := p.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err = c.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, "shared-space:"+id); err != nil {
		// Cancellation can race with successful lock acquisition. A session
		// with an uncertain lock state must never return to the pool.
		sharedDiscardConnection(c)
		return nil, nil, err
	}
	release := func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		err := c.QueryRowContext(cleanup, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, "shared-space:"+id).Scan(&unlocked)
		if err != nil || !unlocked {
			sharedDiscardConnection(c)
			return
		}
		_ = c.Close()
	}
	return c, release, nil
}

// Conn.Raw routes driver.ErrBadConn through database/sql's release path,
// discarding the driver connection instead of returning it to the idle pool.
func sharedDiscardConnection(c *sql.Conn) {
	_ = c.Raw(func(any) error { return driver.ErrBadConn })
	_ = c.Close()
}

func (p *portal) sharedList(w http.ResponseWriter, r *http.Request, i identity) error {
	rows, err := p.db.QueryContext(r.Context(), `SELECT s.id,s.company_id,s.project_id,s.name,m.role FROM shared_spaces s JOIN shared_space_members m ON m.space_id=s.id WHERE s.deployment_id=$1 AND m.user_id=$2 AND m.role<>'revoked' ORDER BY s.created_at DESC`, i.DeploymentID, i.UserID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	out := []sharedSpace{}
	for rows.Next() {
		var s sharedSpace
		if err = rows.Scan(&s.ID, &s.CompanyID, &s.ProjectID, &s.Name, &s.Role); err != nil {
			return err
		}
		out = append(out, s)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"spaces": out})
	return nil
}

// Membership targets are resolved in the current AEP deployment directory.
func (p *portal) sharedUserExists(ctx context.Context, id string) error {
	user, found, err := newReferenceResolver(p, ctx).lookupUser(id)
	if err != nil {
		return sharedFail(503, "企业成员目录暂不可用")
	}
	if !found || user.ID != id || user.Status != "active" || user.Kind != "human" {
		return sharedFail(400, "成员必须是当前企业的有效人员")
	}
	return nil
}
func (p *portal) sharedBind(w http.ResponseWriter, r *http.Request, i identity) error {
	if !i.isAdmin(p.cfg) {
		return sharedFail(403, "仅平台管理员可绑定项目")
	}
	var b struct {
		CompanyID string `json:"companyId"`
		ProjectID string `json:"projectId"`
		OwnerID   string `json:"ownerId"`
		RequestID string `json:"requestId"`
	}
	if err := sharedDecode(w, r, &b); err != nil {
		return err
	}
	if b.CompanyID == "" || b.ProjectID == "" || b.OwnerID == "" || b.RequestID == "" {
		return sharedFail(400, "请提供项目和负责人")
	}
	if err := p.sharedUserExists(r.Context(), b.OwnerID); err != nil {
		return err
	}
	var project struct {
		ID        string `json:"id"`
		CompanyID string `json:"companyId"`
		Name      string `json:"name"`
	}
	if err := p.sharedPC(r.Context(), "GET", "/projects/"+url.PathEscape(b.ProjectID), nil, &project); err != nil {
		return err
	}
	if project.ID != b.ProjectID || project.CompanyID != b.CompanyID {
		return sharedFail(400, "项目不属于指定企业")
	}
	id := collaborationID("shared-space", i.DeploymentID, b.ProjectID)
	c, release, err := p.sharedLock(r.Context(), "bootstrap:"+i.DeploymentID)
	if err != nil {
		return err
	}
	defer release()
	tx, err := c.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var oldActor, oldHash string
	var oldReceipt []byte
	err = tx.QueryRowContext(r.Context(), `SELECT actor_id,payload_hash,receipt FROM shared_space_bootstrap_commands WHERE deployment_id=$1 AND request_id=$2`, i.DeploymentID, b.RequestID).Scan(&oldActor, &oldHash, &oldReceipt)
	if err == nil {
		if oldActor != i.UserID || oldHash != sharedHash(b) {
			return sharedFail(409, "请求编号已用于其他操作")
		}
		writeJSON(w, 200, json.RawMessage(oldReceipt))
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var exists bool
	if err = tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM shared_spaces WHERE project_id=$1)`, b.ProjectID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return sharedFail(409, "项目已绑定协作空间")
	}
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO shared_spaces(id,deployment_id,company_id,project_id,name) VALUES($1,$2,$3,$4,$5)`, id, i.DeploymentID, b.CompanyID, b.ProjectID, project.Name); err != nil {
		return err
	}
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES($1,$2,'owner',1)`, id, b.OwnerID); err != nil {
		return err
	}
	receipt := map[string]any{"space": sharedSpace{ID: id, CompanyID: b.CompanyID, ProjectID: b.ProjectID, Name: project.Name}}
	raw, _ := json.Marshal(receipt)
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO shared_space_bootstrap_commands(deployment_id,request_id,actor_id,payload_hash,receipt) VALUES($1,$2,$3,$4,$5)`, i.DeploymentID, b.RequestID, i.UserID, sharedHash(b), string(raw)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	writeJSON(w, 201, map[string]any{"space": sharedSpace{ID: id, CompanyID: b.CompanyID, ProjectID: b.ProjectID, Name: project.Name}})
	return nil
}
func (p *portal) sharedDetail(w http.ResponseWriter, r *http.Request, i identity) error {
	s, err := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	rows, err := p.db.QueryContext(r.Context(), `SELECT user_id,role,version FROM shared_space_members WHERE space_id=$1 ORDER BY user_id`, s.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	members := []sharedMember{}
	for rows.Next() {
		var m sharedMember
		if err = rows.Scan(&m.UserID, &m.Role, &m.Version); err != nil {
			return err
		}
		members = append(members, m)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"space": s, "members": members, "capabilities": sharedCaps(s.Role)})
	return nil
}
func (p *portal) sharedSetMember(w http.ResponseWriter, r *http.Request, i identity) error {
	var b struct {
		Role            string `json:"role"`
		ExpectedVersion int    `json:"expectedVersion"`
		RequestID       string `json:"requestId"`
	}
	if err := sharedDecode(w, r, &b); err != nil {
		return err
	}
	if b.Role != "owner" && b.Role != "editor" && b.Role != "viewer" && b.Role != "revoked" {
		return sharedFail(400, "成员角色不正确")
	}
	if b.RequestID == "" {
		return sharedFail(400, "缺少请求编号")
	}
	c, release, err := p.sharedLock(r.Context(), r.PathValue("space"))
	if err != nil {
		return err
	}
	defer release()
	s, err := p.sharedLoad(r.Context(), c, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	if s.Role != "owner" {
		return sharedFail(403, "仅空间负责人可管理成员")
	}
	user := r.PathValue("user")
	if b.Role != "revoked" {
		if err = p.sharedUserExists(r.Context(), user); err != nil {
			return err
		}
	}
	hash := sharedHash([]any{user, b})
	if receipt, ok, err := sharedReplay(r.Context(), c, s.ID, b.RequestID, i.UserID, "member", hash); err != nil {
		return err
	} else if ok {
		writeJSON(w, 200, receipt)
		return nil
	}
	tx, err := c.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var role string
	var version int
	err = tx.QueryRowContext(r.Context(), `SELECT role,version FROM shared_space_members WHERE space_id=$1 AND user_id=$2`, s.ID, user).Scan(&role, &version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if role == "owner" && b.Role != "owner" {
		var owners int
		if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FROM shared_space_members WHERE space_id=$1 AND role='owner'`, s.ID).Scan(&owners); err != nil {
			return err
		}
		if owners < 2 {
			return sharedFail(409, "不能移除或降级最后一位负责人")
		}
	}
	if version != b.ExpectedVersion {
		return sharedFail(409, "成员已变化，请刷新")
	}
	member := sharedMember{user, b.Role, version + 1}
	receipt := map[string]any{"member": member}
	raw, _ := json.Marshal(receipt)
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES($1,$2,$3,$4) ON CONFLICT(space_id,user_id) DO UPDATE SET role=excluded.role,version=excluded.version`, s.ID, user, b.Role, version+1); err != nil {
		return err
	}
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO shared_space_commands(space_id,request_id,task_id,actor_id,action,payload_hash,state,receipt) VALUES($1,$2,'',$3,'member',$4,'applied',$5)`, s.ID, b.RequestID, i.UserID, hash, string(raw)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	writeJSON(w, 200, receipt)
	return nil
}
func (p *portal) sharedCheckTask(ctx context.Context, s sharedSpace, id string) (map[string]any, error) {
	var task map[string]any
	if err := p.sharedPC(ctx, "GET", "/issues/"+url.PathEscape(id), nil, &task); err != nil {
		return nil, err
	}
	if task["companyId"] != s.CompanyID || task["projectId"] != s.ProjectID || task["id"] != id {
		return nil, sharedFail(404, "任务不属于当前空间")
	}
	return task, nil
}
func (p *portal) sharedTasks(w http.ResponseWriter, r *http.Request, i identity) error {
	s, err := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	limit, offset := 30, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 1 || n > 100 {
			return sharedFail(400, "分页大小应为1至100")
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 0 {
			return sharedFail(400, "分页位置不正确")
		}
		offset = n
	}
	query := url.Values{"projectId": {s.ProjectID}, "limit": {strconv.Itoa(limit + 1)}, "offset": {strconv.Itoa(offset)}}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		query.Set("q", q)
	}
	tasks := []map[string]any{}
	if err = p.sharedPC(r.Context(), "GET", "/companies/"+url.PathEscape(s.CompanyID)+"/issues?"+query.Encode(), nil, &tasks); err != nil {
		return err
	}
	// Fail closed if a mismatched upstream ignores project filters; never return
	// a post-filtered page with hidden task counts or cross-project records.
	for _, t := range tasks {
		if t["companyId"] != s.CompanyID || t["projectId"] != s.ProjectID {
			return sharedFail(502, "任务服务返回了错误的空间范围")
		}
	}
	hasMore := len(tasks) > limit
	if hasMore {
		tasks = tasks[:limit]
	}
	items := sharedTaskListDTO(tasks)
	rows, err := p.db.QueryContext(r.Context(), `SELECT DISTINCT ON(r.task_id) r.task_id,CASE WHEN v.decision='accept' THEN 'accepted' WHEN v.decision='request_changes' THEN 'changes_requested' ELSE 'submitted' END FROM shared_space_results r LEFT JOIN shared_space_reviews v ON v.result_id=r.id WHERE r.space_id=$1 ORDER BY r.task_id,r.version DESC`, s.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	states := map[string]string{}
	for rows.Next() {
		var task, state string
		if err = rows.Scan(&task, &state); err != nil {
			return err
		}
		states[task] = state
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, item := range items {
		if state := states[fmt.Sprint(item["id"])]; state != "" {
			item["reviewState"] = state
		}
	}
	writeJSON(w, 200, map[string]any{"tasks": items, "hasMore": hasMore, "nextOffset": offset + len(items)})
	return nil
}

const sharedResultsSelect = `SELECT r.id,r.task_id,r.version,r.title,r.document_key,r.document_id,r.revision_id,r.body_hash,r.submitted_by,CASE WHEN v.decision='accept' THEN 'accepted' WHEN v.decision='request_changes' THEN 'changes_requested' ELSE 'submitted' END,r.created_at FROM shared_space_results r LEFT JOIN shared_space_reviews v ON v.result_id=r.id WHERE r.space_id=$1 AND r.task_id=$2 ORDER BY r.version`

func sharedResults(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, space, task string) ([]sharedResult, error) {
	rows, err := q.QueryContext(ctx, sharedResultsSelect, space, task)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []sharedResult{}
	for rows.Next() {
		var x sharedResult
		if err = rows.Scan(&x.ID, &x.TaskID, &x.Version, &x.Title, &x.DocumentKey, &x.DocumentID, &x.RevisionID, &x.BodyHash, &x.SubmittedBy, &x.State, &x.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
func (p *portal) sharedTaskDetail(w http.ResponseWriter, r *http.Request, i identity) error {
	s, err := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	task, err := p.sharedCheckTask(r.Context(), s, r.PathValue("task"))
	if err != nil {
		return err
	}
	results, err := sharedResults(r.Context(), p.db, s.ID, r.PathValue("task"))
	if err != nil {
		return err
	}
	reviews := []sharedReview{}
	commands := []sharedCommand{}
	rows, err := p.db.QueryContext(r.Context(), `SELECT v.result_id,v.decision,v.reason,v.actor_id,v.created_at FROM shared_space_reviews v JOIN shared_space_results r ON r.id=v.result_id WHERE r.space_id=$1 AND r.task_id=$2 ORDER BY v.created_at`, s.ID, r.PathValue("task"))
	if err != nil {
		return err
	}
	for rows.Next() {
		var v sharedReview
		if err = rows.Scan(&v.ResultID, &v.Decision, &v.Reason, &v.ActorID, &v.CreatedAt); err != nil {
			_ = rows.Close()
			return err
		}
		reviews = append(reviews, v)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	rows, err = p.db.QueryContext(r.Context(), `SELECT request_id,state,action FROM shared_space_commands WHERE space_id=$1 AND task_id=$2 ORDER BY created_at`, s.ID, r.PathValue("task"))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	blocked := false
	for rows.Next() {
		var c sharedCommand
		if err = rows.Scan(&c.RequestID, &c.State, &c.Action); err != nil {
			return err
		}
		commands = append(commands, c)
		if c.State != "applied" {
			blocked = true
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	caps := sharedCaps(s.Role)
	caps["review"] = false
	if len(results) > 0 {
		latest := results[len(results)-1]
		caps["review"] = s.Role == "owner" && latest.State == "submitted" && latest.SubmittedBy != i.UserID
		caps["submit"] = caps["submit"] && latest.State == "changes_requested"
	}
	if blocked || task["status"] == "done" || task["status"] == "cancelled" {
		caps["review"] = false
		caps["submit"] = false
	}
	writeJSON(w, 200, map[string]any{"task": sharedTaskDTO(task), "results": results, "reviews": reviews, "commands": commands, "capabilities": caps})
	return nil
}
func (p *portal) sharedResultBody(w http.ResponseWriter, r *http.Request, i identity) error {
	s, err := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	if _, err = p.sharedCheckTask(r.Context(), s, r.PathValue("task")); err != nil {
		return err
	}
	results, err := sharedResults(r.Context(), p.db, s.ID, r.PathValue("task"))
	if err != nil {
		return err
	}
	for _, x := range results {
		if x.ID == r.PathValue("result") {
			var revisions []struct {
				ID         string `json:"id"`
				DocumentID string `json:"documentId"`
				IssueID    string `json:"issueId"`
				CompanyID  string `json:"companyId"`
				Body       string `json:"body"`
			}
			if err = p.sharedPC(r.Context(), "GET", "/issues/"+url.PathEscape(x.TaskID)+"/documents/"+url.PathEscape(x.DocumentKey)+"/revisions", nil, &revisions); err != nil {
				return err
			}
			found := false
			body := ""
			for _, revision := range revisions {
				if revision.ID == x.RevisionID && revision.DocumentID == x.DocumentID && revision.IssueID == x.TaskID && revision.CompanyID == s.CompanyID && sharedHash(revision.Body) == x.BodyHash {
					body = revision.Body
					found = true
					break
				}
			}
			if !found {
				return sharedFail(409, "成果原始版本无法核验")
			}

			writeJSON(w, 200, map[string]any{"result": x, "body": body})
			return nil
		}
	}
	return sharedFail(404, "成果不存在")
}
func sharedReplay(ctx context.Context, c *sql.Conn, space, request, actor, action, hash string) (json.RawMessage, bool, error) {
	var priorActor, priorAction, priorHash, state string
	var raw []byte
	err := c.QueryRowContext(ctx, `SELECT actor_id,action,payload_hash,state,receipt FROM shared_space_commands WHERE space_id=$1 AND request_id=$2`, space, request).Scan(&priorActor, &priorAction, &priorHash, &state, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if priorActor != actor || priorAction != action || priorHash != hash {
		return nil, false, sharedFail(409, "请求编号已用于其他操作")
	}
	if state != "applied" {
		return nil, false, sharedFail(423, "上次操作结果待核验，请勿重复提交")
	}
	return json.RawMessage(raw), true, nil
}
func sharedStart(ctx context.Context, c *sql.Conn, space, task, request, actor, action, hash string, expected any) error {
	var blocked bool
	if err := c.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM shared_space_commands WHERE space_id=$1 AND task_id=$2 AND state<>'applied')`, space, task).Scan(&blocked); err != nil {
		return err
	}
	if blocked {
		return sharedFail(423, "任务存在待核验操作")
	}
	raw, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, `INSERT INTO shared_space_commands(space_id,request_id,task_id,actor_id,action,payload_hash,state,expected) VALUES($1,$2,$3,$4,$5,$6,'sending',$7)`, space, request, task, actor, action, hash, string(raw))
	return err
}
func sharedUnknown(c *sql.Conn, space, request string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.ExecContext(ctx, `UPDATE shared_space_commands SET state='unknown' WHERE space_id=$1 AND request_id=$2 AND state='sending'`, space, request)
	return err
}
func sharedFinish(ctx context.Context, tx *sql.Tx, space, request string, receipt any) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE shared_space_commands SET state='applied',receipt=$3 WHERE space_id=$1 AND request_id=$2`, space, request, string(raw))
	return err
}

type sharedSubmission struct {
	RequestID       string `json:"requestId"`
	ExpectedVersion int    `json:"expectedVersion"`
	Title           string `json:"title"`
	Body            string `json:"body"`
}

func (p *portal) sharedSubmit(w http.ResponseWriter, r *http.Request, i identity) error {
	var b sharedSubmission
	if err := sharedDecode(w, r, &b); err != nil {
		return err
	}
	status, receipt, err := p.sharedSubmitInput(r.Context(), r.PathValue("space"), r.PathValue("task"), i, b)
	if err == nil {
		writeJSON(w, status, receipt)
	}
	return err
}
func (p *portal) sharedSubmitInput(ctx context.Context, spaceID, taskID string, i identity, b sharedSubmission, guards ...func(context.Context, *sql.Conn) error) (int, any, error) {
	if b.RequestID == "" || strings.TrimSpace(b.Title) == "" || strings.TrimSpace(b.Body) == "" {
		return 0, nil, sharedFail(400, "请填写成果标题和内容")
	}
	c, release, err := p.sharedLock(ctx, spaceID)
	if err != nil {
		return 0, nil, err
	}
	defer release()
	for _, guard := range guards {
		if err := guard(ctx, c); err != nil {
			return 0, nil, err
		}
	}
	s, err := p.sharedLoad(ctx, c, spaceID, i)
	if err != nil {
		return 0, nil, err
	}
	if !sharedCaps(s.Role)["submit"] {
		return 0, nil, sharedFail(403, "当前成员只能查看")
	}
	task, err := p.sharedCheckTask(ctx, s, taskID)
	if err != nil {
		return 0, nil, err
	}
	hash := sharedHash([]any{taskID, b})
	if receipt, ok, err := sharedReplay(ctx, c, s.ID, b.RequestID, i.UserID, "submit", hash); err != nil {
		return 0, nil, err
	} else if ok {
		return 200, receipt, nil
	}
	if task["status"] == "done" || task["status"] == "cancelled" {
		return 0, nil, sharedFail(409, "任务已结束，不能修改成果")
	}
	results, err := sharedResults(ctx, c, s.ID, taskID)
	if err != nil {
		return 0, nil, err
	}
	if len(results) != b.ExpectedVersion {
		return 0, nil, sharedFail(409, "成果版本已变化，请刷新")
	}
	if len(results) > 0 && results[len(results)-1].State != "changes_requested" {
		return 0, nil, sharedFail(409, "请先完成当前成果审阅")
	}
	baseRevision := ""
	if len(results) > 0 {
		previous := results[len(results)-1]
		var current sharedDocument
		if err = p.sharedPC(ctx, "GET", "/issues/"+url.PathEscape(taskID)+"/documents/"+previous.DocumentKey, nil, &current); err != nil {
			return 0, nil, err
		}
		if current.ID != previous.DocumentID || current.LatestRevisionID != previous.RevisionID || sharedHash(current.Body) != previous.BodyHash {
			return 0, nil, sharedFail(409, "成果版本发生变化，请先核验")
		}
		baseRevision = previous.RevisionID
	}
	if err = sharedStart(ctx, c, s.ID, taskID, b.RequestID, i.UserID, "submit", hash, map[string]any{"method": "PUT", "path": "/issues/" + url.PathEscape(taskID) + "/documents/shared-space-result", "title": b.Title, "bodyHash": sharedHash(b.Body), "baseRevisionId": baseRevision, "resultId": collaborationID(s.ID, b.RequestID), "version": len(results) + 1, "submittedBy": i.UserID}); err != nil {
		return 0, nil, err
	}
	// The document retains Paperclip native revisions. CAS fences concurrent
	// draft edits; a lost acknowledgement blocks and is never blindly retried.
	result := sharedResult{ID: collaborationID(s.ID, b.RequestID), TaskID: taskID, Version: len(results) + 1, Title: b.Title, DocumentKey: "shared-space-result", SubmittedBy: i.UserID, State: "submitted", CreatedAt: time.Now().UTC(), BodyHash: sharedHash(b.Body)}
	var doc sharedDocument
	documentRequest := map[string]any{"title": b.Title, "body": b.Body, "format": "markdown", "changeSummary": "Shared space submission " + result.ID}
	if baseRevision != "" {
		documentRequest["baseRevisionId"] = baseRevision
	}
	err = p.sharedPC(ctx, "PUT", "/issues/"+url.PathEscape(taskID)+"/documents/"+result.DocumentKey, documentRequest, &doc)
	if err != nil || doc.ID == "" || doc.LatestRevisionID == "" || doc.Body != b.Body || (len(results) > 0 && doc.ID != results[len(results)-1].DocumentID) {
		_ = sharedUnknown(c, s.ID, b.RequestID)
		return 0, nil, sharedFail(423, "成果写入结果待核验，请勿重试")
	}
	result.RevisionID = doc.LatestRevisionID
	result.DocumentID = doc.ID
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO shared_space_results(id,space_id,task_id,version,title,document_key,document_id,revision_id,body_hash,submitted_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, result.ID, s.ID, taskID, result.Version, result.Title, result.DocumentKey, result.DocumentID, result.RevisionID, result.BodyHash, i.UserID, result.CreatedAt); err != nil {
		return 0, nil, err
	}
	if len(guards) > 0 {
		if err = p.sharedRuntimeEnqueue(ctx, tx, s.ID, taskID, result, "submitted", ""); err != nil {
			return 0, nil, err
		}
	}
	receipt := map[string]any{"result": result}
	if err = sharedFinish(ctx, tx, s.ID, b.RequestID, receipt); err != nil {
		return 0, nil, err
	}
	if err = tx.Commit(); err != nil {
		return 0, nil, err
	}
	return 201, receipt, nil
}
func (p *portal) sharedReviewResult(w http.ResponseWriter, r *http.Request, i identity) error {
	var b struct {
		RequestID       string `json:"requestId"`
		ExpectedVersion int    `json:"expectedVersion"`
		Decision        string `json:"decision"`
		Reason          string `json:"reason"`
	}
	if err := sharedDecode(w, r, &b); err != nil {
		return err
	}
	if b.RequestID == "" || (b.Decision != "accept" && b.Decision != "request_changes") {
		return sharedFail(400, "审阅操作不正确")
	}
	if b.Decision == "request_changes" && strings.TrimSpace(b.Reason) == "" {
		return sharedFail(400, "请说明需要修改的内容")
	}
	c, release, err := p.sharedLock(r.Context(), r.PathValue("space"))
	if err != nil {
		return err
	}
	defer release()
	s, err := p.sharedLoad(r.Context(), c, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	if s.Role != "owner" {
		return sharedFail(403, "仅空间负责人可审阅")
	}
	taskID := r.PathValue("task")
	task, err := p.sharedCheckTask(r.Context(), s, taskID)
	if err != nil {
		return err
	}
	hash := sharedHash([]any{taskID, r.PathValue("result"), b})
	if receipt, ok, err := sharedReplay(r.Context(), c, s.ID, b.RequestID, i.UserID, "review", hash); err != nil {
		return err
	} else if ok {
		writeJSON(w, 200, receipt)
		return nil
	}
	if task["status"] == "done" || task["status"] == "cancelled" {
		return sharedFail(409, "任务已结束，不能修改成果")
	}
	results, err := sharedResults(r.Context(), c, s.ID, taskID)
	if err != nil {
		return err
	}
	if len(results) == 0 || len(results) != b.ExpectedVersion {
		return sharedFail(409, "成果版本已变化，请刷新")
	}
	result := results[len(results)-1]
	if result.ID != r.PathValue("result") || result.State != "submitted" {
		return sharedFail(409, "只能审阅最新待审成果")
	}
	if result.SubmittedBy == i.UserID {
		return sharedFail(403, "请由其他负责人审阅成果")
	}
	var doc sharedDocument
	if err = p.sharedPC(r.Context(), "GET", "/issues/"+url.PathEscape(taskID)+"/documents/"+url.PathEscape(result.DocumentKey), nil, &doc); err != nil {
		return err
	}
	if doc.ID != result.DocumentID || doc.LatestRevisionID != result.RevisionID || sharedHash(doc.Body) != result.BodyHash {
		return sharedFail(409, "成果版本发生变化，不能审阅")
	}
	// Paperclip forbids in_progress without a human or Agent assignee.
	// Returning work does not authorize fabricating an assignment.
	targetStatus := "todo"
	agentID, _ := task["assigneeAgentId"].(string)
	userID, _ := task["assigneeUserId"].(string)
	if agentID != "" || userID != "" {
		targetStatus = "in_progress"
	}
	if b.Decision == "accept" {
		targetStatus = "done"
	}
	if err = sharedStart(r.Context(), c, s.ID, taskID, b.RequestID, i.UserID, "review", hash, map[string]any{"method": "PATCH", "path": "/issues/" + url.PathEscape(taskID), "resultId": result.ID, "revisionId": result.RevisionID, "decision": b.Decision, "reason": b.Reason, "actorId": i.UserID, "expectedTaskStatus": task["status"], "targetStatus": targetStatus}); err != nil {
		return err
	}
	var updated map[string]any
	err = p.sharedPC(r.Context(), "PATCH", "/issues/"+url.PathEscape(taskID), map[string]any{"status": targetStatus}, &updated)
	if err != nil || updated["id"] != taskID || updated["status"] != targetStatus || updated["projectId"] != s.ProjectID || updated["companyId"] != s.CompanyID {
		_ = sharedUnknown(c, s.ID, b.RequestID)
		return sharedFail(423, "审阅写入结果待核验，请勿重试")
	}
	tx, err := c.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO shared_space_reviews(id,result_id,decision,reason,actor_id) VALUES($1,$2,$3,$4,$5)`, collaborationID(s.ID, b.RequestID), result.ID, b.Decision, b.Reason, i.UserID); err != nil {
		return err
	}
	result.State = "changes_requested"
	if b.Decision == "accept" {
		result.State = "accepted"
	}
	if err = p.sharedRuntimeEnqueue(r.Context(), tx, s.ID, taskID, result, b.Decision, b.Reason); err != nil {
		return err
	}
	receipt := map[string]any{"result": result}
	if err = sharedFinish(r.Context(), tx, s.ID, b.RequestID, receipt); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	writeJSON(w, 200, receipt)
	return nil
}

// Deliberate browser DTO: upstream execution configuration and private context
// never leave the service, even when Paperclip adds new response fields.
func sharedTaskDTO(task map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"id", "identifier", "title", "description", "status", "priority", "assigneeAgentId", "assigneeUserId", "createdAt", "updatedAt"} {
		if value, ok := task[key]; ok {
			out[key] = value
		}
	}
	return out
}
func sharedTaskListDTO(tasks []map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, task := range tasks {
		out = append(out, sharedTaskDTO(task))
	}
	return out
}

type sharedDocument struct {
	ID               string `json:"id"`
	LatestRevisionID string `json:"latestRevisionId"`
	Body             string `json:"body"`
}
