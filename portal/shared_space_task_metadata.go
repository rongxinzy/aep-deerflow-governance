package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

type sharedMetadataInput struct {
	RequestID   string  `json:"requestId"`
	Token       string  `json:"expectedMetadataToken"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Priority    *string `json:"priority"`
}

func (in *sharedMetadataInput) validate() error {
	if in.Token == "" || in.Title == nil || in.Description == nil || in.Priority == nil || *in.Priority == "" {
		return sharedFail(400, "请提供基本信息版本与完整标题、描述和优先级")
	}
	b := sharedTaskCreation{Title: *in.Title, Description: *in.Description, Priority: *in.Priority, RequestID: in.RequestID}
	if err := b.validate(); err != nil {
		return err
	}
	*in.Title = b.Title
	return nil
}
func sharedMetadataToken(task map[string]any) string {
	return sharedHash([]any{task["id"], task["updatedAt"], task["title"], task["description"], task["priority"]})
}
func (in sharedMetadataInput) body() map[string]any {
	return map[string]any{"title": *in.Title, "description": *in.Description, "priority": *in.Priority}
}
func (in sharedMetadataInput) matches(task map[string]any) bool {
	return task["title"] == *in.Title && sharedCreatedDescriptionMatches(task["description"], *in.Description) && task["priority"] == *in.Priority
}
func (p *portal) sharedMetadataReceipt(ctx context.Context, c *sql.Conn, s sharedSpace, task, actor, hash string, in sharedMetadataInput) (json.RawMessage, bool, error) {
	var priorActor, action, priorHash, state string
	var raw []byte
	err := c.QueryRowContext(ctx, `SELECT actor_id,action,payload_hash,state,receipt FROM shared_space_commands WHERE space_id=$1 AND request_id=$2`, s.ID, in.RequestID).Scan(&priorActor, &action, &priorHash, &state, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if priorActor != actor || action != "task_metadata" || priorHash != hash {
		return nil, false, sharedFail(409, "请求编号已用于其他操作")
	}
	if state == "applied" {
		return raw, true, nil
	}
	// Persisted intent permits only GET reconciliation, never a second PATCH.
	current, err := p.sharedCheckTask(ctx, s, task)
	if err != nil {
		return nil, false, err
	}
	if !in.matches(current) {
		return nil, false, sharedFail(423, "基本信息修改结果待核验，请勿重复提交")
	}
	raw, err = sharedMetadataFinish(ctx, c, s.ID, in.RequestID, current)
	return raw, true, err
}
func sharedMetadataFinish(ctx context.Context, c *sql.Conn, space, request string, task map[string]any) (json.RawMessage, error) {
	receipt := map[string]any{"task": sharedTaskDTO(task), "metadataToken": sharedMetadataToken(task), "requestId": request, "state": "applied"}
	if err := sharedDependencyFinish(ctx, c, space, request, receipt); err != nil {
		return nil, err
	}
	return json.Marshal(receipt)
}
func (p *portal) sharedSetMetadata(w http.ResponseWriter, r *http.Request, i identity) error {
	var in sharedMetadataInput
	if err := sharedDecode(w, r, &in); err != nil {
		return err
	}
	if err := in.validate(); err != nil {
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
		return sharedFail(403, "当前成员只能查看基本信息")
	}
	taskID := r.PathValue("task")
	hash := sharedHash([]any{taskID, in})
	if raw, ok, e := p.sharedMetadataReceipt(r.Context(), c, s, taskID, i.UserID, hash, in); e != nil {
		return e
	} else if ok {
		writeJSON(w, 200, raw)
		return nil
	}
	if err = runtimeCommandGate(r.Context(), c, s.ID, taskID); err != nil {
		return err
	}
	task, err := p.sharedCheckTask(r.Context(), s, taskID)
	if err != nil {
		return err
	}
	reason, err := p.sharedTaskEditReason(r.Context(), s, task, "基本信息")
	if err != nil {
		return err
	}
	if reason != "" {
		return sharedFail(409, reason)
	}
	if sharedMetadataToken(task) != in.Token {
		return sharedFail(409, "任务基本信息版本已变化，请刷新")
	}
	if err = sharedStart(r.Context(), c, s.ID, taskID, in.RequestID, i.UserID, "task_metadata", hash, in.body()); err != nil {
		return err
	}
	unknown := func() error {
		_ = sharedUnknown(c, s.ID, in.RequestID)
		return sharedFail(423, "基本信息修改结果待核验，请勿重复提交")
	}
	if err = p.sharedPC(r.Context(), "PATCH", "/issues/"+url.PathEscape(taskID), in.body(), nil); err != nil {
		return unknown()
	}
	updated, err := p.sharedCheckTask(r.Context(), s, taskID)
	if err != nil || !in.matches(updated) {
		return unknown()
	}
	raw, err := sharedMetadataFinish(r.Context(), c, s.ID, in.RequestID, updated)
	if err != nil {
		return unknown()
	}
	writeJSON(w, 200, raw)
	return nil
}
