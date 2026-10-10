package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	ID              string      `json:"id"`
	KnowledgeBaseID string      `json:"knowledge_base_id"`
	TenantID        json.Number `json:"tenant_id"`
	Type            string      `json:"type"`
	Name            string      `json:"name"`
	Title           string      `json:"title"`
	Description     string      `json:"description"`
	Source          string      `json:"source"`
	FileName        string      `json:"file_name"`
	FileType        string      `json:"file_type"`
	FileSize        int64       `json:"file_size"`
	ParseStatus     string      `json:"parse_status"`
	EnableStatus    string      `json:"enable_status"`
	ManualDisabled  *bool       `json:"manual_disabled,omitempty"`
	SummaryStatus   string      `json:"summary_status"`
	CreatedAt       string      `json:"created_at"`
}

func normalizeManagedKnowledge(doc *managedKnowledge) {
	if doc.Name != "" {
		return
	}
	if doc.Title != "" {
		doc.Name = doc.Title
	} else if doc.FileName != "" {
		doc.Name = doc.FileName
	} else {
		doc.Name = doc.Source
	}
}

type managedChunk struct {
	ID              string              `json:"id"`
	KnowledgeID     string              `json:"knowledge_id"`
	KnowledgeBaseID string              `json:"knowledge_base_id"`
	TenantID        json.Number         `json:"tenant_id"`
	SeqID           int64               `json:"seq_id"`
	Content         string              `json:"content"`
	ChunkType       string              `json:"chunk_type"`
	IsEnabled       bool                `json:"is_enabled"`
	ImageInfo       string              `json:"image_info,omitempty"`
	Images          []managedChunkImage `json:"images,omitempty"`
}

type managedChunkImage struct {
	Index int    `json:"index"`
	URL   string `json:"url"`
}

var managedResourceHandle = regexp.MustCompile(`^resource://[A-Za-z0-9_-]{22}$`)

type managedSpan struct {
	KnowledgeID  string        `json:"knowledge_id"`
	Attempt      int           `json:"attempt"`
	SpanID       string        `json:"span_id"`
	ParentSpanID string        `json:"parent_span_id,omitempty"`
	Name         string        `json:"name"`
	Kind         string        `json:"kind"`
	Status       string        `json:"status"`
	ErrorCode    string        `json:"error_code,omitempty"`
	StartedAt    *time.Time    `json:"started_at,omitempty"`
	FinishedAt   *time.Time    `json:"finished_at,omitempty"`
	DurationMs   int64         `json:"duration_ms,omitempty"`
	Children     []managedSpan `json:"children,omitempty"`
}

type managedError struct {
	Error struct {
		Code        string `json:"code"`
		Message     string `json:"message"`
		OperationID string `json:"operationId,omitempty"`
		ResourceID  string `json:"resourceId,omitempty"`
	} `json:"error"`
}

const managedKnowledgeFileMaxBytes int64 = 256 << 20

var errManagedKnowledgeFileTooLarge = errors.New("knowledge file exceeds size limit")

// File responses may need a bounded disk spool because the pinned WeKnora
// handlers use chunked streaming without Content-Length. Keep at most one
// such transfer per portal process so temporary-disk use is bounded to 256 MiB.
var managedKnowledgeFileSpoolSlots = make(chan struct{}, 1)

type managedKnowledgeFileSpool struct {
	file *os.File
	size int64
}

func (s *managedKnowledgeFileSpool) close() {
	if s == nil || s.file == nil {
		return
	}
	name := s.file.Name()
	_ = s.file.Close()
	_ = os.Remove(name)
}

