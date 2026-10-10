package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Managed operations are intentionally KB-scoped adapters. They never accept
// an upstream URL/path from the caller and never forward arbitrary JSON.
type managedTag struct {
	ID             string `json:"id"`
	SeqID          int64  `json:"seq_id"`
	Name           string `json:"name"`
	Color          string `json:"color"`
	SortOrder      int    `json:"sort_order"`
	KnowledgeCount int64  `json:"knowledge_count,omitempty"`
	ChunkCount     int64  `json:"chunk_count,omitempty"`
}

type managedFAQEntry struct {
	ID                int64    `json:"id"`
	KnowledgeID       string   `json:"knowledge_id"`
	KnowledgeBaseID   string   `json:"knowledge_base_id"`
	TagID             int64    `json:"tag_id"`
	TagName           string   `json:"tag_name"`
	IsEnabled         bool     `json:"is_enabled"`
	IsRecommended     bool     `json:"is_recommended"`
	StandardQuestion  string   `json:"standard_question"`
	SimilarQuestions  []string `json:"similar_questions"`
	NegativeQuestions []string `json:"negative_questions"`
	Answers           []string `json:"answers"`
	AnswerStrategy    string   `json:"answer_strategy"`
	UpdatedAt         string   `json:"updated_at"`
	CreatedAt         string   `json:"created_at"`
}

