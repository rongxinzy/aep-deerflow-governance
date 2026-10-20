package main

import (
	"net/http"
	"net/url"
	"strconv"
)

func sharedRuntimeResourceAction(action string) bool {
	return action == "space-resources" || action == "space-document" || action == "space-attachment"
}

// The caller has passed runtimeAuthorize. Resource scope comes only from the
// active binding, never a caller supplied space or task identifier.
func (p *portal) sharedRuntimeResource(w http.ResponseWriter, r *http.Request, b runtimeBinding) error {
	q := r.URL.Query()
	generation, err := strconv.Atoi(q.Get("generation"))
	if err != nil || generation != b.Generation || q.Get("piTaskId") == "" || q.Get("piTaskId") != b.PiTaskID || q.Get("executionId") == "" || q.Get("executionId") != b.ExecutionID {
		return sharedFail(409, "执行代次已变化")
	}
	s, err := p.sharedRuntimeSpace(r.Context(), b.SpaceID)
	if err != nil {
		return err
	}
	source := map[string]any{"spaceId": b.SpaceID, "taskId": b.TaskID}
	switch r.PathValue("action") {
	case "space-attachment":
		if q.Get("attachmentId") == "" {
			return sharedFail(400, "请提供附件标识")
		}
		return p.sharedAttachmentContentInSpace(w, r, s, b.TaskID, q.Get("attachmentId"))
	case "space-document":
		key := q.Get("key")
		if !sharedValidReferenceKey(key) {
			return sharedFail(400, "资料标识必须以zhiyuan-reference-开头")
		}
		var doc map[string]any
		if err = p.sharedPC(r.Context(), "GET", "/issues/"+url.PathEscape(b.TaskID)+"/documents/"+url.PathEscape(key), nil, &doc); err != nil {
			return err
		}
		if doc["issueId"] != b.TaskID || doc["companyId"] != s.CompanyID || doc["key"] != key {
			return sharedFail(502, "资料服务返回了错误的任务范围")
		}
		source["documentKey"], source["documentId"], source["revisionId"] = key, doc["id"], doc["latestRevisionId"]
		writeJSON(w, 200, map[string]any{"document": sharedReferenceDTO(doc), "source": source})
	case "space-resources":
		var docs []map[string]any
		if err = p.sharedPC(r.Context(), "GET", "/issues/"+url.PathEscape(b.TaskID)+"/documents", nil, &docs); err != nil {
			return err
		}
		safe := []map[string]any{}
		for _, doc := range docs {
			if doc["issueId"] != b.TaskID || doc["companyId"] != s.CompanyID {
				return sharedFail(502, "资料服务返回了错误的任务范围")
			}
			key, _ := doc["key"].(string)
			if !sharedValidReferenceKey(key) {
				continue
			}
			out := map[string]any{}
			for _, field := range []string{"key", "id", "title", "latestRevisionId", "updatedAt"} {
				if v, ok := doc[field]; ok {
					out[field] = v
				}
			}
			safe = append(safe, out)
		}
		attachments, err := p.sharedReadAttachments(r.Context(), b.TaskID)
		if err != nil {
			return err
		}
		files := []map[string]any{}
		for _, a := range attachments {
			files = append(files, map[string]any{"id": a.ID, "taskId": b.TaskID, "originalFilename": a.OriginalFilename, "contentType": a.ContentType, "byteSize": a.ByteSize, "createdAt": a.CreatedAt, "updatedAt": a.UpdatedAt})
		}
		writeJSON(w, 200, map[string]any{"documents": safe, "attachments": files, "source": source})
	}
	return nil
}
