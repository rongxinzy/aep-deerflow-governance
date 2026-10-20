package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

type sharedLabel struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}
type sharedCompanyLabel struct {
	sharedLabel
	CompanyID string `json:"companyId"`
}
type sharedLabelCreation struct {
	RequestID string `json:"requestId"`
	Name      string `json:"name"`
	Color     string `json:"color"`
}
type sharedTaskLabelsInput struct {
	RequestID string   `json:"requestId"`
	Token     string   `json:"expectedLabelsToken"`
	IDs       []string `json:"labelIds"`
}

var sharedLabelColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func sharedValidRequestID(id string) bool { return strings.TrimSpace(id) != "" && len(id) <= 200 }
func (in *sharedLabelCreation) validate() error {
	in.Name = strings.TrimSpace(in.Name)
	in.Color = strings.ToLower(in.Color)
	if !sharedValidRequestID(in.RequestID) || in.Name == "" || utf8.RuneCountInString(in.Name) > 48 || !sharedLabelColor.MatchString(in.Color) {
		return sharedFail(400, "请填写有效标签名称、颜色与请求编号")
	}
	return nil
}
func sharedSortedLabelIDs(ids []string) []string {
	unique := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		if !unique[id] {
			out = append(out, id)
			unique[id] = true
		}
	}
	sort.Strings(out)
	return out
}
func sharedTaskLabelValues(task map[string]any) ([]sharedLabel, []string) {
	labels := []sharedLabel{}
	raw, _ := json.Marshal(task["labels"])
	_ = json.Unmarshal(raw, &labels)
	if labels == nil {
		labels = []sharedLabel{}
	}
	ids := []string{}
	raw, _ = json.Marshal(task["labelIds"])
	_ = json.Unmarshal(raw, &ids)
	if task["labelIds"] == nil {
		for _, label := range labels {
			ids = append(ids, label.ID)
		}
	}
	return labels, sharedSortedLabelIDs(ids)
}
func sharedLabelsToken(task map[string]any) string {
	_, ids := sharedTaskLabelValues(task)
	return sharedHash([]any{task["id"], ids})
}
func sharedLabelsEditReason(role string, task map[string]any) string {
	if !sharedCaps(role)["createTask"] {
		return "当前成员只能查看标签"
	}
	// Native policy transitions run on every PATCH and can repair status or
	// assignee even for label-only input. Ordinary Pi execution has neither.
	for _, key := range []string{"executionPolicy", "executionState"} {
		if value := task[key]; value != nil {
			raw, _ := json.Marshal(value)
			if string(raw) != "{}" && string(raw) != "null" {
				return "原生执行策略任务暂不支持修改标签"
			}
		}
	}
	return ""
}
func (p *portal) sharedReadLabels(ctx context.Context, s sharedSpace) ([]sharedLabel, error) {
	native := []sharedCompanyLabel{}
	if err := p.sharedPC(ctx, "GET", "/companies/"+url.PathEscape(s.CompanyID)+"/labels", nil, &native); err != nil {
		return nil, err
	}
	out := []sharedLabel{}
	for _, label := range native {
		if label.CompanyID != s.CompanyID || label.ID == "" {
			return nil, sharedFail(502, "标签目录企业范围无法确认")
		}
		out = append(out, label.sharedLabel)
	}
	return out, nil
}
func (p *portal) sharedLabels(w http.ResponseWriter, r *http.Request, i identity) error {
	s, err := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	labels, err := p.sharedReadLabels(r.Context(), s)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"labels": labels, "scope": "company", "capabilities": map[string]bool{"createLabel": sharedCaps(s.Role)["createTask"]}})
	return nil
}
func sharedLabelNamed(labels []sharedLabel, name, color string) (*sharedLabel, error) {
	for _, label := range labels {
		if label.Name == name {
			if !strings.EqualFold(label.Color, color) {
				return nil, sharedFail(409, "同名标签颜色不同，请选择已有企业标签")
			}
			return &label, nil
		}
	}
	return nil, nil
}