type managedFAQPage struct {
	Data     []managedFAQEntry `json:"data"`
	Total    int64             `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
}

type managedFolderNode struct {
	Path          string              `json:"path"`
	Name          string              `json:"name"`
	DocumentCount int64               `json:"document_count"`
	TotalCount    int64               `json:"total_count"`
	Children      []managedFolderNode `json:"children,omitempty"`
}

type managedFolderTree struct {
	RootDocumentCount  int64               `json:"root_document_count"`
	TotalDocumentCount int64               `json:"total_document_count"`
	Folders            []managedFolderNode `json:"folders"`
}

func (p *portal) managedOperationsBase(w http.ResponseWriter, r *http.Request, ident identity) (string, string, managedKB, bool) {
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return "", "", managedKB{}, false
	}
	id, safe := safeKnowledgeID(r.PathValue("baseID"))
	if !safe {
		p.managedWriteError(w, http.StatusNotFound, "KNOWLEDGE_BASE_NOT_FOUND", "未找到已登记知识库", "")
		return "", "", managedKB{}, false
	}
	kb, found, err := p.managedFetchKB(r.Context(), key, tenant, ident.DeploymentID, id)
	if err != nil {
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库归属验证失败", "")
		return "", "", managedKB{}, false
	}
	if !found {
		p.managedWriteError(w, http.StatusNotFound, "KNOWLEDGE_BASE_NOT_FOUND", "未找到已登记知识库", "")
		return "", "", managedKB{}, false
	}
	return key, tenant, kb, true
}

func managedOperationFailure(status int) (int, string) {
	switch {
	case status == http.StatusConflict:
		return http.StatusConflict, "conflict"
	case status >= 400 && status < 500:
		return http.StatusUnprocessableEntity, "failed"
	default:
		return http.StatusBadGateway, "uncertain"
	}
}

func codeForManagedOutcome(outcome string) string {
	if outcome == "conflict" {
		return "KNOWLEDGE_CONFLICT"
	}
	if outcome == "failed" {
		return "KNOWLEDGE_OPERATION_REJECTED"
	}
	return "KNOWLEDGE_UPSTREAM_UNAVAILABLE"
}

func decodeManagedPage(resp *http.Response, out any) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("upstream data missing")
	}
	return json.Unmarshal(envelope.Data, out)
}

func (p *portal) handleManagedKnowledgeTags(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, kb, ok := p.managedOperationsBase(w, r, ident)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		for name := range q {
			if name != "keyword" {
				p.managedWriteError(w, 400, "INVALID_QUERY", "查询参数无效", "")
				return
			}
		}
		if len(q["keyword"]) > 1 || len(q.Get("keyword")) > 100 {
			p.managedWriteError(w, 400, "INVALID_QUERY", "查询参数无效", "")
			return
		}
		params := url.Values{"page": {"1"}, "page_size": {"200"}}
		if q.Get("keyword") != "" {
			params.Set("keyword", q.Get("keyword"))
		}
		resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "knowledge-bases/"+url.PathEscape(kb.ID)+"/tags", params, nil, "")
		if err != nil {
			p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "标签读取失败", "")
			return
		}
		var page struct {
			Data     []managedTag `json:"data"`
			Total    int64        `json:"total"`
			Page     int          `json:"page"`
			PageSize int          `json:"page_size"`
		}
		if decodeManagedPage(resp, &page) != nil || page.Total > int64(len(page.Data)) {
			p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "标签列表无法完整验证", "")
			return
		}
		for _, tag := range page.Data {
			if tag.ID == "" || tag.SeqID <= 0 {
				p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "标签列表包含无效项目", "")
				return
			}
		}
		if err := p.auditManagedKnowledgeRead(r.Context(), ident, tenant, "list_tags", "knowledge_base", kb.ID, ""); err != nil {
			p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法记录标签读取审计", "")
			return
		}
		writeJSON(w, 200, page)
		return
	}
	if r.Method == http.MethodPost {
		var in struct {
			Name      string `json:"name"`
			Color     string `json:"color"`
			SortOrder int    `json:"sort_order"`
		}
		if decodeStrictJSON(w, r, &in, 16<<10) != nil || strings.TrimSpace(in.Name) == "" || len(in.Name) > 128 || len(in.Color) > 32 {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "标签字段无效", "")
			return
		}
		p.managedTagMutation(w, r, ident, key, tenant, kb, "create_tag", "POST", "knowledge-bases/"+url.PathEscape(kb.ID)+"/tags", in, "", "")
		return
	}
	p.managedWriteError(w, 405, "METHOD_NOT_ALLOWED", "不支持的请求方法", "")
}

func (p *portal) handleManagedKnowledgeTag(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, kb, ok := p.managedOperationsBase(w, r, ident)
	if !ok {
		return
	}
	tagID, safe := safeKnowledgeID(r.PathValue("tagID"))
	if !safe {
		p.managedWriteError(w, 404, "TAG_NOT_FOUND", "未找到标签", "")
		return
	}
	tag, found := p.managedFindTag(w, r, key, kb.ID, tagID)
	if !found {
		return
	}
	if r.Method == http.MethodPut {
		var in struct {
			Name      *string `json:"name"`
			Color     *string `json:"color"`
			SortOrder *int    `json:"sort_order"`
		}
		if decodeStrictJSON(w, r, &in, 16<<10) != nil || (in.Name == nil && in.Color == nil && in.SortOrder == nil) || (in.Name != nil && (strings.TrimSpace(*in.Name) == "" || len(*in.Name) > 128)) || (in.Color != nil && len(*in.Color) > 32) {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "标签字段无效", "")
			return
		}
		p.managedTagMutation(w, r, ident, key, tenant, kb, "update_tag", "PUT", "knowledge-bases/"+url.PathEscape(kb.ID)+"/tags/"+url.PathEscape(tag.ID), in, tag.ID, "")
		return
	}
	if r.Method == http.MethodDelete {
		if r.URL.RawQuery != "" || r.ContentLength > 0 {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "删除标签不接受强制或内容删除参数", "")
			return
		}
		p.managedTagMutation(w, r, ident, key, tenant, kb, "delete_tag", "DELETE", "knowledge-bases/"+url.PathEscape(kb.ID)+"/tags/"+url.PathEscape(tag.ID), nil, tag.ID, "")
		return
	}
	p.managedWriteError(w, 405, "METHOD_NOT_ALLOWED", "不支持的请求方法", "")
}

func (p *portal) managedFindTag(w http.ResponseWriter, r *http.Request, key, baseID, tagID string) (managedTag, bool) {
	tag, found, err := p.managedTagByID(r.Context(), key, baseID, tagID)
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "无法完整验证标签归属", "")
		return managedTag{}, false
	}
	if !found {
		p.managedWriteError(w, 404, "TAG_NOT_FOUND", "未找到当前知识库中的标签", "")
		return managedTag{}, false
	}
	return tag, true
}

func (p *portal) managedTagByID(ctx context.Context, key, baseID, tagID string) (managedTag, bool, error) {
	q := url.Values{"page": {"1"}, "page_size": {"200"}}
	resp, err := p.managedRequest(ctx, key, http.MethodGet, "knowledge-bases/"+url.PathEscape(baseID)+"/tags", q, nil, "")
	if err != nil {
		return managedTag{}, false, err
	}
	var page struct {
		Data  []managedTag `json:"data"`
		Total int64        `json:"total"`
	}
	if decodeManagedPage(resp, &page) != nil || page.Total > int64(len(page.Data)) {
		return managedTag{}, false, errors.New("tag membership response incomplete")
	}
	for _, tag := range page.Data {
		if tag.ID == tagID {
			return tag, true, nil
		}
	}
	return managedTag{}, false, nil
}

func (p *portal) managedTagMutation(w http.ResponseWriter, r *http.Request, ident identity, key, tenant string, kb managedKB, action, method, path string, payload any, resourceID, contentType string) {
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, action, "knowledge_tag", resourceID, kb.ID)
	if err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法记录标签操作审计", "")
		return
	}
	var body io.Reader
	if payload != nil {
		raw, _ := json.Marshal(payload)
		body = bytes.NewReader(raw)
		contentType = "application/json"
	}
	resp, err := p.managedRequest(r.Context(), key, method, path, nil, body, contentType)
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, resourceID, "uncertain", 502)
		p.managedWriteErrorResource(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "标签操作结果待核实", op, resourceID)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	status := resp.StatusCode
	var data managedTag
	var decodeErr error
	if method == http.MethodDelete {
		if status < 200 || status >= 300 {
			decodeErr = errors.New("upstream rejected delete")
		}
	} else {
		decodeErr = decodeManagedData(resp, &data)
	}
	if decodeErr != nil || status < 200 || status >= 300 || (method != http.MethodDelete && (data.ID == "" || data.SeqID <= 0 || (resourceID != "" && data.ID != resourceID))) {
		codeStatus, outcome := managedOperationFailure(status)
		_ = p.finishManagedAudit(r.Context(), op, resourceID, outcome, status)
		p.managedWriteErrorResource(w, codeStatus, codeForManagedOutcome(outcome), "标签操作失败或结果待核实", op, resourceID)
		return
	}
	if method != http.MethodDelete {
		// The mutation URL is KB-scoped, but the response is not an ownership
		// proof. Confirm the created/updated tag is present in this KB's list.
		verified, found, verifyErr := p.managedTagByID(r.Context(), key, kb.ID, data.ID)
		if verifyErr != nil || !found || verified.SeqID != data.SeqID {
			_ = p.finishManagedAudit(r.Context(), op, "", "unverified", http.StatusBadGateway)
			p.managedWriteErrorResource(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_INVALID", "标签已发送变更，但归属结果无法验证", op, "")
			return
		}
		data = verified
		resourceID = data.ID
	} else {
		// A successful response alone is not enough if the tag remains in the
		// KB-scoped catalog.
		_, found, verifyErr := p.managedTagByID(r.Context(), key, kb.ID, resourceID)
		if verifyErr != nil || found {
			_ = p.finishManagedAudit(r.Context(), op, resourceID, "uncertain", http.StatusBadGateway)
			p.managedWriteErrorResource(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "标签删除结果待核实", op, resourceID)
			return
		}
	}
	if err := p.finishManagedAudit(r.Context(), op, resourceID, "succeeded", status); err != nil {
		p.managedWriteErrorResource(w, 503, "AUDIT_UNAVAILABLE", "标签操作成功但审计更新失败", op, resourceID)
		return
	}
	if method == http.MethodDelete {
		writeJSON(w, 200, map[string]any{"deleted": true, "operationId": op})
		return
	}
	writeJSON(w, status, map[string]any{"data": data, "operationId": op})
}

// TenantID is not present on the tag response type in all WeKnora builds;
// membership in the complete, KB-scoped tags list is the ownership proof.
func (t *managedTag) UnmarshalJSON(raw []byte) error {
	type alias managedTag
	var v struct {
		alias
		Tenant json.Number `json:"tenant_id"`
		BaseID string      `json:"knowledge_base_id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*t = managedTag(v.alias)
	return nil
}