func spoolManagedKnowledgeFile(src io.Reader, declaredSize, maxBytes int64, tempDir string) (*managedKnowledgeFileSpool, error) {
	if declaredSize > maxBytes {
		return nil, errManagedKnowledgeFileTooLarge
	}
	f, err := os.CreateTemp(tempDir, "zhiyuan-managed-knowledge-*")
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return nil, err
	}
	n, err := io.Copy(f, io.LimitReader(src, maxBytes+1))
	if err != nil {
		cleanup()
		return nil, err
	}
	if n > maxBytes {
		cleanup()
		return nil, errManagedKnowledgeFileTooLarge
	}
	if n == 0 {
		cleanup()
		return nil, errors.New("knowledge file is empty")
	}
	if declaredSize >= 0 && n != declaredSize {
		cleanup()
		return nil, errors.New("knowledge file length mismatch")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	return &managedKnowledgeFileSpool{file: f, size: n}, nil
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
	for index := range page.Data {
		document := &page.Data[index]
		if document.ID == "" || document.TenantID.String() != tenant || document.KnowledgeBaseID != id {
			p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "文档列表包含不属于当前知识库的项目", "")
			return
		}
		normalizeManagedKnowledge(document)
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
	if decodeManagedData(resp, &result) != nil || result.ID == "" || result.TenantID.String() != tenant || result.KnowledgeBaseID != id {
		_ = p.finishManagedAudit(r.Context(), op, result.ID, "unverified", 502)
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "文件上传结果无法验证，请使用操作编号核对", op)
		return
	}
	normalizeManagedKnowledge(&result)
	if err := p.finishManagedAudit(r.Context(), op, result.ID, "accepted", 202); err != nil {
		p.managedWriteErrorResource(w, 503, "AUDIT_UNAVAILABLE", "文件已提交但审计结果更新失败", op, result.ID)
		return
	}
	writeJSON(w, 202, map[string]any{"data": result, "operationId": op})
}

func (p *portal) handleManagedKnowledgeImportURL(w http.ResponseWriter, r *http.Request, ident identity) {
	var in struct {
		URL      string `json:"url"`
		Title    string `json:"title"`
		FileName string `json:"file_name"`
		FileType string `json:"file_type"`
	}
	if decodeStrictJSON(w, r, &in, 16<<10) != nil || len(in.URL) > 2048 || len(in.Title) > 200 || len(in.FileName) > 255 || len(in.FileType) > 32 {
		p.managedWriteError(w, 400, "INVALID_REQUEST", "URL 导入参数无效", "")
		return
	}
	if err := validateManagedKnowledgeURL(r.Context(), in.URL); err != nil {
		p.managedWriteError(w, 400, "INVALID_SOURCE_URL", "仅允许可公开访问的 HTTP(S) 来源地址", "")
		return
	}
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return
	}
	baseID := r.PathValue("baseID")
	if _, found, err := p.managedFetchKB(r.Context(), key, tenant, ident.DeploymentID, baseID); err != nil || !found {
		p.managedWriteError(w, 404, "KNOWLEDGE_BASE_NOT_FOUND", "未找到已登记知识库", "")
		return
	}
	input := map[string]any{"url": in.URL}
	if strings.TrimSpace(in.Title) != "" {
		input["title"] = strings.TrimSpace(in.Title)
	}
	if strings.TrimSpace(in.FileName) != "" {
		input["file_name"] = filepath.Base(strings.TrimSpace(in.FileName))
	}
	if strings.TrimSpace(in.FileType) != "" {
		input["file_type"] = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(in.FileType)), ".")
	}
	p.managedCreateDocument(w, r, ident, key, tenant, baseID, "url", input)
}

func validateManagedKnowledgeURL(ctx context.Context, raw string) error {
	return validateManagedKnowledgeURLWithLookup(ctx, raw, net.DefaultResolver.LookupIPAddr)
}

func validateManagedKnowledgeURLWithLookup(ctx context.Context, raw string, lookup func(context.Context, string) ([]net.IPAddr, error)) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid source URL")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return errors.New("private host")
	}
	ips := []net.IP{}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else {
		dnsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		resolved, lookupErr := lookup(dnsCtx, host)
		if lookupErr != nil || len(resolved) == 0 {
			return errors.New("source host lookup failed")
		}
		for _, entry := range resolved {
			ips = append(ips, entry.IP)
		}
	}
	privateCGN := net.ParseIP("100.64.0.0").To4()
	privateCGNMask := net.CIDRMask(10, 32)
	privateCGNNet := net.IPNet{IP: privateCGN, Mask: privateCGNMask}
	for _, ip := range ips {
		if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
			return errors.New("private host")
		}
		if v4 := ip.To4(); v4 != nil && privateCGNNet.Contains(v4) {
			return errors.New("private host")
		}
	}
	return nil
}

