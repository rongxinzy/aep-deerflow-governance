package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
)

type sharedAssignment struct {
	Token       string             `json:"token"`
	Participant *sharedParticipant `json:"participant"`
	Human       *sharedHuman       `json:"human"`
	External    bool               `json:"external"`
	Claimed     bool               `json:"claimed"`
}
type sharedAssignmentInput struct {
	RequestID               string `json:"requestId"`
	ExpectedAssignmentToken string `json:"expectedAssignmentToken"`
	ParticipantID           string `json:"participantId"`
	MemberUserID            string `json:"memberUserId,omitempty"`
}

func (p *portal) sharedAssignmentDTO(ctx context.Context, s sharedSpace, t map[string]any, i identity) (a sharedAssignment, e error) {
	a.Claimed = t["status"] == "in_progress"
	agent, _ := t["assigneeAgentId"].(string)
	human, _ := t["assigneeUserId"].(string)
	a.External = agent != "" || human != ""
	if agent != "" {
		x, err := participantScan(p.db.QueryRowContext(ctx, participantSelect+` AND agent_id=$2`, s.ID, agent))
		if err == nil {
			x = p.participantDecorate(ctx, s, x, i)
			a.Participant = &x
			a.External = false
		} else if err != sql.ErrNoRows {
			return a, err
		}
	}
	if human != "" {
		x, err := p.sharedHumanForNative(ctx, s, human)
		if err != nil {
			return a, err
		}
		if x != nil {
			a.Human = x
			a.External = false
		}
	}
	var epoch int
	e = p.db.QueryRowContext(ctx, `SELECT count(*) FROM shared_space_commands WHERE space_id=$1 AND ((task_id=$2 AND action IN ('assign','claim')) OR action='participant' OR (task_id=$2 AND action='handoff'))`, s.ID, t["id"]).Scan(&epoch)
	a.Token = sharedAssignmentToken(t, epoch)
	return
}
func (p *portal) sharedAssignmentChange(w http.ResponseWriter, r *http.Request, i identity) error {
	action := r.PathValue("assignmentAction")
	if action != "assignment" && action != "claim" {
		return sharedFail(404, "not found")
	}
	var b sharedAssignmentInput
	if e := sharedDecode(w, r, &b); e != nil {
		return e
	}
	c, release, e := p.sharedLock(r.Context(), r.PathValue("space"))
	if e != nil {
		return e
	}
	defer release()
	s, e := p.sharedLoad(r.Context(), c, r.PathValue("space"), i)
	if e != nil {
		return e
	}
	out, e := p.sharedAssignmentLocked(r.Context(), c, s, r.PathValue("task"), i, b, action == "claim")
	if e == nil {
		writeJSON(w, 200, out)
	}
	return e
}
func (p *portal) sharedAssignmentLocked(ctx context.Context, c *sql.Conn, s sharedSpace, id string, i identity, b sharedAssignmentInput, claim bool) (any, error) {
	if b.MemberUserID != "" {
		if b.ParticipantID != "" {
			return nil, sharedFail(400, "请选择一位承接者")
		}
		return p.sharedHumanAssignmentLocked(ctx, c, s, id, i, b, claim)
	}
	if b.RequestID == "" || b.ParticipantID == "" || b.ExpectedAssignmentToken == "" {
		return nil, sharedFail(400, "领取或指派内容不完整")
	}
	if !sharedCaps(s.Role)["submit"] {
		return nil, sharedFail(403, "无指派权限")
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
	t, e := p.sharedCheckTask(ctx, s, id)
	if e != nil {
		return nil, e
	}
	a, e := p.sharedAssignmentDTO(ctx, s, t, i)
	if e != nil {
		return nil, e
	}
	if a.External {
		return nil, sharedFail(409, "任务已有外部承接者，不能覆盖")
	}
	if a.Token != b.ExpectedAssignmentToken {
		return nil, sharedFail(409, "任务负责人版本已变化")
	}
	x, e := participantScan(c.QueryRowContext(ctx, participantSelect+` AND id=$2`, s.ID, b.ParticipantID))
	if e != nil {
		return nil, sharedFail(404, "参与者不存在")
	}
	x = p.participantDecorate(ctx, s, x, i)
	if !x.CanAssign || claim && !x.CanClaim {
		return nil, sharedFail(403, "Agent未获得有效参与授权")
	}
	agent, _ := t["assigneeAgentId"].(string)
	human, _ := t["assigneeUserId"].(string)
	for _, key := range []string{"executionRunId", "checkoutRunId"} {
		if value := t[key]; value != nil && value != "" {
			return nil, sharedFail(409, "任务已有原生执行，不能接管")
		}
	}
	if t["status"] == "done" || t["status"] == "cancelled" {
		return nil, sharedFail(409, "任务已结束")
	}
	if e = runtimeCommandGate(ctx, c, s.ID, id); e != nil {
		return nil, e
	}
	if t["status"] != "backlog" && t["status"] != "todo" && t["status"] != "in_progress" {
		return nil, sharedFail(409, "任务当前阶段不允许改派或领取")
	}
	if claim && t["status"] == "in_progress" && agent == x.AgentID && human == "" {
		tx, e := c.BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		defer func() { _ = tx.Rollback() }()
		if e = sharedStart(ctx, tx, s.ID, id, b.RequestID, i.UserID, action, hash, map[string]any{"input": b, "participantId": x.ID, "participantVersion": x.Version, "noop": true}); e != nil {
			return nil, e
		}
		var epoch int
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM shared_space_commands WHERE space_id=$1 AND ((task_id=$2 AND action IN ('assign','claim')) OR action='participant' OR (task_id=$2 AND action='handoff'))`, s.ID, id).Scan(&epoch); e != nil {
			return nil, e
		}
		a.Token = sharedAssignmentToken(t, epoch)
		receipt := map[string]any{"task": sharedTaskDTO(t), "assignment": a, "state": "applied", "requestId": b.RequestID}
		if e = sharedFinish(ctx, tx, s.ID, b.RequestID, receipt); e != nil {
			return nil, e
		}
		if e = tx.Commit(); e != nil {
			return nil, e
		}
		return receipt, nil
	}
	var bound bool
	if e = c.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM shared_space_runtime_bindings WHERE space_id=$1 AND task_id=$2 AND binding_state='active')`, s.ID, id).Scan(&bound); e != nil {
		return nil, e
	}
	if bound || t["status"] == "in_progress" || human != "" || claim && agent != "" && agent != x.AgentID {
		return nil, sharedFail(409, "任务已有当前承接者")
	}
	method := "PATCH"
	path := "/issues/" + url.PathEscape(id)
	status := "todo"
	body := map[string]any{"assigneeAgentId": x.AgentID, "assigneeUserId": nil, "status": status}
	if claim {
		method = "POST"
		path += "/checkout"
		status = "in_progress"
		body = map[string]any{"agentId": x.AgentID, "expectedStatuses": []any{t["status"]}}
	}
	if e = sharedStart(ctx, c, s.ID, id, b.RequestID, i.UserID, action, hash, map[string]any{"input": b, "participantId": x.ID, "participantVersion": x.Version, "method": method, "path": path, "body": body}); e != nil {
		return nil, e
	}
	var updated map[string]any
	e = p.sharedPC(ctx, method, path, body, &updated)
	if e == nil {
		updated, e = p.sharedCheckTask(ctx, s, id)
	}
	if e != nil || updated["assigneeAgentId"] != x.AgentID || updated["assigneeUserId"] != nil && updated["assigneeUserId"] != "" || updated["status"] != status {
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
func (p *portal) participantForRuntime(ctx context.Context, s sharedSpace, workspace, companion, uid, owner string) (sharedParticipant, error) {
	return p.participantRuntimeGrant(ctx, s, workspace, companion, uid, owner, true)
}
func (p *portal) participantRuntimeGrant(ctx context.Context, s sharedSpace, workspace, companion, uid, owner string, write bool) (sharedParticipant, error) {
	x, e := participantScan(p.db.QueryRowContext(ctx, participantSelect+` AND kind='companion' AND target=$2`, s.ID, workspace+"/"+companion))
	if e != nil {
		return x, sharedFail(403, "搭档尚未成为空间参与者")
	}
	x = p.participantDecorate(ctx, s, x, identity{UserID: owner, DeploymentID: s.DeploymentID})
	if !x.ResourceValid || x.State != "active" || x.Registration != "ready" || x.AgentID == "" || write && !x.CanClaim || x.UID != uid || x.OwnerID != owner {
		return x, sharedFail(403, "搭档参与授权已失效")
	}
	return x, nil
}

func sharedAssignmentToken(t map[string]any, epoch int) string {
	return sharedHash([]any{t["id"], t["assigneeAgentId"], t["assigneeUserId"], t["status"], t["updatedAt"], epoch})
}