func (p *portal) handleManagedKnowledgeFolders(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, kb, ok := p.managedOperationsBase(w, r, ident)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		if r.URL.RawQuery != "" {
			p.managedWriteError(w, 400, "INVALID_QUERY", "不支持查询参数", "")
			return
		}
		resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "knowledge-bases/"+url.PathEscape(kb.ID)+"/knowledge/folders", nil, nil, "")
		if err != nil {
			p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "文件夹读取失败", "")
			return
		}
		var tree managedFolderTree
		if decodeManagedData(resp, &tree) != nil {
			p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "文件夹数据无效", "")
			return
		}
		if err := p.auditManagedKnowledgeRead(r.Context(), ident, tenant, "list_folders", "knowledge_base", kb.ID, ""); err != nil {
			p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法记录文件夹读取审计", "")
			return
		}
		writeJSON(w, 200, map[string]any{"data": tree})
		return
	}
	if r.Method == http.MethodPost {
		var in struct {
			DocumentIDs []string `json:"document_ids"`
			FolderPath  string   `json:"folder_path"`
		}
		if decodeStrictJSON(w, r, &in, 64<<10) != nil || !validManagedBatchIDs(in.DocumentIDs, 200) || len(in.FolderPath) > 512 || !safeManagedFolderPath(in.FolderPath) || !managedFolderPathWithinLimits(in.FolderPath) {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "文件夹移动字段无效", "")
			return
		}
		if !p.managedValidateDocuments(w, r, key, tenant, ident.DeploymentID, kb.ID, in.DocumentIDs) {
			return
		}
		folderPath := normalizeManagedFolderPath(in.FolderPath)
		payload := map[string]any{"kb_id": kb.ID, "knowledge_ids": in.DocumentIDs, "folder_path": folderPath}
		p.managedOperationMutation(w, r, ident, key, tenant, kb.ID, "move_documents", "document_batch", strings.Join(in.DocumentIDs, ","), "POST", "knowledge/folder", payload, "")
		return
	}
	if r.Method == http.MethodPut {
		var in struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if decodeStrictJSON(w, r, &in, 16<<10) != nil || strings.TrimSpace(in.From) == "" || strings.TrimSpace(in.To) == "" || len(in.From) > 512 || len(in.To) > 512 || !safeManagedFolderPath(in.From) || !safeManagedFolderPath(in.To) || !managedFolderPathWithinLimits(in.From) || !managedFolderPathWithinLimits(in.To) {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "文件夹路径无效", "")
			return
		}
		from := normalizeManagedFolderPath(in.From)
		to := normalizeManagedFolderPath(in.To)
		if from == "" || to == "" {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "Folder path is invalid", "")
			return
		}
		payload := map[string]string{"from": from, "to": to}
		p.managedOperationMutation(w, r, ident, key, tenant, kb.ID, "rename_folder", "knowledge_folder", from, "PUT", "knowledge-bases/"+url.PathEscape(kb.ID)+"/knowledge/folders", payload, "")
		return
	}
	p.managedWriteError(w, 405, "METHOD_NOT_ALLOWED", "不支持的请求方法", "")
}

