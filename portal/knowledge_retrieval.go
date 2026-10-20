package main

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type employeeKnowledgeSearchRequest struct {
	Query            string   `json:"query"`
	KnowledgeBaseIDs []string `json:"knowledge_base_ids"`
	Mode             string   `json:"mode"`
	Limit            int      `json:"limit"`
}

type employeeKnowledgeSearchSource struct {
	KnowledgeBaseID string `json:"knowledge_base_id"`
	DocumentID      string `json:"document_id"`
	ChunkID         string `json:"chunk_id"`
	Title           string `json:"title"`
}

type employeeKnowledgeSearchResult struct {
	Score   float64                       `json:"score"`
	Content string                        `json:"content"`
	Source  employeeKnowledgeSearchSource `json:"source"`
}

type employeeKnowledgeSourceDocument struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	FileName string `json:"file_name"`
	FileType string `json:"file_type"`
	Source   string `json:"source"`
	BaseID   string `json:"knowledge_base_id"`
}

type employeeKnowledgeSourceChunk struct {
	ID        string `json:"id"`
	SeqID     int64  `json:"seq_id"`
	ChunkType string `json:"chunk_type"`
	Content   string `json:"content"`
}

func (p *portal) employeeKnowledgeReadContext(w http.ResponseWriter, r *http.Request, ident identity) (identity, string, string, bool) {
	// The native Admin Console authenticates with an AEP bearer token and
	// does not need a separate chat/Portal cookie. Cookie-authenticated reads
	// still check durable Portal revocation; both paths reverify AEP below.
	if bearer(r) == "" {
		cookie, err := r.Cookie(portalSessionCookie)
		if err != nil || !p.knowledgeSessionActive(r.Context(), cookie.Value, ident.UserID) {
			p.managedWriteError(w, http.StatusUnauthorized, "KNOWLEDGE_SESSION_UNAVAILABLE", "知识库会话已失效，请重新登录", "")
			return identity{}, "", "", false
		}
	}
	current, err := p.verifyKnowledgeRequester(r.Context(), ident)
	if err != nil {
		p.managedWriteError(w, http.StatusUnauthorized, "KNOWLEDGE_IDENTITY_UNAVAILABLE", "无法校验知识库访问身份，请重新登录", "")
		return identity{}, "", "", false
	}
	key, tenant, ok := p.managedContext(w, r, current)
	if !ok {
		return identity{}, "", "", false
	}
	return current, key, tenant, true
}

