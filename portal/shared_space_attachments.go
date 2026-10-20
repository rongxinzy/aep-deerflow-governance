package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
)

type sharedAttachment struct {
	ID               string `json:"id"`
	TaskID           string `json:"taskId"`
	OriginalFilename string `json:"originalFilename"`
	ContentType      string `json:"contentType"`
	ByteSize         int64  `json:"byteSize"`
	CreatedAt        string `json:"createdAt"`
	UpdatedAt        string `json:"updatedAt"`
	ContentPath      string `json:"contentPath"`
	DownloadPath     string `json:"downloadPath"`
}
type sharedNativeAttachment struct {
	sharedAttachment
	IssueID string `json:"issueId"`
}

func sharedAttachmentLimit() int64 { return 10 << 20 }

func sharedAttachmentDTO(s sharedSpace, task string, a sharedNativeAttachment) sharedAttachment {
	out := a.sharedAttachment
	out.TaskID = task
	out.ContentPath = "/api/v1/shared-spaces/" + url.PathEscape(s.ID) + "/tasks/" + url.PathEscape(task) + "/attachments/" + url.PathEscape(a.ID) + "/content"
	out.DownloadPath = out.ContentPath + "?download=1"
	return out
}
func sharedAttachmentReason(role string, task map[string]any) string {
	if !sharedCaps(role)["createTask"] {
		return "当前成员只能查看附件"
	}
	if v := task["conversationAgentId"]; v != nil && v != "" {
		return "会话任务暂不支持上传附件"
	}
	return ""
}
func (p *portal) sharedReadAttachments(ctx context.Context, task string) ([]sharedNativeAttachment, error) {
	out := []sharedNativeAttachment{}
	err := p.sharedPC(ctx, "GET", "/issues/"+url.PathEscape(task)+"/attachments", nil, &out)
	if err != nil {
		return nil, err
	}
	for _, a := range out {
		if a.ID == "" || a.IssueID != task {
			return nil, sharedFail(502, "附件任务范围无法确认")
		}
	}
	return out, nil
}
func (p *portal) sharedAttachments(w http.ResponseWriter, r *http.Request, i identity) error {
	s, err := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	task := r.PathValue("task")
	native, err := p.sharedCheckTask(r.Context(), s, task)
	if err != nil {
		return err
	}
	attachments, err := p.sharedReadAttachments(r.Context(), task)
	if err != nil {
		return err
	}
	out := []sharedAttachment{}
	for _, a := range attachments {
		out = append(out, sharedAttachmentDTO(s, task, a))
	}
	caps := map[string]any{"uploadAttachment": sharedAttachmentReason(s.Role, native) == ""}
	if reason := sharedAttachmentReason(s.Role, native); reason != "" {
		caps["uploadAttachmentUnavailableReason"] = reason
	}
	writeJSON(w, 200, map[string]any{"attachments": out, "maxUploadBytes": sharedAttachmentLimit(), "capabilities": caps})
	return nil
}
func sharedAttachmentReceipt(ctx context.Context, c *sql.Conn, s, task, request, actor, hash string) (map[string]any, bool, error) {
	var priorActor, action, priorTask, priorHash, state string
	var raw []byte
	err := c.QueryRowContext(ctx, `SELECT actor_id,action,task_id,payload_hash,state,receipt FROM shared_space_commands WHERE space_id=$1 AND request_id=$2`, s, request).Scan(&priorActor, &action, &priorTask, &priorHash, &state, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if priorActor != actor || action != "upload_attachment" || priorTask != "attachment:"+task+":"+request || (hash != "" && hash != priorHash) {
		return nil, false, sharedFail(409, "请求编号已用于其他操作")
	}
	if state == "applied" {
		var receipt map[string]any
		err = json.Unmarshal(raw, &receipt)
		return receipt, true, err
	}
	return map[string]any{"requestId": request, "status": state}, true, nil
}
func (p *portal) sharedAttachmentCommand(w http.ResponseWriter, r *http.Request, i identity) error {
	c, release, err := p.sharedLock(r.Context(), r.PathValue("space"))
	if err != nil {
		return err
	}
	defer release()
	s, err := p.sharedLoad(r.Context(), c, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	if _, err = p.sharedCheckTask(r.Context(), s, r.PathValue("task")); err != nil {
		return err
	}
	receipt, ok, err := sharedAttachmentReceipt(r.Context(), c, s.ID, r.PathValue("task"), r.PathValue("request"), i.UserID, "")
	if err != nil {
		return err
	}
	if !ok {
		return sharedFail(404, "未找到附件操作回执")
	}
	writeJSON(w, 200, receipt)
	return nil
}
func (p *portal) sharedUploadAttachment(w http.ResponseWriter, r *http.Request, i identity) error {
	c, release, err := p.sharedLock(r.Context(), r.PathValue("space"))
	if err != nil {
		return err
	}
	defer release()
	s, err := p.sharedLoad(r.Context(), c, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	task := r.PathValue("task")
	native, err := p.sharedCheckTask(r.Context(), s, task)
	if err != nil {
		return err
	}
	if reason := sharedAttachmentReason(s.Role, native); reason != "" {
		return sharedFail(403, reason)
	}
	limit := sharedAttachmentLimit()
	r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20))
	if err = r.ParseMultipartForm(limit); err != nil {
		return sharedFail(400, "附件表单无效或超过大小限制")
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	if !sharedValidRequestID(r.FormValue("requestId")) || len(r.MultipartForm.File["file"]) != 1 || len(r.MultipartForm.File) != 1 {
		return sharedFail(400, "请选择一个附件并提供请求编号")
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		return sharedFail(400, "缺少附件")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	if len(data) == 0 || int64(len(data)) > limit {
		return sharedFail(400, "附件为空或超过大小限制")
	}
	request := r.FormValue("requestId")
	ct := header.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	hash := sharedHash([]any{header.Filename, ct, data})
	if receipt, ok, e := sharedAttachmentReceipt(r.Context(), c, s.ID, task, request, i.UserID, hash); e != nil {
		return e
	} else if ok {
		code := 200
		if receipt["status"] != "applied" {
			code = 202
		}
		writeJSON(w, code, receipt)
		return nil
	}
	if err = sharedStart(r.Context(), c, s.ID, "attachment:"+task+":"+request, request, i.UserID, "upload_attachment", hash, map[string]any{"taskId": task}); err != nil {
		return err
	}
	unknown := func() {
		_ = sharedUnknown(c, s.ID, request)
		writeJSON(w, 202, map[string]any{"requestId": request, "status": "unknown"})
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": header.Filename}))
	h.Set("Content-Type", ct)
	part, err := mw.CreatePart(h)
	if err != nil {
		unknown()
		return nil
	}
	if _, err = part.Write(data); err != nil {
		unknown()
		return nil
	}
	if err = mw.Close(); err != nil {
		unknown()
		return nil
	}
	req, err := http.NewRequestWithContext(r.Context(), "POST", p.cfg.paperclipBaseURL+"/companies/"+url.PathEscape(s.CompanyID)+"/issues/"+url.PathEscape(task)+"/attachments", &body)
	if err != nil {
		unknown()
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.paperclipToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := sharedHTTP.Do(req)
	if err != nil {
		unknown()
		return nil
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 400 && res.StatusCode < 500 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&failure)
		if failure.Error == "" {
			failure.Error = "任务服务拒绝上传附件"
		}
		if _, err = c.ExecContext(r.Context(), `DELETE FROM shared_space_commands WHERE space_id=$1 AND request_id=$2 AND state='sending'`, s.ID, request); err != nil {
			unknown()
			return nil
		}
		return sharedFail(res.StatusCode, failure.Error)
	}
	var a sharedNativeAttachment
	if res.StatusCode != 201 || json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&a) != nil || a.ID == "" || a.IssueID != task {
		unknown()
		return nil
	}
	receipt := map[string]any{"requestId": request, "status": "applied", "attachment": sharedAttachmentDTO(s, task, a)}
	tx, err := c.BeginTx(r.Context(), nil)
	if err != nil {
		unknown()
		return nil
	}
	defer func() { _ = tx.Rollback() }()
	if err = sharedFinish(r.Context(), tx, s.ID, request, receipt); err != nil {
		_ = tx.Rollback()
		unknown()
		return nil
	}
	if err = tx.Commit(); err != nil {
		unknown()
		return nil
	}
	writeJSON(w, 201, receipt)
	return nil
}
func (p *portal) sharedAttachmentContent(w http.ResponseWriter, r *http.Request, i identity) error {
	s, err := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	task := r.PathValue("task")
	if _, err = p.sharedCheckTask(r.Context(), s, task); err != nil {
		return err
	}
	attachments, err := p.sharedReadAttachments(r.Context(), task)
	if err != nil {
		return err
	}
	found := false
	for _, a := range attachments {
		if a.ID == r.PathValue("attachment") {
			found = true
			break
		}
	}
	if !found {
		return sharedFail(404, "附件不属于当前任务")
	}
	path := "/attachments/" + url.PathEscape(r.PathValue("attachment")) + "/content"
	if r.URL.Query().Get("download") == "1" {
		path += "?download=true"
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", p.cfg.paperclipBaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.paperclipToken)
	if v := r.Header.Get("Range"); v != "" {
		req.Header.Set("Range", v)
	}
	res, err := sharedHTTP.Do(req)
	if err != nil {
		return sharedFail(502, "附件内容暂不可用")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 && res.StatusCode != 206 && res.StatusCode != 416 {
		return sharedFail(502, "附件内容暂不可用")
	}
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Disposition", "Content-Range", "Accept-Ranges", "Cache-Control", "X-Content-Type-Options", "Content-Security-Policy"} {
		if v := res.Header.Get(key); v != "" {
			w.Header().Set(key, v)
		}
	}
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, res.Body)
	return nil
}

// A single two-segment route avoids ServeMux's ambiguous commands/content intersection.
func (p *portal) sharedAttachmentReadRoute(w http.ResponseWriter, r *http.Request, i identity) error {
	if r.PathValue("attachment") == "commands" {
		r.SetPathValue("request", r.PathValue("operation"))
		return p.sharedAttachmentCommand(w, r, i)
	}
	if r.PathValue("operation") != "content" {
		return sharedFail(404, "未找到附件入口")
	}
	return p.sharedAttachmentContent(w, r, i)
}