func validManagedBatchIDs(ids []string, max int) bool {
	if len(ids) == 0 || len(ids) > max {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if _, ok := safeKnowledgeID(id); !ok || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func safeManagedFolderPath(value string) bool {
	if strings.ContainsRune(value, '\x00') {
		return false
	}
	for _, segment := range strings.Split(strings.ReplaceAll(value, "\\", "/"), "/") {
		segment = strings.TrimSpace(segment)
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

const (
	managedFolderMaxDepth        = 16
	managedFolderMaxSegmentBytes = 128
	managedFolderMaxPathBytes    = 1024
)

// managedFolderPathWithinLimits rejects paths that WeKnora would silently
// truncate, so the audited path and the persisted path remain identical.
func managedFolderPathWithinLimits(value string) bool {
	path := normalizeManagedFolderPath(value)
	if len(path) > managedFolderMaxPathBytes {
		return false
	}
	if path == "" {
		return true
	}
	segments := strings.Split(path, "/")
	if len(segments) > managedFolderMaxDepth {
		return false
	}
	for _, segment := range segments {
		if len(segment) > managedFolderMaxSegmentBytes {
			return false
		}
	}
	return true
}

// normalizeManagedFolderPath mirrors WeKnora's NormalizeKnowledgeFolderPath
// for Portal payloads so the stored path and the audited path cannot diverge.
func normalizeManagedFolderPath(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	segments := strings.Split(value, "/")
	normalized := make([]string, 0, len(segments))
	for _, segment := range segments {
		segment = strings.TrimRight(strings.TrimSpace(segment), ". ")
		if segment == "" {
			continue
		}
		normalized = append(normalized, segment)
	}
	return strings.Join(normalized, "/")
}

func (p *portal) managedValidateDocuments(w http.ResponseWriter, r *http.Request, key, tenant, deployment, baseID string, ids []string) bool {
	for _, id := range ids {
		doc, found, err := p.managedFetchDocument(r.Context(), key, tenant, deployment, baseID, id)
		if err != nil {
			p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "批量资料归属验证失败", "")
			return false
		}
		if !found || doc.KnowledgeBaseID != baseID {
			p.managedWriteError(w, 404, "KNOWLEDGE_NOT_FOUND", "批量操作包含不属于当前知识库的资料", "")
			return false
		}
	}
	return true
}

func (p *portal) handleManagedKnowledgeTagsForDocuments(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, kb, ok := p.managedOperationsBase(w, r, ident)
	if !ok {
		return
	}
	var in struct {
		Updates map[string][]string `json:"updates"`
	}
	if decodeStrictJSON(w, r, &in, 256<<10) != nil || len(in.Updates) == 0 || len(in.Updates) > 200 {
		p.managedWriteError(w, 400, "INVALID_REQUEST", "资料标签更新字段无效", "")
		return
	}
	ids := make([]string, 0, len(in.Updates))
	for id, tags := range in.Updates {
		if _, ok := safeKnowledgeID(id); !ok || len(tags) > 200 {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "资料标签更新字段无效", "")
			return
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if !p.managedValidateDocuments(w, r, key, tenant, ident.DeploymentID, kb.ID, ids) {
		return
	}
	validTags, err := p.managedTagUUIDSet(r, key, kb.ID)
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "无法验证标签归属", "")
		return
	}
	for _, tags := range in.Updates {
		for _, tag := range tags {
			if tag != "" && !validTags[tag] {
				p.managedWriteError(w, 404, "TAG_NOT_FOUND", "标签不属于当前知识库", " ")
				return
			}
		}
	}
	payload := map[string]any{"kb_id": kb.ID, "updates": in.Updates}
	p.managedOperationMutation(w, r, ident, key, tenant, kb.ID, "update_document_tags", "document_batch", strings.Join(ids, ","), "PUT", "knowledge/tags", payload, "")
}

func (p *portal) managedTagUUIDSet(r *http.Request, key, baseID string) (map[string]bool, error) {
	resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "knowledge-bases/"+url.PathEscape(baseID)+"/tags", url.Values{"page": {"1"}, "page_size": {"200"}}, nil, "")
	if err != nil {
		return nil, err
	}
	var page struct {
		Data  []managedTag `json:"data"`
		Total int64        `json:"total"`
	}
	if decodeManagedPage(resp, &page) != nil || page.Total > int64(len(page.Data)) {
		return nil, errors.New("incomplete tag list")
	}
	set := map[string]bool{}
	for _, tag := range page.Data {
		set[tag.ID] = true
	}
	return set, nil
}

func (p *portal) handleManagedKnowledgeFAQ(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, kb, ok := p.managedOperationsBase(w, r, ident)
	if !ok {
		return
	}
	if kb.Type != "faq" {
		p.managedWriteError(w, 409, "FAQ_NOT_SUPPORTED", "仅 FAQ 知识库支持 FAQ 管理", "")
		return
	}
	q := url.Values{}
	for name, values := range r.URL.Query() {
		switch name {
		case "page", "page_size", "keyword", "search_field", "sort_order", "is_enabled", "tag_ids":
		default:
			p.managedWriteError(w, 400, "INVALID_QUERY", "查询参数无效", "")
			return
		}
		if len(values) != 1 || len(values[0]) > 512 {
			p.managedWriteError(w, 400, "INVALID_QUERY", "查询参数无效", "")
			return
		}
		q.Set(name, values[0])
	}
	for _, name := range []string{"page", "page_size"} {
		if val := q.Get(name); val != "" {
			n, e := strconv.Atoi(val)
			max := 200
			if name == "page" {
				max = 1 << 30
			}
			if e != nil || n < 1 || (name == "page_size" && n > max) {
				p.managedWriteError(w, 400, "INVALID_QUERY", "分页参数无效", "")
				return
			}
		}
	}
	resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "knowledge-bases/"+url.PathEscape(kb.ID)+"/faq/entries", q, nil, "")
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "FAQ 列表读取失败", "")
		return
	}
	var page managedFAQPage
	if decodeManagedPage(resp, &page) != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "FAQ 列表无效", "")
		return
	}
	for _, entry := range page.Data {
		if entry.ID <= 0 || entry.KnowledgeBaseID != kb.ID || entry.KnowledgeID == "" {
			p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "FAQ 列表包含无效归属", "")
			return
		}
	}
	if err := p.auditManagedKnowledgeRead(r.Context(), ident, tenant, "list_faq", "knowledge_base", kb.ID, ""); err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法记录 FAQ 读取审计", "")
		return
	}
	writeJSON(w, 200, page)
}