func (p *portal) managedCreateDocument(w http.ResponseWriter, r *http.Request, ident identity, key, tenant, baseID, kind string, input map[string]any) {
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, "create_"+kind, "document", "", baseID)
	if err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法持久化操作审计，未导入资料", "")
		return
	}
	payload, _ := json.Marshal(input)
	resp, err := p.managedRequest(r.Context(), key, http.MethodPost, "knowledge-bases/"+url.PathEscape(baseID)+"/knowledge/"+kind, nil, bytes.NewReader(payload), "application/json")
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, "", "uncertain", 502)
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "资料导入结果不确定，请使用操作编号核对", op)
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status := resp.StatusCode
		_ = resp.Body.Close()
		_ = p.finishManagedAudit(r.Context(), op, "", "failed", status)
		if status == http.StatusConflict {
			p.managedWriteError(w, 409, "DUPLICATE_KNOWLEDGE", "来源资料已存在或与现有资料重复", op)
			return
		}
		if status == http.StatusRequestEntityTooLarge {
			p.managedWriteError(w, 413, "KNOWLEDGE_INPUT_TOO_LARGE", "导入内容超过上游限制", op)
			return
		}
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_REJECTED", "上游拒绝导入资料", op)
		return
	}
	var created managedKnowledge
	if err := decodeManagedData(resp, &created); err != nil || created.ID == "" || created.KnowledgeBaseID != baseID || created.TenantID.String() != tenant {
		_ = p.finishManagedAudit(r.Context(), op, created.ID, "unverified", 502)
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "上游导入结果无法安全验证", op)
		return
	}
	normalizeManagedKnowledge(&created)
	if err := p.finishManagedAudit(r.Context(), op, created.ID, "accepted", http.StatusAccepted); err != nil {
		p.managedWriteErrorResource(w, 503, "AUDIT_UNAVAILABLE", "资料已导入但审计结果更新失败", op, created.ID)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"data": created, "operationId": op})
}

func (p *portal) managedFetchDocument(ctx context.Context, key, tenant, deployment, baseID, documentID string) (managedKnowledge, bool, error) {
	if _, ok := safeKnowledgeID(documentID); !ok {
		return managedKnowledge{}, false, nil
	}
	if baseID != "" {
		if _, found, err := p.managedFetchKB(ctx, key, tenant, deployment, baseID); err != nil || !found {
			return managedKnowledge{}, false, err
		}
	}
	resp, err := p.managedRequest(ctx, key, http.MethodGet, "knowledge/"+url.PathEscape(documentID), nil, nil, "")
	if err != nil {
		return managedKnowledge{}, false, err
	}
	var doc managedKnowledge
	if err := decodeManagedData(resp, &doc); err != nil {
		return managedKnowledge{}, false, err
	}
	if _, safe := safeKnowledgeID(doc.KnowledgeBaseID); !safe || doc.ID != documentID || doc.TenantID.String() != tenant || (baseID != "" && doc.KnowledgeBaseID != baseID) {
		return managedKnowledge{}, false, errors.New("document ownership mismatch")
	}
	normalizeManagedKnowledge(&doc)
	if baseID == "" {
		if _, found, err := p.managedFetchKB(ctx, key, tenant, deployment, doc.KnowledgeBaseID); err != nil || !found {
			return managedKnowledge{}, false, err
		}
	}
	return doc, true, nil
}

func (p *portal) managedDocumentContext(w http.ResponseWriter, r *http.Request, ident identity) (string, string, managedKnowledge, bool) {
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return "", "", managedKnowledge{}, false
	}
	docID, ok := safeKnowledgeID(r.PathValue("documentID"))
	if !ok {
		p.managedWriteError(w, 404, "KNOWLEDGE_NOT_FOUND", "未找到知识条目", "")
		return "", "", managedKnowledge{}, false
	}
	baseID := r.PathValue("baseID")
	// Document routes resolve the parent knowledge base from a required, public API path value.
	doc, found, err := p.managedFetchDocument(r.Context(), key, tenant, ident.DeploymentID, baseID, docID)
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "资料归属验证失败", "")
		return "", "", managedKnowledge{}, false
	}
	if !found {
		p.managedWriteError(w, 404, "KNOWLEDGE_NOT_FOUND", "未找到已登记资料", "")
		return "", "", managedKnowledge{}, false
	}
	return key, tenant, doc, true
}

