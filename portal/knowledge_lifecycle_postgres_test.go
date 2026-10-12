package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// This test owns an isolated schema, never production registry rows. Run with
// PORTAL_TEST_POSTGRES_URL pointing to a disposable PostgreSQL database.
func TestKnowledgeLifecyclePostgresSerializesDeleteAndBinding(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("knowledge_lock_%d", time.Now().UnixNano())
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	cfg.RuntimeParams["search_path"] = schema
	registryDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = registryDB.Close() })
	_, err = registryDB.Exec(`CREATE TABLE knowledge_base_registry (deployment_id TEXT, weknora_tenant_id TEXT, knowledge_base_id TEXT, lifecycle_state TEXT NOT NULL DEFAULT 'active', PRIMARY KEY(deployment_id,weknora_tenant_id,knowledge_base_id)); INSERT INTO knowledge_base_registry VALUES ('demo','10000','kb-1','active')`)
	require.NoError(t, err)
	newPortal := func(database *sql.DB) *portal {
		return &portal{db: database, cfg: config{aepDeploymentID: "demo", managedKnowledge: managedKnowledgeConfig{tenantID: "10000"}}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deleting, err := newPortal(registryDB).lockKnowledgeBindings(ctx, "demo", []string{"kb-1"})
	require.NoError(t, err)
	defer func() { _ = deleting.Rollback() }()
	result := make(chan error, 1)
	go func() {
		tx, lockErr := newPortal(registryDB).lockKnowledgeBindings(ctx, "demo", []string{"kb-1"})
		if tx != nil {
			_ = tx.Rollback()
		}
		result <- lockErr
	}()
	// Observe the actual second PostgreSQL transaction waiting on the first;
	// merely sleeping or checking a mock would not prove serialization.
	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := registryDB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE NOT granted AND pid IN (SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND query LIKE 'SELECT lifecycle_state FROM knowledge_base_registry%'))`).Scan(&waiting)
		return queryErr == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	_, err = deleting.ExecContext(ctx, `UPDATE knowledge_base_registry SET lifecycle_state='deleting' WHERE knowledge_base_id='kb-1'`)
	require.NoError(t, err)
	require.NoError(t, deleting.Commit())
	select {
	case lockErr := <-result:
		require.ErrorIs(t, lockErr, errKnowledgeBaseInactive)
	case <-ctx.Done():
		t.Fatal("binding did not resume after deletion barrier commit")
	}
	// A newly opened pool/Portal must observe the durable barrier after restart.
	restartedDB := stdlib.OpenDB(*cfg)
	defer func() { _ = restartedDB.Close() }()
	tx, err := newPortal(restartedDB).lockKnowledgeBindings(ctx, "demo", []string{"kb-1"})
	require.Nil(t, tx)
	require.ErrorIs(t, err, errKnowledgeBaseInactive)
}
