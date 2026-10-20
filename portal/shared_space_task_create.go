package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

type sharedTaskCreation struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Priority    string `json:"priority"`
	RequestID   string `json:"requestId"`
	ParentID    string `json:"parentId,omitempty"`
}

func (b *sharedTaskCreation) validate() error {
	b.Title = strings.TrimSpace(b.Title)
	if b.Priority == "" {
		b.Priority = "medium"
	}
	if b.Title == "" || utf8.RuneCountInString(b.Title) > 240 || utf8.RuneCountInString(b.Description) > 24000 || strings.TrimSpace(b.RequestID) == "" || len(b.RequestID) > 200 {
		return sharedFail(400, "请填写有效的任务标题和请求编号")
	}
	switch b.Priority {
	case "critical", "high", "medium", "low":
	default:
		return sharedFail(400, "任务优先级不正确")
	}
	return nil
}

// A read of local evidence only. No verified upstream lookup by idempotency key
// is available; an absent receipt never licenses another create request.
func sharedCreationReceipt(ctx context.Context, c *sql.Conn, space, request, actor, hash string) (map[string]any, bool, error) {
	var priorActor, action, priorHash, state string
	var raw []byte
	err := c.QueryRowContext(ctx, `SELECT actor_id,action,payload_hash,state,receipt FROM shared_space_commands WHERE space_id=$1 AND request_id=$2`, space, request).Scan(&priorActor, &action, &priorHash, &state, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if priorActor != actor || action != "create_task" || (hash != "" && priorHash != hash) {
		return nil, false, sharedFail(409, "请求编号已用于其他操作")
	}
	if state == "applied" {
		var receipt map[string]any
		if err = json.Unmarshal(raw, &receipt); err != nil {
			return nil, false, err
		}
		return receipt, true, nil
	}
	return map[string]any{"requestId": request, "state": "unknown"}, true, nil
}

func (p *portal) sharedCreateTask(w http.ResponseWriter, r *http.Request, i identity) error {
	var b sharedTaskCreation
	if err := sharedDecode(w, r, &b); err != nil {
		return err
	}
	if err := b.validate(); err != nil {
		return err
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
	if !sharedCaps(s.Role)["createTask"] {
		return sharedFail(403, "当前成员只能查看")
	}
	hash := sharedHash(b)
	if receipt, ok, err := sharedCreationReceipt(r.Context(), c, s.ID, b.RequestID, i.UserID, hash); err != nil {
		return err
	} else if ok {
		status := 200
		if receipt["state"] != "applied" {
			status = 202
		}
		writeJSON(w, status, receipt)
		return nil
	}
	if b.ParentID != "" {
		if _, err = p.sharedCheckTask(r.Context(), s, b.ParentID); err != nil {
			return err
		}
	}
	// The per-request task placeholder prevents an uncertain create from fencing
	// unrelated new tasks. The space lock still serializes ACL changes.
	key := collaborationID("shared-task-create", s.ID, b.RequestID)
	body := map[string]any{"title": b.Title, "description": b.Description, "priority": b.Priority, "projectId": s.ProjectID, "status": "backlog", "idempotencyKey": key, "allowDuplicate": true}
	if b.ParentID != "" {
		body["parentId"] = b.ParentID
	}
	if err = sharedStart(r.Context(), c, s.ID, "create:"+key, b.RequestID, i.UserID, "create_task", hash, map[string]any{"method": "POST", "path": "/companies/" + url.PathEscape(s.CompanyID) + "/issues", "body": body}); err != nil {
		return err
	}
	unknown := func() {
		_ = sharedUnknown(c, s.ID, b.RequestID)
		writeJSON(w, 202, map[string]any{"requestId": b.RequestID, "state": "unknown"})
	}
	var task map[string]any
	err = p.sharedPC(r.Context(), "POST", "/companies/"+url.PathEscape(s.CompanyID)+"/issues", body, &task)
	id, _ := task["id"].(string)
	if err != nil || id == "" || task["companyId"] != s.CompanyID || task["projectId"] != s.ProjectID || task["status"] != "backlog" || task["title"] != b.Title || !sharedCreatedDescriptionMatches(task["description"], b.Description) || !sharedCreatedDescriptionMatches(task["parentId"], b.ParentID) || task["priority"] != b.Priority || (task["assigneeUserId"] != nil && task["assigneeUserId"] != "") || (task["assigneeAgentId"] != nil && task["assigneeAgentId"] != "") {
		unknown()
		return nil
	}
	receipt := map[string]any{"requestId": b.RequestID, "state": "applied", "task": sharedTaskDTO(task)}
	tx, err := c.BeginTx(r.Context(), nil)
	if err != nil {
		unknown()
		return nil
	}
	defer func() { _ = tx.Rollback() }()
	if err = sharedFinish(r.Context(), tx, s.ID, b.RequestID, receipt); err != nil {
		_ = tx.Rollback()
		unknown()
		return nil
	}
	if err = tx.Commit(); err != nil {
		unknown()
		return nil
	}
	writeJSON(w, 201, receipt)
	return nil
}

func (p *portal) sharedTaskCommand(w http.ResponseWriter, r *http.Request, i identity) error {
	c, release, err := p.sharedLock(r.Context(), r.PathValue("space"))
	if err != nil {
		return err
	}
	defer release()
	s, err := p.sharedLoad(r.Context(), c, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	if !sharedCaps(s.Role)["createTask"] {
		return sharedFail(403, "当前成员只能查看")
	}
	if h, e := handoffScan(c.QueryRowContext(r.Context(), handoffSelect+` AND request_id=$2`, s.ID, r.PathValue("request"))); e == nil {
		if h.Actor != i.UserID {
			return sharedFail(403, "无权读取操作回执")
		}
		return p.handoffResponse(w, r.Context(), s, h, i)
	} else if e != sql.ErrNoRows {
		return e
	}
	receipt, ok, err := sharedCreationReceipt(r.Context(), c, s.ID, r.PathValue("request"), i.UserID, "")
	if err != nil {
		return err
	}
	if !ok {
		return sharedFail(404, "未找到创建回执，不能据此推断任务未创建")
	}
	writeJSON(w, 200, receipt)
	return nil
}

// Paperclip may represent an omitted description as null. Only an empty input
// accepts that representation; nonempty descriptions must match exactly.
func sharedCreatedDescriptionMatches(value any, expected string) bool {
	if value == nil {
		return expected == ""
	}
	actual, ok := value.(string)
	return ok && actual == expected
}
