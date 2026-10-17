package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	knowledgeContextHeader = "X-DeerFlow-Knowledge-Context"
	knowledgeContextTTL    = 5 * time.Minute
)

var errKnowledgeContextUnavailable = errors.New("knowledge runtime context unavailable")

func knowledgeRuntimeAction(value string) string {
	allowed := map[string]string{
		"search": "runtime_search", "list_bases": "runtime_list_bases", "read_base": "runtime_read_base",
		"list_documents": "runtime_list_documents", "read_document": "runtime_read_document",
		"read_chunks": "runtime_read_chunks", "wiki_search": "runtime_wiki_search",
		"wiki_read_page": "runtime_wiki_read_page", "wiki_index": "runtime_wiki_index",
		"runtime_read": "runtime_read",
	}
	return allowed[value]
}

func (p *portal) auditKnowledgeRuntime(ctx context.Context, requester identity, tenant, action, resourceType, resourceID, parentResourceID string) error {
	op, err := p.beginManagedAudit(ctx, requester, tenant, action, resourceType, resourceID, parentResourceID)
	if err != nil {
		return err
	}
	return p.finishManagedAudit(ctx, op, resourceID, "succeeded", http.StatusOK)
}

// Runtime callbacks may finish only the operation started by this employee and
// this exact opaque run context. Legacy/admin audit rows have empty bindings
// and deliberately remain unavailable to the runtime completion endpoint.
func (p *portal) beginKnowledgeRuntimeAudit(ctx context.Context, requester identity, tenant, employee, contextHash, action, resourceType, resourceID, parentResourceID string, knowledgeBaseIDs ...string) (string, error) {
	validAction := false
	for _, input := range []string{"search", "list_bases", "read_base", "list_documents", "read_document", "read_chunks", "wiki_search", "wiki_read_page", "wiki_index", "runtime_read"} {
		if action == knowledgeRuntimeAction(input) {
			validAction = true
			break
		}
	}
	if p == nil || p.db == nil || tenant == "" || tenant != p.cfg.managedKnowledge.tenantID || !namePattern.MatchString(employee) || len(contextHash) != sha256.Size*2 || !validAction || requester.UserID == "" || !p.managedReadyIdentity(requester) {
		return "", errors.New("invalid knowledge runtime audit context")
	}
	op := "kbrtop-" + randomSessionToken()
	if len(knowledgeBaseIDs) > 0 {
		bases, err := json.Marshal(knowledgeBaseIDs)
		if err != nil {
			return "", err
		}
		_, err = p.db.ExecContext(ctx, `INSERT INTO knowledge_audit_events (operation_id,deployment_id,weknora_tenant_id,actor_user_id,action,resource_type,resource_id,parent_resource_id,runtime_employee_name,runtime_context_hash,knowledge_base_ids,outcome,status_code) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,'pending',0)`, op, requester.DeploymentID, tenant, requester.UserID, action, resourceType, resourceID, parentResourceID, employee, contextHash, string(bases))
		return op, err
	}
	_, err := p.db.ExecContext(ctx, `INSERT INTO knowledge_audit_events (operation_id,deployment_id,weknora_tenant_id,actor_user_id,action,resource_type,resource_id,parent_resource_id,runtime_employee_name,runtime_context_hash,outcome,status_code) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'pending',0)`, op, requester.DeploymentID, tenant, requester.UserID, action, resourceType, resourceID, parentResourceID, employee, contextHash)
	return op, err
}

func (p *portal) completeKnowledgeRuntimeAudit(ctx context.Context, requester identity, employee, contextHash, operationID, outcome string, statusCode int) error {
	validResult := (outcome == "succeeded" && statusCode >= 200 && statusCode < 300) || (outcome == "failed" && statusCode >= 400 && statusCode <= 599)
	if p == nil || p.db == nil || p.cfg.managedKnowledge.tenantID == "" || requester.UserID == "" || !p.managedReadyIdentity(requester) || !namePattern.MatchString(employee) || len(contextHash) != sha256.Size*2 || operationID == "" || !validResult {
		return errors.New("invalid knowledge runtime audit completion")
	}
	result, err := p.db.ExecContext(ctx, `UPDATE knowledge_audit_events SET outcome=$4,status_code=$5 WHERE operation_id=$1 AND deployment_id=$2 AND actor_user_id=$3 AND outcome='pending' AND weknora_tenant_id=$6 AND runtime_employee_name=$7 AND runtime_context_hash=$8 AND action IN ('runtime_search','runtime_list_bases','runtime_read_base','runtime_list_documents','runtime_read_document','runtime_read_chunks','runtime_wiki_search','runtime_wiki_read_page','runtime_wiki_index','runtime_read')`, operationID, requester.DeploymentID, requester.UserID, outcome, statusCode, p.cfg.managedKnowledge.tenantID, employee, contextHash)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return errors.New("knowledge runtime audit operation is unavailable")
	}
	return nil
}

