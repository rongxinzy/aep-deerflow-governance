package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"net/http"
	"net/url"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

const sharedRuntimeSchema = `
CREATE TABLE IF NOT EXISTS shared_space_runtime_bindings (
 id TEXT PRIMARY KEY, space_id TEXT NOT NULL REFERENCES shared_spaces(id), task_id TEXT NOT NULL,
 workspace_name TEXT NOT NULL, workspace_uid TEXT NOT NULL, owner_id TEXT NOT NULL, companion TEXT NOT NULL,
 source_conversation_id BIGINT NOT NULL, pi_task_id TEXT NOT NULL DEFAULT '', generation INTEGER NOT NULL,
 execution_id TEXT NOT NULL, expected_version INTEGER NOT NULL, bind_hash TEXT NOT NULL, assignment_hash TEXT NOT NULL,
 UNIQUE(space_id,task_id)
);
CREATE TABLE IF NOT EXISTS shared_space_runtime_outbox (
 command_id TEXT PRIMARY KEY, binding_id TEXT NOT NULL REFERENCES shared_space_runtime_bindings(id),
 action TEXT NOT NULL, payload JSONB NOT NULL, payload_hash TEXT NOT NULL,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 state TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','unknown','applied','blocked')),
 receipt JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
 );
ALTER TABLE shared_space_runtime_bindings ADD COLUMN IF NOT EXISTS participant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE shared_space_runtime_bindings ADD COLUMN IF NOT EXISTS participant_version INTEGER NOT NULL DEFAULT 0;`

type runtimeBinding struct {
	ParticipantID        string `json:"participantId"`
	ParticipantVersion   int    `json:"participantVersion"`
	ID                   string `json:"bindingId"`
	SpaceID              string `json:"spaceId"`
	TaskID               string `json:"taskId"`
	Workspace            string `json:"-"`
	WorkspaceUID         string `json:"-"`
	Owner                string `json:"-"`
	Companion            string `json:"-"`
	SourceConversationID int64  `json:"sourceConversationId"`
	PiTaskID             string `json:"piTaskId"`
	Generation           int    `json:"generation"`
	ExecutionID          string `json:"executionId"`
	ExpectedVersion      int    `json:"expectedVersion"`
	BindHash             string `json:"-"`
	AssignmentHash       string `json:"-"`
}

