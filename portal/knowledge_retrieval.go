package main

import (
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
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "knowledge session unavailable"})
			return identity{}, "", "", false
		}
	}
	current, err := p.verifyKnowledgeRequester(r.Context(), ident)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "knowledge identity unavailable"})
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid employee name"})
		return
	}
	var input employeeKnowledgeSearchRequest
	if err := decodeStrictJSON(w, r, &input, 32<<10); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid knowledge search request"})
		return
	}
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" || len(input.Query) > 4000 || input.Mode != "hybrid" || input.Limit < 1 || input.Limit > 30 || input.KnowledgeBaseIDs == nil || len(input.KnowledgeBaseIDs) == 0 || len(input.KnowledgeBaseIDs) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query, hybrid mode, limit 1-30 and explicit knowledge_base_ids are required"})
		return
	}
	seen := make(map[string]struct{}, len(input.KnowledgeBaseIDs))
	for _, id := range input.KnowledgeBaseIDs {
		if _, ok := safeKnowledgeID(id); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid knowledge base id"})
			return
		}
		if _, duplicate := seen[id]; duplicate {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "duplicate knowledge base id"})
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
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge authorization unavailable"})
		return
	}
	if len(allowed) != len(input.KnowledgeBaseIDs) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "knowledge access denied"})
		return
	}
	results := make([]employeeKnowledgeSearchResult, 0, input.Limit)
	for _, baseID := range allowed {
		kb, found, err := p.managedFetchKB(r.Context(), key, tenant, current.DeploymentID, baseID)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge service unavailable"})
			return
		}
		if !found || kb.TenantID != tenant {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "knowledge access denied"})
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"query_text": input.Query, "vector_threshold": 0.5, "keyword_threshold": 0.3,
			"match_count": input.Limit,
		})
		resp, err := p.managedRequest(r.Context(), key, http.MethodPost,
			"knowledge-bases/"+url.PathEscape(baseID)+"/hybrid-search", nil, strings.NewReader(string(payload)), "application/json")
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge search unavailable"})
			return
		}
		var hits []struct {
			ID             string  `json:"id"`
			Content        string  `json:"content"`
			KnowledgeID    string  `json:"knowledge_id"`
			KnowledgeTitle string  `json:"knowledge_title"`
			Score          float64 `json:"score"`
		}
		if err := decodeManagedData(resp, &hits); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge search response invalid"})
			return
		}
		verifiedDocuments := make(map[string]managedKnowledge)
		for _, hit := range hits {
			if _, safe := safeKnowledgeID(hit.ID); !safe || hit.KnowledgeID == "" || math.IsNaN(hit.Score) || math.IsInf(hit.Score, 0) {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge search response ownership invalid"})
				return
			}
			doc, checked := verifiedDocuments[hit.KnowledgeID]
			if !checked {
				var docFound bool
				doc, docFound, err = p.managedFetchDocument(r.Context(), key, tenant, current.DeploymentID, baseID, hit.KnowledgeID)
				if err != nil {
					writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge search source unavailable"})
					return
				}
				if !docFound {
					writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge search source ownership invalid"})
					return
				}
				verifiedDocuments[hit.KnowledgeID] = doc
			}
			results = append(results, employeeKnowledgeSearchResult{
				Score: hit.Score, Content: hit.Content,
				Source: employeeKnowledgeSearchSource{KnowledgeBaseID: baseID, DocumentID: hit.KnowledgeID, ChunkID: hit.ID, Title: doc.Name},
			})
		}
		if err := p.auditKnowledgeRuntime(r.Context(), current, tenant, "employee_search", "knowledge_base", baseID, ""); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge audit unavailable"})
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

func sortEmployeeKnowledgeResults(results []employeeKnowledgeSearchResult) {
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
}