func (p *portal) handleManagedKnowledgeImportManual(w http.ResponseWriter, r *http.Request, ident identity) {
	var in struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if decodeStrictJSON(w, r, &in, 1<<20) != nil || strings.TrimSpace(in.Title) == "" || len(in.Title) > 200 || strings.TrimSpace(in.Content) == "" || len(in.Content) > 1<<20 {
		p.managedWriteError(w, 400, "INVALID_REQUEST", "手动资料标题或正文无效", "")
		return
	}
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return
	}
	baseID := r.PathValue("baseID")
	if _, found, err := p.managedFetchKB(r.Context(), key, tenant, ident.DeploymentID, baseID); err != nil || !found {
		p.managedWriteError(w, 404, "KNOWLEDGE_BASE_NOT_FOUND", "未找到已登记知识库", "")
		return
	}
	p.managedCreateDocument(w, r, ident, key, tenant, baseID, "manual", map[string]any{"title": strings.TrimSpace(in.Title), "content": in.Content, "status": "published"})
}

func (p *portal) handleManagedKnowledgeGetDocument(w http.ResponseWriter, r *http.Request, ident identity) {
	_, _, doc, ok := p.managedDocumentContext(w, r, ident)
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"data": doc})
}

func (p *portal) handleManagedKnowledgeSetDocumentEnabled(w http.ResponseWriter, r *http.Request, ident identity) {
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if decodeStrictJSON(w, r, &input, 4096) != nil || input.Enabled == nil {
		p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "enabled 必须为布尔值", "")
		return
	}
	key, tenant, doc, ok := p.managedDocumentContext(w, r, ident)
	if !ok {
		return
	}
	if *input.Enabled && doc.ParseStatus != "completed" {
		p.managedWriteError(w, http.StatusConflict, "KNOWLEDGE_NOT_COMPLETED", "资料处理完成后才能启用", "")
		return
	}
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, "set_enabled", "document", doc.ID, doc.KnowledgeBaseID)
	if err != nil {
		p.managedWriteError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "无法记录资料状态变更审计", "")
		return
	}
	body, _ := json.Marshal(map[string]bool{"enabled": *input.Enabled})
	resp, err := p.managedRequest(r.Context(), key, http.MethodPut, "knowledge/"+url.PathEscape(doc.ID)+"/enable-status", nil, bytes.NewReader(body), "application/json")
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "uncertain", http.StatusBadGateway)
		p.managedWriteErrorResource(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "资料状态变更结果待核实", op, doc.ID)
		return
	}
	var updated managedKnowledge
	decodeErr := decodeManagedData(resp, &updated)
	expected := "disabled"
	if *input.Enabled {
		expected = "enabled"
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || decodeErr != nil || updated.ID != doc.ID || updated.TenantID.String() != tenant || updated.KnowledgeBaseID != doc.KnowledgeBaseID || updated.EnableStatus != expected {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "unverified", http.StatusBadGateway)
		p.managedWriteErrorResource(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_INVALID", "资料状态响应无法安全验证", op, doc.ID)
		return
	}
	if err := p.finishManagedAudit(r.Context(), op, doc.ID, "succeeded", http.StatusOK); err != nil {
		p.managedWriteErrorResource(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "资料状态已变更但审计结果更新失败", op, doc.ID)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": updated, "operationId": op})
}

func (p *portal) handleManagedKnowledgeChunkImage(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, doc, ok := p.managedDocumentContext(w, r, ident)
	if !ok {
		return
	}
	chunkID, safe := safeKnowledgeID(r.PathValue("chunkID"))
	index, indexErr := strconv.Atoi(r.PathValue("index"))
	if !safe || indexErr != nil || index < 0 {
		p.managedWriteError(w, http.StatusNotFound, "KNOWLEDGE_IMAGE_NOT_FOUND", "未找到资料图片", "")
		return
	}
	// Verify the requested chunk's document, KB, tenant and resource handle
	// before issuing the narrower image-byte read. No upstream URL/path is
	// accepted from the caller or returned in this response.
	chunkResp, err := p.managedRequest(r.Context(), key, http.MethodGet, "chunks/by-id/"+url.PathEscape(chunkID), nil, nil, "")
	if err != nil {
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "资料图片归属验证失败", "")
		return
	}
	var chunkEnvelope struct {
		Data managedChunk `json:"data"`
	}
	chunkErr := json.NewDecoder(io.LimitReader(chunkResp.Body, 1<<20)).Decode(&chunkEnvelope)
	_ = chunkResp.Body.Close()
	if chunkResp.StatusCode < 200 || chunkResp.StatusCode >= 300 || chunkErr != nil || chunkEnvelope.Data.ID != chunkID || chunkEnvelope.Data.KnowledgeID != doc.ID || chunkEnvelope.Data.KnowledgeBaseID != doc.KnowledgeBaseID || chunkEnvelope.Data.TenantID.String() != tenant {
		p.managedWriteError(w, http.StatusNotFound, "KNOWLEDGE_IMAGE_NOT_FOUND", "未找到资料图片", "")
		return
	}
	var refs []struct {
		URL string `json:"url"`
	}
	if json.Unmarshal([]byte(chunkEnvelope.Data.ImageInfo), &refs) != nil || index >= len(refs) || !managedResourceHandle.MatchString(refs[index].URL) {
		p.managedWriteError(w, http.StatusNotFound, "KNOWLEDGE_IMAGE_NOT_FOUND", "未找到资料图片", "")
		return
	}
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, "read_chunk_image", "document", doc.ID, doc.KnowledgeBaseID)
	if err != nil {
		p.managedWriteError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "无法记录资料图片读取审计", "")
		return
	}
	select {
	case managedKnowledgeFileSpoolSlots <- struct{}{}:
		defer func() { <-managedKnowledgeFileSpoolSlots }()
	default:
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", http.StatusServiceUnavailable)
		p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_FILE_BUSY", "图片读取繁忙，请稍后重试", op)
		return
	}
	resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "knowledge/"+url.PathEscape(doc.ID)+"/chunks/"+url.PathEscape(chunkID)+"/images/"+strconv.Itoa(index), nil, nil, "")
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", http.StatusBadGateway)
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_IMAGE_UNAVAILABLE", "资料图片暂不可用", op)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	imageType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || (imageType != "image/png" && imageType != "image/jpeg" && imageType != "image/gif" && imageType != "image/webp") {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", http.StatusBadGateway)
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_IMAGE_INVALID", "资料图片响应无法验证", op)
		return
	}
	spool, err := spoolManagedKnowledgeFile(resp.Body, resp.ContentLength, 20<<20, "")
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", http.StatusBadGateway)
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_IMAGE_INVALID", "资料图片读取失败或超过限制", op)
		return
	}
	defer spool.close()
	prefix := make([]byte, 512)
	n, readErr := io.ReadFull(spool.file, prefix)
	if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", http.StatusBadGateway)
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_IMAGE_INVALID", "资料图片格式无法验证", op)
		return
	}
	actualType := http.DetectContentType(prefix[:n])
	if actualType != imageType || (actualType != "image/png" && actualType != "image/jpeg" && actualType != "image/gif" && actualType != "image/webp") {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", http.StatusBadGateway)
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_IMAGE_INVALID", "资料图片格式无法验证", op)
		return
	}
	if _, err := spool.file.Seek(0, io.SeekStart); err != nil {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", http.StatusBadGateway)
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_IMAGE_INVALID", "资料图片读取失败", op)
		return
	}
	w.Header().Set("Content-Type", imageType)
	w.Header().Set("Content-Length", strconv.FormatInt(spool.size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if _, err := io.CopyN(w, spool.file, spool.size); err != nil {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "uncertain", http.StatusBadGateway)
		return
	}
	if err := p.finishManagedAudit(r.Context(), op, doc.ID, "succeeded", http.StatusOK); err != nil {
		return
	}
}

