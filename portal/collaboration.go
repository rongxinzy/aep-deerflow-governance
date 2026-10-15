package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const collaborationSchema = `
CREATE TABLE IF NOT EXISTS companion_collaborations (
 id TEXT PRIMARY KEY, requester TEXT NOT NULL, owner_id TEXT NOT NULL,
 target TEXT NOT NULL, source TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS companion_collaboration_messages (
 id TEXT PRIMARY KEY, thread_id TEXT NOT NULL REFERENCES companion_collaborations(id), request_id TEXT NOT NULL,
 text TEXT NOT NULL, reply TEXT NOT NULL DEFAULT '', owner_question TEXT NOT NULL DEFAULT '', owner_reply TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL DEFAULT 'pending', returned TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
 attempt_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), UNIQUE(thread_id,request_id)
);
CREATE INDEX IF NOT EXISTS companion_collaboration_pending ON companion_collaboration_messages(state,attempt_at);
`

type collaborationSource struct {
	Workspace      string `json:"workspace"`
	Companion      string `json:"companion"`
	ConversationID int64  `json:"conversationId"`
}
type collaborationThread struct {
	ID        string              `json:"id"`
	Requester string              `json:"requester"`
	OwnerID   string              `json:"ownerId"`
	Target    colleagueCompanion  `json:"target"`
	Source    collaborationSource `json:"source"`
	CreatedAt time.Time           `json:"createdAt"`
}
type collaborationMessage struct {
	ID            string    `json:"id"`
	Text          string    `json:"text"`
	Reply         string    `json:"reply"`
	OwnerQuestion string    `json:"ownerQuestion"`
	OwnerReply    string    `json:"ownerReply"`
	State         string    `json:"state"`
	Returned      string    `json:"returned"`
	Error         string    `json:"error"`
	CreatedAt     time.Time `json:"createdAt"`
}

