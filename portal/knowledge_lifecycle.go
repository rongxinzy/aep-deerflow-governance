package main

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

var errKnowledgeBaseInactive = errors.New("knowledge base is missing or pending deletion")

// Every Portal binding path holds the same registry rows until the Kubernetes
// write or pending request is durable. Sorted locks avoid deadlocks for multi-KB
// bindings. Direct Kubernetes writes are outside the Portal's lock boundary.
func (p *portal) lockKnowledgeBindings(ctx context.Context, deployment string, ids []string) (*sql.Tx, error) {
	if p.cfg.managedKnowledge.tenantID == "" || len(ids) == 0 {
		return nil, nil
	}
	if p.db == nil || deployment == "" || deployment != p.cfg.aepDeploymentID {
		return nil, errKnowledgeBaseInactive
	}
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	for index, id := range ids {
		if index > 0 && ids[index-1] == id {
			continue
		}
		var state string
		err = tx.QueryRowContext(ctx, `SELECT lifecycle_state FROM knowledge_base_registry WHERE deployment_id=$1 AND weknora_tenant_id=$2 AND knowledge_base_id=$3 FOR UPDATE`, deployment, p.cfg.managedKnowledge.tenantID, id).Scan(&state)
		if err != nil || state != "active" {
			_ = tx.Rollback()
			return nil, errKnowledgeBaseInactive
		}
	}
	return tx, nil
}

func employeeKnowledgeIDs(spec dev1alpha1.DigitalEmployee) []string {
	if spec.Spec.Knowledge == nil {
		return nil
	}
	ids := make([]string, 0, len(spec.Spec.Knowledge.Bases))
	for _, base := range spec.Spec.Knowledge.Bases {
		ids = append(ids, base.ID)
	}
	return ids
}