func (p *portal) handleManagedKnowledgeFAQEntry(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, kb, ok := p.managedOperationsBase(w, r, ident)
	if !ok {
		return
	}
	if kb.Type != "faq" {
		p.managedWriteError(w, 409, "FAQ_NOT_SUPPORTED", "仅 FAQ 知识库支持 FAQ 管理", "")
		return
	}
	entryID := r.PathValue("entryID")
	if entryID != "" {
		n, e := strconv.ParseInt(entryID, 10, 64)
		if e != nil || n <= 0 {
			p.managedWriteError(w, 404, "FAQ_NOT_FOUND", "未找到 FAQ 条目", "")
			return
		}
		entryID = strconv.FormatInt(n, 10)
	}
	var in managedFAQPayload
	if r.Method == http.MethodPost {
		if decodeStrictJSON(w, r, &in, 256<<10) != nil || strings.TrimSpace(in.StandardQuestion) == "" {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "FAQ 字段无效", "")
			return
		}
		if !p.managedValidateFAQTag(w, r, key, kb.ID, in.TagID) {
			return
		}
		p.managedFAQMutation(w, r, ident, key, tenant, kb, "create_faq", "POST", "knowledge-bases/"+url.PathEscape(kb.ID)+"/faq/entry", in, "")
		return
	}
	if r.Method == http.MethodPut {
		if entryID == "" || decodeStrictJSON(w, r, &in, 256<<10) != nil || strings.TrimSpace(in.StandardQuestion) == "" {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "FAQ 字段无效", "")
			return
		}
		old, found := p.managedGetFAQEntry(w, r, key, tenant, ident.DeploymentID, kb.ID, entryID)
		if !found {
			return
		}
		if !p.managedValidateFAQTag(w, r, key, kb.ID, in.TagID) {
			return
		}
		p.managedFAQMutation(w, r, ident, key, tenant, kb, "update_faq", "PUT", "knowledge-bases/"+url.PathEscape(kb.ID)+"/faq/entries/"+entryID, in, fmt.Sprint(old.ID))
		return
	}
	p.managedWriteError(w, 405, "METHOD_NOT_ALLOWED", "不支持的请求方法", "")
}

type managedFAQPayload struct {
	StandardQuestion  string   `json:"standard_question"`
	SimilarQuestions  []string `json:"similar_questions"`
	NegativeQuestions []string `json:"negative_questions"`
	Answers           []string `json:"answers"`
	AnswerStrategy    *string  `json:"answer_strategy,omitempty"`
	TagID             int64    `json:"tag_id,omitempty"`
	IsEnabled         *bool    `json:"is_enabled,omitempty"`
	IsRecommended     *bool    `json:"is_recommended,omitempty"`
}

func (p *portal) managedValidateFAQTag(w http.ResponseWriter, r *http.Request, key, baseID string, seq int64) bool {
	if seq <= 0 {
		return true
	}
	resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "knowledge-bases/"+url.PathEscape(baseID)+"/tags", url.Values{"page": {"1"}, "page_size": {"200"}}, nil, "")
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "FAQ 标签验证失败", "")
		return false
	}
	var page struct {
		Data  []managedTag `json:"data"`
		Total int64        `json:"total"`
	}
	if decodeManagedPage(resp, &page) != nil || page.Total > int64(len(page.Data)) {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "FAQ 标签无法验证", "")
		return false
	}
	for _, tag := range page.Data {
		if tag.SeqID == seq {
			return true
		}
	}
	p.managedWriteError(w, 404, "TAG_NOT_FOUND", "FAQ 标签不属于当前知识库", "")
	return false
}

func (p *portal) managedGetFAQEntry(w http.ResponseWriter, r *http.Request, key, tenant, deployment, baseID, id string) (managedFAQEntry, bool) {
	resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "knowledge-bases/"+url.PathEscape(baseID)+"/faq/entries/"+id, nil, nil, "")
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "FAQ 归属验证失败", "")
		return managedFAQEntry{}, false
	}
	var entry managedFAQEntry
	if decodeManagedData(resp, &entry) != nil || entry.ID <= 0 || entry.KnowledgeBaseID != baseID || entry.KnowledgeID == "" {
		p.managedWriteError(w, 404, "FAQ_NOT_FOUND", "未找到当前知识库中的 FAQ 条目", "")
		return managedFAQEntry{}, false
	}
	if _, found, err := p.managedFetchDocument(r.Context(), key, tenant, deployment, baseID, entry.KnowledgeID); err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "FAQ 资料归属验证失败", "")
		return managedFAQEntry{}, false
	} else if !found {
		p.managedWriteError(w, 404, "FAQ_NOT_FOUND", "FAQ 条目不属于当前知识库", "")
		return managedFAQEntry{}, false
	}
	return entry, true
}