func collaborationID(parts ...string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x00"))))[:32]
}
func (p *portal) loadCollaboration(ctx context.Context, id, user string) (*collaborationThread, error) {
	var thread collaborationThread
	var target, source string
	err := p.db.QueryRowContext(ctx, `SELECT id,requester,owner_id,target,source,created_at FROM companion_collaborations WHERE id=$1 AND (requester=$2 OR owner_id=$2)`, id, user).Scan(&thread.ID, &thread.Requester, &thread.OwnerID, &target, &source, &thread.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(target), &thread.Target); err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(source), &thread.Source); err != nil {
		return nil, err
	}
	return &thread, nil
}
func (p *portal) collaborationMessages(ctx context.Context, id string) ([]collaborationMessage, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT id,text,reply,owner_question,owner_reply,state,returned,error,created_at FROM companion_collaboration_messages WHERE thread_id=$1 ORDER BY created_at,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []collaborationMessage{}
	for rows.Next() {
		var m collaborationMessage
		if err := rows.Scan(&m.ID, &m.Text, &m.Reply, &m.OwnerQuestion, &m.OwnerReply, &m.State, &m.Returned, &m.Error, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func (p *portal) getCollaboration(w http.ResponseWriter, r *http.Request, ident identity) {
	thread, err := p.loadCollaboration(r.Context(), r.PathValue("id"), ident.UserID)
	if err != nil {
		code := 500
		if errors.Is(err, sql.ErrNoRows) {
			code = 404
		}
		writeJSON(w, code, map[string]string{"error": "协作会话暂不可读取"})
		return
	}
	messages, err := p.collaborationMessages(r.Context(), thread.ID)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "协作消息暂不可读取"})
		return
	}
	writeJSON(w, 200, map[string]any{"thread": thread, "messages": messages})
}
func (p *portal) listCollaborations(w http.ResponseWriter, r *http.Request, ident identity) {
	rows, err := p.db.QueryContext(r.Context(), `SELECT c.id,c.requester,c.owner_id,c.target,c.source,c.created_at,
 EXISTS(SELECT 1 FROM companion_collaboration_messages m WHERE m.thread_id=c.id AND m.state='waiting_owner')
 FROM companion_collaborations c WHERE c.requester=$1 OR c.owner_id=$1 ORDER BY COALESCE((SELECT max(created_at) FROM companion_collaboration_messages WHERE thread_id=c.id),c.created_at) DESC LIMIT 100`, ident.UserID)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "协作列表暂不可用"})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var t collaborationThread
		var target, source string
		var waiting bool
		if err := rows.Scan(&t.ID, &t.Requester, &t.OwnerID, &target, &source, &t.CreatedAt, &waiting); err != nil {
			writeJSON(w, 503, map[string]string{"error": "协作列表暂不可用"})
			return
		}
		if json.Unmarshal([]byte(target), &t.Target) != nil || json.Unmarshal([]byte(source), &t.Source) != nil {
			writeJSON(w, 503, map[string]string{"error": "协作记录无效"})
			return
		}
		out = append(out, map[string]any{"thread": t, "waitingForOwner": waiting})
	}
	if rows.Err() != nil {
		writeJSON(w, 503, map[string]string{"error": "协作列表暂不可用"})
		return
	}
	writeJSON(w, 200, map[string]any{"collaborations": out})
}
func (p *portal) sendCollaboration(w http.ResponseWriter, r *http.Request, ident identity, agentSource *collaborationSource) {
	var input struct {
		TargetID             string              `json:"targetId"`
		Message              string              `json:"message"`
		RequestID            string              `json:"requestId"`
		Source               collaborationSource `json:"source"`
		SourceConversationID int64               `json:"sourceConversationId"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&input) != nil || strings.TrimSpace(input.Message) == "" || len(input.Message) > 12000 || input.RequestID == "" || len(input.RequestID) > 200 {
		writeJSON(w, 400, map[string]string{"error": "请填写消息"})
		return
	}
	if agentSource != nil {
		input.Source = *agentSource
		input.Source.ConversationID = input.SourceConversationID
	}
	if input.Source.Workspace != "" {
		var source dev.PersonalWorkspace
		if input.Source.ConversationID <= 0 || p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: input.Source.Workspace}, &source) != nil || source.Spec.Owner != ident.UserID {
			writeJSON(w, 403, map[string]string{"error": "原对话不属于你"})
			return
		}
		found := false
		for _, c := range source.Spec.Companions {
			if c.Name == input.Source.Companion {
				found = true
			}
		}
		if !found {
			writeJSON(w, 400, map[string]string{"error": "来源搭档不存在"})
			return
		}
		// Verify the conversation before saving a return address supplied by the UI.
		if agentSource == nil {
			if _, err := p.collaborationCall(r.Context(), &source, input.Source.Companion, "/api/conversations/"+fmt.Sprint(input.Source.ConversationID), nil); err != nil {
				writeJSON(w, 409, map[string]string{"error": "原对话暂不可用，请在搭档对话中重试"})
				return
			}
		}
	} else {
		input.Source = collaborationSource{}
	}
	target, _, err := p.collaborationTarget(r.Context(), input.TargetID, ident, true)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	targetJSON, _ := json.Marshal(target)
	sourceJSON, _ := json.Marshal(input.Source)
	id := collaborationID(ident.UserID, target.ID, target.OwnerID, string(sourceJSON))
	mid := collaborationID(id, input.RequestID)
	tx, err := p.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "消息尚未接收"})
		return
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(r.Context(), `INSERT INTO companion_collaborations(id,requester,owner_id,target,source) VALUES($1,$2,$3,$4,$5) ON CONFLICT(id) DO NOTHING`, id, ident.UserID, target.OwnerID, string(targetJSON), string(sourceJSON))
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO companion_collaboration_messages(id,thread_id,request_id,text) VALUES($1,$2,$3,$4) ON CONFLICT(thread_id,request_id) DO NOTHING`, mid, id, input.RequestID, input.Message)
	}
	var existing string
	if err == nil {
		err = tx.QueryRowContext(r.Context(), `SELECT text FROM companion_collaboration_messages WHERE id=$1`, mid).Scan(&existing)
	}
	if err == nil && existing != input.Message {
		writeJSON(w, 409, map[string]string{"error": "该发送请求已使用，请刷新后重新交办"})
		return
	}
	if err != nil || tx.Commit() != nil {
		writeJSON(w, 503, map[string]string{"error": "消息尚未确认接收，可使用同一请求重试"})
		return
	}
	writeJSON(w, 202, map[string]any{"id": id, "messageId": mid, "state": "pending", "note": "已保存传话请求，实际回复会回到原对话；这不代表对方本人已读。"})
}
func (p *portal) replyCollaboration(w http.ResponseWriter, r *http.Request, ident identity) {
	thread, err := p.loadCollaboration(r.Context(), r.PathValue("id"), ident.UserID)
	if err != nil || thread.OwnerID != ident.UserID {
		writeJSON(w, 404, map[string]string{"error": "待回复事项不存在"})
		return
	}
	var input struct {
		MessageID string `json:"messageId"`
		Text      string `json:"text"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&input) != nil || strings.TrimSpace(input.Text) == "" || len(input.Text) > 12000 {
		writeJSON(w, 400, map[string]string{"error": "请填写回复"})
		return
	}
	result, err := p.db.ExecContext(r.Context(), `UPDATE companion_collaboration_messages SET owner_reply=$1,state='replied',attempt_at=NULL,error='' WHERE id=$2 AND thread_id=$3 AND state='waiting_owner'`, input.Text, input.MessageID, thread.ID)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "回复未保存"})
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		var existing string
		err = p.db.QueryRowContext(r.Context(), `SELECT owner_reply FROM companion_collaboration_messages WHERE id=$1 AND thread_id=$2`, input.MessageID, thread.ID).Scan(&existing)
		if err != nil || existing != input.Text {
			writeJSON(w, 409, map[string]string{"error": "事项已更新，请刷新后查看"})
			return
		}
	}
	writeJSON(w, 200, map[string]any{"replied": true})
}
func (p *portal) retryCollaboration(w http.ResponseWriter, r *http.Request, ident identity) {
	thread, err := p.loadCollaboration(r.Context(), r.PathValue("id"), ident.UserID)
	if err != nil || thread.Requester != ident.UserID {
		writeJSON(w, 404, map[string]string{"error": "协作不存在"})
		return
	}
	if _, _, err = p.collaborationTarget(r.Context(), thread.Target.ID, ident, true); err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	_, err = p.db.ExecContext(r.Context(), `UPDATE companion_collaboration_messages SET state='pending',attempt_at=NULL,error='' WHERE thread_id=$1 AND state='failed'`, thread.ID)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "重试未提交"})
		return
	}
	writeJSON(w, 202, map[string]bool{"retrying": true})
}
func (p *portal) collaborationCall(ctx context.Context, ws *dev.PersonalWorkspace, companion, path string, input any) (map[string]any, error) {
	if ws.Status.Phase != dev.WorkspacePhaseRunning {
		return nil, errors.New("对方工作区暂未就绪")
	}
	token, err := p.collaborationWorkspaceToken(ctx, ws)
	if err != nil {
		return nil, errors.New("协作凭据未就绪")
	}
	var body io.Reader
	method := "GET"
	if input != nil {
		raw, _ := json.Marshal(input)
		body = bytes.NewReader(raw)
		method = "POST"
	}
	req, err := http.NewRequestWithContext(ctx, method, p.workspaceChatBaseURL(ws)+"/"+url.PathEscape(companion)+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(collaborationTokenHeader, token)
	req.Header.Set(workspaceTokenHeader, token)
	resp, err := (&http.Client{Timeout: 150 * time.Second}).Do(req)
	if err != nil {
		return nil, errors.New("搭档暂时未能回复")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("搭档服务暂不可用（%d）", resp.StatusCode)
	}
	var result map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result) != nil {
		return nil, errors.New("搭档返回内容无效")
	}
	return result, nil
}
func (p *portal) runCollaborations(ctx context.Context) {
	for i := 0; i < 4; i++ {
		go func() {
			for {
				if ctx.Err() != nil {
					return
				}
				worked := p.processCollaboration(ctx)
				if !worked {
					select {
					case <-ctx.Done():
						return
					case <-time.After(2 * time.Second):
					}
				}
			}
		}()
	}
}
func (p *portal) processCollaboration(ctx context.Context) bool {
	// A five-minute claim survives portal restarts and is longer than the runtime timeout.
	var id, tid, text, state, reply, question, ownerReply, returned string
	err := p.db.QueryRowContext(ctx, `UPDATE companion_collaboration_messages SET attempt_at=now() WHERE id=(
 SELECT m.id FROM companion_collaboration_messages m WHERE (m.attempt_at IS NULL OR m.attempt_at<now()-interval '5 minutes')
 AND (m.state='pending' OR (m.state IN ('replied','waiting_owner') AND m.returned<>CASE WHEN m.owner_reply<>'' THEN 'owner' ELSE 'companion' END))
 AND NOT EXISTS(SELECT 1 FROM companion_collaboration_messages earlier WHERE earlier.thread_id=m.thread_id AND earlier.created_at<m.created_at AND earlier.state='pending')
 ORDER BY m.created_at FOR UPDATE SKIP LOCKED LIMIT 1)
 RETURNING id,thread_id,text,state,reply,owner_question,owner_reply,returned`).Scan(&id, &tid, &text, &state, &reply, &question, &ownerReply, &returned)
	if err != nil {
		return false
	}
	var requester string
	if p.db.QueryRowContext(ctx, `SELECT requester FROM companion_collaborations WHERE id=$1`, tid).Scan(&requester) != nil {
		return true
	}
	thread, err := p.loadCollaboration(ctx, tid, requester)
	if err != nil {
		return true
	}
	if state == "pending" {
		ident := identity{UserID: requester}
		target, ws, targetErr := p.collaborationTarget(ctx, thread.Target.ID, ident, true)
		var result map[string]any
		if targetErr == nil && target.OwnerID != thread.OwnerID {
			targetErr = errors.New("搭档归属已变化，请重新发起联系")
		}
		if targetErr == nil {
			history, historyErr := p.collaborationMessages(ctx, tid)
			if historyErr != nil {
				return true
			}
			prior := []map[string]string{}
			for _, m := range history {
				if m.ID == id {
					break
				}
				prior = append(prior, map[string]string{"request": m.Text, "reply": m.Reply, "ownerReply": m.OwnerReply})
			}
			if len(prior) > 20 {
				prior = prior[len(prior)-20:]
			}
			result, targetErr = p.collaborationCall(ctx, ws, target.Name, "/internal/collaboration/respond", map[string]any{"requestId": id, "message": text, "from": requester, "history": prior})
		}
		if targetErr != nil {
			_, _ = p.db.ExecContext(ctx, `UPDATE companion_collaboration_messages SET state='failed',error=$1 WHERE id=$2`, targetErr.Error(), id)
			return true
		}
		reply, _ = result["reply"].(string)
		question, _ = result["ownerQuestion"].(string)
		state = "replied"
		if question != "" {
			state = "waiting_owner"
		}
		if reply == "" {
			_, _ = p.db.ExecContext(ctx, `UPDATE companion_collaboration_messages SET state='failed',error='搭档未返回有效回复' WHERE id=$1`, id)
			return true
		}
		if _, err = p.db.ExecContext(ctx, `UPDATE companion_collaboration_messages SET reply=$1,owner_question=$2,state=$3,error='' WHERE id=$4`, reply, question, state, id); err != nil {
			return true
		}
	}
	stage := "companion"
	answer := reply
	if ownerReply != "" {
		stage = "owner"
		answer = ownerReply
	}
	if thread.Source.Workspace != "" {
		var ws dev.PersonalWorkspace
		if p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: thread.Source.Workspace}, &ws) != nil || ws.Spec.Owner != requester {
			return true
		}
		label := thread.Target.DisplayName + "（" + thread.Target.OwnerName + "的搭档）"
		if stage == "owner" {
			label = thread.Target.OwnerName + "（经搭档转达）"
		}
		receipt := label + "回复：\n\n" + answer + "\n\n[查看这次协作](#/collaboration/" + tid + ")"
		_, err = p.collaborationCall(ctx, &ws, thread.Source.Companion, "/internal/collaboration/return", map[string]any{"requestId": id + ":" + stage, "conversationId": thread.Source.ConversationID, "text": receipt, "collaborationId": tid})
		if err != nil {
			_, _ = p.db.ExecContext(ctx, `UPDATE companion_collaboration_messages SET error='回复已保存，正在等待返回原对话' WHERE id=$1`, id)
			return true
		}
	}
	_, _ = p.db.ExecContext(ctx, `UPDATE companion_collaboration_messages SET returned=$1,error='',attempt_at=NULL WHERE id=$2`, stage, id)
	return true
}