// issueKnowledgeRuntimeContext creates a short-lived opaque handle bound to
// the authenticated Portal cookie session and one employee. Only the hash is
// used for lookup; no AEP token is sent to or stored by the runtime.
func (p *portal) issueKnowledgeRuntimeContext(ctx context.Context, sessionToken, employee string) (string, error) {
	if p == nil || p.db == nil || strings.TrimSpace(sessionToken) == "" || strings.TrimSpace(employee) == "" {
		return "", errKnowledgeContextUnavailable
	}
	handle := randomSessionToken()
	digest := sha256.Sum256([]byte(handle))
	_, err := p.db.ExecContext(ctx, `INSERT INTO knowledge_runtime_contexts (handle_hash,session_token,employee_name,expires_at) VALUES ($1,$2,$3,$4)`,
		hex.EncodeToString(digest[:]), sessionToken, employee, time.Now().Add(knowledgeContextTTL).UTC())
	if err != nil {
		return "", errKnowledgeContextUnavailable
	}
	_, _ = p.db.ExecContext(ctx, `DELETE FROM knowledge_runtime_contexts WHERE expires_at < now()`)
	return handle, nil
}

// handleKnowledgeRuntimeAuthorize is an internal service-to-service gate
// intended to run immediately at the MCP execution boundary. The short-lived
// high-entropy opaque handle binds the request to the Portal session and one
// employee. The resolved grant set is recomputed from live AEP/CR/DB state.
func (p *portal) handleKnowledgeRuntimeAuthorize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	employeeName := strings.TrimSpace(r.PathValue("name"))
	if !namePattern.MatchString(employeeName) || p.k8s == nil || p.db == nil || p.sessions == nil {
		p.managedWriteError(w, http.StatusUnauthorized, "KNOWLEDGE_CONTEXT_UNAVAILABLE", "知识库运行上下文不可用", "")
		return
	}
	// The 256-bit opaque handle is the only credential available to the
	// independent WeKnora MCP service. It is bound to employee + cookie
	// session in Portal storage and expires quickly; no AEP token or runtime
	// internal token crosses the service boundary.
	handle := strings.TrimSpace(r.Header.Get(knowledgeContextHeader))
	if len(handle) != 43 {
		p.managedWriteError(w, http.StatusUnauthorized, "KNOWLEDGE_CONTEXT_UNAVAILABLE", "知识库运行上下文不可用", "")
		return
	}
	digest := sha256.Sum256([]byte(handle))
	contextHash := hex.EncodeToString(digest[:])
	var sessionToken string
	err := p.db.QueryRowContext(r.Context(), `SELECT session_token FROM knowledge_runtime_contexts WHERE handle_hash=$1 AND employee_name=$2 AND expires_at > now()`, contextHash, employeeName).Scan(&sessionToken)
	if errors.Is(err, sql.ErrNoRows) {
		p.managedWriteError(w, http.StatusUnauthorized, "KNOWLEDGE_CONTEXT_UNAVAILABLE", "知识库运行上下文不可用", "")
		return
	}
	if err != nil {
		p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_AUTHORIZATION_UNAVAILABLE", "知识库授权校验暂不可用", "")
		return
	}
	requester, _, ok := p.sessions.get(sessionToken)
	if !ok || !p.knowledgeSessionActive(r.Context(), sessionToken, requester.UserID) {
		p.managedWriteError(w, http.StatusUnauthorized, "KNOWLEDGE_CONTEXT_UNAVAILABLE", "知识库运行上下文不可用", "")
		return
	}
	var input struct {
		KnowledgeBaseIDs []string `json:"knowledge_base_ids"`
		DocumentID       string   `json:"document_id"`
		RequireAll       *bool    `json:"require_all"`
		Action           string   `json:"action"`
		OperationID      string   `json:"operation_id"`
		Outcome          string   `json:"outcome"`
		StatusCode       int      `json:"status_code"`
	}
	if err := decodeStrictJSON(w, r, &input, 16<<10); err != nil {
		p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "知识库授权请求无效", "")
		return
	}
	if input.OperationID != "" {
		if input.DocumentID != "" || len(input.KnowledgeBaseIDs) != 0 || input.Action != "" || input.RequireAll != nil {
			p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "审计完成请求无效", "")
			return
		}
		if err := p.completeKnowledgeRuntimeAudit(r.Context(), requester, employeeName, contextHash, input.OperationID, input.Outcome, input.StatusCode); err != nil {
			p.managedWriteError(w, http.StatusConflict, "KNOWLEDGE_CONFLICT", "知识库审计操作不可用或与已有记录冲突", "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	action := knowledgeRuntimeAction(input.Action)
	if action == "" {
		p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "知识库操作类型无效", "")
		return
	}
	if input.DocumentID != "" {
		if len(input.KnowledgeBaseIDs) != 0 {
			p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "请求必须且只能指定资料或知识库", "")
			return
		}
		key, err := p.platformSecretKey(r.Context(), p.cfg.managedKnowledge.apiKeySecretField)
		if err != nil || p.cfg.managedKnowledge.tenantID == "" {
			p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_AUTHORIZATION_UNAVAILABLE", "知识库授权校验暂不可用", "")
			return
		}
		tenant, err := p.managedTenant(r.Context(), key)
		if err != nil || tenant != p.cfg.managedKnowledge.tenantID {
			p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_AUTHORIZATION_UNAVAILABLE", "知识库授权校验暂不可用", "")
			return
		}
		doc, found, err := p.managedFetchDocument(r.Context(), key, tenant, requester.DeploymentID, "", input.DocumentID)
		if err != nil {
			p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_AUTHORIZATION_UNAVAILABLE", "知识库授权校验暂不可用", "")
			return
		}
		if !found {
			p.managedWriteError(w, http.StatusForbidden, "KNOWLEDGE_ACCESS_DENIED", "所请求的资料不属于当前部署或租户", "")
			return
		}
		if err := p.authorizeEmployeeKnowledgeBase(r.Context(), requester, employeeName, doc.KnowledgeBaseID); err != nil {
			if errors.Is(err, errKnowledgeIdentityUnavailable) {
				p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_AUTHORIZATION_UNAVAILABLE", "知识库授权校验暂不可用", "")
			} else {
				p.managedWriteError(w, http.StatusForbidden, "KNOWLEDGE_NOT_GRANTED", "该数字员工没有绑定该知识库或无权访问", "")
			}
			return
		}
		op, err := p.beginKnowledgeRuntimeAudit(r.Context(), requester, tenant, employeeName, contextHash, action, "document", doc.ID, doc.KnowledgeBaseID)
		if err != nil {
			p.managedWriteError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "无法记录知识库操作审计", "")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"knowledge_base_id": doc.KnowledgeBaseID, "operation_id": op})
		return
	}
	if len(input.KnowledgeBaseIDs) == 0 || len(input.KnowledgeBaseIDs) > 100 {
		p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "knowledge_base_ids 必须是显式列表", "")
		return
	}
	seen := make(map[string]struct{}, len(input.KnowledgeBaseIDs))
	for _, id := range input.KnowledgeBaseIDs {
		if _, ok := safeKnowledgeID(id); !ok {
			p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "知识库 ID 无效", "")
			return
		}
		if _, duplicate := seen[id]; duplicate {
			p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "知识库 ID 重复", "")
			return
		}
		seen[id] = struct{}{}
	}
	allowed, err := p.resolveEmployeeKnowledgeAccess(r.Context(), requester, employeeName, input.KnowledgeBaseIDs)
	if err != nil {
		p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_AUTHORIZATION_UNAVAILABLE", "知识库授权校验暂不可用", "")
		return
	}
	requireAll := input.RequireAll == nil || *input.RequireAll
	if (requireAll && len(allowed) != len(input.KnowledgeBaseIDs)) || len(allowed) == 0 {
		denied := managedError{}
		denied.Error.Code, denied.Error.Message = "KNOWLEDGE_NOT_GRANTED", "该数字员工没有绑定所请求的知识库"
		writeJSON(w, http.StatusForbidden, map[string]any{"error": denied.Error, "knowledge_base_ids": []string{}})
		return
	}
	resourceID := strings.Join(allowed, ",")
	op, err := p.beginKnowledgeRuntimeAudit(r.Context(), requester, p.cfg.managedKnowledge.tenantID, employeeName, contextHash, action, "knowledge_scope", resourceID, "", allowed...)
	if err != nil {
		p.managedWriteError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "无法记录知识库操作审计", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"knowledge_base_ids": allowed, "operation_id": op})
}
