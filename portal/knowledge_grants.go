package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// knowledgeGrantDTO is the fixed admin-management wire shape. It is never
// accepted by a retrieval path as proof of the caller's own identity.
type knowledgeGrantDTO struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	GrantedBy string    `json:"granted_by,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

type replaceKnowledgeGrantsRequest struct {
	Grants []knowledgeGrantInput `json:"grants"`
}

// Read-side provenance fields are server-owned and are not valid write inputs.
type knowledgeGrantInput struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// handleKnowledgeBaseGrants is intended to be registered only behind both
// session auth and adminOnly. The explicit check remains defense in depth if
// a future route accidentally omits the wrapper. PUT replaces the grant set;
// an empty set therefore revokes all user/team access.
func (p *portal) handleKnowledgeBaseGrants(w http.ResponseWriter, r *http.Request, ident identity) {
	if !ident.isAdmin(p.cfg) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "administrator role required"})
		return
	}
	if !p.managedReadyIdentity(ident) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "session deployment is not configured for knowledge management"})
		return
	}
	if p.db == nil || p.cfg.managedKnowledge.tenantID == "" || ident.DeploymentID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge grant storage unavailable"})
		return
	}
	baseID, ok := safeKnowledgeID(r.PathValue("baseID"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid knowledge base id"})
		return
	}
	tenant := p.cfg.managedKnowledge.tenantID
	var registered string
	err := p.db.QueryRowContext(r.Context(), `SELECT knowledge_base_id FROM knowledge_base_registry WHERE deployment_id=$1 AND weknora_tenant_id=$2 AND knowledge_base_id=$3 AND lifecycle_state='active'`, ident.DeploymentID, tenant, baseID).Scan(&registered)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "knowledge base is not registered for this deployment"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge grant registry unavailable"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := p.db.QueryContext(r.Context(), `SELECT subject_type,subject_id,granted_by,created_at FROM knowledge_base_grants WHERE deployment_id=$1 AND weknora_tenant_id=$2 AND knowledge_base_id=$3 ORDER BY subject_type,subject_id`, ident.DeploymentID, tenant, baseID)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge grants unavailable"})
			return
		}
		defer func() { _ = rows.Close() }()
		grants := make([]knowledgeGrantDTO, 0)
		for rows.Next() {
			var grant knowledgeGrantDTO
			if err := rows.Scan(&grant.Type, &grant.ID, &grant.GrantedBy, &grant.CreatedAt); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge grants unavailable"})
				return
			}
			grants = append(grants, grant)
		}
		if err := rows.Err(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge grants unavailable"})
			return
		}
		if err := p.auditManagedKnowledgeRead(r.Context(), ident, tenant, "list_grants", "knowledge_base", baseID, ""); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge grant audit unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"knowledge_base_id": baseID, "tenant_id": tenant, "grants": grants})
	case http.MethodPut:
		var request replaceKnowledgeGrantsRequest
		if err := decodeStrictJSON(w, r, &request, 64<<10); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid grants request"})
			return
		}
		if request.Grants == nil || len(request.Grants) > 500 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "grants must be an explicit list of at most 500 entries"})
			return
		}
		seen := make(map[string]struct{}, len(request.Grants))
		for index := range request.Grants {
			grant := &request.Grants[index]
			grant.ID = strings.TrimSpace(grant.ID)
			if (grant.Type != "user" && grant.Type != "team") || grant.ID == "" || len(grant.ID) > 256 || strings.ContainsRune(grant.ID, '\x00') {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "each grant needs a valid user/team type and id"})
				return
			}
			key := grant.Type + "\x00" + grant.ID
			if _, duplicate := seen[key]; duplicate {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "duplicate grant"})
				return
			}
			seen[key] = struct{}{}
		}
		sort.Slice(request.Grants, func(i, j int) bool {
			if request.Grants[i].Type == request.Grants[j].Type {
				return request.Grants[i].ID < request.Grants[j].ID
			}
			return request.Grants[i].Type < request.Grants[j].Type
		})
		// 授权按不透明 id 存储，形状校验拦不住打错的 UUID：那会静默存下一行
		// 指向不存在主体的授权（审计还记 succeeded）。删除既有授权之前先逐个
		// 确认主体存在于 AEP 目录，顺序让返回的未知清单稳定可读。
		unknownSubjects, lookupErr := p.resolveKnowledgeGrantSubjects(r.Context(), request.Grants)
		if lookupErr != nil {
			// Fail closed：校验依赖的目录不可用就不放行，并在审计里留一条
			// 失败记录，运维能看到「授权没保存」源于上游而不是静默成功。
			p.recordKnowledgeGrantFailure(r.Context(), ident, tenant, baseID, lookupErr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "知识库授权主体校验暂不可用"})
			return
		}
		if len(unknownSubjects) > 0 {
			// 面向控制台：错误消息只列前几个，完整清单由 unknown_subjects 给出。
			preview := unknownSubjects
			if len(preview) > 5 {
				preview = preview[:5]
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":            fmt.Sprintf("以下授权主体不存在：%s（共 %d 项）", strings.Join(preview, "、"), len(unknownSubjects)),
				"unknown_subjects": unknownSubjects,
			})
			return
		}
		grants := make([]knowledgeGrantDTO, 0, len(request.Grants))
		for _, grant := range request.Grants {
			grants = append(grants, knowledgeGrantDTO{Type: grant.Type, ID: grant.ID, GrantedBy: ident.UserID})
		}
		p.replaceKnowledgeBaseGrants(w, r, ident, tenant, baseID, grants)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (p *portal) replaceKnowledgeBaseGrants(w http.ResponseWriter, r *http.Request, ident identity, tenant, baseID string, grants []knowledgeGrantDTO) {
	op, err := p.beginManagedAudit(r.Context(), ident, tenant, "replace_grants", "knowledge_base", baseID, "")
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge grant audit unavailable"})
		return
	}
	tx, err := p.db.BeginTx(r.Context(), nil)
	if err != nil {
		_ = p.finishManagedAudit(r.Context(), op, baseID, "failed", http.StatusServiceUnavailable)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge grants unavailable"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM knowledge_base_grants WHERE deployment_id=$1 AND weknora_tenant_id=$2 AND knowledge_base_id=$3`, ident.DeploymentID, tenant, baseID); err != nil {
		_ = p.finishManagedAudit(r.Context(), op, baseID, "failed", http.StatusServiceUnavailable)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge grants unavailable"})
		return
	}
	for _, grant := range grants {
		if _, err := tx.ExecContext(r.Context(), `INSERT INTO knowledge_base_grants (deployment_id,weknora_tenant_id,knowledge_base_id,subject_type,subject_id,granted_by) VALUES ($1,$2,$3,$4,$5,$6)`, ident.DeploymentID, tenant, baseID, grant.Type, grant.ID, ident.UserID); err != nil {
			_ = p.finishManagedAudit(r.Context(), op, baseID, "failed", http.StatusServiceUnavailable)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge grants unavailable"})
			return
		}
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE knowledge_audit_events SET outcome='succeeded',status_code=$2 WHERE operation_id=$1`, op, http.StatusOK); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge grant audit unavailable"})
		return
	}
	if err := tx.Commit(); err != nil {
		_ = p.finishManagedAudit(r.Context(), op, baseID, "uncertain", http.StatusServiceUnavailable)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge grants commit uncertain"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"knowledge_base_id": baseID, "tenant_id": tenant, "grants": grants})
}

// resolveKnowledgeGrantSubjects confirms every grant subject exists in the
// live AEP directory and returns the ids that do not, as "type:id" so the
// console can name them. Callers must have completed shape validation: the
// type is always user or team here. A non-nil error means the directory
// needed for the check was unavailable — callers must fail closed. One
// lookup per distinct subject: referenceResolver caches per request.
func (p *portal) resolveKnowledgeGrantSubjects(ctx context.Context, grants []knowledgeGrantInput) ([]string, error) {
	resolver := newReferenceResolver(p, ctx)
	unknown := make([]string, 0, len(grants))
	for _, grant := range grants {
		var (
			found bool
			err   error
		)
		if grant.Type == "user" {
			_, found, err = resolver.lookupUser(grant.ID)
		} else {
			_, found, err = resolver.lookupTeam(grant.ID)
		}
		if err != nil {
			return nil, err
		}
		if !found {
			unknown = append(unknown, grant.Type+":"+grant.ID)
		}
	}
	return unknown, nil
}

// recordKnowledgeGrantFailure keeps a failed replace_grants attempt visible
// when subject validation could not run. The audit write is best effort: it
// must not change the fail-closed response the caller already owes.
func (p *portal) recordKnowledgeGrantFailure(ctx context.Context, ident identity, tenant, baseID string, cause error) {
	p.logger.Warn("knowledge grants: subject directory unavailable", "error", cause)
	op, err := p.beginManagedAudit(ctx, ident, tenant, "replace_grants", "knowledge_base", baseID, "")
	if err != nil {
		return
	}
	_ = p.finishManagedAudit(ctx, op, baseID, "failed", http.StatusBadGateway)
}
