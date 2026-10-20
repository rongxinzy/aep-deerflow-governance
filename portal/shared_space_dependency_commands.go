package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
)

type sharedDependencyInput struct {
	RequestID string   `json:"requestId"`
	Token     string   `json:"expectedDependencyToken"`
	IDs       []string `json:"blockedByIssueIds"`
}

func sharedDependencyFinish(ctx context.Context, c *sql.Conn, space, request string, receipt any) error {
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = sharedFinish(ctx, tx, space, request, receipt); err != nil {
		return err
	}
	return tx.Commit()
}
func (p *portal) sharedDependencyReceipt(ctx context.Context, c *sql.Conn, s sharedSpace, task, actor, hash string, in sharedDependencyInput) (json.RawMessage, bool, error) {
	var priorActor, action, priorHash, state string
	var raw []byte
	err := c.QueryRowContext(ctx, `SELECT actor_id,action,payload_hash,state,receipt FROM shared_space_commands WHERE space_id=$1 AND request_id=$2`, s.ID, in.RequestID).Scan(&priorActor, &action, &priorHash, &state, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if priorActor != actor || action != "dependencies" || priorHash != hash {
		return nil, false, sharedFail(409, "请求编号已用于其他操作")
	}
	if state == "applied" {
		var receipt struct {
			Rejected bool `json:"rejected"`
		}
		if err = json.Unmarshal(raw, &receipt); err != nil {
			return nil, false, err
		}
		if receipt.Rejected {
			return nil, false, sharedFail(400, "任务服务拒绝此依赖修改")
		}
		return raw, true, nil
	}
	// A persisted intent licenses only a read-back, never another PATCH. The
	// desired native relation set itself is sufficient evidence to finish it.
	current, err := p.sharedCheckTask(ctx, s, task)
	if err != nil {
		return nil, false, err
	}
	got, err := sharedBlockedIDs(current)
	if err != nil {
		return nil, false, err
	}
	if sharedHash(got) != sharedHash(in.IDs) {
		return nil, false, sharedFail(423, "依赖修改结果待核验，请勿重复提交")
	}
	deps, err := p.sharedReadDependencies(ctx, s, current)
	if err != nil {
		return nil, false, err
	}
	receipt := map[string]any{"dependencies": deps}
	if err = sharedDependencyFinish(ctx, c, s.ID, in.RequestID, receipt); err != nil {
		return nil, false, err
	}
	raw, err = json.Marshal(receipt)
	return raw, true, err
}
func (p *portal) sharedSetDependencies(w http.ResponseWriter, r *http.Request, i identity) error {
	var in sharedDependencyInput
	if err := sharedDecode(w, r, &in); err != nil {
		return err
	}
	if in.RequestID == "" || in.Token == "" || in.IDs == nil {
		return sharedFail(400, "请提供依赖版本与完整前置任务列表")
	}
	unique := map[string]bool{}
	ids := []string{}
	for _, id := range in.IDs {
		if id == "" {
			return sharedFail(400, "前置任务标识不正确")
		}
		if !unique[id] {
			ids = append(ids, id)
			unique[id] = true
		}
	}
	sort.Strings(ids)
	in.IDs = ids
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
		return sharedFail(403, "当前成员只能查看依赖")
	}
	taskID := r.PathValue("task")
	hash := sharedHash([]any{taskID, in})
	if raw, ok, e := p.sharedDependencyReceipt(r.Context(), c, s, taskID, i.UserID, hash, in); e != nil {
		return e
	} else if ok {
		writeJSON(w, 200, raw)
		return nil
	}
	task, err := p.sharedCheckTask(r.Context(), s, taskID)
	if err != nil {
		return err
	}
	reason, err := p.sharedDependencyEditReason(r.Context(), s, task)
	if err != nil {
		return err
	}
	if reason != "" {
		return sharedFail(409, reason)
	}
	old, err := p.sharedReadDependencies(r.Context(), s, task)
	if err != nil {
		return err
	}
	if old.Token != in.Token {
		return sharedFail(409, "任务依赖版本已变化，请刷新")
	}
	for _, id := range in.IDs {
		if id == taskID {
			return sharedFail(400, "任务不能依赖自身")
		}
		if _, err = p.sharedCheckTask(r.Context(), s, id); err != nil {
			return err
		}
	}
	body := map[string]any{"blockedByIssueIds": in.IDs}
	if err = sharedStart(r.Context(), c, s.ID, taskID, in.RequestID, i.UserID, "dependencies", hash, body); err != nil {
		return err
	}
	if err = p.sharedPC(r.Context(), "PATCH", "/issues/"+url.PathEscape(taskID), body, nil); err != nil {
		var rejected *sharedError
		if errors.As(err, &rejected) && (rejected.code == 400 || rejected.code == 409) {
			if e := sharedDependencyFinish(r.Context(), c, s.ID, in.RequestID, map[string]any{"rejected": true}); e != nil {
				return e
			}
			return err
		}
		_ = sharedUnknown(c, s.ID, in.RequestID)
		return sharedFail(423, "依赖修改结果待核验，请勿重复提交")
	}
	updated, err := p.sharedCheckTask(r.Context(), s, taskID)
	if err != nil {
		return err
	}
	got, err := sharedBlockedIDs(updated)
	if err != nil {
		return err
	}
	if sharedHash(got) != sharedHash(in.IDs) {
		_ = sharedUnknown(c, s.ID, in.RequestID)
		return sharedFail(423, "依赖修改结果待核验，请勿重复提交")
	}
	deps, err := p.sharedReadDependencies(r.Context(), s, updated)
	if err != nil {
		return err
	}
	receipt := map[string]any{"dependencies": deps}
	if err = sharedDependencyFinish(r.Context(), c, s.ID, in.RequestID, receipt); err != nil {
		return err
	}
	writeJSON(w, 200, receipt)
	return nil
}
