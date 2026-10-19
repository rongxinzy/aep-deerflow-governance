package main

import (
	"context"
	"database/sql"
	"encoding/json"
	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"net/http"
	"net/url"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
)

const sharedHandoffSchema = `
CREATE TABLE IF NOT EXISTS shared_space_task_epochs(space_id TEXT NOT NULL REFERENCES shared_spaces(id),task_id TEXT NOT NULL,epoch INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(space_id,task_id));
CREATE TABLE IF NOT EXISTS shared_space_handoffs(space_id TEXT NOT NULL REFERENCES shared_spaces(id),task_id TEXT NOT NULL,request_id TEXT NOT NULL,actor_id TEXT NOT NULL,target_participant_id TEXT NOT NULL DEFAULT '',target_agent_id TEXT NOT NULL DEFAULT '',target_version INTEGER NOT NULL DEFAULT 0,source_agent_id TEXT NOT NULL DEFAULT '',source_participant_id TEXT NOT NULL DEFAULT '',source_name TEXT NOT NULL DEFAULT '',target_name TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL,phase TEXT NOT NULL,binding_id TEXT NOT NULL DEFAULT '',epoch INTEGER NOT NULL,payload_hash TEXT NOT NULL,stop_payload JSONB NOT NULL,next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),PRIMARY KEY(space_id,request_id));
CREATE UNIQUE INDEX IF NOT EXISTS shared_space_handoff_pending ON shared_space_handoffs(space_id,task_id) WHERE phase<>'applied';
CREATE TABLE IF NOT EXISTS shared_space_result_bindings(result_id TEXT PRIMARY KEY REFERENCES shared_space_results(id),binding_id TEXT NOT NULL REFERENCES shared_space_runtime_bindings(id));
INSERT INTO shared_space_result_bindings(result_id,binding_id)
SELECT r.id,o.binding_id FROM shared_space_results r JOIN shared_space_runtime_outbox o ON o.command_id='return:'||r.id||':submitted' JOIN shared_space_runtime_bindings b ON b.id=o.binding_id
WHERE b.space_id=r.space_id AND b.task_id=r.task_id AND o.payload->>'resultId'=r.id AND o.action='return'
ON CONFLICT(result_id) DO NOTHING;
`

type sharedHandoff struct {
	SourceAgentID string `json:"-"`
	FromID        string `json:"-"`
	SourceName    string `json:"-"`
	TargetName    string `json:"-"`
	RequestID     string `json:"requestId"`
	Phase         string `json:"phase"`
	TargetID      string `json:"targetParticipantId"`
	Reason        string `json:"reason"`
	TaskID        string `json:"-"`
	Actor         string `json:"-"`
	AgentID       string `json:"-"`
	TargetVersion int    `json:"-"`
	BindingID     string `json:"-"`
	Epoch         int    `json:"-"`
	Hash          string `json:"-"`
	Stop          []byte `json:"-"`
}

const handoffSelect = `SELECT request_id,phase,target_participant_id,reason,task_id,actor_id,target_agent_id,target_version,binding_id,epoch,payload_hash,stop_payload,source_participant_id,source_name,target_name,source_agent_id FROM shared_space_handoffs WHERE space_id=$1`

