package main

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var sharedReferenceKey = regexp.MustCompile(`^zhiyuan-reference-[a-z0-9][a-z0-9_-]*$`)

func sharedValidReferenceKey(key string) bool {
	return len(key) <= 64 && sharedReferenceKey.MatchString(key)
}
func sharedReferenceDTO(document map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"id", "documentId", "key", "title", "format", "body", "latestRevisionId", "latestRevisionNumber", "revisionNumber", "changeSummary", "createdAt", "updatedAt"} {
		if v, ok := document[key]; ok {
			out[key] = v
		}
	}
	return out
}

func (p *portal) sharedDocuments(w http.ResponseWriter, r *http.Request, i identity) error {
	key, task := r.PathValue("key"), r.PathValue("task")
	if key != "" && !sharedValidReferenceKey(key) {
		return sharedFail(400, "资料标识必须以zhiyuan-reference-开头")
	}
	s, err := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	if _, err = p.sharedCheckTask(r.Context(), s, task); err != nil {
		return err
	}
	path := "/issues/" + url.PathEscape(task) + "/documents"
	if key != "" {
		path += "/" + url.PathEscape(key)
	}
	if strings.HasSuffix(r.URL.Path, "/revisions") {
		path += "/revisions"
	}
	if key == "" || strings.HasSuffix(path, "/revisions") {
		var docs []map[string]any
		if err = p.sharedPC(r.Context(), "GET", path, nil, &docs); err != nil {
			return err
		}
		out := []map[string]any{}
		for _, doc := range docs {
			docKey, _ := doc["key"].(string)
			if doc["issueId"] != task || doc["companyId"] != s.CompanyID {
				return sharedFail(502, "资料服务返回了错误的任务范围")
			}
			if sharedValidReferenceKey(docKey) && (key == "" || key == docKey) {
				out = append(out, sharedReferenceDTO(doc))
			}
		}
		field := "documents"
		if key != "" {
			field = "revisions"
		}
		writeJSON(w, 200, map[string]any{field: out})
		return nil
	}
	var doc map[string]any
	if err = p.sharedPC(r.Context(), "GET", path, nil, &doc); err != nil {
		return err
	}
	if doc["issueId"] != task || doc["companyId"] != s.CompanyID || doc["key"] != key {
		return sharedFail(502, "资料服务返回了错误的任务范围")
	}
	writeJSON(w, 200, map[string]any{"document": sharedReferenceDTO(doc)})
	return nil
}

func (p *portal) sharedPutDocument(w http.ResponseWriter, r *http.Request, i identity) error {
	key, task := r.PathValue("key"), r.PathValue("task")
	if !sharedValidReferenceKey(key) {
		return sharedFail(400, "资料标识必须以zhiyuan-reference-开头")
	}
	var b struct {
		Title          string  `json:"title"`
		Body           string  `json:"body"`
		BaseRevisionID *string `json:"baseRevisionId"`
		ChangeSummary  string  `json:"changeSummary"`
	}
	if err := sharedDecode(w, r, &b); err != nil {
		return err
	}
	if strings.TrimSpace(b.Title) == "" {
		return sharedFail(400, "请填写资料标题")
	}
	c, release, err := p.sharedLock(r.Context(), r.PathValue("space"))
	if err != nil {
		return err
	}
	defer release()
	s, err := p.sharedLoad(r.Context(), c, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	if !sharedCaps(s.Role)["createTask"] {
		return sharedFail(403, "当前成员只能查看")
	}
	if _, err = p.sharedCheckTask(r.Context(), s, task); err != nil {
		return err
	}
	body := map[string]any{"format": "markdown", "title": b.Title, "body": b.Body, "baseRevisionId": b.BaseRevisionID, "changeSummary": b.ChangeSummary}
	var doc map[string]any
	if err = p.sharedPC(r.Context(), "PUT", "/issues/"+url.PathEscape(task)+"/documents/"+url.PathEscape(key), body, &doc); err != nil {
		return err
	}
	if doc["issueId"] != task || doc["companyId"] != s.CompanyID || doc["key"] != key {
		return sharedFail(502, "资料服务返回了错误的任务范围")
	}
	writeJSON(w, 200, map[string]any{"document": sharedReferenceDTO(doc)})
	return nil
}
