package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The managed API is deliberately a fixed adapter, not a general-purpose
// WeKnora proxy. Every route is admin-only and bound to the verified AEP
// deployment plus an explicitly configured WeKnora tenant.
type managedKB struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	TenantID         string `json:"tenant_id"`
	EmbeddingModelID string `json:"embedding_model_id"`
	Type             string `json:"type"`
}

func (kb *managedKB) UnmarshalJSON(raw []byte) error {
	type wire struct {
		ID               string          `json:"id"`
		Name             string          `json:"name"`
		Description      string          `json:"description"`
		TenantID         json.RawMessage `json:"tenant_id"`
		EmbeddingModelID string          `json:"embedding_model_id"`
		Type             string          `json:"type"`
	}
	var value wire
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	var tenant string
	if len(value.TenantID) > 0 {
		if err := json.Unmarshal(value.TenantID, &tenant); err != nil {
			tenant = strings.Trim(string(value.TenantID), `"`)
		}
	}
	*kb = managedKB{ID: value.ID, Name: value.Name, Description: value.Description, TenantID: tenant, EmbeddingModelID: value.EmbeddingModelID, Type: value.Type}
	return nil
}

type managedModel struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Parameters struct {
		Embedding struct {
			Dimension int `json:"dimension"`
		} `json:"embedding_parameters"`
	} `json:"parameters"`
}

type managedKnowledge struct {
	ID              string `json:"id"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
	Name            string `json:"name"`
	FileName        string `json:"file_name"`
	ParseStatus     string `json:"parse_status"`
	CreatedAt       string `json:"created_at"`
}

type managedError struct {
	Error struct {
		Code        string `json:"code"`
		Message     string `json:"message"`
		OperationID string `json:"operationId,omitempty"`
		ResourceID  string `json:"resourceId,omitempty"`
	} `json:"error"`
}

func (p *portal) managedReadyIdentity(ident identity) bool {
	return ident.DeploymentID != "" && ident.DeploymentID == p.cfg.aepDeploymentID
}

func (p *portal) managedWriteError(w http.ResponseWriter, status int, code, message, op string) {
	body := managedError{}
	body.Error.Code, body.Error.Message, body.Error.OperationID = code, message, op
	writeJSON(w, status, body)
}

func (p *portal) managedWriteErrorResource(w http.ResponseWriter, status int, code, message, op, resourceID string) {
	body := managedError{}
	body.Error.Code, body.Error.Message = code, message
	body.Error.OperationID, body.Error.ResourceID = op, resourceID
	writeJSON(w, status, body)
}

func (p *portal) managedContext(w http.ResponseWriter, r *http.Request, ident identity) (string, string, bool) {
	if !p.managedReadyIdentity(ident) {
		p.managedWriteError(w, http.StatusForbidden, "DEPLOYMENT_MISMATCH", "会话部署与当前服务不匹配", "")
		return "", "", false
	}
	if p.cfg.managedKnowledge.tenantID == "" || p.cfg.managedKnowledge.apiKeySecretField == "" {
		p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_NOT_CONFIGURED", "知识库服务尚未完成租户和凭据配置", "")
		return "", "", false
	}
	key, err := p.platformSecretKey(r.Context(), p.cfg.managedKnowledge.apiKeySecretField)
	if err != nil || strings.TrimSpace(key) == "" {
		p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_CREDENTIAL_UNAVAILABLE", "知识库服务凭据不可用", "")
		return "", "", false
	}
	tenant, err := p.managedTenant(r.Context(), key)
	if err != nil {
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "无法验证知识库服务租户", "")
		return "", "", false
	}
	if tenant != p.cfg.managedKnowledge.tenantID {
		p.managedWriteError(w, http.StatusForbidden, "KNOWLEDGE_TENANT_MISMATCH", "知识库服务租户与配置不匹配", "")
		return "", "", false
	}
	return key, tenant, true
}

func (p *portal) managedRequest(ctx context.Context, key, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	target := strings.TrimRight(p.cfg.knowledgeAPIURL, "/") + "/api/v1/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return knowledgeTransferClient.Do(req)
}

func (p *portal) managedTenant(ctx context.Context, key string) (string, error) {
	resp, err := p.managedRequest(ctx, key, http.MethodGet, "auth/me", nil, nil, "")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tenant verification rejected")
	}
	var body struct {
		Data struct {
			Tenant struct {
				ID json.Number `json:"id"`
			} `json:"tenant"`
		} `json:"data"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return "", err
	}
	if body.Data.Tenant.ID == "" || body.Data.Tenant.ID == "0" {
		return "", errors.New("tenant missing")
	}
	return body.Data.Tenant.ID.String(), nil
}

