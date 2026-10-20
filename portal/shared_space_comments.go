package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var sharedCommentRequest = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Native human comments can reopen closed/blocked work or approve execution
// policy reviews. Keep those tasks read-only through this discussion surface.
func sharedCanComment(task map[string]any) bool {
	return sharedCommentUnavailableReason(task) == ""
}

func sharedCommentUnavailableReason(task map[string]any) string {
	if task["status"] == "done" || task["status"] == "cancelled" || task["status"] == "blocked" {
		return "当前任务已结束或阻塞，讨论仅供查看；请通过任务流程继续处理"
	}
	for _, key := range []string{"executionRunId", "checkoutRunId", "executionState", "executionPolicy"} {
		if task[key] != nil && task[key] != "" {
			return "当前任务由Paperclip执行流程管理，讨论仅供查看"
		}
	}
	return ""
}

func sharedCommentDTO(comment map[string]any, author any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"id", "issueId", "body", "createdAt", "updatedAt"} {
		if v, ok := comment[key]; ok {
			out[key] = v
		}
	}
	if author == nil {
		author = map[string]any{"displayName": "Paperclip 发布者"}
	}
	out["author"] = author
	return out
}

func (p *portal) sharedComments(w http.ResponseWriter, r *http.Request, i identity) error {
	s, err := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	task := r.PathValue("task")
	if _, err = p.sharedCheckTask(r.Context(), s, task); err != nil {
		return err
	}
	var comments []map[string]any
	if err = p.sharedPC(r.Context(), "GET", "/issues/"+url.PathEscape(task)+"/comments?order=asc", nil, &comments); err != nil {
		return err
	}
	rows, err := p.db.QueryContext(r.Context(), `SELECT actor_id,receipt FROM shared_space_commands WHERE space_id=$1 AND receipt->'comment'->>'issueId'=$2 AND action='comment' AND state='applied'`, s.ID, task)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	authors := map[string]any{}
	for rows.Next() {
		var actor string
		var raw []byte
		if err = rows.Scan(&actor, &raw); err != nil {
			return err
		}
		var receipt struct {
			Comment struct {
				ID     string         `json:"id"`
				Author map[string]any `json:"author"`
			} `json:"comment"`
		}
		if err = json.Unmarshal(raw, &receipt); err != nil {
			return err
		}
		authors[receipt.Comment.ID] = map[string]any{"userId": actor, "displayName": receipt.Comment.Author["displayName"]}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	out := []map[string]any{}
	for _, comment := range comments {
		if comment["issueId"] != task {
			return sharedFail(502, "讨论服务返回了错误的任务范围")
		}
		id, _ := comment["id"].(string)
		out = append(out, sharedCommentDTO(comment, authors[id]))
	}
	writeJSON(w, 200, map[string]any{"comments": out})
	return nil
}

func (p *portal) sharedAddComment(w http.ResponseWriter, r *http.Request, i identity) error {
	var b struct {
		Body            string `json:"body"`
		ClientRequestID string `json:"clientRequestId"`
	}
	if err := sharedDecode(w, r, &b); err != nil {
		return err
	}
	if strings.TrimSpace(b.Body) == "" || !sharedCommentRequest.MatchString(b.ClientRequestID) {
		return sharedFail(400, "请填写讨论内容和有效的请求编号")
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
	task := r.PathValue("task")
	current, err := p.sharedCheckTask(r.Context(), s, task)
	if err != nil {
		return err
	}
	hash := sharedHash([]any{task, b})
	if receipt, ok, e := sharedReplay(r.Context(), c, s.ID, b.ClientRequestID, i.UserID, "comment", hash); e != nil {
		return e
	} else if ok {
		writeJSON(w, 200, receipt)
		return nil
	}
	if !sharedCanComment(current) {
		return sharedFail(409, sharedCommentUnavailableReason(current))
	}
	name := i.DisplayName
	if strings.TrimSpace(name) == "" {
		name = i.UserID
	}
	body := map[string]any{"body": b.Body + "\n\n" + name + " · 经知远发布", "clientRequestId": b.ClientRequestID}
	// A discussion acknowledgement cannot fence task assignment or review.
	// Its own request still replays or remains unknown without another dispatch.
	if err = sharedStart(r.Context(), c, s.ID, "comment:"+task+":"+b.ClientRequestID, b.ClientRequestID, i.UserID, "comment", hash, body); err != nil {
		return err
	}
	var comment map[string]any
	if err = p.sharedPC(r.Context(), "POST", "/issues/"+url.PathEscape(task)+"/comments", body, &comment); err != nil || comment["issueId"] != task || comment["id"] == nil {
		_ = sharedUnknown(c, s.ID, b.ClientRequestID)
		return sharedFail(423, "讨论发布结果待核验，请勿重复提交")
	}
	receipt := map[string]any{"comment": sharedCommentDTO(comment, map[string]any{"userId": i.UserID, "displayName": name})}
	tx, err := c.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = sharedFinish(r.Context(), tx, s.ID, b.ClientRequestID, receipt); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	writeJSON(w, 201, receipt)
	return nil
}
