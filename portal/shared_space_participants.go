package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"net/http"
	"net/url"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
)

const sharedParticipantSchema = `
CREATE TABLE IF NOT EXISTS shared_space_participants (
 id TEXT PRIMARY KEY, space_id TEXT NOT NULL REFERENCES shared_spaces(id), kind TEXT NOT NULL, target TEXT NOT NULL,
 name TEXT NOT NULL, owner_id TEXT NOT NULL, resource_uid TEXT NOT NULL, role TEXT NOT NULL,
 state TEXT NOT NULL, version INTEGER NOT NULL, agent_id TEXT NOT NULL DEFAULT '', registration_state TEXT NOT NULL DEFAULT 'pending',
 UNIQUE(space_id,kind,target)
);
`

type sharedParticipant struct {
	ResourceValid bool   `json:"-"`
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Target        string `json:"target"`
	Name          string `json:"name"`
	OwnerID       string `json:"ownerId"`
	OwnerName     string `json:"ownerName,omitempty"`
	Role          string `json:"role"`
	State         string `json:"state"`
	Version       int    `json:"version"`
	CanApprove    bool   `json:"canApprove"`
	CanRevoke     bool   `json:"canRevoke"`
	CanAssign     bool   `json:"canAssign"`
	CanClaim      bool   `json:"canClaim"`
	UID           string `json:"-"`
	AgentID       string `json:"-"`
	Registration  string `json:"-"`
}

const participantSelect = `SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=$1`

func participantScan(row interface{ Scan(...any) error }) (x sharedParticipant, err error) {
	err = row.Scan(&x.ID, &x.Kind, &x.Target, &x.Name, &x.OwnerID, &x.UID, &x.Role, &x.State, &x.Version, &x.AgentID, &x.Registration)
	return
}
func (p *portal) participantResource(ctx context.Context, kind, target string, i identity, invite bool) (sharedParticipant, error) {
	x := sharedParticipant{Kind: kind, Target: target}
	if p.k8s == nil {
		return x, sharedFail(503, "Agent目录不可用")
	}
	switch kind {
	case "companion":
		parts := strings.Split(target, "/")
		if len(parts) != 2 {
			return x, sharedFail(400, "搭档标识不正确")
		}
		var ws dev.PersonalWorkspace
		if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: parts[0]}, &ws); err != nil {
			return x, sharedFail(404, "搭档不存在")
		}
		if invite && ws.Spec.Owner != i.UserID && contactSettings(&ws)[parts[1]].Mode != "organization" {
			return x, sharedFail(403, "搭档尚未开放联系")
		}
		for _, c := range ws.Spec.Companions {
			if c.Name == parts[1] {
				x.Name = c.DisplayName
				if x.Name == "" {
					x.Name = c.Name
				}
				x.OwnerID = ws.Spec.Owner
				x.UID = string(ws.UID)
			}
		}
		x.CanApprove = x.OwnerID == i.UserID
	case "digital_employee":
		var e dev.DigitalEmployee
		if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: target}, &e); err != nil {
			return x, sharedFail(404, "数字员工不存在")
		}
		if invite && !i.canAccessEmployee(p.cfg, e.Spec.Team, e.Spec.Owner, e.Spec.Visibility) {
			return x, sharedFail(403, "数字员工不可见")
		}
		x.Name = e.Spec.DisplayName
		if x.Name == "" {
			x.Name = e.Name
		}
		x.OwnerID = e.Spec.Owner
		x.UID = string(e.UID)
		x.CanApprove = x.OwnerID == i.UserID || i.isAdmin(p.cfg)
	default:
		return x, sharedFail(400, "参与者类型不正确")
	}
	if x.OwnerID == "" || x.UID == "" {
		return x, sharedFail(409, "Agent身份无法确认")
	}
	if err := p.sharedUserExists(ctx, x.OwnerID); err != nil {
		return x, err
	}
	if owner, ok, err := newReferenceResolver(p, ctx).lookupUser(x.OwnerID); err == nil && ok {
		x.OwnerName = owner.DisplayName
		if x.OwnerName == "" {
			x.OwnerName = owner.Username
		}
	}
	return x, nil
}
func (p *portal) participantDecorate(ctx context.Context, s sharedSpace, x sharedParticipant, i identity) sharedParticipant {
	resource, err := p.participantResource(ctx, x.Kind, x.Target, i, false)
	valid := err == nil && resource.UID == x.UID && resource.OwnerID == x.OwnerID
	x.OwnerName = resource.OwnerName
	x.ResourceValid = valid
	x.CanApprove = valid && resource.CanApprove && x.State == "pending"
	x.CanRevoke = s.Role == "owner" || valid && resource.CanApprove
	if x.State == "active" {
		valid = valid && x.Role == "editor" && x.AgentID != "" && x.Registration == "ready"
		x.CanAssign = valid && sharedCaps(s.Role)["submit"]
		x.CanClaim = valid && resource.CanApprove && sharedCaps(s.Role)["submit"]
	}
	return x
}
func (p *portal) participantList(ctx context.Context, s sharedSpace, i identity) ([]sharedParticipant, error) {
	rows, err := p.db.QueryContext(ctx, participantSelect+` ORDER BY name,id`, s.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []sharedParticipant{}
	for rows.Next() {
		x, e := participantScan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, p.participantDecorate(ctx, s, x, i))
	}
	return out, rows.Err()
}
func (p *portal) sharedParticipants(w http.ResponseWriter, r *http.Request, i identity) error {
	s, e := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if e != nil {
		return e
	}
	xs, e := p.participantList(r.Context(), s, i)
	if e == nil {
		writeJSON(w, 200, map[string]any{"participants": xs})
	}
	return e
}
func (p *portal) sharedAgentCandidates(w http.ResponseWriter, r *http.Request, i identity) error {
	s, e := p.sharedLoad(r.Context(), p.db, r.PathValue("space"), i)
	if e != nil {
		return e
	}
	if s.Role != "owner" {
		return sharedFail(403, "仅空间负责人可邀请")
	}
	out := []sharedParticipant{}
	var ws dev.PersonalWorkspaceList
	if e = p.k8s.List(r.Context(), &ws, client.InNamespace(p.cfg.namespace)); e != nil {
		return e
	}
	for _, v := range ws.Items {
		for _, c := range v.Spec.Companions {
			x, e := p.participantResource(r.Context(), "companion", v.Name+"/"+c.Name, i, true)
			if e == nil {
				out = append(out, x)
			}
		}
	}
	var es dev.DigitalEmployeeList
	if e = p.k8s.List(r.Context(), &es, client.InNamespace(p.cfg.namespace)); e != nil {
		return e
	}
	for _, v := range es.Items {
		x, e := p.participantResource(r.Context(), "digital_employee", v.Name, i, true)
		if e == nil {
			out = append(out, x)
		}
	}
	writeJSON(w, 200, map[string]any{"candidates": out})
	return nil
}

