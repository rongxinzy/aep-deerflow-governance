package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
)

func sharedDependencyContext(deps sharedDependencies) []map[string]any {
	out := []map[string]any{}
	for _, dep := range deps.BlockedBy {
		var result any
		if r := dep.AcceptedResult; r != nil {
			result = map[string]any{"id": r.ID, "version": r.Version, "title": r.Title, "revisionId": r.RevisionID, "body": r.Body}
		}
		out = append(out, map[string]any{"taskId": dep.ID, "title": dep.Title, "status": dep.Status, "acceptedResult": result})
	}
	return out
}
func (p *portal) runtimeDependencyWaiting(ctx context.Context, b runtimeBinding, task map[string]any) (runtimeBinding, error) {
	ids, err := sharedBlockedIDs(task)
	if err != nil || len(ids) == 0 {
		return b, err
	}
	s, err := p.sharedRuntimeSpace(ctx, b.SpaceID)
	if err != nil {
		return b, err
	}
	deps, err := p.sharedReadDependencies(ctx, s, task)
	if err != nil {
		return b, err
	}
	b.WaitingDependencies = !deps.Ready
	b.WaitingReason = deps.WaitingReason
	if len(deps.BlockedBy) > 0 {
		b.Dependencies = &deps
	}
	return b, nil
}

// The task is already assigned and bound while waiting. This checkout uses the
// existing durable command evidence, without bypassing the normal ownership,
// epoch or native dependency checks. Unknown results are never dispatched again.
func (p *portal) runtimeDependencyCheckout(ctx context.Context, c *sql.Conn, s sharedSpace, b runtimeBinding, task map[string]any) error {
	if task["status"] == "in_progress" {
		return nil
	}
	if task["status"] != "todo" && task["status"] != "backlog" {
		return sharedFail(409, "任务当前阶段不能继续领取")
	}
	request := "dependency-claim:" + b.ID + ":" + fmt.Sprint(b.Generation)
	hash := sharedHash([]any{b.ID, b.Generation, b.AssignmentEpoch, b.AssignmentHash})
	if _, ok, err := sharedReplay(ctx, c, s.ID, request, b.Owner, "dependency_claim", hash); err != nil {
		return err
	} else if ok {
		return sharedFail(409, "领取后的任务状态已变化，请核验")
	}
	body := map[string]any{"agentId": task["assigneeAgentId"], "expectedStatuses": []any{task["status"]}}
	if err := sharedStart(ctx, c, s.ID, b.TaskID, request, b.Owner, "dependency_claim", hash, map[string]any{"bindingId": b.ID, "generation": b.Generation, "method": "POST", "path": "/issues/" + url.PathEscape(b.TaskID) + "/checkout", "body": body}); err != nil {
		return err
	}
	var updated map[string]any
	err := p.sharedPC(ctx, "POST", "/issues/"+url.PathEscape(b.TaskID)+"/checkout", body, &updated)
	if err == nil {
		updated, err = p.sharedCheckTask(ctx, s, b.TaskID)
	}
	if err != nil || updated["status"] != "in_progress" || runtimeAssignment(updated) != b.AssignmentHash {
		_ = sharedUnknown(c, s.ID, request)
		return sharedFail(423, "依赖就绪后的领取结果待核验，请勿重复执行")
	}
	tx, err := c.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = sharedFinish(ctx, tx, s.ID, request, map[string]any{"state": "applied", "task": sharedTaskDTO(updated)}); err != nil {
		return err
	}
	return tx.Commit()
}

// A successful native checkout can outlive its HTTP acknowledgement or local
// receipt commit. Reconcile only this binding's durable command, by reading its
// unchanged assignee and running state; never send the checkout again.
func (p *portal) runtimeRecoverDependencyCheckout(ctx context.Context, c *sql.Conn, s sharedSpace, b runtimeBinding, task map[string]any) error {
	ids, err := sharedBlockedIDs(task)
	if err != nil || len(ids) == 0 || task["status"] != "in_progress" {
		return err
	}
	request := "dependency-claim:" + b.ID + ":" + fmt.Sprint(b.Generation)
	var actor, action, hash, state string
	var expected []byte
	err = c.QueryRowContext(ctx, `SELECT actor_id,action,payload_hash,state,expected FROM shared_space_commands WHERE space_id=$1 AND request_id=$2`, s.ID, request).Scan(&actor, &action, &hash, &state, &expected)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil || state == "applied" {
		return err
	}
	var intent struct {
		BindingID  string `json:"bindingId"`
		Generation int    `json:"generation"`
	}
	if json.Unmarshal(expected, &intent) != nil || intent.BindingID != b.ID || intent.Generation != b.Generation || actor != b.Owner || action != "dependency_claim" || hash != sharedHash([]any{b.ID, b.Generation, b.AssignmentEpoch, b.AssignmentHash}) || runtimeAssignment(task) != b.AssignmentHash {
		return sharedFail(423, "依赖领取证据尚无法核验")
	}
	return sharedDependencyFinish(ctx, c, s.ID, request, map[string]any{"state": "applied", "task": sharedTaskDTO(task)})
}

// Freeze accepted shared content before the existing start command is sent.
// No dependency copies become an independently editable business record.
func (p *portal) runtimePrepareDependencies(ctx context.Context, c *sql.Conn, s sharedSpace, b runtimeBinding, task map[string]any, id string, payload map[string]any) (bool, string, error) {
	deps, err := p.sharedReadDependencies(ctx, s, task)
	if err != nil {
		return false, "", err
	}
	if !deps.Ready {
		return false, "", nil
	}
	if len(deps.BlockedBy) == 0 {
		return true, "", nil
	}
	payload["dependencyContext"] = sharedDependencyContext(deps)
	raw, err := json.Marshal(payload)
	if err != nil {
		return false, "", err
	}
	hash := sharedHash(payload)
	if _, err = c.ExecContext(ctx, `UPDATE shared_space_runtime_outbox SET payload=$2,payload_hash=$3 WHERE command_id=$1 AND state='pending'`, id, string(raw), hash); err != nil {
		return false, "", err
	}
	if err = p.runtimeDependencyCheckout(ctx, c, s, b, task); err != nil {
		return false, "", err
	}
	return true, hash, nil
}
