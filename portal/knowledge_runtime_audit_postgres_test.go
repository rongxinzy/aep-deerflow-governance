package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// Owns only a newly created schema in an explicitly disposable PostgreSQL DB.
// The real UPDATE must reject rows belonging to administrative operations,
// old unbound runtimes, other tenants/employees/contexts and completed reads.
func TestKnowledgeRuntimeAuditPostgresBindsOperationToRunContext(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("knowledge_runtime_audit_%d", time.Now().UnixNano())
	owner := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = owner.Close() })
	_, err = owner.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = owner.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	// Simulate the old audit table and run the actual additive startup DDL.
	_, err = db.Exec(`CREATE TABLE knowledge_audit_events (
		id BIGSERIAL PRIMARY KEY, operation_id TEXT NOT NULL UNIQUE,
		deployment_id TEXT NOT NULL, weknora_tenant_id TEXT NOT NULL,
		actor_user_id TEXT NOT NULL, action TEXT NOT NULL,
		resource_type TEXT NOT NULL, resource_id TEXT NOT NULL DEFAULT '',
		parent_resource_id TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL,
		status_code INTEGER NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	); INSERT INTO knowledge_audit_events (operation_id,deployment_id,weknora_tenant_id,actor_user_id,action,resource_type,outcome,status_code)
		VALUES ('legacy','demo','tenant-1','user-1','runtime_search','knowledge_scope','pending',0)`)
	require.NoError(t, err)
	require.NoError(t, migrate(db))
	require.NoError(t, migrate(db), "startup DDL must be repeatable")
	p := &portal{db: db, cfg: config{aepDeploymentID: "demo", managedKnowledge: managedKnowledgeConfig{tenantID: "tenant-1"}}}
	requester := identity{UserID: "user-1", DeploymentID: "demo"}
	digest := sha256.Sum256([]byte(strings.Repeat("A", 43)))
	contextHash := hex.EncodeToString(digest[:])
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := func() string {
		t.Helper()
		op, startErr := p.beginKnowledgeRuntimeAudit(ctx, requester, "tenant-1", "employee-1", contextHash, "runtime_search", "knowledge_scope", "kb-1", "")
		require.NoError(t, startErr)
		return op
	}
	checkPending := func(op string) {
		t.Helper()
		var outcome string
		var status int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT outcome,status_code FROM knowledge_audit_events WHERE operation_id=$1`, op).Scan(&outcome, &status))
		require.Equal(t, "pending", outcome)
		require.Zero(t, status)
	}
	require.Error(t, p.completeKnowledgeRuntimeAudit(ctx, requester, "employee-1", contextHash, "legacy", "succeeded", 200))
	checkPending("legacy")
	adminOperation, err := p.beginManagedAudit(ctx, requester, "tenant-1", "delete", "knowledge-base", "kb-1", "")
	require.NoError(t, err)
	require.Error(t, p.completeKnowledgeRuntimeAudit(ctx, requester, "employee-1", contextHash, adminOperation, "succeeded", 200))
	checkPending(adminOperation)
	for _, field := range []string{"runtime_employee_name", "runtime_context_hash", "weknora_tenant_id", "actor_user_id", "deployment_id", "action"} {
		t.Run(field, func(t *testing.T) {
			op := start()
			_, updateErr := db.ExecContext(ctx, `UPDATE knowledge_audit_events SET `+field+`=$1 WHERE operation_id=$2`, "foreign", op)
			require.NoError(t, updateErr)
			require.Error(t, p.completeKnowledgeRuntimeAudit(ctx, requester, "employee-1", contextHash, op, "succeeded", 200))
			checkPending(op)
		})
	}
	for _, result := range []struct {
		outcome string
		status  int
	}{{"succeeded", 200}, {"failed", 502}} {
		op := start()
		require.NoError(t, p.completeKnowledgeRuntimeAudit(ctx, requester, "employee-1", contextHash, op, result.outcome, result.status))
		var outcome string
		var status int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT outcome,status_code FROM knowledge_audit_events WHERE operation_id=$1`, op).Scan(&outcome, &status))
		require.Equal(t, result.outcome, outcome)
		require.Equal(t, result.status, status)
		require.Error(t, p.completeKnowledgeRuntimeAudit(ctx, requester, "employee-1", contextHash, op, "succeeded", 200), "completion cannot be replayed")
	}
}