func decodeManagedData(resp *http.Response, out any) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("upstream data missing")
	}
	return json.Unmarshal(envelope.Data, out)
}

func (p *portal) managedRegistered(ctx context.Context, deployment, tenant, id string) (managedKB, bool, error) {
	var kb managedKB
	err := p.db.QueryRowContext(ctx, `SELECT name_snapshot FROM knowledge_base_registry WHERE deployment_id=$1 AND weknora_tenant_id=$2 AND knowledge_base_id=$3`, deployment, tenant, id).Scan(&kb.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return managedKB{}, false, nil
	}
	if err != nil {
		return managedKB{}, false, err
	}
	kb.ID, kb.TenantID = id, tenant
	return kb, true, nil
}

func (p *portal) managedFetchKB(ctx context.Context, key, tenant, deployment, id string) (managedKB, bool, error) {
	if _, ok := safeKnowledgeID(id); !ok {
		return managedKB{}, false, nil
	}
	if _, ok, err := p.managedRegistered(ctx, deployment, tenant, id); err != nil || !ok {
		return managedKB{}, ok, err
	}
	resp, err := p.managedRequest(ctx, key, http.MethodGet, "knowledge-bases/"+url.PathEscape(id), nil, nil, "")
	if err != nil {
		return managedKB{}, false, err
	}
	var result managedKB
	if err := decodeManagedData(resp, &result); err != nil {
		return managedKB{}, false, err
	}
	if result.ID != id || result.TenantID != tenant {
		return managedKB{}, false, errors.New("knowledge base tenant mismatch")
	}
	return result, true, nil
}

func safeKnowledgeID(id string) (string, bool) {
	if id == "" || len(id) > 128 || strings.ContainsAny(id, "/\\\x00") {
		return "", false
	}
	return id, true
}

func (p *portal) beginManagedAudit(ctx context.Context, ident identity, tenant, action, resourceType, resourceID, parentResourceID string) (string, error) {
	op := fmt.Sprintf("kbop-%d", time.Now().UnixNano())
	_, err := p.db.ExecContext(ctx, `INSERT INTO knowledge_audit_events (operation_id,deployment_id,weknora_tenant_id,actor_user_id,action,resource_type,resource_id,parent_resource_id,outcome,status_code) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'pending',0)`, op, ident.DeploymentID, tenant, ident.UserID, action, resourceType, resourceID, parentResourceID)
	return op, err
}

func (p *portal) finishManagedAudit(ctx context.Context, op, resourceID, outcome string, status int) error {
	result, err := p.db.ExecContext(ctx, `UPDATE knowledge_audit_events SET resource_id=$2,outcome=$3,status_code=$4 WHERE operation_id=$1`, op, resourceID, outcome, status)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errors.New("audit intent missing")
	}
	return nil
}

func (p *portal) rememberManagedAuditResource(ctx context.Context, op, resourceID string) error {
	result, err := p.db.ExecContext(ctx, `UPDATE knowledge_audit_events SET resource_id=$2 WHERE operation_id=$1`, op, resourceID)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return errors.New("audit intent missing")
	}
	return nil
}