func (p *portal) handleEmployeeKnowledgeSearch(w http.ResponseWriter, r *http.Request, ident identity) {
	w.Header().Set("Cache-Control", "private, no-store")
	employee := strings.TrimSpace(r.PathValue("name"))
	if !namePattern.MatchString(employee) {
		p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "数字员工名称无效", "")
		return
	}
	var input employeeKnowledgeSearchRequest
	if err := decodeStrictJSON(w, r, &input, 32<<10); err != nil {
		p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "知识库检索请求无效", "")
		return
	}
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" || len(input.Query) > 4000 || input.Mode != "hybrid" || input.Limit < 1 || input.Limit > 30 || input.KnowledgeBaseIDs == nil || len(input.KnowledgeBaseIDs) == 0 || len(input.KnowledgeBaseIDs) > 100 {
		p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "需要 query、hybrid 模式、limit 1-30 与显式 knowledge_base_ids", "")
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
	current, key, tenant, ok := p.employeeKnowledgeReadContext(w, r, ident)
	if !ok {
		return
	}
	allowed, err := p.resolveEmployeeKnowledgeAccess(r.Context(), current, employee, input.KnowledgeBaseIDs)
	if err != nil {
		p.managedWriteError(w, http.StatusServiceUnavailable, "KNOWLEDGE_AUTHORIZATION_UNAVAILABLE", "知识库授权校验暂不可用", "")
		return
	}
	if len(allowed) != len(input.KnowledgeBaseIDs) {
		p.managedWriteError(w, http.StatusForbidden, "KNOWLEDGE_NOT_GRANTED", "该数字员工没有绑定所请求的知识库", "")
		return
	}
	results := make([]employeeKnowledgeSearchResult, 0, input.Limit)
	for _, baseID := range allowed {
		hits, failure := p.searchKnowledgeBase(r.Context(), key, tenant, current.DeploymentID, baseID, input)
		if failure != nil {
			p.managedWriteError(w, failure.status, failure.code, failure.message, "")
			return
		}
		results = append(results, hits...)
		if err := p.auditKnowledgeRuntime(r.Context(), current, tenant, "employee_search", "knowledge_base", baseID, ""); err != nil {
			p.managedWriteError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "无法记录知识库检索审计", "")
			return
		}
	}
	// Search each authorized base separately; return only a bounded, stable DTO.
	sortEmployeeKnowledgeResults(results)
	if len(results) > input.Limit {
		results = results[:input.Limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": input.Query, "mode": input.Mode, "results": results})
}

type knowledgeReadFailure struct {
	status        int
	code, message string
}

// Both management checks and employee reads use the same native search and
// ownership verification. Authorization is completed by the caller, separately.
func (p *portal) searchKnowledgeBase(ctx context.Context, key, tenant, deployment, baseID string, input employeeKnowledgeSearchRequest) ([]employeeKnowledgeSearchResult, *knowledgeReadFailure) {
	kb, found, err := p.managedFetchKB(ctx, key, tenant, deployment, baseID)
	if err != nil {
		return nil, &knowledgeReadFailure{http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库服务暂不可用"}
	}
	if !found || kb.TenantID != tenant {
		return nil, &knowledgeReadFailure{http.StatusForbidden, "KNOWLEDGE_ACCESS_DENIED", "所请求的知识库不属于当前部署或租户"}
	}
	payload, _ := json.Marshal(map[string]any{
		"query_text": input.Query, "vector_threshold": 0.5, "keyword_threshold": 0.3,
		"match_count": input.Limit,
	})
	resp, err := p.managedRequest(ctx, key, http.MethodPost,
		"knowledge-bases/"+url.PathEscape(baseID)+"/hybrid-search", nil, strings.NewReader(string(payload)), "application/json")
	if err != nil {
		return nil, &knowledgeReadFailure{http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库检索暂不可用"}
	}
	var hits []struct {
		ID             string  `json:"id"`
		Content        string  `json:"content"`
		KnowledgeID    string  `json:"knowledge_id"`
		KnowledgeTitle string  `json:"knowledge_title"`
		Score          float64 `json:"score"`
	}
	if err := decodeManagedData(resp, &hits); err != nil {
		return nil, &knowledgeReadFailure{http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_INVALID", "知识库检索结果无法校验"}
	}
	results := make([]employeeKnowledgeSearchResult, 0, len(hits))
	verifiedDocuments := make(map[string]managedKnowledge)
	for _, hit := range hits {
		if _, safe := safeKnowledgeID(hit.ID); !safe || hit.KnowledgeID == "" || math.IsNaN(hit.Score) || math.IsInf(hit.Score, 0) {
			return nil, &knowledgeReadFailure{http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_INVALID", "知识库检索结果归属校验失败"}
		}
		doc, checked := verifiedDocuments[hit.KnowledgeID]
		if !checked {
			var docFound bool
			doc, docFound, err = p.managedFetchDocument(ctx, key, tenant, deployment, baseID, hit.KnowledgeID)
			if err != nil {
				return nil, &knowledgeReadFailure{http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库资料元数据暂不可用"}
			}
			if !docFound {
				return nil, &knowledgeReadFailure{http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_INVALID", "知识库资料归属校验失败"}
			}
			verifiedDocuments[hit.KnowledgeID] = doc
		}
		results = append(results, employeeKnowledgeSearchResult{
			Score: hit.Score, Content: hit.Content,
			Source: employeeKnowledgeSearchSource{KnowledgeBaseID: baseID, DocumentID: hit.KnowledgeID, ChunkID: hit.ID, Title: doc.Name},
		})
	}
	return results, nil
}

func sortEmployeeKnowledgeResults(results []employeeKnowledgeSearchResult) {
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
}

func (p *portal) handleEmployeeKnowledgeDocument(w http.ResponseWriter, r *http.Request, ident identity) {
	w.Header().Set("Cache-Control", "private, no-store")
	employee := strings.TrimSpace(r.PathValue("name"))
	documentID, safe := safeKnowledgeID(r.PathValue("documentID"))
	if !namePattern.MatchString(employee) || !safe {
		p.managedWriteError(w, http.StatusNotFound, "KNOWLEDGE_NOT_FOUND", "未找到知识库资料", "")
		return
	}
	page, pageSize, ok := p.knowledgeSourcePagination(w, r)
	if !ok {
		return
	}
	current, key, tenant, ok := p.employeeKnowledgeReadContext(w, r, ident)
	if !ok {
		return
	}
	doc, found, err := p.managedFetchDocument(r.Context(), key, tenant, current.DeploymentID, "", documentID)
	if err != nil {
		p.managedWriteError(w, http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库资料暂不可用", "")
		return
	}
	if !found || p.authorizeEmployeeKnowledgeBase(r.Context(), current, employee, doc.KnowledgeBaseID) != nil {
		p.managedWriteError(w, http.StatusForbidden, "KNOWLEDGE_NOT_GRANTED", "该数字员工没有绑定该知识库或无权访问", "")
		return
	}
	response, failure := p.readKnowledgeSource(r.Context(), key, tenant, doc, page, pageSize)
	if failure != nil {
		p.managedWriteError(w, failure.status, failure.code, failure.message, "")
		return
	}
	if err := p.auditKnowledgeRuntime(r.Context(), current, tenant, "employee_read_source", "document", doc.ID, doc.KnowledgeBaseID); err != nil {
		p.managedWriteError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "无法记录知识库资料读取审计", "")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (p *portal) readKnowledgeSource(ctx context.Context, key, tenant string, doc managedKnowledge, page, pageSize int) (map[string]any, *knowledgeReadFailure) {
	documentID := doc.ID
	query := url.Values{"page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)}}
	// The upstream defaults to text chunks. FAQ sources contain faq chunks;
	// choose the fixed type from verified document metadata, never client input.
	if doc.Type == "faq" {
		query.Set("chunk_type", "faq")
	} else {
		query.Set("chunk_type", "text")
	}
	resp, err := p.managedRequest(ctx, key, http.MethodGet, "chunks/"+url.PathEscape(documentID), query, nil, "")
	if err != nil {
		return nil, &knowledgeReadFailure{http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库资料内容暂不可用"}
	}
	var chunkPage struct {
		Data  []managedChunk `json:"data"`
		Total int            `json:"total"`
		Page  int            `json:"page"`
		Size  int            `json:"page_size"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&chunkPage)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || decodeErr != nil || chunkPage.Page != page || chunkPage.Size != pageSize {
		return nil, &knowledgeReadFailure{http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_INVALID", "知识库资料内容无法校验"}
	}
	chunks := make([]employeeKnowledgeSourceChunk, 0, len(chunkPage.Data))
	for _, chunk := range chunkPage.Data {
		if chunk.ID == "" || chunk.KnowledgeID != doc.ID || chunk.KnowledgeBaseID != doc.KnowledgeBaseID || chunk.TenantID.String() != tenant {
			return nil, &knowledgeReadFailure{http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_INVALID", "知识库资料内容归属校验失败"}
		}
		chunks = append(chunks, employeeKnowledgeSourceChunk{ID: chunk.ID, SeqID: chunk.SeqID, ChunkType: chunk.ChunkType, Content: chunk.Content})
	}
	return map[string]any{
		"document": employeeKnowledgeSourceDocument{ID: doc.ID, Title: doc.Name, FileName: doc.FileName, FileType: doc.FileType, Source: doc.Source, BaseID: doc.KnowledgeBaseID},
		"chunks":   chunks, "total": chunkPage.Total, "page": chunkPage.Page, "page_size": chunkPage.Size,
	}, nil
}

func (p *portal) handleEmployeeKnowledgeChunkImage(w http.ResponseWriter, r *http.Request, ident identity) {
	w.Header().Set("Cache-Control", "private, no-store")
	employee := strings.TrimSpace(r.PathValue("name"))
	if !namePattern.MatchString(employee) {
		p.managedWriteError(w, http.StatusNotFound, "KNOWLEDGE_IMAGE_NOT_FOUND", "未找到知识库图片", "")
		return
	}
	current, key, tenant, ok := p.employeeKnowledgeReadContext(w, r, ident)
	if !ok {
		return
	}
	doc, found, err := p.managedFetchDocument(r.Context(), key, tenant, current.DeploymentID, "", r.PathValue("documentID"))
	if err != nil || !found || p.authorizeEmployeeKnowledgeBase(r.Context(), current, employee, doc.KnowledgeBaseID) != nil {
		p.managedWriteError(w, http.StatusForbidden, "KNOWLEDGE_NOT_GRANTED", "该数字员工没有绑定该知识库或无权访问", "")
		return
	}
	p.handleManagedKnowledgeChunkImage(w, r, current)
}

func (p *portal) handleEmployeeKnowledgeDocumentDownload(w http.ResponseWriter, r *http.Request, ident identity) {
	w.Header().Set("Cache-Control", "private, no-store")
	employee := strings.TrimSpace(r.PathValue("name"))
	if !namePattern.MatchString(employee) {
		p.managedWriteError(w, http.StatusNotFound, "KNOWLEDGE_NOT_FOUND", "未找到知识库资料", "")
		return
	}
	current, key, tenant, ok := p.employeeKnowledgeReadContext(w, r, ident)
	if !ok {
		return
	}
	doc, found, err := p.managedFetchDocument(r.Context(), key, tenant, current.DeploymentID, "", r.PathValue("documentID"))
	if err != nil || !found || p.authorizeEmployeeKnowledgeBase(r.Context(), current, employee, doc.KnowledgeBaseID) != nil {
		p.managedWriteError(w, http.StatusForbidden, "KNOWLEDGE_NOT_GRANTED", "该数字员工没有绑定该知识库或无权访问", "")
		return
	}
	r.SetPathValue("fileKind", "download")
	// The existing file handler independently verifies document ownership,
	// content type and size, then logs the real actor in the managed audit log.
	p.handleManagedKnowledgeFile(w, r, current)
}