func (p *portal) handleManagedKnowledgeDocumentAction(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, doc, ok := p.managedDocumentContext(w, r, ident)
	if !ok {
		return
	}
	action := r.PathValue("action")
	method, path, auditAction := http.MethodPost, "", ""
	switch action {
	case "reparse":
		path, auditAction = "knowledge/"+url.PathEscape(doc.ID)+"/reparse", "reparse"
	case "cancel-parse":
		path, auditAction = "knowledge/"+url.PathEscape(doc.ID)+"/cancel-parse", "cancel_parse"
	case "delete":
		method, path, auditAction = http.MethodDelete, "knowledge/"+url.PathEscape(doc.ID), "delete_document"
	default:
		p.managedWriteError(w, 404, "NOT_FOUND", "未找到操作", "")
		return
	}
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, auditAction, "document", doc.ID, doc.KnowledgeBaseID)
	if err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法持久化操作审计，未执行操作", "")
		return
	}
	resp, err := p.managedRequest(r.Context(), key, method, path, nil, nil, "")
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "uncertain", 502)
		p.managedWriteErrorResource(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "操作结果不确定，请使用操作编号核对", op, doc.ID)
		return
	}
	status := resp.StatusCode
	var result managedKnowledge
	if method != http.MethodDelete && status >= 200 && status < 300 {
		if err := decodeManagedData(resp, &result); err != nil || result.ID != doc.ID || result.KnowledgeBaseID != doc.KnowledgeBaseID || result.TenantID.String() != tenant {
			_ = p.finishManagedAudit(r.Context(), op, doc.ID, "unverified", 502)
			p.managedWriteErrorResource(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "操作结果无法安全验证", op, doc.ID)
			return
		}
	} else {
		_ = resp.Body.Close()
	}
	if status < 200 || status >= 300 {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", status)
		if status == http.StatusConflict {
			p.managedWriteErrorResource(w, 409, "KNOWLEDGE_OPERATION_CONFLICT", "当前资料状态不允许此操作", op, doc.ID)
			return
		}
		p.managedWriteErrorResource(w, 502, "KNOWLEDGE_UPSTREAM_REJECTED", "上游拒绝执行资料操作", op, doc.ID)
		return
	}
	if err := p.finishManagedAudit(r.Context(), op, doc.ID, "succeeded", status); err != nil {
		p.managedWriteErrorResource(w, 503, "AUDIT_UNAVAILABLE", "资料操作已执行但审计写入失败", op, doc.ID)
		return
	}
	if action == "delete" {
		writeJSON(w, 200, map[string]any{"deleted": true, "operationId": op, "resourceId": doc.ID})
		return
	}
	writeJSON(w, 202, map[string]any{"data": result, "operationId": op})
}