func (p *portal) registerCreatedManagedBase(ctx context.Context, ident identity, tenant string, kb managedKB, op string) error {
	// Persist the upstream ID first so a later transaction failure is still
	// recoverable from the pending audit intent, then atomically commit the
	// registry row and successful audit outcome.
	if err := p.rememberManagedAuditResource(ctx, op, kb.ID); err != nil {
		return err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE knowledge_audit_events SET outcome='succeeded',status_code=201 WHERE operation_id=$1`, op); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO knowledge_base_registry (deployment_id,weknora_tenant_id,knowledge_base_id,name_snapshot,created_by) VALUES ($1,$2,$3,$4,$5)`, ident.DeploymentID, tenant, kb.ID, kb.Name, ident.UserID); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *portal) handleManagedKnowledgeReadiness(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return
	}
	modelsResp, err := p.managedRequest(r.Context(), key, http.MethodGet, "models", nil, nil, "")
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "无法读取模型配置", "")
		return
	}
	var models []managedModel
	if err := decodeManagedData(modelsResp, &models); err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "模型配置读取失败", "")
		return
	}
	var selected *managedModel
	for i := range models {
		if models[i].ID == p.cfg.managedKnowledge.embeddingModelID && strings.EqualFold(models[i].Type, "embedding") {
			selected = &models[i]
			break
		}
	}
	modelState := "not_configured"
	if p.cfg.managedKnowledge.embeddingModelID != "" {
		modelState = "unavailable"
		if selected != nil {
			modelState = "configured"
		}
	}
	writeJSON(w, 200, map[string]any{"deploymentId": ident.DeploymentID, "tenantId": tenant, "tenant": "verified", "embeddingModel": map[string]any{"state": modelState, "id": p.cfg.managedKnowledge.embeddingModelID, "availability": "unverified"}, "storage": "unverified", "parser": "unverified"})
}

func (p *portal) handleManagedKnowledgeBases(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return
	}
	rows, err := p.db.QueryContext(r.Context(), `SELECT knowledge_base_id,name_snapshot,created_by,created_at,updated_at FROM knowledge_base_registry WHERE deployment_id=$1 AND weknora_tenant_id=$2 ORDER BY created_at DESC`, ident.DeploymentID, tenant)
	if err != nil {
		p.managedWriteError(w, 500, "REGISTRY_UNAVAILABLE", "知识库目录暂不可用", "")
		return
	}
	defer func() { _ = rows.Close() }()
	result := make([]managedKB, 0)
	for rows.Next() {
		var kb managedKB
		var by string
		var created, updated time.Time
		if err := rows.Scan(&kb.ID, &kb.Name, &by, &created, &updated); err != nil {
			p.managedWriteError(w, 500, "REGISTRY_UNAVAILABLE", "知识库目录读取失败", "")
			return
		}
		kb.TenantID = tenant
		result = append(result, kb)
	}
	if err := rows.Err(); err != nil {
		p.managedWriteError(w, 500, "REGISTRY_UNAVAILABLE", "知识库目录读取失败", "")
		return
	}
	// Validate each registry entry against upstream; drifted resources fail closed.
	for i := range result {
		resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "knowledge-bases/"+url.PathEscape(result[i].ID), nil, nil, "")
		if err != nil {
			p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库服务暂不可用", "")
			return
		}
		var live managedKB
		if err := decodeManagedData(resp, &live); err != nil || live.ID != result[i].ID || live.TenantID != tenant {
			p.managedWriteError(w, 409, "KNOWLEDGE_TENANT_MISMATCH", "已登记知识库与上游租户不一致", "")
			return
		}
		result[i].Name = live.Name
		result[i].Description = live.Description
		result[i].Type = live.Type
		result[i].EmbeddingModelID = live.EmbeddingModelID
	}
	writeJSON(w, 200, map[string]any{"data": result})
}

func (p *portal) handleManagedKnowledgeGetBase(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return
	}
	kb, found, err := p.managedFetchKB(r.Context(), key, tenant, ident.DeploymentID, r.PathValue("baseID"))
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库读取失败", "")
		return
	}
	if !found {
		p.managedWriteError(w, 404, "KNOWLEDGE_BASE_NOT_FOUND", "未找到已登记知识库", "")
		return
	}
	writeJSON(w, 200, map[string]any{"data": kb})
}