func runtimeLoad(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (b runtimeBinding, err error) {
	err = q.QueryRowContext(ctx, `SELECT id,space_id,task_id,workspace_name,workspace_uid,owner_id,companion,source_conversation_id,pi_task_id,generation,execution_id,expected_version,bind_hash,assignment_hash,participant_id,participant_version FROM shared_space_runtime_bindings WHERE id=$1`, id).Scan(&b.ID, &b.SpaceID, &b.TaskID, &b.Workspace, &b.WorkspaceUID, &b.Owner, &b.Companion, &b.SourceConversationID, &b.PiTaskID, &b.Generation, &b.ExecutionID, &b.ExpectedVersion, &b.BindHash, &b.AssignmentHash, &b.ParticipantID, &b.ParticipantVersion)
	return
}
func runtimeEnqueue(ctx context.Context, tx *sql.Tx, b runtimeBinding, id, action string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO shared_space_runtime_outbox(command_id,binding_id,action,payload,payload_hash) VALUES($1,$2,$3,$4,$5) ON CONFLICT(command_id) DO NOTHING`, id, b.ID, action, string(raw), sharedHash(payload))
	return err
}
func (p *portal) sharedRuntimeEnqueue(ctx context.Context, tx *sql.Tx, space, task string, result sharedResult, stage, reason string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM shared_space_runtime_bindings WHERE space_id=$1 AND task_id=$2`, space, task).Scan(&id)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	b, err := runtimeLoad(ctx, tx, id)
	if err != nil {
		return err
	}
	if result.SubmittedBy != b.Owner {
		return nil
	}
	if stage != "submitted" {
		var linked bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM shared_space_runtime_outbox WHERE command_id=$1 AND binding_id=$2)`, "return:"+result.ID+":submitted", b.ID).Scan(&linked); err != nil {
			return err
		}
		if !linked {
			return nil
		}
	}
	if stage == "request_changes" {
		b.Generation++
		b.ExecutionID = collaborationID(b.ID, fmt.Sprint(b.Generation))
		b.ExpectedVersion = result.Version
		_, err = tx.ExecContext(ctx, `UPDATE shared_space_runtime_bindings SET generation=$2,execution_id=$3,expected_version=$4 WHERE id=$1`, b.ID, b.Generation, b.ExecutionID, b.ExpectedVersion)
		if err != nil {
			return err
		}
		payload := map[string]any{"bindingId": b.ID, "piTaskId": b.PiTaskID, "generation": b.Generation, "executionId": b.ExecutionID, "expectedVersion": b.ExpectedVersion, "resultId": result.ID, "revisionId": result.RevisionID, "reason": reason}
		if err = runtimeEnqueue(ctx, tx, b, "revise:"+result.ID, "revise", payload); err != nil {
			return err
		}
	}
	text := fmt.Sprintf("『%s』第 %d 版%s", result.Title, result.Version, map[string]string{"submitted": "已提交，等待审阅", "request_changes": "已退回修改", "accept": "已接受"}[stage])
	if reason != "" {
		text += "：" + reason
	}
	return runtimeEnqueue(ctx, tx, b, "return:"+result.ID+":"+stage, "return", map[string]any{"bindingId": b.ID, "generation": b.Generation, "executionId": b.ExecutionID, "conversationId": b.SourceConversationID, "resultId": result.ID, "revisionId": result.RevisionID, "stage": stage, "text": text})
}
func (p *portal) runtimeAuthorize(ctx context.Context, b runtimeBinding, ws *dev.PersonalWorkspace, i identity) error {
	if b.Workspace != ws.Name || b.WorkspaceUID != string(ws.UID) || b.Owner != ws.Spec.Owner {
		return sharedFail(403, "执行来源已失效")
	}
	owner, ok, err := newReferenceResolver(p, ctx).lookupUser(b.Owner)
	if err != nil {
		return err
	}
	if !ok || owner.Kind != "human" || owner.Status != "active" {
		return sharedFail(403, "账号不可用")
	}
	found := false
	for _, companion := range ws.Spec.Companions {
		if companion.Name == b.Companion {
			found = true
		}
	}
	if !found {
		return sharedFail(403, "搭档已移除")
	}
	s, err := p.sharedRuntimeSpace(ctx, b.SpaceID)
	if err != nil {
		return err
	}
	if !sharedCaps(s.Role)["submit"] {
		return sharedFail(403, "执行权限已撤销")
	}
	x, err := p.participantForRuntime(ctx, s, b.Workspace, b.Companion, b.WorkspaceUID, b.Owner)
	if err != nil {
		return err
	}
	if b.ParticipantID != x.ID || b.ParticipantVersion != x.Version {
		return sharedFail(403, "参与授权代次已失效")
	}
	task, err := p.sharedCheckTask(ctx, s, b.TaskID)
	if err == nil {
		for _, key := range []string{"executionRunId", "checkoutRunId"} {
			if value := task[key]; value != nil && value != "" {
				return sharedFail(409, "任务已有原生执行")
			}
		}
	}
	if err == nil && (runtimeAssignment(task) != b.AssignmentHash || task["assigneeAgentId"] != x.AgentID) {
		return sharedFail(409, "任务负责人已变化")
	}
	return err
}
func (p *portal) sharedRuntimeAgent(w http.ResponseWriter, r *http.Request, i identity, ws *dev.PersonalWorkspace, companion string) error {
	if p.db == nil {
		return sharedFail(503, "空间运行未配置")
	}
	i.DeploymentID = p.cfg.aepDeploymentID
	if r.PathValue("action") == "space-discover" && r.Method == "GET" {
		rows, err := p.db.QueryContext(r.Context(), `SELECT s.id FROM shared_spaces s JOIN shared_space_participants p ON p.space_id=s.id WHERE s.deployment_id=$1 AND p.kind='companion' AND p.target=$2 AND p.state='active'`, i.DeploymentID, ws.Name+"/"+companion)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		out := []map[string]any{}
		for _, id := range ids {
			s, err := p.sharedRuntimeSpace(r.Context(), id)
			if err != nil {
				continue
			}
			participant, err := p.participantRuntimeGrant(r.Context(), s, ws.Name, companion, string(ws.UID), i.UserID, false)
			if err != nil {
				continue
			}
			s.Role = participant.Role
			var tasks []map[string]any
			if err = p.sharedPC(r.Context(), "GET", "/companies/"+url.PathEscape(s.CompanyID)+"/issues?projectId="+url.QueryEscape(s.ProjectID), nil, &tasks); err != nil {
				return err
			}
			safe := []map[string]any{}
			for _, task := range tasks {
				if task["projectId"] == s.ProjectID && task["companyId"] == s.CompanyID {
					dto := sharedTaskDTO(task)
					assignment, e := p.sharedAssignmentDTO(r.Context(), s, task, i)
					if e != nil {
						return e
					}
					dto["assignment"] = assignment
					safe = append(safe, dto)
				}
			}
			out = append(out, map[string]any{"space": s, "tasks": safe})
		}
		writeJSON(w, 200, map[string]any{"spaces": out})
		return nil
	}
	if r.PathValue("action") == "space-claim" && r.Method == "POST" {
		var in struct {
			SpaceID                 string `json:"spaceId"`
			TaskID                  string `json:"taskId"`
			RequestID               string `json:"requestId"`
			ExpectedAssignmentToken string `json:"expectedAssignmentToken"`
		}
		if err := sharedDecode(w, r, &in); err != nil {
			return err
		}
		c, release, err := p.sharedLock(r.Context(), in.SpaceID)
		if err != nil {
			return err
		}
		defer release()
		s, err := p.sharedRuntimeSpace(r.Context(), in.SpaceID)
		if err != nil {
			return err
		}
		x, err := p.participantForRuntime(r.Context(), s, ws.Name, companion, string(ws.UID), i.UserID)
		if err != nil {
			return err
		}
		out, err := p.sharedAssignmentLocked(r.Context(), c, s, in.TaskID, i, sharedAssignmentInput{RequestID: in.RequestID, ExpectedAssignmentToken: in.ExpectedAssignmentToken, ParticipantID: x.ID}, true)
		if err == nil {
			writeJSON(w, 200, out)
		}
		return err
	}
	if r.PathValue("action") == "space-bind" && r.Method == "POST" {
		var in struct {
			SpaceID              string `json:"spaceId"`
			TaskID               string `json:"taskId"`
			SourceConversationID int64  `json:"sourceConversationId"`
			RequestID            string `json:"requestId"`
			Title                string `json:"title"`
			Instruction          string `json:"instruction"`
		}
		if err := sharedDecode(w, r, &in); err != nil {
			return err
		}
		if in.SourceConversationID <= 0 || in.RequestID == "" || in.Title == "" || in.Instruction == "" {
			return sharedFail(400, "交办内容不完整")
		}
		c, release, err := p.sharedLock(r.Context(), in.SpaceID)
		if err != nil {
			return err
		}
		defer release()
		s, err := p.sharedRuntimeSpace(r.Context(), in.SpaceID)
		if err != nil {
			return err
		}
		if !sharedCaps(s.Role)["submit"] {
			return sharedFail(403, "无交办权限")
		}
		participant, err := p.participantForRuntime(r.Context(), s, ws.Name, companion, string(ws.UID), i.UserID)
		if err != nil {
			return err
		}
		task, err := p.sharedCheckTask(r.Context(), s, in.TaskID)
		if err != nil {
			return err
		}
		if task["status"] == "done" || task["status"] == "cancelled" {
			return sharedFail(409, "任务已结束")
		}
		if source, sourceErr := p.collaborationCall(r.Context(), ws, companion, "/api/conversations/"+fmt.Sprint(in.SourceConversationID), nil); sourceErr != nil || source["conversationId"] != float64(in.SourceConversationID) {
			return sharedFail(409, "原对话无法确认")
		}
		id := collaborationID(in.SpaceID, in.TaskID)
		hash := sharedHash(in)
		old, err := runtimeLoad(r.Context(), c, id)
		if err == nil {
			if old.BindHash != hash || old.WorkspaceUID != string(ws.UID) || old.Companion != companion {
				return sharedFail(409, "任务已绑定其他执行")
			}
			if err = p.runtimeAuthorize(r.Context(), old, ws, i); err != nil {
				return err
			}
			writeJSON(w, 200, old)
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if err := runtimeCommandGate(r.Context(), c, in.SpaceID, in.TaskID); err != nil {
			return err
		}
		results, err := sharedResults(r.Context(), c, in.SpaceID, in.TaskID)
		if err != nil {
			return err
		}
		if len(results) > 0 {
			return sharedFail(409, "已有成果的任务不能重新绑定")
		}
		assignment, err := p.sharedAssignmentDTO(r.Context(), s, task, i)
		if err != nil {
			return err
		}
		claimInput := sharedAssignmentInput{RequestID: "runtime-claim:" + in.RequestID, ExpectedAssignmentToken: assignment.Token, ParticipantID: participant.ID}
		var actor, action, state string
		var expected []byte
		priorErr := c.QueryRowContext(r.Context(), `SELECT actor_id,action,state,expected FROM shared_space_commands WHERE space_id=$1 AND request_id=$2`, s.ID, claimInput.RequestID).Scan(&actor, &action, &state, &expected)
		if priorErr == nil {
			var prior struct {
				Input              sharedAssignmentInput `json:"input"`
				ParticipantID      string                `json:"participantId"`
				ParticipantVersion int                   `json:"participantVersion"`
			}
			if json.Unmarshal(expected, &prior) != nil || actor != i.UserID || action != "claim" || state != "applied" || prior.ParticipantID != participant.ID || prior.ParticipantVersion != participant.Version || prior.Input.RequestID != claimInput.RequestID {
				return sharedFail(423, "原领取操作待核验")
			}
			claimInput = prior.Input
		} else if priorErr != sql.ErrNoRows {
			return priorErr
		}
		if _, err = p.sharedAssignmentLocked(r.Context(), c, s, in.TaskID, i, claimInput, true); err != nil {
			return err
		}
		for _, key := range []string{"executionRunId", "checkoutRunId"} {
			if value := task[key]; value != nil && value != "" {
				return sharedFail(409, "任务已有未知原生执行")
			}
		}

		task, err = p.sharedCheckTask(r.Context(), s, in.TaskID)
		if err != nil {
			return err
		}
		if task["assigneeAgentId"] != participant.AgentID || task["status"] != "in_progress" || task["assigneeUserId"] != nil && task["assigneeUserId"] != "" {
			return sharedFail(409, "领取后的任务承接者已变化")
		}
		for _, key := range []string{"executionRunId", "checkoutRunId"} {
			if value := task[key]; value != nil && value != "" {
				return sharedFail(409, "任务已有未知原生执行")
			}
		}
		b := runtimeBinding{ParticipantID: participant.ID, ParticipantVersion: participant.Version, ID: id, SpaceID: in.SpaceID, TaskID: in.TaskID, Workspace: ws.Name, WorkspaceUID: string(ws.UID), Owner: i.UserID, Companion: companion, SourceConversationID: in.SourceConversationID, Generation: 1, ExecutionID: collaborationID(id, "1"), ExpectedVersion: 0, BindHash: hash, AssignmentHash: runtimeAssignment(task)}
		tx, err := c.BeginTx(r.Context(), nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(r.Context(), `INSERT INTO shared_space_runtime_bindings(id,space_id,task_id,workspace_name,workspace_uid,owner_id,companion,source_conversation_id,generation,execution_id,expected_version,bind_hash,assignment_hash,participant_id,participant_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, b.ID, b.SpaceID, b.TaskID, b.Workspace, b.WorkspaceUID, b.Owner, b.Companion, b.SourceConversationID, b.Generation, b.ExecutionID, b.ExpectedVersion, b.BindHash, b.AssignmentHash, b.ParticipantID, b.ParticipantVersion)
		if err != nil {
			return err
		}
		payload := map[string]any{"bindingId": id, "spaceId": b.SpaceID, "taskId": b.TaskID, "sourceConversationId": b.SourceConversationID, "generation": 1, "executionId": b.ExecutionID, "expectedVersion": 0, "title": in.Title, "instruction": in.Instruction}
		if err = runtimeEnqueue(r.Context(), tx, b, "start:"+id, "start", payload); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		writeJSON(w, 202, b)
		return nil
	}
	bindingID := r.URL.Query().Get("bindingId")
	var in struct {
		BindingID       string `json:"bindingId"`
		PiTaskID        string `json:"piTaskId"`
		Generation      int    `json:"generation"`
		ExecutionID     string `json:"executionId"`
		ExpectedVersion int    `json:"expectedVersion"`
		ArtifactID      string `json:"artifactId"`
		Title           string `json:"title"`
		Body            string `json:"body"`
		RequestID       string `json:"requestId"`
	}
	if r.Method == "POST" {
		if err := sharedDecode(w, r, &in); err != nil {
			return err
		}
		bindingID = in.BindingID
	}
	b, err := runtimeLoad(r.Context(), p.db, bindingID)
	if err != nil {
		return sharedFail(404, "执行不存在")
	}
	if b.Companion != companion {
		return sharedFail(403, "搭档不匹配")
	}
	if err = p.runtimeAuthorize(r.Context(), b, ws, i); err != nil {
		return err
	}
	if r.PathValue("action") == "space-status" && r.Method == "GET" {
		writeJSON(w, 200, b)
		return nil
	}
	if r.PathValue("action") == "space-receipt" && r.Method == "GET" {
		var hash, state string
		var raw []byte
		err = p.db.QueryRowContext(r.Context(), `SELECT payload_hash,state,receipt FROM shared_space_commands WHERE space_id=$1 AND request_id=$2 AND actor_id=$3`, b.SpaceID, r.URL.Query().Get("commandId"), i.UserID).Scan(&hash, &state, &raw)
		if err != nil {
			return sharedFail(404, "receipt absent; execution not disproved")
		}
		writeJSON(w, 200, map[string]any{"commandId": r.URL.Query().Get("commandId"), "payloadHash": hash, "state": state, "receipt": json.RawMessage(raw)})
		return nil
	}
	if r.PathValue("action") != "space-submit" || r.Method != "POST" {
		return sharedFail(404, "not found")
	}
	if b.PiTaskID != in.PiTaskID || b.Generation != in.Generation || b.ExecutionID != in.ExecutionID || b.ExpectedVersion != in.ExpectedVersion {
		return sharedFail(409, "执行代次已变化")
	}
	remote, err := p.collaborationCall(r.Context(), ws, companion, "/api/tasks/"+url.PathEscape(b.PiTaskID), nil)
	if err != nil {
		return err
	}
	t, _ := remote["task"].(map[string]any)
	meta, _ := t["sharedRuntime"].(map[string]any)
	if t["status"] != "completed" || t["executionId"] != b.ExecutionID || t["generation"] != float64(b.Generation) || meta["bindingId"] != b.ID || t["selectedArtifactId"] != in.ArtifactID {
		return sharedFail(409, "成果尚未由当前执行确认")
	}
	artifact, err := p.collaborationCall(r.Context(), ws, companion, "/internal/shared-space/artifact?id="+url.QueryEscape(in.ArtifactID), nil)
	if err != nil {
		return err
	}
	if artifact["taskId"] != b.PiTaskID || artifact["body"] != in.Body || artifact["title"] != in.Title {
		return sharedFail(409, "成果内容不匹配")
	}
	status, receipt, err := p.sharedSubmitInput(r.Context(), b.SpaceID, b.TaskID, i, sharedSubmission{RequestID: in.RequestID, ExpectedVersion: b.ExpectedVersion, Title: in.Title, Body: in.Body}, func(ctx context.Context, c *sql.Conn) error {
		current, err := runtimeLoad(ctx, c, b.ID)
		if err != nil {
			return err
		}
		if current.Generation != in.Generation || current.ExecutionID != in.ExecutionID || current.PiTaskID != in.PiTaskID {
			return sharedFail(409, "执行代次已变化")
		}
		var fresh dev.PersonalWorkspace
		if err = p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: current.Workspace}, &fresh); err != nil {
			return err
		}
		return p.runtimeAuthorize(ctx, current, &fresh, i)
	})
	if err != nil {
		return err
	}
	writeJSON(w, status, map[string]any{"commandId": in.RequestID, "payloadHash": sharedHash([]any{b.TaskID, sharedSubmission{RequestID: in.RequestID, ExpectedVersion: b.ExpectedVersion, Title: in.Title, Body: in.Body}}), "state": "applied", "receipt": receipt})
	return nil
}
func (p *portal) runSharedRuntime(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.processSharedRuntime(ctx)
			}
		}
	}()
}
func (p *portal) processSharedRuntime(ctx context.Context) {
	if p.db == nil {
		return
	}
	var id, bid, action, state, hash string
	var raw []byte
	err := p.db.QueryRowContext(ctx, `SELECT command_id,binding_id,action,state,payload,payload_hash FROM shared_space_runtime_outbox WHERE state IN ('pending','unknown') AND next_attempt_at<=now() ORDER BY next_attempt_at,created_at,command_id LIMIT 1`).Scan(&id, &bid, &action, &state, &raw, &hash)
	if err != nil {
		return
	}
	b, err := runtimeLoad(ctx, p.db, bid)
	if err != nil {
		return
	}
	c, release, err := p.sharedLock(ctx, b.SpaceID)
	if err != nil {
		return
	}
	defer release()
	b, err = runtimeLoad(ctx, c, bid)
	if err != nil {
		return
	}
	_, _ = c.ExecContext(ctx, `UPDATE shared_space_runtime_outbox SET next_attempt_at=now()+interval '10 seconds' WHERE command_id=$1`, id)
	var current string
	if c.QueryRowContext(ctx, `SELECT state FROM shared_space_runtime_outbox WHERE command_id=$1`, id).Scan(&current) != nil || current == "applied" || current == "blocked" {
		return
	}
	var ws dev.PersonalWorkspace
	if p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: b.Workspace}, &ws) != nil {
		return
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return
	}
	ident := identity{UserID: b.Owner, DeploymentID: p.cfg.aepDeploymentID}
	if authErr := p.runtimeAuthorize(ctx, b, &ws, ident); authErr != nil {
		if rejected, ok := authErr.(*sharedError); ok && (rejected.code == 403 || rejected.code == 404 || rejected.code == 409) {
			_, _ = c.ExecContext(ctx, `UPDATE shared_space_runtime_outbox SET state='blocked' WHERE command_id=$1`, id)
		}
		return
	}
	if action != "return" && payload["executionId"] != b.ExecutionID {
		_, _ = c.ExecContext(ctx, `UPDATE shared_space_runtime_outbox SET state='blocked' WHERE command_id=$1`, id)
		return
	}
	if action != "return" {
		space, authErr := p.sharedRuntimeSpace(ctx, b.SpaceID)
		if authErr != nil {
			return
		}
		task, taskErr := p.sharedCheckTask(ctx, space, b.TaskID)
		if taskErr != nil {
			return
		}
		if task["status"] == "done" || task["status"] == "cancelled" {
			_, _ = c.ExecContext(ctx, `UPDATE shared_space_runtime_outbox SET state='blocked' WHERE command_id=$1`, id)
			return
		}
		if runtimeCommandGate(ctx, c, b.SpaceID, b.TaskID) != nil {
			return
		}
	}
	var receipt map[string]any
	if current == "unknown" {
		receipt, err = p.collaborationCall(ctx, &ws, b.Companion, "/internal/shared-space/receipt?commandId="+url.QueryEscape(id), nil)
	} else {
		_, err = c.ExecContext(ctx, `UPDATE shared_space_runtime_outbox SET state='unknown' WHERE command_id=$1`, id)
		if err != nil {
			return
		}
		receipt, err = p.collaborationCall(ctx, &ws, b.Companion, "/internal/shared-space/"+action, map[string]any{"commandId": id, "payloadHash": hash, "payload": payload})
	}
	if err != nil || receipt["commandId"] != id || receipt["payloadHash"] != hash || receipt["state"] != "applied" {
		return
	}
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	if action == "start" {
		t, _ := receipt["task"].(map[string]any)
		taskID, _ := t["id"].(string)
		if taskID == "" || t["executionId"] != b.ExecutionID || t["generation"] != float64(b.Generation) {
			return
		}
		if _, err = tx.ExecContext(ctx, `UPDATE shared_space_runtime_bindings SET pi_task_id=$2 WHERE id=$1 AND pi_task_id=''`, b.ID, taskID); err != nil {
			return
		}
	}
	encoded, _ := json.Marshal(receipt)
	if _, err = tx.ExecContext(ctx, `UPDATE shared_space_runtime_outbox SET state='applied',receipt=$2 WHERE command_id=$1`, id, string(encoded)); err == nil {
		_ = tx.Commit()
	}
}

func runtimeCommandGate(ctx context.Context, c *sql.Conn, space, task string) error {
	var blocked bool
	err := c.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM shared_space_commands WHERE space_id=$1 AND task_id=$2 AND state<>'applied')`, space, task).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return sharedFail(423, "任务存在待核验命令")
	}
	return nil
}

func runtimeAssignment(task map[string]any) string {
	return sharedHash([]any{task["assigneeUserId"], task["assigneeAgentId"]})
}