func (p *portal) managedFAQMutation(w http.ResponseWriter, r *http.Request, ident identity, key, tenant string, kb managedKB, action, method, path string, payload any, resourceID string) {
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, action, "faq_entry", resourceID, kb.ID)
	if err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法记录 FAQ 操作审计", "")
		return
	}
	raw, _ := json.Marshal(payload)
	resp, err := p.managedRequest(r.Context(), key, method, path, nil, bytes.NewReader(raw), "application/json")
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, resourceID, "uncertain", 502)
		p.managedWriteErrorResource(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "FAQ 操作结果待核实", op, resourceID)
		return
	}
	var entry managedFAQEntry
	decodeErr := decodeManagedData(resp, &entry)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		status, outcome := managedOperationFailure(resp.StatusCode)
		_ = p.finishManagedAudit(r.Context(), op, resourceID, outcome, resp.StatusCode)
		p.managedWriteErrorResource(w, status, codeForManagedOutcome(outcome), "FAQ 操作失败或结果待核实", op, resourceID)
		return
	}
	if decodeErr != nil || entry.KnowledgeBaseID != kb.ID || entry.ID <= 0 || entry.KnowledgeID == "" || (resourceID != "" && strconv.FormatInt(entry.ID, 10) != resourceID) {
		_ = p.finishManagedAudit(r.Context(), op, "", "unverified", http.StatusBadGateway)
		p.managedWriteErrorResource(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_INVALID", "FAQ 已发送变更，但归属结果无法验证", op, "")
		return
	}
	if _, found, err := p.managedFetchDocument(r.Context(), key, tenant, ident.DeploymentID, kb.ID, entry.KnowledgeID); err != nil || !found {
		_ = p.finishManagedAudit(r.Context(), op, "", "unverified", http.StatusBadGateway)
		p.managedWriteErrorResource(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_INVALID", "FAQ 已发送变更，但资料归属无法验证", op, "")
		return
	}
	resourceID = strconv.FormatInt(entry.ID, 10)
	if err := p.finishManagedAudit(r.Context(), op, resourceID, "succeeded", resp.StatusCode); err != nil {
		p.managedWriteErrorResource(w, 503, "AUDIT_UNAVAILABLE", "FAQ 已变更但审计更新失败", op, resourceID)
		return
	}
	writeJSON(w, resp.StatusCode, map[string]any{"data": entry, "operationId": op})
}

func (p *portal) handleManagedKnowledgeFAQBatch(w http.ResponseWriter, r *http.Request, ident identity) {
	action := r.PathValue("batchAction")
	if (r.Method == http.MethodDelete && action != "delete") || (r.Method == http.MethodPut && action != "fields" && action != "tags") || (r.Method != http.MethodDelete && r.Method != http.MethodPut) {
		p.managedWriteError(w, http.StatusNotFound, "NOT_FOUND", "未找到操作", "")
		return
	}
	key, tenant, kb, ok := p.managedOperationsBase(w, r, ident)
	if !ok {
		return
	}
	if kb.Type != "faq" {
		p.managedWriteError(w, 409, "FAQ_NOT_SUPPORTED", "仅 FAQ 知识库支持 FAQ 管理", "")
		return
	}
	switch r.PathValue("batchAction") {
	case "fields":
		var in struct {
			ByID map[int64]struct {
				IsEnabled     *bool  `json:"is_enabled,omitempty"`
				IsRecommended *bool  `json:"is_recommended,omitempty"`
				TagID         *int64 `json:"tag_id,omitempty"`
			} `json:"by_id"`
		}
		if decodeStrictJSON(w, r, &in, 256<<10) != nil || len(in.ByID) == 0 || len(in.ByID) > 200 {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "FAQ 批量字段无效", "")
			return
		}
		ids := make([]int64, 0, len(in.ByID))
		for id, v := range in.ByID {
			if id <= 0 || (v.IsEnabled == nil && v.IsRecommended == nil && v.TagID == nil) {
				p.managedWriteError(w, 400, "INVALID_REQUEST", "FAQ 批量字段无效", "")
				return
			}
			ids = append(ids, id)
			if v.TagID != nil && !p.managedValidateFAQTag(w, r, key, kb.ID, *v.TagID) {
				return
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			if _, found := p.managedGetFAQEntry(w, r, key, tenant, ident.DeploymentID, kb.ID, strconv.FormatInt(id, 10)); !found {
				return
			}
		}
		payload := map[string]any{"by_id": in.ByID}
		p.managedOperationMutation(w, r, ident, key, tenant, kb.ID, "update_faq_fields", "faq_batch", fmt.Sprint(ids), "PUT", "knowledge-bases/"+url.PathEscape(kb.ID)+"/faq/entries/fields", payload, "")
	case "tags":
		var in struct {
			Updates map[int64]*int64 `json:"updates"`
		}
		if decodeStrictJSON(w, r, &in, 128<<10) != nil || len(in.Updates) == 0 || len(in.Updates) > 200 {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "FAQ 标签批量字段无效", "")
			return
		}
		ids := make([]int64, 0, len(in.Updates))
		for id, tag := range in.Updates {
			if id <= 0 {
				p.managedWriteError(w, 400, "INVALID_REQUEST", "FAQ 条目标识无效", "")
				return
			}
			ids = append(ids, id)
			if tag != nil && !p.managedValidateFAQTag(w, r, key, kb.ID, *tag) {
				return
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for _, id := range ids {
			if _, found := p.managedGetFAQEntry(w, r, key, tenant, ident.DeploymentID, kb.ID, strconv.FormatInt(id, 10)); !found {
				return
			}
		}
		p.managedOperationMutation(w, r, ident, key, tenant, kb.ID, "update_faq_tags", "faq_batch", fmt.Sprint(ids), "PUT", "knowledge-bases/"+url.PathEscape(kb.ID)+"/faq/entries/tags", map[string]any{"updates": in.Updates}, "")
	case "delete":
		var in struct {
			IDs []int64 `json:"ids"`
		}
		if decodeStrictJSON(w, r, &in, 128<<10) != nil || len(in.IDs) == 0 || len(in.IDs) > 200 {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "FAQ 删除范围无效", "")
			return
		}
		seen := map[int64]bool{}
		for _, id := range in.IDs {
			if id <= 0 || seen[id] {
				p.managedWriteError(w, 400, "INVALID_REQUEST", "FAQ 标识无效", "")
				return
			}
			seen[id] = true
		}
		sort.Slice(in.IDs, func(i, j int) bool { return in.IDs[i] < in.IDs[j] })
		for _, id := range in.IDs {
			if _, found := p.managedGetFAQEntry(w, r, key, tenant, ident.DeploymentID, kb.ID, strconv.FormatInt(id, 10)); !found {
				return
			}
		}
		p.managedOperationMutation(w, r, ident, key, tenant, kb.ID, "delete_faq_entries", "faq_batch", fmt.Sprint(in.IDs), "DELETE", "knowledge-bases/"+url.PathEscape(kb.ID)+"/faq/entries", map[string]any{"ids": in.IDs}, "")
	default:
		p.managedWriteError(w, 404, "NOT_FOUND", "未找到操作", "")
	}
}

func (p *portal) handleManagedKnowledgeDocumentTags(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, doc, ok := p.managedDocumentContext(w, r, ident)
	if !ok {
		return
	}
	var in struct {
		TagIDs []string `json:"tag_ids"`
	}
	if decodeStrictJSON(w, r, &in, 32<<10) != nil || len(in.TagIDs) > 200 {
		p.managedWriteError(w, 400, "INVALID_REQUEST", "标签字段无效", "")
		return
	}
	set, err := p.managedTagUUIDSet(r, key, doc.KnowledgeBaseID)
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_INVALID", "无法验证标签归属", "")
		return
	}
	for _, id := range in.TagIDs {
		if !set[id] {
			p.managedWriteError(w, 404, "TAG_NOT_FOUND", "标签不属于当前知识库", "")
			return
		}
	}
	p.managedOperationMutation(w, r, ident, key, tenant, doc.KnowledgeBaseID, "update_document_tags", "document", doc.ID, "PUT", "knowledge/tags", map[string]any{"kb_id": doc.KnowledgeBaseID, "updates": map[string][]string{doc.ID: in.TagIDs}}, doc.ID)
}

