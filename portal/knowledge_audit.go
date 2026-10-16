package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type knowledgeAuditRecord struct {
	Cursor           string    `json:"cursor"`
	OperationID      string    `json:"operationId"`
	ActorUserID      string    `json:"actorUserId"`
	Action           string    `json:"action"`
	ResourceType     string    `json:"resourceType"`
	ResourceID       string    `json:"resourceId"`
	ParentResourceID string    `json:"parentResourceId"`
	Outcome          string    `json:"outcome"`
	StatusCode       int       `json:"statusCode"`
	CreatedAt        time.Time `json:"createdAt"`
}

// Audits remain readable when the upstream is down. Tenant and deployment
// always come from configuration and the freshly authenticated administrator.
func (p *portal) handleKnowledgeAudit(w http.ResponseWriter, r *http.Request, ident identity) {
	if !ident.isAdmin(p.cfg) || !p.managedReadyIdentity(ident) {
		p.managedWriteError(w, 403, "FORBIDDEN", "无权读取知识审计", "")
		return
	}
	if p.db == nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "知识审计暂不可用", "")
		return
	}
	where := []string{"deployment_id=$1", "weknora_tenant_id=$2"}
	args := []any{ident.DeploymentID, p.cfg.managedKnowledge.tenantID}
	fields := map[string]string{"operationId": "operation_id", "actorUserId": "actor_user_id", "action": "action", "resourceId": "resource_id", "outcome": "outcome"}
	pageSize := 50
	for key, values := range r.URL.Query() {
		if len(values) != 1 || len(values[0]) > 256 {
			p.managedWriteError(w, 400, "INVALID_QUERY", "审计筛选无效", "")
			return
		}
		value := values[0]
		if column, ok := fields[key]; ok {
			if value != "" {
				args = append(args, value)
				where = append(where, fmt.Sprintf("%s=$%d", column, len(args)))
			}
			continue
		}
		switch key {
		case "knowledgeBaseId":
			if value == "" {
				continue
			}
			if _, valid := safeKnowledgeID(value); !valid {
				p.managedWriteError(w, 400, "INVALID_QUERY", "审计筛选无效", "")
				return
			}
			// 库级操作把库 ID 写在 resource_id，文档级操作写在 parent_resource_id：
			// 按库筛选必须两者都算，否则「查看本库操作记录」会漏掉文档相关动作。
			args = append(args, value)
			where = append(where, fmt.Sprintf("(resource_id=$%d OR parent_resource_id=$%d)", len(args), len(args)))
		case "cursor":
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id <= 0 {
				p.managedWriteError(w, 400, "INVALID_QUERY", "审计游标无效", "")
				return
			}
			args = append(args, id)
			where = append(where, fmt.Sprintf("id<$%d", len(args)))
		case "limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 100 {
				p.managedWriteError(w, 400, "INVALID_QUERY", "审计分页无效", "")
				return
			}
			pageSize = n
		default:
			p.managedWriteError(w, 400, "INVALID_QUERY", "审计筛选无效", "")
			return
		}
	}
	args = append(args, pageSize+1)
	rows, err := p.db.QueryContext(r.Context(), `SELECT id,operation_id,actor_user_id,action,resource_type,resource_id,parent_resource_id,outcome,status_code,created_at FROM knowledge_audit_events WHERE `+strings.Join(where, " AND ")+fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "知识审计读取失败", "")
		return
	}
	defer func() { _ = rows.Close() }()
	items := make([]knowledgeAuditRecord, 0)
	var next *string
	for rows.Next() {
		var id int64
		var item knowledgeAuditRecord
		if err := rows.Scan(&id, &item.OperationID, &item.ActorUserID, &item.Action, &item.ResourceType, &item.ResourceID, &item.ParentResourceID, &item.Outcome, &item.StatusCode, &item.CreatedAt); err != nil {
			p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "知识审计读取失败", "")
			return
		}
		if len(items) == pageSize {
			value := items[len(items)-1].Cursor
			next = &value
			break
		}
		item.Cursor = strconv.FormatInt(id, 10)
		items = append(items, item)
	}
	if rows.Err() != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "知识审计读取失败", "")
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "nextCursor": next})
}