func (p *portal) handleManagedKnowledgeSpans(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, doc, ok := p.managedDocumentContext(w, r, ident)
	if !ok {
		return
	}
	query := url.Values{}
	allowedQuery := map[string]bool{"attempt": true}
	for name := range r.URL.Query() {
		if !allowedQuery[name] {
			p.managedWriteError(w, 400, "INVALID_QUERY", "解析过程查询参数无效", "")
			return
		}
	}
	if values, present := r.URL.Query()["attempt"]; present {
		if len(values) != 1 {
			p.managedWriteError(w, 400, "INVALID_QUERY", "解析轮次参数无效", "")
			return
		}
		attempt, err := strconv.Atoi(values[0])
		if err != nil || attempt < 0 || attempt > 1000000 {
			p.managedWriteError(w, 400, "INVALID_QUERY", "解析轮次参数无效", "")
			return
		}
		query.Set("attempt", values[0])
	}
	resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "knowledge/"+url.PathEscape(doc.ID)+"/spans", query, nil, "")
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "解析过程读取失败", "")
		return
	}
	var envelope struct {
		KnowledgeID   string          `json:"knowledge_id"`
		Attempt       int             `json:"attempt"`
		LatestAttempt int             `json:"latest_attempt"`
		ParseStatus   string          `json:"parse_status"`
		CurrentStage  string          `json:"current_stage"`
		Trace         json.RawMessage `json:"trace"`
	}
	if err := decodeManagedData(resp, &envelope); err != nil || envelope.KnowledgeID != doc.ID {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "解析过程响应无法验证", "")
		return
	}
	var trace managedSpan
	if len(envelope.Trace) > 0 && json.Unmarshal(envelope.Trace, &trace) != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "解析过程响应无法验证", "")
		return
	}
	if err := p.auditManagedRead(r.Context(), ident, tenant, doc, "read_spans"); err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法记录解析过程读取审计", "")
		return
	}
	writeJSON(w, 200, map[string]any{"data": map[string]any{"knowledge_id": doc.ID, "attempt": envelope.Attempt, "latest_attempt": envelope.LatestAttempt, "parse_status": envelope.ParseStatus, "current_stage": envelope.CurrentStage, "trace": trace}})
}