func (p *portal) handleManagedKnowledgeChunkUpdate(w http.ResponseWriter, r *http.Request, ident identity) {
	key, tenant, doc, ok := p.managedDocumentContext(w, r, ident)
	if !ok {
		return
	}
	chunkID, safe := safeKnowledgeID(r.PathValue("chunkID"))
	if !safe {
		p.managedWriteError(w, 404, "CHUNK_NOT_FOUND", "未找到分块", "")
		return
	}
	var in struct {
		Content          *string `json:"content,omitempty"`
		IsEnabled        *bool   `json:"is_enabled,omitempty"`
		ExpectedRevision *int    `json:"expected_revision,omitempty"`
	}
	if decodeStrictJSON(w, r, &in, 1<<20) != nil || (in.Content == nil && in.IsEnabled == nil) || (in.Content != nil && len(*in.Content) > 1<<20) || (in.Content != nil && in.ExpectedRevision == nil) || (in.ExpectedRevision != nil && *in.ExpectedRevision < 0) {
		p.managedWriteError(w, 400, "INVALID_REQUEST", "分块更新字段无效", "")
		return
	}
	resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "chunks/by-id/"+url.PathEscape(chunkID), nil, nil, "")
	if err != nil {
		p.managedWriteError(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "分块归属验证失败", "")
		return
	}
	var old struct {
		ID              string      `json:"id"`
		KnowledgeID     string      `json:"knowledge_id"`
		KnowledgeBaseID string      `json:"knowledge_base_id"`
		TenantID        json.Number `json:"tenant_id"`
		SeqID           int64       `json:"seq_id"`
		ChunkType       string      `json:"chunk_type"`
		Content         string      `json:"content"`
		IsEnabled       bool        `json:"is_enabled"`
		ContentRevision int         `json:"content_revision"`
	}
	if decodeManagedData(resp, &old) != nil || old.ID != chunkID || old.KnowledgeID != doc.ID || old.KnowledgeBaseID != doc.KnowledgeBaseID || old.TenantID.String() != tenant {
		p.managedWriteError(w, 404, "CHUNK_NOT_FOUND", "未找到当前资料中的分块", "")
		return
	}
	if in.ExpectedRevision != nil && *in.ExpectedRevision != old.ContentRevision {
		p.managedWriteError(w, 409, "KNOWLEDGE_CONFLICT", "分块已被其他操作修改，请刷新后重试", "")
		return
	}
	in.ExpectedRevision = &old.ContentRevision
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, "update_chunk", "chunk", chunkID, doc.ID)
	if err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法记录分块更新审计", "")
		return
	}
	raw, _ := json.Marshal(in)
	updatedResp, err := p.managedRequest(r.Context(), key, http.MethodPut, "chunks/"+url.PathEscape(doc.ID)+"/"+url.PathEscape(chunkID), nil, bytes.NewReader(raw), "application/json")
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, chunkID, "uncertain", 502)
		p.managedWriteErrorResource(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "分块更新结果待核实", op, chunkID)
		return
	}
	var updated struct {
		ID              string      `json:"id"`
		KnowledgeID     string      `json:"knowledge_id"`
		KnowledgeBaseID string      `json:"knowledge_base_id"`
		TenantID        json.Number `json:"tenant_id"`
		Content         string      `json:"content"`
		IsEnabled       bool        `json:"is_enabled"`
		ContentRevision int         `json:"content_revision"`
		IndexStatus     string      `json:"index_status"`
		SeqID           int64       `json:"seq_id"`
		ChunkType       string      `json:"chunk_type"`
	}
	decodeErr := decodeManagedData(updatedResp, &updated)
	expectedContent := old.Content
	if in.Content != nil {
		expectedContent = strings.TrimSpace(*in.Content)
	}
	expectedEnabled := old.IsEnabled
	if in.IsEnabled != nil {
		expectedEnabled = *in.IsEnabled
	}
	changed := expectedContent != old.Content || expectedEnabled != old.IsEnabled
	expectedRevision := old.ContentRevision
	if changed {
		expectedRevision++
	}
	if updatedResp.StatusCode == 409 {
		_ = p.finishManagedAudit(r.Context(), op, chunkID, "failed", 409)
		p.managedWriteErrorResource(w, 409, "KNOWLEDGE_CONFLICT", "分块已被其他操作修改，请刷新后重试", op, chunkID)
		return
	}
	if updatedResp.StatusCode < 200 || updatedResp.StatusCode >= 300 || decodeErr != nil || updated.ID != chunkID || updated.KnowledgeID != doc.ID || updated.KnowledgeBaseID != doc.KnowledgeBaseID || updated.TenantID.String() != tenant || updated.SeqID != old.SeqID || updated.ChunkType != old.ChunkType || updated.Content != expectedContent || updated.IsEnabled != expectedEnabled || !managedChunkIndexStatusValid(updated.IndexStatus) || updated.ContentRevision != expectedRevision {
		status, outcome := managedOperationFailure(updatedResp.StatusCode)
		_ = p.finishManagedAudit(r.Context(), op, chunkID, outcome, updatedResp.StatusCode)
		p.managedWriteErrorResource(w, status, codeForManagedOutcome(outcome), "分块更新失败或结果待核实", op, chunkID)
		return
	}
	if err := p.finishManagedAudit(r.Context(), op, chunkID, "succeeded", 200); err != nil {
		p.managedWriteErrorResource(w, 503, "AUDIT_UNAVAILABLE", "分块已更新但审计结果更新失败", op, chunkID)
		return
	}
	writeJSON(w, 200, map[string]any{"data": updated, "operationId": op})
}