func handoffScan(q interface{ Scan(...any) error }) (h sharedHandoff, e error) {
	e = q.Scan(&h.RequestID, &h.Phase, &h.TargetID, &h.Reason, &h.TaskID, &h.Actor, &h.AgentID, &h.TargetVersion, &h.BindingID, &h.Epoch, &h.Hash, &h.Stop, &h.FromID, &h.SourceName, &h.TargetName, &h.SourceAgentID)
	return
}
func (p *portal) sharedTaskEpoch(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, space, task string) (int, error) {
	var epoch int
	e := q.QueryRowContext(ctx, `SELECT epoch FROM shared_space_task_epochs WHERE space_id=$1 AND task_id=$2`, space, task).Scan(&epoch)
	if e == sql.ErrNoRows {
		return 0, nil
	}
	return epoch, e
}
func (p *portal) sharedHandoffGate(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, space, task string) error {
	var pending bool
	if e := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM shared_space_handoffs WHERE space_id=$1 AND task_id=$2 AND phase<>'applied')`, space, task).Scan(&pending); e != nil {
		return e
	}
	if pending {
		return sharedFail(423, "任务正在交接，等待停止与负责人核验")
	}
	return nil
}
func (p *portal) sharedHandoffDTO(ctx context.Context, space, task string) (*sharedHandoff, error) {
	h, e := handoffScan(p.db.QueryRowContext(ctx, handoffSelect+` AND task_id=$2 AND phase<>'applied'`, space, task))
	if e == sql.ErrNoRows {
		return nil, nil
	}
	return &h, e
}
func (p *portal) sharedHandoffTask(w http.ResponseWriter, r *http.Request, i identity) error {
	var in struct {
		RequestID     string  `json:"requestId"`
		Token         string  `json:"expectedAssignmentToken"`
		ParticipantID *string `json:"participantId"`
		Reason        string  `json:"reason"`
	}
	if e := sharedDecode(w, r, &in); e != nil {
		return e
	}
	if in.RequestID == "" || in.Token == "" || strings.TrimSpace(in.Reason) == "" {
		return sharedFail(400, "请填写交接原因与版本")
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
	hash := sharedHash([]any{r.PathValue("task"), in})
	if old, e := handoffScan(c.QueryRowContext(r.Context(), handoffSelect+` AND request_id=$2`, s.ID, in.RequestID)); e == nil {
		if old.Actor != i.UserID || old.Hash != hash {
			return sharedFail(409, "请求编号已用于其他操作")
		}
		p.processHandoffLocked(r.Context(), c, s, &old)
		return p.handoffResponse(w, r.Context(), s, old, i)
	} else if e != sql.ErrNoRows {
		return e
	}
	task, e := p.sharedCheckTask(r.Context(), s, r.PathValue("task"))
	if e != nil {
		return e
	}
	a, e := p.sharedAssignmentDTO(r.Context(), s, task, i)
	if e != nil {
		return e
	}
	if a.Token != in.Token {
		return sharedFail(409, "任务负责人版本已变化")
	}
	if a.External || a.Participant == nil {
		return sharedFail(409, "任务没有可交接的空间Agent承接者")
	}
	if s.Role != "owner" && (!sharedCaps(s.Role)["submit"] || !a.Participant.CanClaim) {
		return sharedFail(403, "仅空间负责人或当前Agent管理成员可交接")
	}
	if task["status"] == "in_review" || task["status"] == "done" || task["status"] == "cancelled" {
		return sharedFail(409, "任务当前阶段不能交接")
	}
	for _, k := range []string{"executionRunId", "checkoutRunId"} {
		if v := task[k]; v != nil && v != "" {
			return sharedFail(409, "原生执行中的任务不能交接")
		}
	}
	results, e := sharedResults(r.Context(), c, s.ID, r.PathValue("task"))
	if e != nil {
		return e
	}
	if len(results) > 0 && results[len(results)-1].State != "changes_requested" {
		return sharedFail(409, "请先完成当前成果审阅")
	}
	if e = runtimeCommandGate(r.Context(), c, s.ID, r.PathValue("task")); e != nil {
		return e
	}
	h := sharedHandoff{RequestID: in.RequestID, Phase: "stop_pending", TaskID: r.PathValue("task"), Actor: i.UserID, Reason: in.Reason, Hash: hash, FromID: a.Participant.ID, SourceName: a.Participant.Name, SourceAgentID: a.Participant.AgentID}
	if in.ParticipantID != nil {
		if *in.ParticipantID == a.Participant.ID {
			return sharedFail(409, "请选择其他承接者或交还任务")
		}
		x, e := participantScan(c.QueryRowContext(r.Context(), participantSelect+` AND id=$2`, s.ID, *in.ParticipantID))
		if e != nil {
			return sharedFail(404, "目标参与者不存在")
		}
		x = p.participantDecorate(r.Context(), s, x, i)
		if !x.CanAssign {
			return sharedFail(403, "目标Agent未获有效编辑授权")
		}
		h.TargetID = x.ID
		h.AgentID = x.AgentID
		h.TargetVersion = x.Version
		h.TargetName = x.Name
	}
	var bindingID string
	e = c.QueryRowContext(r.Context(), `SELECT id FROM shared_space_runtime_bindings WHERE space_id=$1 AND task_id=$2 AND binding_state='active'`, s.ID, h.TaskID).Scan(&bindingID)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	h.BindingID = bindingID
	stop := map[string]any{}
	if bindingID != "" {
		b, e := runtimeLoad(r.Context(), c, bindingID)
		if e != nil {
			return e
		}
		stop = map[string]any{"bindingId": b.ID, "executionId": b.ExecutionID, "generation": b.Generation, "piTaskId": b.PiTaskID, "assignmentEpoch": b.AssignmentEpoch}
	}
	tx, e := c.BeginTx(r.Context(), nil)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	if e = sharedStart(r.Context(), tx, s.ID, h.TaskID, h.RequestID, i.UserID, "handoff", hash, in); e != nil {
		return e
	}
	if e = tx.QueryRowContext(r.Context(), `INSERT INTO shared_space_task_epochs(space_id,task_id,epoch) VALUES($1,$2,1) ON CONFLICT(space_id,task_id) DO UPDATE SET epoch=shared_space_task_epochs.epoch+1 RETURNING epoch`, s.ID, h.TaskID).Scan(&h.Epoch); e != nil {
		return e
	}
	if bindingID != "" {
		if _, e = tx.ExecContext(r.Context(), `UPDATE shared_space_runtime_bindings SET binding_state='retiring' WHERE id=$1`, bindingID); e != nil {
			return e
		}
		if _, e = tx.ExecContext(r.Context(), `UPDATE shared_space_runtime_outbox SET state='blocked' WHERE binding_id=$1 AND state<>'applied'`, bindingID); e != nil {
			return e
		}
	} else {
		h.Phase = "patch_pending"
	}
	raw, _ := json.Marshal(stop)
	h.Stop = raw
	if _, e = tx.ExecContext(r.Context(), `INSERT INTO shared_space_handoffs(space_id,task_id,request_id,actor_id,target_participant_id,target_agent_id,target_version,reason,phase,binding_id,epoch,payload_hash,stop_payload,source_participant_id,source_name,target_name,source_agent_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, s.ID, h.TaskID, h.RequestID, h.Actor, h.TargetID, h.AgentID, h.TargetVersion, h.Reason, h.Phase, h.BindingID, h.Epoch, h.Hash, string(raw), h.FromID, h.SourceName, h.TargetName, h.SourceAgentID); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	p.processHandoffLocked(r.Context(), c, s, &h)
	return p.handoffResponse(w, r.Context(), s, h, i)
}
func (p *portal) handoffResponse(w http.ResponseWriter, ctx context.Context, s sharedSpace, h sharedHandoff, i identity) error {
	state := "unknown"
	code := 202
	if h.Phase == "applied" {
		state = "applied"
		code = 200
	}
	out := map[string]any{"requestId": h.RequestID, "state": state, "phase": h.publicPhase(), "handoff": h}
	if task, e := p.sharedCheckTask(ctx, s, h.TaskID); e == nil {
		out["task"] = sharedTaskDTO(task)
		if a, e := p.sharedAssignmentDTO(ctx, s, task, i); e == nil {
			out["assignment"] = a
		}
	}
	writeJSON(w, code, out)
	return nil
}
func (p *portal) processSharedHandoffs(ctx context.Context) {
	var space, request string
	if p.db.QueryRowContext(ctx, `SELECT space_id,request_id FROM shared_space_handoffs WHERE phase<>'applied' AND next_attempt_at<=now() ORDER BY next_attempt_at,request_id LIMIT 1`).Scan(&space, &request) != nil {
		return
	}
	c, release, e := p.sharedLock(ctx, space)
	if e != nil {
		return
	}
	defer release()
	s, e := p.sharedRuntimeSpace(ctx, space)
	if e != nil {
		return
	}
	h, e := handoffScan(c.QueryRowContext(ctx, handoffSelect+` AND request_id=$2`, space, request))
	if e == nil {
		_, _ = c.ExecContext(ctx, `UPDATE shared_space_handoffs SET next_attempt_at=now()+interval '10 seconds' WHERE space_id=$1 AND request_id=$2`, space, request)
		p.processHandoffLocked(ctx, c, s, &h)
	}
}
func (p *portal) processHandoffLocked(ctx context.Context, c *sql.Conn, s sharedSpace, h *sharedHandoff) {
	if h.Phase == "applied" {
		return
	}
	setPhase := func(phase string) bool {
		_, e := c.ExecContext(ctx, `UPDATE shared_space_handoffs SET phase=$3 WHERE space_id=$1 AND request_id=$2`, s.ID, h.RequestID, phase)
		if e == nil {
			h.Phase = phase
		}
		return e == nil
	}
	if h.Phase == "stop_pending" || h.Phase == "stop_unknown" {
		b, e := runtimeLoad(ctx, c, h.BindingID)
		if e != nil {
			return
		}
		var ws dev.PersonalWorkspace
		if p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: b.Workspace}, &ws) != nil || string(ws.UID) != b.WorkspaceUID || ws.Spec.Owner != b.Owner {
			return
		}
		var payload map[string]any
		if json.Unmarshal(h.Stop, &payload) != nil {
			return
		}
		id := "handoff-stop:" + s.ID + ":" + h.RequestID
		hash := sharedHash(payload)
		var receipt map[string]any
		if h.Phase == "stop_pending" {
			if !setPhase("stop_unknown") {
				return
			}
			receipt, e = p.collaborationCall(ctx, &ws, b.Companion, "/internal/shared-space/stop", map[string]any{"commandId": id, "payloadHash": hash, "payload": payload})
		} else {
			receipt, e = p.collaborationCall(ctx, &ws, b.Companion, "/internal/shared-space/receipt?commandId="+url.QueryEscape(id), nil)
		}
		if e != nil || receipt["commandId"] != id || receipt["payloadHash"] != hash || receipt["state"] != "applied" || receipt["bindingId"] != b.ID || receipt["executionId"] != b.ExecutionID || receipt["assignmentEpoch"] != float64(b.AssignmentEpoch) || receipt["fenced"] != true || receipt["schedulerStopped"] != true {
			return
		}
		if !setPhase("patch_pending") {
			return
		}
	}
	if h.Phase == "patch_pending" {
		actorSpace, e := p.sharedLoad(ctx, c, s.ID, identity{UserID: h.Actor, DeploymentID: s.DeploymentID})
		if e != nil {
			return
		}
		if actorSpace.Role != "owner" {
			x, e := participantScan(c.QueryRowContext(ctx, participantSelect+` AND id=$2`, s.ID, h.FromID))
			if e != nil {
				return
			}
			x = p.participantDecorate(ctx, actorSpace, x, identity{UserID: h.Actor, DeploymentID: s.DeploymentID})
			if !x.CanClaim {
				return
			}
		}

		if h.TargetID != "" {
			x, e := participantScan(c.QueryRowContext(ctx, participantSelect+` AND id=$2`, s.ID, h.TargetID))
			if e != nil {
				return
			}
			x = p.participantDecorate(ctx, s, x, identity{UserID: h.Actor, DeploymentID: s.DeploymentID})
			if !x.ResourceValid || x.State != "active" || x.Role != "editor" || x.Version != h.TargetVersion || x.AgentID != h.AgentID {
				return
			}
		}
		task, e := p.sharedCheckTask(ctx, s, h.TaskID)
		if e != nil {
			return
		}
		if task["assigneeAgentId"] != h.SourceAgentID || task["assigneeUserId"] != nil && task["assigneeUserId"] != "" {
			return
		}
		for _, k := range []string{"executionRunId", "checkoutRunId"} {
			if v := task[k]; v != nil && v != "" {
				return
			}
		}
		if task["status"] == "done" || task["status"] == "cancelled" || task["status"] == "in_review" {
			return
		}
		if !setPhase("patch_unknown") {
			return
		}
		var ignored map[string]any
		var target any
		if h.AgentID != "" {
			target = h.AgentID
		}
		_ = p.sharedPC(ctx, "PATCH", "/issues/"+url.PathEscape(h.TaskID), map[string]any{"assigneeAgentId": target, "assigneeUserId": nil, "status": "todo"}, &ignored)
	}
	if h.Phase == "patch_unknown" {
		if h.TargetID != "" {
			x, e := participantScan(c.QueryRowContext(ctx, participantSelect+` AND id=$2`, s.ID, h.TargetID))
			if e != nil {
				return
			}
			x = p.participantDecorate(ctx, s, x, identity{UserID: h.Actor, DeploymentID: s.DeploymentID})
			if !x.ResourceValid || x.State != "active" || x.Role != "editor" || x.Version != h.TargetVersion || x.AgentID != h.AgentID {
				return
			}
		}
		task, e := p.sharedCheckTask(ctx, s, h.TaskID)
		if e != nil {
			return
		}
		for _, k := range []string{"executionRunId", "checkoutRunId"} {
			if v := task[k]; v != nil && v != "" {
				return
			}
		}
		agent, _ := task["assigneeAgentId"].(string)
		human, _ := task["assigneeUserId"].(string)
		if agent != h.AgentID || human != "" || task["status"] != "todo" {
			return
		}
		tx, e := c.BeginTx(ctx, nil)
		if e != nil {
			return
		}
		defer func() { _ = tx.Rollback() }()
		if h.BindingID != "" {
			if _, e = tx.ExecContext(ctx, `UPDATE shared_space_runtime_bindings SET binding_state='retired' WHERE id=$1`, h.BindingID); e != nil {
				return
			}
		}
		if _, e = tx.ExecContext(ctx, `UPDATE shared_space_handoffs SET phase='applied' WHERE space_id=$1 AND request_id=$2`, s.ID, h.RequestID); e != nil {
			return
		}
		if e = sharedFinish(ctx, tx, s.ID, h.RequestID, map[string]any{"requestId": h.RequestID, "state": "applied", "phase": "applied"}); e != nil {
			return
		}
		if tx.Commit() == nil {
			h.Phase = "applied"
		}
	}
}
func (p *portal) sharedHandoffHistory(ctx context.Context, space, task string) ([]sharedHandoff, error) {
	rows, e := p.db.QueryContext(ctx, handoffSelect+` AND task_id=$2 ORDER BY epoch`, space, task)
	if e != nil {
		return nil, e
	}
	defer func() { _ = rows.Close() }()
	out := []sharedHandoff{}
	for rows.Next() {
		h, e := handoffScan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (h sharedHandoff) publicPhase() string {
	if h.Phase == "applied" {
		return "applied"
	}
	if h.Phase == "patch_pending" || h.Phase == "patch_unknown" {
		return "assignment_unknown"
	}
	return "stopping"
}
func (h sharedHandoff) MarshalJSON() ([]byte, error) {
	var target any
	var from, to any
	if h.TargetID != "" {
		target = h.TargetID
		to = map[string]any{"id": h.TargetID, "name": h.TargetName}
	}
	if h.FromID != "" {
		from = map[string]any{"id": h.FromID, "name": h.SourceName}
	}
	status := "unknown"
	if h.Phase == "applied" {
		status = "applied"
	}
	return json.Marshal(map[string]any{"requestId": h.RequestID, "phase": h.publicPhase(), "status": status, "targetParticipantId": target, "reason": h.Reason, "from": from, "to": to})
}