func (p *portal) handleManagedKnowledgeCreateBase(w http.ResponseWriter, r *http.Request, ident identity) {
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeStrictJSON(w, r, &in, 16<<10); err != nil || strings.TrimSpace(in.Name) == "" || len(in.Name) > 200 || len(in.Description) > 2000 {
		p.managedWriteError(w, 400, "INVALID_REQUEST", "知识库名称或描述无效", "")
		return
	}
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return
	}
	if p.cfg.managedKnowledge.embeddingModelID == "" {
		p.managedWriteError(w, 422, "KNOWLEDGE_MODEL_UNAVAILABLE", "尚未配置可用的 Embedding 模型", "")
		return
	}
	modelsResp, err := p.managedRequest(r.Context(), key, http.MethodGet, "models", nil, nil, "")
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "模型预检失败", "")
		return
	}
	var models []managedModel
	if decodeManagedData(modelsResp, &models) != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "模型预检失败", "")
		return
	}
	valid := false
	for _, m := range models {
		if m.ID == p.cfg.managedKnowledge.embeddingModelID && strings.EqualFold(m.Type, "embedding") {
			valid = true
			break
		}
	}
	if !valid {
		p.managedWriteError(w, 422, "KNOWLEDGE_MODEL_UNAVAILABLE", "配置的 Embedding 模型不存在或类型不匹配", "")
		return
	}
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, "create", "knowledge-base", "", "")
	if err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法持久化操作审计，未创建知识库", "")
		return
	}
	input := map[string]any{"name": strings.TrimSpace(in.Name), "description": strings.TrimSpace(in.Description), "type": "document", "embedding_model_id": p.cfg.managedKnowledge.embeddingModelID, "chunking_config": map[string]any{"chunk_size": 512, "chunk_overlap": 0, "separators": []string{"\n"}, "enable_multimodal": false}, "question_generation_config": map[string]any{"enabled": false}}
	payload, _ := json.Marshal(input)
	resp, err := p.managedRequest(r.Context(), key, http.MethodPost, "knowledge-bases", nil, bytes.NewReader(payload), "application/json")
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, "", "uncertain", 502)
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "创建结果不确定，请使用操作编号核对", op)
		return
	}
	var created managedKB
	if decodeManagedData(resp, &created) != nil || created.ID == "" || created.TenantID != tenant || created.Name != strings.TrimSpace(in.Name) {
		_ = p.finishManagedAudit(r.Context(), op, created.ID, "unverified", 502)
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "上游创建结果无法安全验证，请使用操作编号核对", op)
		return
	}
	if err := p.registerCreatedManagedBase(r.Context(), ident, tenant, created, op); err != nil {
		p.managedWriteErrorResource(w, 503, "REGISTRY_WRITE_FAILED", "知识库已创建但登记失败，请使用操作编号与资源编号核对", op, created.ID)
		return
	}
	writeJSON(w, 201, map[string]any{"data": created, "operationId": op})
}

func decodeStrictJSON(w http.ResponseWriter, r *http.Request, out any, limit int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

func (p *portal) handleManagedKnowledgeUpdateBase(w http.ResponseWriter, r *http.Request, ident identity) {
	var in struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if decodeStrictJSON(w, r, &in, 16<<10) != nil || (in.Name == nil && in.Description == nil) || (in.Name != nil && (strings.TrimSpace(*in.Name) == "" || len(*in.Name) > 200)) || (in.Description != nil && len(*in.Description) > 2000) {
		p.managedWriteError(w, 400, "INVALID_REQUEST", "更新字段无效", "")
		return
	}
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return
	}
	id := r.PathValue("baseID")
	kb, found, err := p.managedFetchKB(r.Context(), key, tenant, ident.DeploymentID, id)
	if err != nil || !found {
		p.managedWriteError(w, 404, "KNOWLEDGE_BASE_NOT_FOUND", "未找到已登记知识库", "")
		return
	}
	if in.Name != nil {
		kb.Name = strings.TrimSpace(*in.Name)
	}
	if in.Description != nil {
		kb.Description = strings.TrimSpace(*in.Description)
	}
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, "update", "knowledge-base", id, "")
	if err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法持久化操作审计，未更新知识库", "")
		return
	}
	payload, _ := json.Marshal(map[string]any{"name": kb.Name, "description": kb.Description})
	resp, err := p.managedRequest(r.Context(), key, http.MethodPut, "knowledge-bases/"+url.PathEscape(id), nil, bytes.NewReader(payload), "application/json")
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, id, "uncertain", 502)
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "更新结果不确定，请使用操作编号核对", op)
		return
	}
	var updated managedKB
	if decodeManagedData(resp, &updated) != nil || updated.ID != id || updated.TenantID != tenant {
		_ = p.finishManagedAudit(r.Context(), op, id, "unverified", 502)
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "更新结果无法验证", "")
		return
	}
	if err := p.finishManagedAudit(r.Context(), op, id, "succeeded", 200); err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "知识库已更新但审计结果更新失败", op)
		return
	}
	if _, err := p.db.ExecContext(r.Context(), `UPDATE knowledge_base_registry SET name_snapshot=$4,updated_at=now() WHERE deployment_id=$1 AND weknora_tenant_id=$2 AND knowledge_base_id=$3`, ident.DeploymentID, tenant, id, updated.Name); err != nil {
		p.managedWriteError(w, 503, "REGISTRY_WRITE_FAILED", "知识库已更新但目录未同步，请使用操作编号核对", op)
		return
	}
	writeJSON(w, 200, map[string]any{"data": updated, "operationId": op})
}

