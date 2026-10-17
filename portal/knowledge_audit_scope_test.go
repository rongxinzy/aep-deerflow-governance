package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

func TestKnowledgeRuntimeAuditStoresExactBaseMembership(t *testing.T) {
	h := newServicesHarness(t, newServicesStubs(t))
	configureManagedTest(t, h, "10000")
	digest := sha256.Sum256([]byte("test run context"))
	hash := hex.EncodeToString(digest[:])
	for _, fail := range []bool{false, true} {
		expect := h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events .*knowledge_base_ids.*\$11::jsonb`).
			WithArgs(sqlmock.AnyArg(), "demo", "10000", "admin-user", "runtime_search", "knowledge_scope", "kb-1,kb-10,with,comma", "", "employee-1", hash, `["kb-1","kb-10","with,comma"]`)
		if fail {
			expect.WillReturnError(fmt.Errorf("audit database unavailable"))
		} else {
			expect.WillReturnResult(sqlmock.NewResult(0, 1))
		}
		_, err := h.beginKnowledgeRuntimeAudit(t.Context(), adminIdentityForManagedTest(), "10000", "employee-1", hash, "runtime_search", "knowledge_scope", "kb-1,kb-10,with,comma", "", "kb-1", "kb-10", "with,comma")
		if fail {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

// Runs the actual migration and predicate against an explicitly disposable DB.
// Rows with coincident IDs in other scopes must not establish chunk ancestry.
func TestKnowledgeAuditBaseMembershipPostgres(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("knowledge_audit_scope_%d", time.Now().UnixNano())
	owner := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = owner.Close() })
	_, err = owner.ExecContext(t.Context(), `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = owner.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate(db))
	require.NoError(t, migrate(db))
	p := &portal{db: db, cfg: config{aepDeploymentID: "demo", adminRoles: set("admin"), managedKnowledge: managedKnowledgeConfig{tenantID: "10000"}}}
	ident := adminIdentityForManagedTest()
	chunkOp, err := p.beginManagedAudit(t.Context(), ident, "10000", "update_chunk", "chunk", "chunk-new", "doc-deleted", "kb-1")
	require.NoError(t, err)
	var parent string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT parent_resource_id FROM knowledge_audit_events WHERE operation_id=$1`, chunkOp).Scan(&parent))
	require.Equal(t, "doc-deleted", parent)
	digest := sha256.Sum256([]byte("test run context"))
	scopeOp, err := p.beginKnowledgeRuntimeAudit(t.Context(), ident, "10000", "employee-1", hex.EncodeToString(digest[:]), "runtime_search", "knowledge_scope", "kb-1,kb-10,with,comma", "", "kb-1", "kb-10", "with,comma")
	require.NoError(t, err)
	for _, row := range []struct{ op, deployment, tenant, kind, id, parent string }{
		{"base", "demo", "10000", "knowledge_base", "kb-1", ""},
		{"document", "demo", "10000", "document", "doc-old", "kb-1"},
		{"chunk-old", "demo", "10000", "chunk", "chunk-old", "doc-old"},
		{"scope-old", "demo", "10000", "knowledge_scope", "kb-1,kb-10", ""},
		{"prefix", "demo", "10000", "knowledge_scope", "kb-100", ""},
		{"foreign-deployment", "other", "10000", "document", "foreign-doc", "kb-1"},
		{"foreign-tenant", "demo", "other", "document", "foreign-doc", "kb-1"},
		{"unresolved-chunk", "demo", "10000", "chunk", "chunk-other", "foreign-doc"},
	} {
		_, err = db.ExecContext(t.Context(), `INSERT INTO knowledge_audit_events (operation_id,deployment_id,weknora_tenant_id,actor_user_id,action,resource_type,resource_id,parent_resource_id,outcome,status_code) VALUES ($1,$2,$3,'admin-user','test',$4,$5,$6,'succeeded',200)`, row.op, row.deployment, row.tenant, row.kind, row.id, row.parent)
		require.NoError(t, err)
	}
	for _, base := range []string{"kb-1", "kb-10", "with,comma", "kb-"} {
		w := httptest.NewRecorder()
		p.handleKnowledgeAudit(w, httptest.NewRequest("GET", "/?knowledgeBaseId="+base, nil), ident)
		require.Equal(t, 200, w.Code, w.Body.String())
		body := w.Body.String()
		switch base {
		case "kb-1":
			for _, op := range []string{chunkOp, scopeOp, "base", "document", "chunk-old", "scope-old"} {
				require.Contains(t, body, `"operationId":"`+op+`"`)
			}
		case "kb-":
			require.Contains(t, body, `"items":[]`)
		default:
			require.Contains(t, body, scopeOp)
		}
		for _, op := range []string{"prefix", "foreign-deployment", "foreign-tenant", "unresolved-chunk"} {
			require.NotContains(t, body, `"operationId":"`+op+`"`)
		}
	}
	// New explicit membership must not interpret one comma-containing ID as two.
	w := httptest.NewRecorder()
	p.handleKnowledgeAudit(w, httptest.NewRequest("GET", "/?knowledgeBaseId=with", nil), ident)
	require.Equal(t, 200, w.Code)
	require.False(t, strings.Contains(w.Body.String(), scopeOp))
}
