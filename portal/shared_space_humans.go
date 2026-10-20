package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const sharedHumanSchema = `
CREATE TABLE IF NOT EXISTS shared_space_human_mappings (
 deployment_id TEXT NOT NULL, company_id TEXT NOT NULL, user_id TEXT NOT NULL,
 native_user_id TEXT NOT NULL, created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(deployment_id,company_id,user_id), UNIQUE(deployment_id,company_id,native_user_id)
);
`

type sharedHuman struct {
	UserID       string `json:"userId"`
	DisplayName  string `json:"displayName"`
	NativeUserID string `json:"-"`
}
type sharedNativeHuman struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email,omitempty"`
}
type sharedHumanMappingState struct {
	State           string `json:"state"`
	PaperclipUserID string `json:"paperclipUserId,omitempty"`
}
type sharedHumanCandidate struct {
	UserID            string                  `json:"userId"`
	DisplayName       string                  `json:"displayName"`
	Role              string                  `json:"role"`
	Mapping           sharedHumanMappingState `json:"mapping"`
	CanAssign         bool                    `json:"canAssign"`
	CanClaim          bool                    `json:"canClaim"`
	UnavailableReason string                  `json:"unavailableReason,omitempty"`
}

func (p *portal) sharedNativeHumans(ctx context.Context, s sharedSpace) ([]sharedNativeHuman, error) {
	var directory struct {
		Users []struct {
			PrincipalID string `json:"principalId"`
			Status      string `json:"status"`
			User        *struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"user"`
		} `json:"users"`
	}
	if err := p.sharedPC(ctx, "GET", "/companies/"+url.PathEscape(s.CompanyID)+"/user-directory", nil, &directory); err != nil {
		return nil, err
	}
	users := []sharedNativeHuman{}
	for _, entry := range directory.Users {
		if entry.Status != "active" || entry.User == nil || entry.User.ID == "" || entry.PrincipalID != entry.User.ID {
			continue
		}
		name := entry.User.Name
		if name == "" {
			name = entry.User.Email
		}
		if name == "" {
			name = "任务系统人员"
		}
		users = append(users, sharedNativeHuman{ID: entry.User.ID, DisplayName: name, Email: entry.User.Email})
	}
	return users, nil
}
func (p *portal) sharedHumanMember(ctx context.Context, s sharedSpace, userID string) (string, string, error) {
	var role string
	err := p.db.QueryRowContext(ctx, `SELECT role FROM shared_space_members WHERE space_id=$1 AND user_id=$2 AND role<>'revoked'`, s.ID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", sharedFail(403, "人员不是当前有效空间成员")
	}
	if err != nil {
		return "", "", err
	}
	user, found, err := newReferenceResolver(p, ctx).lookupUser(userID)
	if err != nil {
		return role, "", sharedFail(503, "企业成员目录暂不可用")
	}
	if !found || user.ID != userID || user.Status != "active" || user.Kind != "human" {
		return role, "", sharedFail(403, "人员不是当前企业的有效人类成员")
	}
	name := user.DisplayName
	if name == "" {
		name = user.Username
	}
	if name == "" {
		name = "空间成员"
	}
	return role, name, nil
}
func (p *portal) sharedHumanNativeID(ctx context.Context, s sharedSpace, userID string) (string, error) {
	var nativeID string
	err := p.db.QueryRowContext(ctx, `SELECT native_user_id FROM shared_space_human_mappings WHERE deployment_id=$1 AND company_id=$2 AND user_id=$3`, s.DeploymentID, s.CompanyID, userID).Scan(&nativeID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return nativeID, err
}
func (p *portal) sharedHumanForUser(ctx context.Context, s sharedSpace, userID string) (*sharedHuman, error) {
	role, name, err := p.sharedHumanMember(ctx, s, userID)
	if err != nil {
		return nil, err
	}
	if role != "owner" && role != "editor" {
		return nil, sharedFail(403, "只读成员不能承接任务")
	}
	nativeID, err := p.sharedHumanNativeID(ctx, s, userID)
	if err != nil {
		return nil, err
	}
	if nativeID == "" {
		return nil, sharedFail(409, "请空间负责人先关联人员身份")
	}
	users, err := p.sharedNativeHumans(ctx, s)
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		if user.ID == nativeID {
			return &sharedHuman{UserID: userID, DisplayName: name, NativeUserID: nativeID}, nil
		}
	}
	return nil, sharedFail(409, "关联的任务系统人员已不可用")
}
func (p *portal) sharedHumanForNative(ctx context.Context, s sharedSpace, nativeID string) (*sharedHuman, error) {
	if nativeID == "" {
		return nil, nil
	}
	var userID string
	err := p.db.QueryRowContext(ctx, `SELECT user_id FROM shared_space_human_mappings WHERE deployment_id=$1 AND company_id=$2 AND native_user_id=$3`, s.DeploymentID, s.CompanyID, nativeID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	human, err := p.sharedHumanForUser(ctx, s, userID)
	if e, ok := err.(*sharedError); ok && (e.code == 403 || e.code == 409) {
		return nil, nil
	}
	return human, err
}
func (p *portal) sharedHumanSpace(ctx context.Context, space string, i identity) (sharedSpace, error) {
	if !i.isAdmin(p.cfg) {
		return p.sharedLoad(ctx, p.db, space, i)
	}
	var s sharedSpace
	err := p.db.QueryRowContext(ctx, `SELECT id,deployment_id,company_id,project_id,name FROM shared_spaces WHERE id=$1 AND deployment_id=$2`, space, i.DeploymentID).Scan(&s.ID, &s.DeploymentID, &s.CompanyID, &s.ProjectID, &s.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return s, sharedFail(404, "空间不存在或无权访问")
	}
	return s, err
}
func (p *portal) sharedHumanCandidateFor(ctx context.Context, s sharedSpace, actor identity, userID string, users []sharedNativeHuman, nativeErr error) sharedHumanCandidate {
	out := sharedHumanCandidate{UserID: userID, DisplayName: "空间成员", Mapping: sharedHumanMappingState{State: "unavailable"}}
	role, name, err := p.sharedHumanMember(ctx, s, userID)
	out.Role = role
	if name != "" {
		out.DisplayName = name
	}
	if err != nil {
		out.UnavailableReason = "人员目录或成员资格当前不可用"
		return out
	}
	id, err := p.sharedHumanNativeID(ctx, s, userID)
	if err != nil {
		out.UnavailableReason = "人员关联暂不可用"
		return out
	}
	if id == "" {
		out.Mapping.State = "unlinked"
		out.UnavailableReason = "请空间负责人关联人员身份"
		return out
	}
	if nativeErr != nil {
		out.UnavailableReason = "任务系统人员目录暂不可用"
		return out
	}
	for _, user := range users {
		if user.ID == id {
			out.Mapping = sharedHumanMappingState{State: "linked", PaperclipUserID: id}
			out.CanAssign = (role == "owner" || role == "editor") && (s.Role == "owner" || s.Role == "editor" || actor.isAdmin(p.cfg))
			out.CanClaim = out.CanAssign && actor.UserID == userID
			if role == "viewer" {
				out.UnavailableReason = "只读成员不能承接任务"
			}
			return out
		}
	}
	out.UnavailableReason = "关联的任务系统人员已不可用"
	return out
}
func (p *portal) sharedHumanAssignees(w http.ResponseWriter, r *http.Request, i identity) error {
	s, err := p.sharedHumanSpace(r.Context(), r.PathValue("space"), i)
	if err != nil {
		return err
	}
	rows, err := p.db.QueryContext(r.Context(), `SELECT user_id FROM shared_space_members WHERE space_id=$1 AND role<>'revoked' ORDER BY user_id`, s.ID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	users, nativeErr := p.sharedNativeHumans(r.Context(), s)
	members := []sharedHumanCandidate{}
	for _, id := range ids {
		members = append(members, p.sharedHumanCandidateFor(r.Context(), s, i, id, users, nativeErr))
	}
	manage := s.Role == "owner" || i.isAdmin(p.cfg)
	if !manage {
		users = []sharedNativeHuman{}
	}
	if users == nil {
		users = []sharedNativeHuman{}
	}
	writeJSON(w, 200, map[string]any{"members": members, "nativeUsers": users, "capabilities": map[string]bool{"manageHumanMappings": manage}})
	return nil
}
func (p *portal) sharedHumanMapping(w http.ResponseWriter, r *http.Request, i identity) error {
	s, err := p.sharedHumanSpace(r.Context(), r.PathValue("space"), i)
	if err != nil {
		return err
	}
	if s.Role != "owner" && !i.isAdmin(p.cfg) {
		return sharedFail(403, "仅空间负责人或管理员可关联人员身份")
	}
	var input struct {
		RequestID string `json:"requestId"`
		NativeID  string `json:"paperclipUserId"`
	}
	if err = sharedDecode(w, r, &input); err != nil {
		return err
	}
	if strings.TrimSpace(input.RequestID) == "" || strings.TrimSpace(input.NativeID) == "" {
		return sharedFail(400, "请选择人员并提供请求编号")
	}
	userID := r.PathValue("user")
	if _, _, err = p.sharedHumanMember(r.Context(), s, userID); err != nil {
		return err
	}
	users, err := p.sharedNativeHumans(r.Context(), s)
	if err != nil {
		return err
	}
	found := false
	for _, user := range users {
		if user.ID == input.NativeID {
			found = true
		}
	}
	if !found {
		return sharedFail(400, "请选择当前企业有效的任务系统人员")
	}
	_, err = p.db.ExecContext(r.Context(), `INSERT INTO shared_space_human_mappings(deployment_id,company_id,user_id,native_user_id,created_by) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, s.DeploymentID, s.CompanyID, userID, input.NativeID, i.UserID)
	if err != nil {
		return err
	}
	current, err := p.sharedHumanNativeID(r.Context(), s, userID)
	if err != nil {
		return err
	}
	if current != input.NativeID {
		return sharedFail(409, "人员身份已关联，或该任务系统人员已关联其他成员；请保留既有关联")
	}
	member := p.sharedHumanCandidateFor(r.Context(), s, i, userID, users, nil)
	writeJSON(w, 200, map[string]any{"member": member})
	return nil
}