// A persisted command is reconciled using a read only. Its intent never
// licenses another native mutation after an uncertain acknowledgement.
func sharedLabelReceipt(ctx context.Context, c *sql.Conn, space, request, actor, action, hash string, read func() (map[string]any, error)) (json.RawMessage, bool, error) {
	var oldActor, oldAction, oldHash, state string
	var raw []byte
	err := c.QueryRowContext(ctx, `SELECT actor_id,action,payload_hash,state,receipt FROM shared_space_commands WHERE space_id=$1 AND request_id=$2`, space, request).Scan(&oldActor, &oldAction, &oldHash, &state, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if oldActor != actor || oldAction != action || oldHash != hash {
		return nil, false, sharedFail(409, "请求编号已用于其他操作")
	}
	if state == "applied" {
		return raw, true, nil
	}
	receipt, err := read()
	if err != nil {
		return nil, false, err
	}
	raw, err = sharedLabelFinish(ctx, c, space, request, receipt)
	return raw, true, err
}
func sharedLabelFinish(ctx context.Context, c *sql.Conn, space, request string, receipt map[string]any) (json.RawMessage, error) {
	receipt["requestId"] = request
	receipt["state"] = "applied"
	if err := sharedDependencyFinish(ctx, c, space, request, receipt); err != nil {
		return nil, err
	}
	return json.Marshal(receipt)
}
func (p *portal) sharedCreateLabel(w http.ResponseWriter, r *http.Request, i identity) error {
	var in sharedLabelCreation
	if err := sharedDecode(w, r, &in); err != nil {
		return err
	}
	if err := in.validate(); err != nil {
		return err
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
		return sharedFail(403, "当前成员只能查看标签")
	}
	read := func() (map[string]any, error) {
		labels, err := p.sharedReadLabels(r.Context(), s)
		if err != nil {
			return nil, err
		}
		label, err := sharedLabelNamed(labels, in.Name, in.Color)
		if err != nil {
			return nil, err
		}
		if label == nil {
			return nil, sharedFail(423, "标签创建结果待核验，请勿重复提交")
		}
		return map[string]any{"label": label, "reused": false}, nil
	}
	hash := sharedHash(in)
	if raw, ok, e := sharedLabelReceipt(r.Context(), c, s.ID, in.RequestID, i.UserID, "create_label", hash, read); e != nil {
		return e
	} else if ok {
		writeJSON(w, 200, raw)
		return nil
	}
	labels, err := p.sharedReadLabels(r.Context(), s)
	if err != nil {
		return err
	}
	label, err := sharedLabelNamed(labels, in.Name, in.Color)
	if err != nil {
		return err
	}
	body := map[string]any{"name": in.Name, "color": in.Color}
	key := "label:" + sharedHash(in.Name)
	if err = sharedStart(r.Context(), c, s.ID, key, in.RequestID, i.UserID, "create_label", hash, body); err != nil {
		return err
	}
	reused := label != nil
	unknown := func() error {
		_ = sharedUnknown(c, s.ID, in.RequestID)
		return sharedFail(423, "标签创建结果待核验，请勿重复提交")
	}
	if label == nil {
		var created sharedCompanyLabel
		err = p.sharedPC(r.Context(), "POST", "/companies/"+url.PathEscape(s.CompanyID)+"/labels", body, &created)
		if err != nil || created.CompanyID != s.CompanyID || created.ID == "" || created.Name != in.Name || !strings.EqualFold(created.Color, in.Color) {
			return unknown()
		}
		label = &created.sharedLabel
	}
	raw, err := sharedLabelFinish(r.Context(), c, s.ID, in.RequestID, map[string]any{"label": label, "reused": reused})
	if err != nil {
		return unknown()
	}
	writeJSON(w, 200, raw)
	return nil
}
func (p *portal) sharedSetTaskLabels(w http.ResponseWriter, r *http.Request, i identity) error {
	var in sharedTaskLabelsInput
	if err := sharedDecode(w, r, &in); err != nil {
		return err
	}
	if !sharedValidRequestID(in.RequestID) || in.Token == "" || in.IDs == nil {
		return sharedFail(400, "请提供标签版本与完整标签列表")
	}
	for _, id := range in.IDs {
		if id == "" {
			return sharedFail(400, "标签标识不正确")
		}
	}
	in.IDs = sharedSortedLabelIDs(in.IDs)
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
		return sharedFail(403, "当前成员只能查看标签")
	}
	taskID := r.PathValue("task")
	hash := sharedHash([]any{taskID, in})
	read := func() (map[string]any, error) {
		task, err := p.sharedCheckTask(r.Context(), s, taskID)
		if err != nil {
			return nil, err
		}
		_, ids := sharedTaskLabelValues(task)
		if sharedHash(ids) != sharedHash(in.IDs) {
			return nil, sharedFail(423, "标签修改结果待核验，请勿重复提交")
		}
		return map[string]any{"task": sharedTaskDTO(task), "labelsToken": sharedLabelsToken(task)}, nil
	}
	if raw, ok, e := sharedLabelReceipt(r.Context(), c, s.ID, in.RequestID, i.UserID, "task_labels", hash, read); e != nil {
		return e
	} else if ok {
		writeJSON(w, 200, raw)
		return nil
	}
	if err = runtimeCommandGate(r.Context(), c, s.ID, taskID); err != nil {
		return err
	}
	if err = p.sharedHandoffGate(r.Context(), c, s.ID, taskID); err != nil {
		return err
	}
	task, err := p.sharedCheckTask(r.Context(), s, taskID)
	if err != nil {
		return err
	}
	if reason := sharedLabelsEditReason(s.Role, task); reason != "" {
		return sharedFail(409, reason)
	}
	if sharedLabelsToken(task) != in.Token {
		return sharedFail(409, "任务标签版本已变化，请刷新")
	}
	labels, err := p.sharedReadLabels(r.Context(), s)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, label := range labels {
		allowed[label.ID] = true
	}
	for _, id := range in.IDs {
		if !allowed[id] {
			return sharedFail(400, "请选择本企业目录中的标签")
		}
	}
	body := map[string]any{"labelIds": in.IDs}
	if err = sharedStart(r.Context(), c, s.ID, taskID, in.RequestID, i.UserID, "task_labels", hash, body); err != nil {
		return err
	}
	unknown := func() error {
		_ = sharedUnknown(c, s.ID, in.RequestID)
		return sharedFail(423, "标签修改结果待核验，请勿重复提交")
	}
	if err = p.sharedPC(r.Context(), "PATCH", "/issues/"+url.PathEscape(taskID), body, nil); err != nil {
		return unknown()
	}
	receipt, err := read()
	if err != nil {
		return unknown()
	}
	raw, err := sharedLabelFinish(r.Context(), c, s.ID, in.RequestID, receipt)
	if err != nil {
		return unknown()
	}
	writeJSON(w, 200, raw)
	return nil
}