// Registration is an intent followed by one POST. An unknown outcome is only
// reconciled by its exact durable native marker, never by resending creation.
func (p *portal) participantRegister(ctx context.Context, c *sql.Conn, s sharedSpace, x *sharedParticipant) error {
	marker := "aep-shared-participant:" + s.DeploymentID + ":" + x.ID + ":" + fmt.Sprint(x.Version)
	verify := func(a map[string]any) bool {
		rc, _ := a["runtimeConfig"].(map[string]any)
		hb, _ := rc["heartbeat"].(map[string]any)
		ac, _ := a["adapterConfig"].(map[string]any)
		return a["companyId"] == s.CompanyID && a["name"] == marker && a["role"] == "general" && a["adapterType"] == "http" && ac["url"] == "http://127.0.0.1:9/unused" && rc["aepSharedParticipant"] == marker && hb["enabled"] == false && hb["wakeOnDemand"] == false
	}
	if x.Registration == "pending" {
		if _, e := c.ExecContext(ctx, `UPDATE shared_space_participants SET registration_state='unknown' WHERE id=$1`, x.ID); e != nil {
			return e
		}
		x.Registration = "unknown"
		var a map[string]any
		if e := p.sharedPC(ctx, "POST", "/companies/"+url.PathEscape(s.CompanyID)+"/agents", map[string]any{"name": marker, "role": "general", "adapterType": "http", "adapterConfig": map[string]any{"url": "http://127.0.0.1:9/unused"}, "runtimeConfig": map[string]any{"aepSharedParticipant": marker, "heartbeat": map[string]any{"enabled": false, "wakeOnDemand": false}}}, &a); e != nil {
			return sharedFail(423, "Agent注册结果待核验")
		}
	}
	var agents []map[string]any
	if e := p.sharedPC(ctx, "GET", "/companies/"+url.PathEscape(s.CompanyID)+"/agents", nil, &agents); e != nil {
		return sharedFail(423, "Agent注册结果待核验")
	}
	id := ""
	matches := 0
	for _, a := range agents {
		rc, _ := a["runtimeConfig"].(map[string]any)
		if a["name"] == marker || rc["aepSharedParticipant"] == marker {
			matches++
			if !verify(a) {
				return sharedFail(423, "Agent注册配置无法确认")
			}
			id, _ = a["id"].(string)
		}
	}
	if matches != 1 {
		return sharedFail(423, "Agent注册身份不唯一")
	}

	if id == "" {
		return sharedFail(423, "Agent注册结果待核验，不会重复创建")
	}
	x.AgentID = id
	x.Registration = "ready"
	x.State = "active"
	return nil
}
func (p *portal) sharedParticipantChange(w http.ResponseWriter, r *http.Request, i identity) error {
	var b struct {
		Kind            string `json:"kind"`
		Target          string `json:"target"`
		Role            string `json:"role"`
		RequestID       string `json:"requestId"`
		Approve         bool   `json:"approve"`
		ExpectedVersion int    `json:"expectedVersion"`
	}
	if e := sharedDecode(w, r, &b); e != nil {
		return e
	}
	if b.RequestID == "" {
		return sharedFail(400, "缺少请求编号")
	}
	c, release, e := p.sharedLock(r.Context(), r.PathValue("space"))
	if e != nil {
		return e
	}
	defer release()
	s, e := p.sharedLoad(r.Context(), c, r.PathValue("space"), i)
	if e != nil && (r.PathValue("participantAction") == "accept" || r.PathValue("participantAction") == "revoke") {
		s, e = p.sharedRuntimeSpace(r.Context(), r.PathValue("space"))
		s.Role = "viewer"
	}
	if e != nil {
		return e
	}
	action := r.PathValue("participantAction")
	hash := sharedHash([]any{r.PathValue("participant"), action, b})
	if raw, ok, err := sharedReplay(r.Context(), c, s.ID, b.RequestID, i.UserID, "participant", hash); err != nil {
		if failure, ok := err.(*sharedError); !ok || failure.code != 423 {
			return err
		}
		// Only inspect a previously persisted registration intent. Never POST again.
		id := r.PathValue("participant")
		if id == "" {
			id = collaborationID(s.ID, b.Kind, b.Target)
		}
		prior, e := participantScan(c.QueryRowContext(r.Context(), participantSelect+` AND id=$2`, s.ID, id))
		if e != nil {
			return err
		}
		resource, e := p.participantResource(r.Context(), prior.Kind, prior.Target, i, false)
		if e != nil || !resource.CanApprove || resource.UID != prior.UID || resource.OwnerID != prior.OwnerID {
			return sharedFail(403, "资源管理身份已变化")
		}
		if prior.State != "pending" || (prior.Registration != "unknown" && prior.Registration != "pending") {
			return err
		}
		if e = p.participantRegister(r.Context(), c, s, &prior); e != nil {
			return e
		}
		prior = p.participantDecorate(r.Context(), s, prior, i)
		receipt := map[string]any{"participant": prior, "state": "applied", "requestId": b.RequestID}
		tx, e := c.BeginTx(r.Context(), nil)
		if e != nil {
			return e
		}
		defer func() { _ = tx.Rollback() }()
		if e = participantPersist(r.Context(), tx, s.ID, prior); e != nil {
			return e
		}
		if e = sharedFinish(r.Context(), tx, s.ID, b.RequestID, receipt); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		writeJSON(w, 200, receipt)
		return nil
	} else if ok {
		writeJSON(w, 200, raw)
		return nil
	}

	var x sharedParticipant
	if action == "" {
		if s.Role != "owner" {
			return sharedFail(403, "仅空间负责人可邀请")
		}
		if b.Role != "editor" && b.Role != "viewer" {
			return sharedFail(400, "参与者角色不正确")
		}
		x, e = p.participantResource(r.Context(), b.Kind, b.Target, i, true)
		if e != nil {
			return e
		}
		old, err := participantScan(c.QueryRowContext(r.Context(), participantSelect+` AND kind=$2 AND target=$3`, s.ID, b.Kind, b.Target))
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && old.State != "revoked" {
			return sharedFail(409, "参与者已存在")
		}
		x.ID = collaborationID(s.ID, b.Kind, b.Target)
		x.Version = old.Version + 1
		x.Role = b.Role
		x.State = "pending"
		x.Registration = "pending"
		if b.Approve && !x.CanApprove {
			return sharedFail(403, "仅资源管理者可确认加入")
		}
	} else {
		x, e = participantScan(c.QueryRowContext(r.Context(), participantSelect+` AND id=$2`, s.ID, r.PathValue("participant")))
		if e != nil {
			return sharedFail(404, "参与者不存在")
		}
		x = p.participantDecorate(r.Context(), s, x, i)
		if x.Version != b.ExpectedVersion {
			return sharedFail(409, "参与者版本已变化")
		}
		switch action {
		case "accept":
			if !x.CanApprove {
				return sharedFail(403, "仅当前资源管理者可确认")
			}
		case "revoke":
			if !x.CanRevoke {
				return sharedFail(403, "无撤销权限")
			}
			x.State = "revoked"
			x.Version++
		default:
			return sharedFail(404, "not found")
		}
	}
	register := action == "accept" || action == "" && b.Approve
	tx, e := c.BeginTx(r.Context(), nil)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	if action == "revoke" {
		superseded := map[string]any{"participant": x, "state": "applied", "superseded": true}
		raw, _ := json.Marshal(superseded)
		if _, e = tx.ExecContext(r.Context(), `UPDATE shared_space_commands SET state='applied',receipt=$3 WHERE space_id=$1 AND task_id=$2 AND action='participant' AND state<>'applied'`, s.ID, "participant:"+x.ID, string(raw)); e != nil {
			return e
		}
	}
	if e = sharedStart(r.Context(), tx, s.ID, "participant:"+x.ID, b.RequestID, i.UserID, "participant", hash, b); e != nil {
		return e
	}
	if e = participantPersist(r.Context(), tx, s.ID, x); e != nil {
		return e
	}
	if register {
		if e = tx.Commit(); e != nil {
			return e
		}
		if e = p.participantRegister(r.Context(), c, s, &x); e != nil {
			_ = sharedUnknown(c, s.ID, b.RequestID)
			return e
		}
		tx, e = c.BeginTx(r.Context(), nil)
		if e != nil {
			return e
		}
		defer func() { _ = tx.Rollback() }()
		if e = participantPersist(r.Context(), tx, s.ID, x); e != nil {
			return e
		}
	}

	x = p.participantDecorate(r.Context(), s, x, i)
	receipt := map[string]any{"participant": x, "state": "applied", "requestId": b.RequestID}
	if e = sharedFinish(r.Context(), tx, s.ID, b.RequestID, receipt); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	writeJSON(w, 200, receipt)
	return nil
}