func (p *portal) handleManagedKnowledgeDeleteBase(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return
	}
	id := r.PathValue("baseID")
	if _, found, err := p.managedFetchKB(r.Context(), key, tenant, ident.DeploymentID, id); err != nil || !found {
		p.managedWriteError(w, 404, "KNOWLEDGE_BASE_NOT_FOUND", "未找到已登记知识库", "")
		return
	}
	used, err := p.managedKnowledgeReferenced(r.Context(), id)
	if err != nil {
		p.managedWriteError(w, 503, "REFERENCE_CHECK_FAILED", "无法确认知识库是否仍被关联，已拒绝删除", "")
		return
	}
	if used {
		p.managedWriteError(w, 409, "KNOWLEDGE_BASE_IN_USE", "该知识库仍被数字员工或待审批请求关联", "")
		return
	}
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, "delete", "knowledge-base", id, "")
	if err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法持久化操作审计，未删除知识库", "")
		return
	}
	resp, err := p.managedRequest(r.Context(), key, http.MethodDelete, "knowledge-bases/"+url.PathEscape(id), nil, nil, "")
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, id, "uncertain", 502)
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "删除结果不确定，请使用操作编号核对", op)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status := resp.StatusCode
		_ = resp.Body.Close()
		_ = p.finishManagedAudit(r.Context(), op, id, "failed", status)
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_REJECTED", "上游拒绝删除知识库", "")
		return
	}
	_ = resp.Body.Close()
	if err := p.finishManagedAudit(r.Context(), op, id, "succeeded", 200); err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "知识库已删除但审计结果更新失败", op)
		return
	}
	_, err = p.db.ExecContext(r.Context(), `DELETE FROM knowledge_base_registry WHERE deployment_id=$1 AND weknora_tenant_id=$2 AND knowledge_base_id=$3`, ident.DeploymentID, tenant, id)
	if err != nil {
		p.managedWriteError(w, 503, "REGISTRY_WRITE_FAILED", "知识库已删除但目录更新失败", op)
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true, "operationId": op})
}

func (p *portal) managedKnowledgeReferenced(ctx context.Context, id string) (bool, error) {
	var employees dev1alpha1.DigitalEmployeeList
	if err := p.k8s.List(ctx, &employees, client.InNamespace(p.cfg.namespace)); err != nil {
		return false, err
	}
	for _, employee := range employees.Items {
		if employee.Spec.Knowledge == nil {
			continue
		}
		for _, base := range employee.Spec.Knowledge.Bases {
			if base.ID == id {
				return true, nil
			}
		}
	}
	rows, err := p.db.QueryContext(ctx, `SELECT spec_json FROM requests WHERE state='pending'`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		var spec dev1alpha1.DigitalEmployee
		if json.Unmarshal([]byte(raw), &spec) != nil {
			return false, errors.New("pending request spec invalid")
		}
		if spec.Spec.Knowledge != nil {
			for _, base := range spec.Spec.Knowledge.Bases {
				if base.ID == id {
					return true, nil
				}
			}
		}
	}
	return false, rows.Err()
}

