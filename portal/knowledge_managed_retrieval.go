package main

import (
	"net/http"
	"strconv"
	"strings"
)

// Administrator inspection is a management capability. It deliberately does
// not call the employee/grant resolver; runtime reads retain that resolver.
// Routes apply freshAdminOnly, then managedContext validates the trusted
// deployment and tenant. Both search and source validate active registration.
func (p *portal) handleManagedKnowledgeSearch(w http.ResponseWriter, r *http.Request, ident identity) {
	w.Header().Set("Cache-Control", "private, no-store")
	baseID, safe := safeKnowledgeID(r.PathValue("baseID"))
	var input struct {
		Query string `json:"query"`
		Mode  string `json:"mode"`
		Limit int    `json:"limit"`
	}
	if !safe || len(r.URL.Query()) != 0 || decodeStrictJSON(w, r, &input, 32<<10) != nil {
		p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "知识库检索请求无效", "")
		return
	}
	input.Query = strings.TrimSpace(input.Query)
	if input.Query == "" || len(input.Query) > 4000 || input.Mode != "hybrid" || input.Limit < 1 || input.Limit > 30 {
		p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "需要 query、hybrid 模式与 limit 1-30", "")
		return
	}
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return
	}
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, "admin_search", "knowledge_base", baseID, "")
	if err != nil {
		p.managedWriteError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "无法记录知识库检索审计", "")
		return
	}
	results, failure := p.searchKnowledgeBase(r.Context(), key, tenant, ident.DeploymentID, baseID, employeeKnowledgeSearchRequest{Query: input.Query, Mode: input.Mode, Limit: input.Limit})
	if failure == nil {
		sortEmployeeKnowledgeResults(results)
		if len(results) > input.Limit {
			results = results[:input.Limit]
		}
	}
	p.writeManagedKnowledgeRead(w, r, op, baseID, map[string]any{"query": input.Query, "mode": input.Mode, "results": results}, failure)
}

func (p *portal) handleManagedKnowledgeSource(w http.ResponseWriter, r *http.Request, ident identity) {
	w.Header().Set("Cache-Control", "private, no-store")
	baseID, safeBase := safeKnowledgeID(r.PathValue("baseID"))
	documentID, safeDoc := safeKnowledgeID(r.PathValue("documentID"))
	if !safeBase || !safeDoc {
		p.managedWriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "知识库来源请求无效", "")
		return
	}
	page, size, ok := p.knowledgeSourcePagination(w, r)
	if !ok {
		return
	}
	key, tenant, ok := p.managedContext(w, r, ident)
	if !ok {
		return
	}
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, "admin_read_source", "document", documentID, baseID)
	if err != nil {
		p.managedWriteError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "无法记录知识库来源审计", "")
		return
	}
	doc, found, err := p.managedFetchDocument(r.Context(), key, tenant, ident.DeploymentID, baseID, documentID)
	var response map[string]any
	var failure *knowledgeReadFailure
	if err != nil {
		failure = &knowledgeReadFailure{http.StatusBadGateway, "KNOWLEDGE_UPSTREAM_UNAVAILABLE", "知识库资料归属无法校验"}
	} else if !found {
		failure = &knowledgeReadFailure{http.StatusForbidden, "KNOWLEDGE_ACCESS_DENIED", "所请求的资料不属于当前已登记知识库"}
	} else {
		response, failure = p.readKnowledgeSource(r.Context(), key, tenant, doc, page, size)
	}
	p.writeManagedKnowledgeRead(w, r, op, documentID, response, failure)
}

// Persist the real outcome before returning any content; failures never turn
// into successful empty results and audit outages do not expose source text.
func (p *portal) writeManagedKnowledgeRead(w http.ResponseWriter, r *http.Request, op, resourceID string, response any, failure *knowledgeReadFailure) {
	status, outcome := http.StatusOK, "succeeded"
	if failure != nil {
		status, outcome = failure.status, "failed"
	}
	if err := p.finishManagedAudit(r.Context(), op, resourceID, outcome, status); err != nil {
		p.managedWriteError(w, http.StatusServiceUnavailable, "AUDIT_UNAVAILABLE", "无法记录知识库读取结果", op)
		return
	}
	if failure != nil {
		p.managedWriteError(w, status, failure.code, failure.message, op)
		return
	}
	writeJSON(w, status, response)
}

func (p *portal) knowledgeSourcePagination(w http.ResponseWriter, r *http.Request) (int, int, bool) {
	for key, values := range r.URL.Query() {
		if (key != "page" && key != "page_size") || len(values) != 1 {
			p.managedWriteError(w, http.StatusBadRequest, "INVALID_QUERY", "不支持的来源查询参数", "")
			return 0, 0, false
		}
	}
	page, size := 1, 20
	for _, field := range []struct {
		name    string
		target  *int
		maximum int
	}{{"page", &page, 1_000_000}, {"page_size", &size, 100}} {
		if raw := r.URL.Query().Get(field.name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > field.maximum {
				p.managedWriteError(w, http.StatusBadRequest, "INVALID_QUERY", "来源页码或每页数量无效", "")
				return 0, 0, false
			}
			*field.target = value
		}
	}
	return page, size, true
}
