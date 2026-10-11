package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// knowledgeSessionActive checks the durable session row even when the local
// session cache has a hit. Logout deletes that row; knowledge capabilities
// must not continue working from an in-memory identity after revocation.
func (p *portal) knowledgeSessionActive(ctx context.Context, token, userID string) bool {
	if p == nil || p.db == nil || strings.TrimSpace(token) == "" || strings.TrimSpace(userID) == "" {
		return false
	}
	var active bool
	if err := p.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE token=$1 AND expires_at > now())`, token).Scan(&active); err != nil {
		return false
	}
	return active
}

// knowledgeGrantSubject is derived only from a freshly verified AEP identity.
// It is deliberately not a request DTO: callers must never accept these values
// from a portal/MCP payload.
type knowledgeGrantSubject struct {
	Type string
	ID   string
}

// knowledgeGrantLookup returns true only when the KB is registered for the
// exact deployment+tenant and at least one supplied user/team grant exists.
// The SQL implementation below uses a composite join, so a shared WeKnora
// tenant does not make another deployment's registration or grants visible.
type knowledgeGrantLookup interface {
	hasKnowledgeGrant(context.Context, string, string, string, []knowledgeGrantSubject) (bool, error)
}

type sqlKnowledgeGrantLookup struct{ db *sql.DB }

func (s sqlKnowledgeGrantLookup) hasKnowledgeGrant(ctx context.Context, deployment, tenant, knowledgeBase string, subjects []knowledgeGrantSubject) (bool, error) {
	if s.db == nil || deployment == "" || tenant == "" || knowledgeBase == "" || len(subjects) == 0 {
		return false, nil
	}
	const query = `SELECT EXISTS (
		SELECT 1
		FROM knowledge_base_registry r
		JOIN knowledge_base_grants g
		  ON g.deployment_id=r.deployment_id
		 AND g.weknora_tenant_id=r.weknora_tenant_id
		 AND g.knowledge_base_id=r.knowledge_base_id
		WHERE r.deployment_id=$1 AND r.weknora_tenant_id=$2 AND r.knowledge_base_id=$3
		  AND g.subject_type=$4 AND g.subject_id=$5
		  AND r.lifecycle_state='active'
	)`
	for _, subject := range subjects {
		if (subject.Type != "user" && subject.Type != "team") || subject.ID == "" {
			continue
		}
		var found bool
		if err := s.db.QueryRowContext(ctx, query, deployment, tenant, knowledgeBase, subject.Type, subject.ID).Scan(&found); err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

var errKnowledgeIdentityUnavailable = errors.New("knowledge requester identity unavailable")
var errKnowledgeAccessDenied = errors.New("knowledge access denied")

// verifyKnowledgeRequester revalidates the server-held AEP access token for
// each sensitive knowledge operation. The cached portal identity is used only
// to bind the session to the same principal/deployment; current roles and team
// memberships come from the AEP response. Missing/invalid/expired credentials
// fail closed. This does not grant administrator bypass for knowledge reads.
func (p *portal) verifyKnowledgeRequester(ctx context.Context, sessionIdentity identity) (identity, error) {
	if p == nil || p.aep == nil || strings.TrimSpace(sessionIdentity.Token) == "" || sessionIdentity.UserID == "" || sessionIdentity.DeploymentID == "" {
		return identity{}, errKnowledgeIdentityUnavailable
	}
	current, err := p.aep.verify(ctx, sessionIdentity.Token)
	if err != nil {
		return identity{}, fmt.Errorf("revalidate knowledge requester: %w", err)
	}
	if current.UserID == "" || current.DeploymentID == "" || current.UserID != sessionIdentity.UserID || current.DeploymentID != sessionIdentity.DeploymentID {
		return identity{}, errKnowledgeIdentityUnavailable
	}
	return current, nil
}

// resolveEmployeeKnowledgeAccess is the server-side composition point for a
// future employee retrieval route. The employee name is only a lookup key;
// bindings, owner/team/visibility and KB configuration are loaded from the
// DigitalEmployee CR. Caller-selected KB ids can narrow but cannot expand the
// CR binding. AEP is reverified before any current team grants are considered.
func (p *portal) resolveEmployeeKnowledgeAccess(ctx context.Context, sessionIdentity identity, employeeName string, requestedKnowledgeBases []string) ([]string, error) {
	current, err := p.verifyKnowledgeRequester(ctx, sessionIdentity)
	if err != nil {
		return nil, err
	}
	if p.k8s == nil || p.db == nil || strings.TrimSpace(employeeName) == "" || p.cfg.managedKnowledge.tenantID == "" {
		return []string{}, nil
	}
	var employee dev1alpha1.DigitalEmployee
	if err := p.k8s.Get(ctx, types.NamespacedName{Namespace: p.cfg.namespace, Name: employeeName}, &employee); err != nil {
		if apierrors.IsNotFound(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("load employee knowledge binding: %w", err)
	}
	if !current.canAccessEmployee(p.cfg, employee.Spec.Team, employee.Spec.Owner, employee.Spec.Visibility) {
		return []string{}, nil
	}
	if employee.Spec.Knowledge == nil || employee.Spec.Knowledge.Bases == nil || len(employee.Spec.Knowledge.Bases) == 0 {
		return []string{}, nil
	}
	bindings := make([]string, 0, len(employee.Spec.Knowledge.Bases))
	for _, base := range employee.Spec.Knowledge.Bases {
		bindings = append(bindings, base.ID)
	}
	return effectiveKnowledgeBases(ctx, current, p.cfg.managedKnowledge.tenantID, bindings, requestedKnowledgeBases, sqlKnowledgeGrantLookup{db: p.db})
}

// authorizeEmployeeKnowledgeBase is the common gate for a specific retrieval,
// citation, document, image, preview, or download. Callers must resolve the
// resource's KB from trusted WeKnora metadata first, then invoke this gate
// immediately before returning the content. User-supplied KB ids only narrow
// the employee CR bindings.
func (p *portal) authorizeEmployeeKnowledgeBase(ctx context.Context, sessionIdentity identity, employeeName, knowledgeBaseID string) error {
	if strings.TrimSpace(knowledgeBaseID) == "" {
		return errKnowledgeAccessDenied
	}
	allowed, err := p.resolveEmployeeKnowledgeAccess(ctx, sessionIdentity, employeeName, []string{knowledgeBaseID})
	if err != nil {
		return err
	}
	for _, id := range allowed {
		if id == knowledgeBaseID {
			return nil
		}
	}
	return errKnowledgeAccessDenied
}

// effectiveKnowledgeBases computes the restrictive intersection for one
// request: explicit employee bindings ∩ caller-selected KBs ∩ current AEP
// user's/team grants ∩ deployment+tenant registry. It has no administrator
// bypass. Nil and empty slices are deny-all; neither means "all KBs".
// employeeBindings must come from the server-side DigitalEmployee CR, while
// requestedKnowledgeBases may come from the caller and only narrows access.
func effectiveKnowledgeBases(ctx context.Context, requester identity, tenant string, employeeBindings, requestedKnowledgeBases []string, grants knowledgeGrantLookup) ([]string, error) {
	if requester.UserID == "" || requester.DeploymentID == "" || tenant == "" || employeeBindings == nil || len(employeeBindings) == 0 || requestedKnowledgeBases == nil || len(requestedKnowledgeBases) == 0 || grants == nil {
		return []string{}, nil
	}
	subjects := make([]knowledgeGrantSubject, 0, 1+len(requester.Teams))
	subjects = append(subjects, knowledgeGrantSubject{Type: "user", ID: requester.UserID})
	for _, team := range requester.Teams {
		if strings.TrimSpace(team) != "" {
			subjects = append(subjects, knowledgeGrantSubject{Type: "team", ID: team})
		}
	}
	bindings := make(map[string]struct{}, len(employeeBindings))
	for _, id := range employeeBindings {
		if id = strings.TrimSpace(id); id != "" {
			bindings[id] = struct{}{}
		}
	}
	result := make([]string, 0)
	seen := make(map[string]struct{})
	for _, id := range requestedKnowledgeBases {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, bound := bindings[id]; !bound {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		allowed, err := grants.hasKnowledgeGrant(ctx, requester.DeploymentID, tenant, id, subjects)
		if err != nil {
			return nil, fmt.Errorf("resolve knowledge grant for %q: %w", id, err)
		}
		if allowed {
			result = append(result, id)
			seen[id] = struct{}{}
		}
	}
	return result, nil
}

// Migration contract for the later integration patch (kept here until
// main.go's schema migration is integrated):
//
// CREATE TABLE knowledge_base_grants (
//   deployment_id TEXT NOT NULL,
//   weknora_tenant_id TEXT NOT NULL,
//   knowledge_base_id TEXT NOT NULL,
//   subject_type TEXT NOT NULL CHECK (subject_type IN ('user','team')),
//   subject_id TEXT NOT NULL,
//   granted_by TEXT NOT NULL,
//   created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
//   PRIMARY KEY (deployment_id,weknora_tenant_id,knowledge_base_id,subject_type,subject_id),
//   FOREIGN KEY (deployment_id,weknora_tenant_id,knowledge_base_id)
//     REFERENCES knowledge_base_registry(deployment_id,weknora_tenant_id,knowledge_base_id)
//     ON DELETE CASCADE
// );