func (p *portal) handleManagedKnowledgeChunks(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, doc, ok := p.managedDocumentContext(w, r, ident)
	if !ok {
		return
	}
	for name := range r.URL.Query() {
		if name != "page" && name != "page_size" && name != "chunk_type" {
			p.managedWriteError(w, 400, "INVALID_QUERY", "分块查询参数无效", "")
			return
		}
	}
	query := url.Values{}
	for name, max := range map[string]int{"page": 8, "page_size": 8} {
		values, present := r.URL.Query()[name]
		if !present {
			continue
		}
		if len(values) != 1 || len(values[0]) > max {
			p.managedWriteError(w, 400, "INVALID_QUERY", "分块分页参数无效", "")
			return
		}
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 1 || (name == "page_size" && n > 100) {
			p.managedWriteError(w, 400, "INVALID_QUERY", "分块分页参数无效", "")
			return
		}
		query.Set(name, values[0])
	}
	if types, present := r.URL.Query()["chunk_type"]; present {
		if len(types) == 0 || len(types) > 12 {
			p.managedWriteError(w, 400, "INVALID_QUERY", "分块类型无效", "")
			return
		}
		allowedTypes := map[string]bool{"text": true, "parent_text": true, "image_ocr": true, "image_caption": true, "summary": true, "entity": true, "relationship": true, "faq": true, "web_search": true, "table_summary": true, "table_column": true, "wiki_page": true}
		for _, chunkType := range types {
			if len(chunkType) > 32 || !allowedTypes[chunkType] {
				p.managedWriteError(w, 400, "INVALID_QUERY", "分块类型无效", "")
				return
			}
			query.Add("chunk_type", chunkType)
		}
	}
	resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "chunks/"+url.PathEscape(doc.ID), query, nil, "")
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "分块读取失败", "")
		return
	}
	var page struct {
		Data     []managedChunk `json:"data"`
		Total    int            `json:"total"`
		Page     int            `json:"page"`
		PageSize int            `json:"page_size"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&page)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || decodeErr != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "分块响应无法验证", "")
		return
	}
	for index := range page.Data {
		chunk := &page.Data[index]
		if chunk.ID == "" || chunk.KnowledgeID != doc.ID || chunk.KnowledgeBaseID != doc.KnowledgeBaseID || chunk.TenantID.String() != tenant {
			p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "分块列表包含其他资料的内容", "")
			return
		}
		chunk.Images = nil
		if strings.TrimSpace(chunk.ImageInfo) != "" {
			var refs []struct {
				URL string `json:"url"`
			}
			if json.Unmarshal([]byte(chunk.ImageInfo), &refs) != nil {
				p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "chunk image metadata could not be verified", "")
				return
			}
			for index, ref := range refs {
				if !managedResourceHandle.MatchString(ref.URL) {
					continue // legacy/external URLs are never returned to the browser.
				}
				chunk.Images = append(chunk.Images, managedChunkImage{Index: index, URL: "/api/v1/knowledge/managed/documents/" + url.PathEscape(doc.ID) + "/chunks/" + url.PathEscape(chunk.ID) + "/images/" + strconv.Itoa(index)})
			}
		}
		chunk.ImageInfo = ""
	}
	if err := p.auditManagedRead(r.Context(), ident, tenant, doc, "read_chunks"); err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法记录分块读取审计", "")
		return
	}
	writeJSON(w, 200, map[string]any{"data": page.Data, "total": page.Total, "page": page.Page, "pageSize": page.PageSize})
}

func (p *portal) auditManagedRead(ctx context.Context, ident identity, tenant string, doc managedKnowledge, action string) error {
	op, err := p.beginManagedAudit(ctx, ident, tenant, action, "document", doc.ID, doc.KnowledgeBaseID)
	if err != nil {
		return err
	}
	return p.finishManagedAudit(ctx, op, doc.ID, "succeeded", http.StatusOK)
}

func managedSafeContentType(fileName string) string {
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".txt", ".md", ".csv":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func managedSafeFileName(raw, fallback string) string {
	name := filepath.Base(strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "." || name == "" {
		return fallback
	}
	return name
}

func (p *portal) handleManagedKnowledgeFile(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, doc, ok := p.managedDocumentContext(w, r, ident)
	if !ok {
		return
	}
	kind := r.PathValue("fileKind")
	if kind != "preview" && kind != "download" {
		p.managedWriteError(w, 404, "NOT_FOUND", "未找到资料文件", "")
		return
	}
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, "read_"+kind, "document", doc.ID, doc.KnowledgeBaseID)
	if err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法记录资料文件访问审计", "")
		return
	}
	select {
	case managedKnowledgeFileSpoolSlots <- struct{}{}:
		defer func() { <-managedKnowledgeFileSpoolSlots }()
	default:
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", http.StatusServiceUnavailable)
		p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_FILE_BUSY", "文件读取繁忙，请稍后重试", "")
		return
	}
	resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "knowledge/"+url.PathEscape(doc.ID)+"/"+kind, nil, nil, "")
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", 502)
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "资料文件读取失败", "")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", resp.StatusCode)
		p.managedWriteError(w, 502, "KNOWLEDGE_FILE_UNAVAILABLE", "资料文件当前不可用", "")
		return
	}
	name := managedSafeFileName(doc.FileName, doc.ID)
	spool, err := spoolManagedKnowledgeFile(resp.Body, resp.ContentLength, managedKnowledgeFileMaxBytes, "")
	if err != nil {
		if errors.Is(err, errManagedKnowledgeFileTooLarge) {
			_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", http.StatusRequestEntityTooLarge)
			p.managedWriteError(w, http.StatusRequestEntityTooLarge, "KNOWLEDGE_FILE_TOO_LARGE", "资料文件超过预览或下载限制", "")
			return
		}
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "failed", http.StatusBadGateway)
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_FILE_INVALID", "资料文件读取不完整或无法验证", "")
		return
	}
	defer spool.close()
	contentType := managedSafeContentType(name)
	disposition := "attachment"
	if kind == "preview" && contentType != "application/octet-stream" {
		disposition = "inline"
	}
	filenameHeader := mime.FormatMediaType(disposition, map[string]string{"filename": name})
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", filenameHeader)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(spool.size, 10))
	if _, err := io.CopyN(w, spool.file, spool.size); err != nil {
		_ = p.finishManagedAudit(r.Context(), op, doc.ID, "uncertain", 502)
		return
	}
	_ = p.finishManagedAudit(r.Context(), op, doc.ID, "succeeded", 200)
}

func (p *portal) managedDocumentAction(action string) func(http.ResponseWriter, *http.Request, identity) {
	return func(w http.ResponseWriter, r *http.Request, ident identity) {
		r.SetPathValue("action", action)
		p.handleManagedKnowledgeDocumentAction(w, r, ident)
	}
}

func (p *portal) managedDocumentFile(kind string) func(http.ResponseWriter, *http.Request, identity) {
	return func(w http.ResponseWriter, r *http.Request, ident identity) {
		r.SetPathValue("fileKind", kind)
		p.handleManagedKnowledgeFile(w, r, ident)
	}
}