func (p *portal) handleEmployeeKnowledgeDocument(w http.ResponseWriter, r *http.Request, ident identity) {
	w.Header().Set("Cache-Control", "private, no-store")
	employee := strings.TrimSpace(r.PathValue("name"))
	documentID, safe := safeKnowledgeID(r.PathValue("documentID"))
	if !namePattern.MatchString(employee) || !safe {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "knowledge document not found"})
		return
	}
	for key, values := range r.URL.Query() {
		if (key != "page" && key != "page_size") || len(values) != 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported source query"})
			return
		}
	}
	page, pageSize := 1, 20
	if raw := r.URL.Query().Get("page"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 1_000_000 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid page"})
			return
		}
		page = value
	}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid page_size"})
			return
		}
		pageSize = value
	}
	current, key, tenant, ok := p.employeeKnowledgeReadContext(w, r, ident)
	if !ok {
		return
	}
	doc, found, err := p.managedFetchDocument(r.Context(), key, tenant, current.DeploymentID, "", documentID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge document unavailable"})
		return
	}
	if !found || p.authorizeEmployeeKnowledgeBase(r.Context(), current, employee, doc.KnowledgeBaseID) != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "knowledge access denied"})
		return
	}
	query := url.Values{"page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)}}
	// The upstream defaults to text chunks. FAQ sources contain faq chunks;
	// choose the fixed type from verified document metadata, never client input.
	if doc.Type == "faq" {
		query.Set("chunk_type", "faq")
	} else {
		query.Set("chunk_type", "text")
	}
	resp, err := p.managedRequest(r.Context(), key, http.MethodGet, "chunks/"+url.PathEscape(documentID), query, nil, "")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge source unavailable"})
		return
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
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge source response invalid"})
		return
	}
	chunks := make([]employeeKnowledgeSourceChunk, 0, len(chunkPage.Data))
	for _, chunk := range chunkPage.Data {
		if chunk.ID == "" || chunk.KnowledgeID != doc.ID || chunk.KnowledgeBaseID != doc.KnowledgeBaseID || chunk.TenantID.String() != tenant {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "knowledge source ownership invalid"})
			return
		}
		chunks = append(chunks, employeeKnowledgeSourceChunk{ID: chunk.ID, SeqID: chunk.SeqID, ChunkType: chunk.ChunkType, Content: chunk.Content})
	}
	if err := p.auditKnowledgeRuntime(r.Context(), current, tenant, "employee_read_source", "document", doc.ID, doc.KnowledgeBaseID); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge audit unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"document": employeeKnowledgeSourceDocument{ID: doc.ID, Title: doc.Name, FileName: doc.FileName, FileType: doc.FileType, Source: doc.Source, BaseID: doc.KnowledgeBaseID},
		"chunks":   chunks, "total": chunkPage.Total, "page": chunkPage.Page, "page_size": chunkPage.Size,
	})
}

func (p *portal) handleEmployeeKnowledgeChunkImage(w http.ResponseWriter, r *http.Request, ident identity) {
	w.Header().Set("Cache-Control", "private, no-store")
	employee := strings.TrimSpace(r.PathValue("name"))
	if !namePattern.MatchString(employee) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "knowledge image not found"})
		return
	}
	current, key, tenant, ok := p.employeeKnowledgeReadContext(w, r, ident)
	if !ok {
		return
	}
	doc, found, err := p.managedFetchDocument(r.Context(), key, tenant, current.DeploymentID, "", r.PathValue("documentID"))
	if err != nil || !found || p.authorizeEmployeeKnowledgeBase(r.Context(), current, employee, doc.KnowledgeBaseID) != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "knowledge access denied"})
		return
	}
	p.handleManagedKnowledgeChunkImage(w, r, current)
}

func (p *portal) handleEmployeeKnowledgeDocumentDownload(w http.ResponseWriter, r *http.Request, ident identity) {
	w.Header().Set("Cache-Control", "private, no-store")
	employee := strings.TrimSpace(r.PathValue("name"))
	if !namePattern.MatchString(employee) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "knowledge document not found"})
		return
	}
	current, key, tenant, ok := p.employeeKnowledgeReadContext(w, r, ident)
	if !ok {
		return
	}
	doc, found, err := p.managedFetchDocument(r.Context(), key, tenant, current.DeploymentID, "", r.PathValue("documentID"))
	if err != nil || !found || p.authorizeEmployeeKnowledgeBase(r.Context(), current, employee, doc.KnowledgeBaseID) != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "knowledge access denied"})
		return
	}
	r.SetPathValue("fileKind", "download")
	// The existing file handler independently verifies document ownership,
	// content type and size, then logs the real actor in the managed audit log.
	p.handleManagedKnowledgeFile(w, r, current)
}
