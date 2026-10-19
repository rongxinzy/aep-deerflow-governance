package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The in-memory store covers the handler logic; this exercises the Postgres
// implementation the portal actually ships — the upsert must replace a partial
// outcome instead of accumulating rows, and the JSON partials must survive the
// round trip, because that row is what the console reads and what the retry
// endpoint decides from.
func TestDepartmentProvisioningPostgresStore(t *testing.T) {
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("requires disposable PORTAL_TEST_POSTGRES_URL")
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("dept_prov_%d", time.Now().UnixNano())
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(departmentProvisionSchema)
	require.NoError(t, err)

	store := &sqlProvisioningStore{db: db}
	ctx := context.Background()

	t.Run("a partial outcome survives and a retry replaces it instead of adding a row", func(t *testing.T) {
		require.NoError(t, store.Save(ctx, departmentProvision{
			ID:          "rd-dept",
			Name:        "研发部",
			Phase:       "partial",
			Partials:    []string{"openviking: unavailable", "weknora: 500"},
			OvAccountID: "tenant-rd-dept",
			Message:     "partially provisioned: [openviking: unavailable weknora: 500]",
			UpdatedAt:   time.Now().UTC(),
		}))

		records, err := store.Load(ctx)
		require.NoError(t, err)
		record, ok := records["rd-dept"]
		require.True(t, ok, "the outcome must be readable after the request ended")
		assert.Equal(t, "partial", record.Phase)
		assert.Equal(t, "研发部", record.Name)
		assert.Equal(t, "tenant-rd-dept", record.OvAccountID)
		assert.Equal(t, []string{"openviking: unavailable", "weknora: 500"}, record.Partials,
			"partials must round-trip exactly: the console shows them as the reason")
		require.False(t, record.UpdatedAt.IsZero())

		require.NoError(t, store.Save(ctx, departmentProvision{ID: "rd-dept", Name: "研发部", Phase: "ready"}))
		records, err = store.Load(ctx)
		require.NoError(t, err)
		require.Len(t, records, 1, "the second save must update the row, never duplicate it")
		assert.Equal(t, "ready", records["rd-dept"].Phase)
		assert.Empty(t, records["rd-dept"].Partials)
		assert.Equal(t, "tenant-rd-dept", records["rd-dept"].OvAccountID,
			"a later save that carries no id must not erase the one already recorded")

		require.NoError(t, store.Save(ctx, departmentProvision{ID: "rd-dept", Name: "研发部", Phase: "ready", OvAccountID: "tenant-repaired"}))
		records, err = store.Load(ctx)
		require.NoError(t, err)
		assert.Equal(t, "tenant-repaired", records["rd-dept"].OvAccountID, "a real id still replaces the old one")
	})

	t.Run("a partial create is persisted and then retried through the endpoint", func(t *testing.T) {
		stubs := newDepartmentStubs(t)
		failingWeKnora(stubs)
		h := newDepartmentHarness(t, stubs)
		h.provisioningStore = &sqlProvisioningStore{db: db}

		require.Equal(t, "partial", createDepartment(t, h).Phase)

		records, err := store.Load(ctx)
		require.NoError(t, err)
		require.Equal(t, "partial", records["rd-dept"].Phase, "the create wrote the outcome to the database")
		require.Equal(t, "tenant-rd-dept", records["rd-dept"].OvAccountID)

		failingWeKnoraRecover(stubs)
		w := httptest.NewRecorder()
		h.handleProvisionDepartment(w, provisionRequest("rd-dept"), adminIdentity())
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "ready", decodeDepartmentStatus(t, w).Phase)

		records, err = store.Load(ctx)
		require.NoError(t, err)
		require.Len(t, records, 1, "retrying must keep one row")
		assert.Equal(t, "ready", records["rd-dept"].Phase)
		assert.Equal(t, "tenant-rd-dept", records["rd-dept"].OvAccountID,
			"the account that already existed is preserved, not re-created")
	})
}
