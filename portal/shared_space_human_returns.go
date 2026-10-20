package main

import (
	"context"
	"database/sql"
	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// Only an applied handoff to this human can carry a source conversation forward.
// No binding ordering can substitute for that recorded provenance.
func sharedHumanReturnSource(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, space, task, user string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT source_binding_id FROM (SELECT source_binding_id,target_user_id FROM shared_space_handoffs WHERE space_id=$1 AND task_id=$2 AND phase='applied' ORDER BY epoch DESC LIMIT 1) current_handoff WHERE target_user_id=$3`, space, task, user).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}
func (p *portal) sharedHumanReturnEnqueue(ctx context.Context, tx *sql.Tx, space, task string, result sharedResult) error {
	id, err := sharedHumanReturnSource(ctx, tx, space, task, result.SubmittedBy)
	if err != nil || id == "" {
		return err
	}
	b, err := runtimeLoad(ctx, tx, id)
	if err != nil {
		return err
	}
	if b.SpaceID != space || b.TaskID != task || b.SourceConversationID <= 0 || b.BindingState != "retired" {
		return sharedFail(409, "成果回送来源无法核验")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO shared_space_result_bindings(result_id,binding_id) VALUES($1,$2)`, result.ID, id); err != nil {
		return err
	}
	return p.sharedRuntimeEnqueue(ctx, tx, space, task, result, "human_submitted", "")
}

// Notification authority survives retirement; execution authority never does.
func (p *portal) runtimeReturnAuthorize(ctx context.Context, b runtimeBinding, ws *dev.PersonalWorkspace, i identity, _ ...*map[string]any) error {
	if b.Workspace != ws.Name || b.WorkspaceUID != string(ws.UID) || b.Owner != ws.Spec.Owner || b.SourceConversationID <= 0 {
		return sharedFail(403, "通知来源已失效")
	}
	owner, ok, err := newReferenceResolver(p, ctx).lookupUser(b.Owner)
	if err != nil {
		return err
	}
	if !ok || owner.Kind != "human" || owner.Status != "active" {
		return sharedFail(403, "账号不可用")
	}
	found := false
	for _, c := range ws.Spec.Companions {
		if c.Name == b.Companion {
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
	_, grantErr := p.participantRuntimeGrant(ctx, s, b.Workspace, b.Companion, b.WorkspaceUID, b.Owner, false)
	if grantErr != nil {
		if _, err = p.sharedLoad(ctx, p.db, b.SpaceID, i); err != nil {
			return err
		}
	}
	_, err = p.sharedCheckTask(ctx, s, b.TaskID)
	return err
}

func (p *portal) sharedRuntimeReturnLinked(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, b runtimeBinding, payload map[string]any) bool {
	if payload["bindingId"] != b.ID || payload["conversationId"] != float64(b.SourceConversationID) {
		return false
	}
	result, ok := payload["resultId"].(string)
	if !ok || result == "" {
		return false
	}
	var linked bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM shared_space_result_bindings rb JOIN shared_space_results r ON r.id=rb.result_id WHERE rb.result_id=$1 AND rb.binding_id=$2 AND r.space_id=$3 AND r.task_id=$4 AND r.revision_id=$5)`, result, b.ID, b.SpaceID, b.TaskID, payload["revisionId"]).Scan(&linked)
	return err == nil && linked
}
