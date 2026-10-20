package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
)

type sharedDependencyResult struct {
	ID         string `json:"id"`
	Version    int    `json:"version"`
	Title      string `json:"title"`
	RevisionID string `json:"revisionId"`
	Body       string `json:"-"`
}
type sharedDependency struct {
	ID             string                  `json:"id"`
	Title          string                  `json:"title"`
	Status         string                  `json:"status"`
	Satisfied      bool                    `json:"satisfied"`
	Reason         string                  `json:"reason"`
	AcceptedResult *sharedDependencyResult `json:"acceptedResult,omitempty"`
}
type sharedDependencies struct {
	Token         string             `json:"token"`
	Ready         bool               `json:"ready"`
	WaitingReason string             `json:"waitingReason"`
	BlockedBy     []sharedDependency `json:"blockedBy"`
}

func sharedBlockedIDs(task map[string]any) ([]string, error) {
	raw, err := json.Marshal(task["blockedBy"])
	if err != nil {
		return nil, err
	}
	var refs []struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(raw, &refs); err != nil {
		return nil, err
	}
	ids := []string{}
	for _, ref := range refs {
		if ref.ID == "" {
			return nil, sharedFail(502, "前置任务标识无法确认")
		}
		ids = append(ids, ref.ID)
	}
	sort.Strings(ids)
	return ids, nil
}
func sharedDependencyState(deps sharedDependencies) map[string]any {
	pending := 0
	for _, dep := range deps.BlockedBy {
		if !dep.Satisfied {
			pending++
		}
	}
	return map[string]any{"ready": deps.Ready, "pendingCount": pending, "totalCount": len(deps.BlockedBy), "waitingReason": deps.WaitingReason}
}
func (p *portal) sharedReadDependencies(ctx context.Context, s sharedSpace, task map[string]any) (sharedDependencies, error) {
	deps := sharedDependencies{Ready: true, BlockedBy: []sharedDependency{}}
	ids, err := sharedBlockedIDs(task)
	if err != nil {
		return deps, err
	}
	deps.Token = sharedHash([]any{task["id"], task["updatedAt"], ids})
	for _, id := range ids {
		source, err := p.sharedCheckTask(ctx, s, id)
		if err != nil {
			return deps, err
		}
		title, _ := source["title"].(string)
		status, _ := source["status"].(string)
		dep := sharedDependency{ID: id, Title: title, Status: status, Satisfied: status == "done"}
		if !dep.Satisfied {
			dep.Reason = "等待前置任务完成"
			if status == "cancelled" {
				dep.Reason = "前置任务已取消，请调整依赖"
			}
		}
		results, err := sharedResults(ctx, p.db, s.ID, id)
		if err != nil {
			return deps, err
		}
		if len(results) > 0 {
			latest := results[len(results)-1]
			if latest.State != "accepted" {
				dep.Satisfied = false
				dep.Reason = "前置任务的最新成果尚未接受"
			} else {
				var doc sharedDocument
				if err = p.sharedPC(ctx, "GET", "/issues/"+url.PathEscape(id)+"/documents/"+url.PathEscape(latest.DocumentKey), nil, &doc); err != nil {
					return deps, err
				}
				if doc.ID != latest.DocumentID || doc.LatestRevisionID != latest.RevisionID || sharedHash(doc.Body) != latest.BodyHash {
					dep.Satisfied = false
					dep.Reason = "前置成果版本已变化，等待核验"
				} else {
					dep.AcceptedResult = &sharedDependencyResult{ID: latest.ID, Version: latest.Version, Title: latest.Title, RevisionID: latest.RevisionID, Body: doc.Body}
				}
			}
		}
		if !dep.Satisfied {
			deps.Ready = false
			if deps.WaitingReason == "" {
				deps.WaitingReason = dep.Reason
			}
		}
		deps.BlockedBy = append(deps.BlockedBy, dep)
	}
	if len(ids) > 0 {
		var diagnostic struct {
			Issue struct {
				ID string `json:"id"`
			} `json:"issue"`
			Readiness *struct {
				Ready bool `json:"isDependencyReady"`
			} `json:"readiness"`
			Blockers []struct {
				ID      string `json:"id"`
				Ready   bool   `json:"isDependencyReady"`
				Pending bool   `json:"isPendingFinalize"`
			} `json:"blockers"`
		}
		id, _ := task["id"].(string)
		if err = p.sharedPC(ctx, "GET", "/issues/"+url.PathEscape(id)+"/diagnostics/blockers", nil, &diagnostic); err != nil {
			return deps, err
		}
		if diagnostic.Issue.ID != id {
			return deps, sharedFail(502, "前置任务诊断范围无法确认")
		}
		if diagnostic.Readiness == nil || !diagnostic.Readiness.Ready {
			deps.Ready = false
			for index := range deps.BlockedBy {
				for _, blocker := range diagnostic.Blockers {
					if blocker.ID == deps.BlockedBy[index].ID && !blocker.Ready && deps.BlockedBy[index].Satisfied {
						deps.BlockedBy[index].Satisfied = false
						deps.BlockedBy[index].Reason = "等待前置任务完成收尾"
					}
				}
			}
			if deps.WaitingReason == "" {
				deps.WaitingReason = "等待前置任务完成收尾"
			}
			if sharedDependencyState(deps)["pendingCount"] == 0 {
				for index := range deps.BlockedBy {
					deps.BlockedBy[index].Satisfied = false
					deps.BlockedBy[index].Reason = deps.WaitingReason
				}
			}
		}
	}
	return deps, nil
}
func (p *portal) sharedDependencyEditReason(ctx context.Context, s sharedSpace, task map[string]any) (string, error) {
	return p.sharedTaskEditReason(ctx, s, task, "依赖")
}
func (p *portal) sharedTaskEditReason(ctx context.Context, s sharedSpace, task map[string]any, field string) (string, error) {
	if !sharedCaps(s.Role)["createTask"] {
		return fmt.Sprintf("当前成员只能查看%s", field), nil
	}
	if task["status"] != "backlog" && task["status"] != "todo" {
		return fmt.Sprintf("仅未运行的任务可编辑%s", field), nil
	}
	for _, key := range []string{"executionRunId", "checkoutRunId"} {
		if task[key] != nil && task[key] != "" {
			return "任务已有原生执行", nil
		}
	}
	var bound bool
	err := p.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM shared_space_runtime_bindings WHERE space_id=$1 AND task_id=$2 AND binding_state='active')`, s.ID, task["id"]).Scan(&bound)
	if err != nil {
		return "", err
	}
	if bound {
		return fmt.Sprintf("任务已绑定搭档执行，不能编辑%s", field), nil
	}
	return "", nil
}
func (p *portal) sharedTaskDependencies(w http.ResponseWriter, r *http.Request, i identity) error {
	s, err := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	task, err := p.sharedCheckTask(r.Context(), s, r.PathValue("task"))
	if err != nil {
		return err
	}
	deps, err := p.sharedReadDependencies(r.Context(), s, task)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"dependencies": deps})
	return nil
}
