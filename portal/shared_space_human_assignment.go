package main

import (
	"context"
	"database/sql"
	"net/url"
)

func (p *portal) sharedHumanAssignmentLocked(ctx context.Context, c *sql.Conn, s sharedSpace, id string, i identity, b sharedAssignmentInput, claim bool) (any, error) {
	if b.RequestID == "" || b.ExpectedAssignmentToken == "" {
		return nil, sharedFail(400, "领取或指派内容不完整")
	}
	if !sharedCaps(s.Role)["submit"] || claim && b.MemberUserID != i.UserID {
		return nil, sharedFail(403, "只能为自己领取任务")
	}
	action := "assign"
	if claim {
		action = "claim"
	}
	hash := sharedHash([]any{id, b})
	if raw, ok, e := sharedReplay(ctx, c, s.ID, b.RequestID, i.UserID, action, hash); e != nil {
		return nil, e
	} else if ok {
		return raw, nil
	}
	task, e := p.sharedCheckTask(ctx, s, id)
	if e != nil {
		return nil, e
	}
	a, e := p.sharedAssignmentDTO(ctx, s, task, i)
	if e != nil {
		return nil, e
	}
	if a.Token != b.ExpectedAssignmentToken {
		return nil, sharedFail(409, "任务负责人版本已变化")
	}
	if a.External {
		return nil, sharedFail(409, "任务已有外部承接者，不能覆盖")
	}
	human, e := p.sharedHumanForUser(ctx, s, b.MemberUserID)
	if e != nil {
		return nil, e
	}
	if human == nil {
		return nil, sharedFail(409, "请先关联当前企业的协作人员身份")
	}
	for _, key := range []string{"executionRunId", "checkoutRunId"} {
		if v := task[key]; v != nil && v != "" {
			return nil, sharedFail(409, "任务已有原生执行，不能接管")
		}
	}
	if task["status"] != "backlog" && task["status"] != "todo" && task["status"] != "in_progress" {
		return nil, sharedFail(409, "任务当前阶段不允许改派或领取")
	}
	if e = runtimeCommandGate(ctx, c, s.ID, id); e != nil {
		return nil, e
	}
	if e = p.sharedHandoffGate(ctx, c, s.ID, id); e != nil {
		return nil, e
	}
	var bound bool
	if e = c.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM shared_space_runtime_bindings WHERE space_id=$1 AND task_id=$2 AND binding_state='active')`, s.ID, id).Scan(&bound); e != nil {
		return nil, e
	}
	own := a.Human != nil && a.Human.UserID == b.MemberUserID
	if bound || a.Participant != nil || a.Human != nil && !own || task["status"] == "in_progress" && (!claim || !own) {
		return nil, sharedFail(409, "请先交接当前承接任务")
	}
	status := "todo"
	if claim {
		status = "in_progress"
		deps, err := p.sharedReadDependencies(ctx, s, task)
		if err != nil {
			return nil, err
		}
		if !deps.Ready {
			return nil, sharedFail(409, deps.WaitingReason)
		}
	}
	path := "/issues/" + url.PathEscape(id)
	body := map[string]any{"assigneeAgentId": nil, "assigneeUserId": human.NativeUserID, "status": status}
	if e = sharedStart(ctx, c, s.ID, id, b.RequestID, i.UserID, action, hash, map[string]any{"input": b, "method": "PATCH", "path": path, "body": body}); e != nil {
		return nil, e
	}
	var updated map[string]any
	e = p.sharedPC(ctx, "PATCH", path, body, &updated)
	if e == nil {
		updated, e = p.sharedCheckTask(ctx, s, id)
	}
	if e != nil || updated["assigneeUserId"] != human.NativeUserID || updated["assigneeAgentId"] != nil && updated["assigneeAgentId"] != "" || updated["status"] != status {
		_ = sharedUnknown(c, s.ID, b.RequestID)
		return nil, sharedFail(423, "任务负责人写入结果待核验")
	}
	a, e = p.sharedAssignmentDTO(ctx, s, updated, i)
	if e != nil {
		return nil, e
	}
	receipt := map[string]any{"task": sharedTaskDTO(updated), "assignment": a, "state": "applied", "requestId": b.RequestID}
	tx, e := c.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer func() { _ = tx.Rollback() }()
	if e = sharedFinish(ctx, tx, s.ID, b.RequestID, receipt); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return receipt, nil
}
