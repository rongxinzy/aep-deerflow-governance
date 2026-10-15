package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Opt-in fixture for zhiyuanBotUI/verify/collaboration-integration.mjs.
// Only Kubernetes and AEP are faked. Routes, cookie/CSRF authentication,
// PostgreSQL, worker delivery and workspace reverse proxy are production code.
func TestCollaborationIntegrationFixture(t *testing.T) {
	file := os.Getenv("COLLABORATION_INTEGRATION_CONFIG")
	if file == "" {
		t.Skip("requires the local cross-repository integration runner")
	}
	var config struct{ Address, SourceGateway, TargetGateway, Output string }
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &config))
	require.True(t, strings.HasPrefix(config.Address, "127.0.0.1:"))
	cfg, err := pgx.ParseConfig(os.Getenv("PORTAL_TEST_POSTGRES_URL"))
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", cfg.Host, "fixture must use disposable local PostgreSQL")
	schema := fmt.Sprintf("collaboration_integration_%d", time.Now().UnixNano())
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate(db))
	h := collaborationHarness(t)
	h.db = db
	for name, endpoint := range map[string]string{"ws-source": config.SourceGateway, "ws-target": config.TargetGateway} {
		var ws dev.PersonalWorkspace
		require.NoError(t, h.k8s.Get(t.Context(), client.ObjectKey{Namespace: h.cfg.namespace, Name: name}, &ws))
		ws.Status.GatewayEndpoint = strings.TrimPrefix(endpoint, "http://")
		ws.Spec.Companions = ws.Spec.Companions[:1]
		ws.Spec.Companions[0].DisplayName = "工作助手"
		require.NoError(t, h.k8s.Update(t.Context(), &ws))
	}
	sessions := map[string]map[string]string{}
	for _, ident := range []identity{
		{UserID: "u-x", Username: "userx", DisplayName: "用户甲", Kind: "human"},
		{UserID: "u-owner", Username: "lisi", DisplayName: "李四", Kind: "human"},
	} {
		ident.TeamsFetchedAt = time.Now()
		token, csrf := h.sessions.issue(ident)
		sessions[ident.UserID] = map[string]string{"session": token, "csrf": csrf}
	}
	server := httptest.NewUnstartedServer(h.routes())
	_ = server.Listener.Close()
	server.Listener, err = net.Listen("tcp", config.Address)
	require.NoError(t, err)
	server.Start()
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for ctx.Err() == nil {
				if !h.processCollaboration(ctx) {
					select {
					case <-ctx.Done():
						return
					case <-time.After(100 * time.Millisecond):
					}
				}
			}
		}()
	}
	t.Cleanup(func() { cancel(); workers.Wait() })
	raw, err = json.Marshal(map[string]any{"url": server.URL, "sessions": sessions})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(config.Output, "portal.json"), raw, 0600))
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(config.Output, "stop")); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("integration runner did not finish before the fixture deadline")
}