func managedChunkIndexStatusValid(value string) bool {
	switch value {
	case "ready", "processing", "failed":
		return true
	default:
		return false
	}
}

func (p *portal) managedOperationMutation(w http.ResponseWriter, r *http.Request, ident identity, key, tenant, parentID, action, resourceType, resourceID, method, path string, payload any, contentType string) {
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, action, resourceType, resourceID, parentID)
	if err != nil {
		p.managedWriteError(w, 503, "AUDIT_UNAVAILABLE", "无法记录知识运营操作审计", "")
		return
	}
	var body io.Reader
	if payload != nil {
		raw, _ := json.Marshal(payload)
		body = bytes.NewReader(raw)
		contentType = "application/json"
	}
	resp, err := p.managedRequest(r.Context(), key, method, path, nil, body, contentType)
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, resourceID, "uncertain", 502)
		p.managedWriteErrorResource(w, 502, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "操作结果待核实", op, resourceID)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	status := resp.StatusCode
	if status < 200 || status >= 300 {
		mapped, outcome := managedOperationFailure(status)
		if err := p.finishManagedAudit(r.Context(), op, resourceID, outcome, status); err != nil {
			p.managedWriteErrorResource(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "操作已发送但审计状态更新失败", op, resourceID)
			return
		}
		p.managedWriteErrorResource(w, mapped, codeForManagedOutcome(outcome), "操作被拒绝或结果待核实", op, resourceID)
		return
	}
	const maxOperationResponseBytes = 4 << 20
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxOperationResponseBytes+1))
	if readErr != nil || len(raw) > maxOperationResponseBytes {
		_ = p.finishManagedAudit(r.Context(), op, resourceID, "uncertain", http.StatusBadGateway)
		p.managedWriteErrorResource(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_INVALID", "操作响应无法完整验证，结果待核实", op, resourceID)
		return
	}
	result, decodeErr := decodeManagedOperationResult(raw, action, payload)
	if decodeErr != nil {
		_ = p.finishManagedAudit(r.Context(), op, resourceID, "unverified", http.StatusBadGateway)
		p.managedWriteErrorResource(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_INVALID", "操作响应无法安全验证，结果待核实", op, resourceID)
		return
	}
	if err := p.finishManagedAudit(r.Context(), op, resourceID, "succeeded", status); err != nil {
		p.managedWriteErrorResource(w, 503, "AUDIT_UNAVAILABLE", "操作已提交但审计结果更新失败", op, resourceID)
		return
	}
	writeJSON(w, status, map[string]any{"data": result, "operationId": op})
}

type managedOperationResult struct {
	Outcome       string `json:"outcome"`
	AffectedCount *int64 `json:"affectedCount,omitempty"`
	FolderPath    string `json:"folderPath,omitempty"`
}

func decodeManagedOperationResult(raw []byte, action string, payload any) (managedOperationResult, error) {
	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message,omitempty"`
		Code    string          `json:"code,omitempty"`
		Data    json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return managedOperationResult{}, err
	}
	if envelope.Success == nil || !*envelope.Success {
		return managedOperationResult{}, errors.New("upstream operation was not confirmed")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return managedOperationResult{}, errors.New("unexpected trailing response")
	}
	result := managedOperationResult{Outcome: "succeeded"}
	if action != "move_documents" && action != "rename_folder" {
		return result, nil
	}
	var data struct {
		MovedCount int64  `json:"moved_count"`
		FolderPath string `json:"folder_path"`
	}
	if len(envelope.Data) == 0 || json.Unmarshal(envelope.Data, &data) != nil || data.MovedCount < 0 {
		return managedOperationResult{}, errors.New("folder operation result missing")
	}
	expected := ""
	switch request := payload.(type) {
	case map[string]any:
		if value, ok := request["folder_path"].(string); ok {
			expected = value
		}
		if value, ok := request["to"].(string); ok {
			expected = value
		}
	case map[string]string:
		if value, ok := request["folder_path"]; ok {
			expected = value
		}
		if value, ok := request["to"]; ok {
			expected = value
		}
	}
	if data.FolderPath != expected {
		return managedOperationResult{}, errors.New("folder operation result does not match request")
	}
	result.AffectedCount = &data.MovedCount
	result.FolderPath = data.FolderPath
	return result, nil
}

func (p *portal) auditManagedKnowledgeRead(ctx context.Context, ident identity, tenant, action, resourceType, resourceID, parent string) error {
	op, err := p.beginManagedAudit(ctx, ident, tenant, action, resourceType, resourceID, parent)
	if err != nil {
		return err
	}
	return p.finishManagedAudit(ctx, op, resourceID, "succeeded", http.StatusOK)
}
