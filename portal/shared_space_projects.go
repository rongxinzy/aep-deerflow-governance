package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Only project presentation fields cross the Portal boundary. Execution and
// repository configuration remain private to Paperclip.
func sharedProjectDTO(project map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"id", "companyId", "name", "description", "status", "targetDate", "color", "icon", "createdAt", "updatedAt"} {
		if value, ok := project[key]; ok {
			out[key] = value
		}
	}
	return out
}

func sharedProjectFields(body map[string]any, create bool) (map[string]any, error) {
	out := map[string]any{}
	for key, value := range body {
		switch key {
		case "name", "description", "status", "targetDate", "color", "icon":
			if value != nil {
				if _, ok := value.(string); !ok {
					return nil, sharedFail(400, "项目字段格式不正确")
				}
			}
			if key == "name" || key == "status" {
				if value == nil || strings.TrimSpace(value.(string)) == "" {
					return nil, sharedFail(400, "请填写项目名称和状态")
				}
			}
			if key == "status" {
				switch value {
				case "backlog", "planned", "in_progress", "completed", "cancelled":
				default:
					return nil, sharedFail(400, "项目状态不正确")
				}
			}
			out[key] = value
		default:
			return nil, sharedFail(400, "项目字段不支持修改")
		}
	}
	if create && out["name"] == nil {
		return nil, sharedFail(400, "请填写空间名称")
	}
	if len(out) == 0 {
		return nil, sharedFail(400, "请填写项目内容")
	}
	return out, nil
}

func (p *portal) sharedProject(w http.ResponseWriter, r *http.Request, i identity) error {
	s, err := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if err != nil {
		return err
	}
	var project map[string]any
	if err = p.sharedPC(r.Context(), "GET", "/projects/"+url.PathEscape(s.ProjectID), nil, &project); err != nil {
		return err
	}
	if project["id"] != s.ProjectID || project["companyId"] != s.CompanyID {
		return sharedFail(502, "项目服务返回了错误的空间范围")
	}
	writeJSON(w, 200, map[string]any{"project": sharedProjectDTO(project)})
	return nil
}

func (p *portal) sharedUpdateProject(w http.ResponseWriter, r *http.Request, i identity) error {
	var input map[string]any
	if err := sharedDecode(w, r, &input); err != nil {
		return err
	}
	body, err := sharedProjectFields(input, false)
	if err != nil {
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
	if s.Role != "owner" {
		return sharedFail(403, "仅空间负责人可编辑概览")
	}
	var project map[string]any
	if err = p.sharedPC(r.Context(), "PATCH", "/projects/"+url.PathEscape(s.ProjectID), body, &project); err != nil {
		return err
	}
	if project["id"] != s.ProjectID || project["companyId"] != s.CompanyID {
		return sharedFail(502, "项目服务返回了错误的空间范围")
	}
	if name, ok := project["name"].(string); ok {
		if _, err = c.ExecContext(r.Context(), `UPDATE shared_spaces SET name=$2 WHERE id=$1`, s.ID, name); err != nil {
			return err
		}
	}
	writeJSON(w, 200, map[string]any{"project": sharedProjectDTO(project)})
	return nil
}

func (p *portal) sharedCreateSpace(w http.ResponseWriter, r *http.Request, i identity) error {
	if p.cfg.paperclipCompanyID == "" {
		return sharedFail(503, "当前企业尚未配置协作空间创建")
	}
	var input map[string]any
	if err := sharedDecode(w, r, &input); err != nil {
		return err
	}
	request, _ := input["requestId"].(string)
	if strings.TrimSpace(request) == "" || len(request) > 200 {
		return sharedFail(400, "请提供有效的请求编号")
	}
	delete(input, "requestId")
	body, err := sharedProjectFields(input, true)
	if err != nil {
		return err
	}
	if err = p.sharedUserExists(r.Context(), i.UserID); err != nil {
		return err
	}
	hash := sharedHash([]any{"create_space", p.cfg.paperclipCompanyID, body})
	c, release, err := p.sharedLock(r.Context(), "bootstrap:"+i.DeploymentID)
	if err != nil {
		return err
	}
	defer release()
	var actor, priorHash string
	var raw []byte
	err = c.QueryRowContext(r.Context(), `SELECT actor_id,payload_hash,receipt FROM shared_space_bootstrap_commands WHERE deployment_id=$1 AND request_id=$2`, i.DeploymentID, request).Scan(&actor, &priorHash, &raw)
	if err == nil {
		if actor != i.UserID || priorHash != hash {
			return sharedFail(409, "请求编号已用于其他操作")
		}
		writeJSON(w, 200, json.RawMessage(raw))
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Paperclip's project idempotency receipt survives a Portal restart or a
	// failed local commit, so retrying this same native key cannot add a project.
	body["idempotencyKey"] = collaborationID("shared-space-create", i.DeploymentID, i.UserID, request)
	var project map[string]any
	if err = p.sharedPC(r.Context(), "POST", "/companies/"+url.PathEscape(p.cfg.paperclipCompanyID)+"/projects", body, &project); err != nil {
		return err
	}
	projectID, _ := project["id"].(string)
	name, _ := project["name"].(string)
	if projectID == "" || project["companyId"] != p.cfg.paperclipCompanyID || name == "" {
		return sharedFail(502, "项目服务未确认空间创建")
	}
	s := sharedSpace{ID: collaborationID("shared-space", i.DeploymentID, projectID), CompanyID: p.cfg.paperclipCompanyID, ProjectID: projectID, Name: name, Role: "owner"}
	receipt := map[string]any{"space": s, "project": sharedProjectDTO(project)}
	raw, err = json.Marshal(receipt)
	if err != nil {
		return err
	}
	tx, err := c.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO shared_spaces(id,deployment_id,company_id,project_id,name) VALUES($1,$2,$3,$4,$5)`, s.ID, i.DeploymentID, s.CompanyID, s.ProjectID, s.Name); err != nil {
		return err
	}
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO shared_space_members(space_id,user_id,role,version) VALUES($1,$2,'owner',1)`, s.ID, i.UserID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO shared_space_bootstrap_commands(deployment_id,request_id,actor_id,payload_hash,receipt) VALUES($1,$2,$3,$4,$5)`, i.DeploymentID, request, i.UserID, hash, string(raw)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	writeJSON(w, 201, receipt)
	return nil
}