// Agent grants are independent from the manager's human membership. This
// metadata loader is only used after a trusted runtime identity is available.
func (p *portal) sharedRuntimeSpace(ctx context.Context, id string) (s sharedSpace, e error) {
	e = p.db.QueryRowContext(ctx, `SELECT id,deployment_id,company_id,project_id,name FROM shared_spaces WHERE id=$1 AND deployment_id=$2`, id, p.cfg.aepDeploymentID).Scan(&s.ID, &s.DeploymentID, &s.CompanyID, &s.ProjectID, &s.Name)
	s.Role = "editor"
	return
}
func (p *portal) sharedInvitations(w http.ResponseWriter, r *http.Request, i identity) error {
	rows, e := p.db.QueryContext(r.Context(), `SELECT id,space_id FROM shared_space_participants WHERE state IN ('pending','active') ORDER BY id`)
	if e != nil {
		return e
	}
	type ref struct{ id, space string }
	refs := []ref{}
	for rows.Next() {
		var x ref
		if e = rows.Scan(&x.id, &x.space); e != nil {
			_ = rows.Close()
			return e
		}
		refs = append(refs, x)
	}
	e = rows.Err()
	_ = rows.Close()
	if e != nil {
		return e
	}
	out := []map[string]any{}
	for _, ref := range refs {
		s, e := p.sharedRuntimeSpace(r.Context(), ref.space)
		if e != nil {
			continue
		}
		x, e := participantScan(p.db.QueryRowContext(r.Context(), participantSelect+` AND id=$2`, s.ID, ref.id))
		if e != nil {
			return e
		}
		x = p.participantDecorate(r.Context(), s, x, i)
		if x.CanApprove || x.CanRevoke {
			out = append(out, map[string]any{"participant": x, "space": map[string]any{"id": s.ID, "name": s.Name}, "spaceId": s.ID, "spaceName": s.Name})
		}
	}
	writeJSON(w, 200, map[string]any{"invitations": out})
	return nil
}
func (p *portal) sharedAcceptInvitation(w http.ResponseWriter, r *http.Request, i identity) error {
	var space string
	if e := p.db.QueryRowContext(r.Context(), `SELECT space_id FROM shared_space_participants WHERE id=$1`, r.PathValue("participant")).Scan(&space); e != nil {
		return sharedFail(404, "邀请不存在")
	}
	r.SetPathValue("space", space)
	action := r.PathValue("invitationAction")
	if action != "accept" && action != "revoke" {
		return sharedFail(404, "not found")
	}
	r.SetPathValue("participantAction", action)
	return p.sharedParticipantChange(w, r, i)
}

func participantPersist(ctx context.Context, tx *sql.Tx, space string, x sharedParticipant) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO shared_space_participants(id,space_id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(id) DO UPDATE SET name=excluded.name,owner_id=excluded.owner_id,resource_uid=excluded.resource_uid,role=excluded.role,state=excluded.state,version=excluded.version,agent_id=excluded.agent_id,registration_state=excluded.registration_state`, x.ID, space, x.Kind, x.Target, x.Name, x.OwnerID, x.UID, x.Role, x.State, x.Version, x.AgentID, x.Registration)
	return e
}