func (p *portal) handleManagedKnowledgeDocuments(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return
	}
	id := r.PathValue("baseID")
	if _, found, err := p.managedFetchKB(r.Context(), key, tenant, ident.DeploymentID, id); err != nil || !found {
		p.managedWriteError(w, 404, "KNOWLEDGE_BASE_NOT_FOUND", "未找到已登记知识库", "")
		return
	}
	q := url.Values{}
	allowedQuery := map[string]bool{"page": true, "page_size": true, "tag_ids": true, "keyword": true, "file_type": true, "parse_status": true, "source": true, "start_time": true, "end_time": true, "folder_path": true, "folder_recursive": true}
	for name := range r.URL.Query() {
		if !allowedQuery[name] {
			p.managedWriteError(w, 400, "INVALID_QUERY", "查询参数无效", "")
			return
		}
	}
	for _, name := range []string{"page", "page_size", "tag_ids", "keyword", "file_type", "parse_status", "source", "start_time", "end_time", "folder_path", "folder_recursive"} {
		if values, ok := r.URL.Query()[name]; ok {
			if len(values) != 1 || len(values[0]) > 200 {
				p.managedWriteError(w, 400, "INVALID_QUERY", "查询参数无效", "")
				return
			}
			q.Set(name, values[0])
		}
	}
	if _, err := strconv.Atoi(q.Get("page")); q.Get("page") != "" && err != nil {
		p.managedWriteError(w, 400, "INVALID_QUERY", "分页参数无效", "")
		return
	}
	if page := q.Get("page"); page != "" {
		n, _ := strconv.Atoi(page)
		if n < 1 {
			p.managedWriteError(w, 400, "INVALID_QUERY", "页码必须大于等于 1", "")
			return
		}
	}
	if pageSize := q.Get("page_size"); pageSize != "" {
		n, err := strconv.Atoi(pageSize)
		if err != nil || n < 1 || n > 200 {
			p.managedWriteError(w, 400, "INVALID_QUERY", "每页数量必须为 1 到 200", "")
			return
		}
	}
	resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "knowledge-bases/"+url.PathEscape(id)+"/knowledge", q, nil, "")
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "文档列表读取失败", "")
		return
	}
	var page struct {
		Data     []managedKnowledge `json:"data"`
		Total    int                `json:"total"`
		Page     int                `json:"page"`
		PageSize int                `json:"page_size"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&page)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || decodeErr != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "文档列表读取失败", "")
		return
	}
	for _, document := range page.Data {
		if document.ID == "" || document.KnowledgeBaseID != id {
			p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "文档列表包含不属于当前知识库的项目", "")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"data": page.Data, "total": page.Total, "page": page.Page, "pageSize": page.PageSize})
}

func (p *portal) handleManagedKnowledgeUpload(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return
	}
	id := r.PathValue("baseID")
	if _, found, err := p.managedFetchKB(r.Context(), key, tenant, ident.DeploymentID, id); err != nil || !found {
		p.managedWriteError(w, 404, "KNOWLEDGE_BASE_NOT_FOUND", "未找到已登记知识库", "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		p.managedWriteError(w, 400, "INVALID_UPLOAD", "上传请求无效或超过大小限制", "")
		return
	}
	if len(r.MultipartForm.Value) != 0 || len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
		p.managedWriteError(w, 400, "INVALID_UPLOAD", "仅允许一个 file 文件字段，不接受其他表单字段", "")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		p.managedWriteError(w, 400, "INVALID_UPLOAD", "必须提供 file 文件字段", "")
		return
	}
	defer func() { _ = file.Close() }()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", header.Filename)
	if err != nil {
		p.managedWriteError(w, 400, "INVALID_UPLOAD", "上传请求无效", "")
		return
	}
	if _, err = io.Copy(part, file); err != nil {
		p.managedWriteError(w, 400, "INVALID_UPLOAD", "读取上传文件失败", "")
		return
	}
	_ = mw.Close()
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, "upload_file", "document", "", id)
	if err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法持久化操作审计，未上传文件", "")
		return
	}
	resp, err := p.managedRequest(r.Context(), key, http.MethodPost, "knowledge-bases/"+url.PathEscape(id)+"/knowledge/file", nil, &body, mw.FormDataContentType())
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, "", "uncertain", 502)
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "文件上传结果不确定，请使用操作编号核对", op)
		return
	}
	var result managedKnowledge
	if decodeManagedData(resp, &result) != nil || result.ID == "" || result.KnowledgeBaseID != id {
		_ = p.finishManagedAudit(r.Context(), op, "", "unverified", 502)
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "文件上传结果无法验证，请使用操作编号核对", op)
		return
	}
	if err := p.finishManagedAudit(r.Context(), op, result.ID, "accepted", 202); err != nil {
		p.managedWriteErrorResource(w, 503, "AUDIT_UNAVAILABLE", "文件已提交但审计结果更新失败", op, result.ID)
		return
	}
	writeJSON(w, 202, map[string]any{"data": result, "operationId": op})
}
