package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestSharedSpacesBrowserPortalHarness is an opt-in local HTTP fixture for
// browser acceptance. It listens only on localhost and exits cleanly on SIGINT
// or SIGTERM. AEP, PostgreSQL and Paperclip remain the real isolated services.
func TestSharedSpacesBrowserPortalHarness(t *testing.T) {
	if os.Getenv("PORTAL_SHARED_SPACE_BROWSER_HARNESS") != "1" {
		t.Skip("set PORTAL_SHARED_SPACE_BROWSER_HARNESS=1 to run the local browser harness")
	}
	dsn := os.Getenv("PORTAL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Fatal("PORTAL_TEST_POSTGRES_URL is required")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test PostgreSQL URL")
	}
	switch cfg.Host {
	case "localhost", "127.0.0.1", "::1":
	default:
		t.Fatal("browser harness requires localhost PostgreSQL")
	}
	schema := fmt.Sprintf("shared_space_browser_%d", time.Now().UnixNano())
	persistentSchema := false
	if configured := os.Getenv("PORTAL_BROWSER_SCHEMA"); configured != "" {
		if len(configured) > 63 {
			t.Fatal("PORTAL_BROWSER_SCHEMA is too long")
		}
		validSchema := true
		for _, ch := range configured {
			if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' {
				continue
			}
			validSchema = false
		}
		if !validSchema {
			t.Fatal("PORTAL_BROWSER_SCHEMA must contain lowercase letters, digits, and underscores")
		}
		schema = configured
		persistentSchema = true
	}
	adminDB := stdlib.OpenDB(*cfg)
	defer func() { _ = adminDB.Close() }()
	if _, err = adminDB.Exec(`CREATE SCHEMA IF NOT EXISTS ` + schema); err != nil {
		t.Fatal("cannot create test schema")
	}
	if !persistentSchema {
		defer func() { _, _ = adminDB.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) }()
	}
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	defer func() { _ = db.Close() }()
	if err = migrate(db); err != nil {
		t.Fatal("cannot initialize test schema")
	}

	required := func(key string) string {
		t.Helper()
		v := os.Getenv(key)
		if v == "" {
			t.Fatalf("%s is required", key)
		}
		return v
	}
	adminUser, adminPassword := required("PORTAL_BROWSER_AEP_ADMIN_USER"), required("PORTAL_BROWSER_AEP_ADMIN_PASSWORD")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "portal-browser-test-credentials", Namespace: "digital-employees"},
		Data:       map[string][]byte{"AEP_ADMIN_USER": []byte(adminUser), "AEP_ADMIN_PASSWORD": []byte(adminPassword)},
	}
	scheme := runtime.NewScheme()
	if err = schemeAddK8s(scheme); err != nil {
		t.Fatal("cannot initialize fake Kubernetes scheme")
	}
	if err = dev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal("cannot initialize DigitalEmployee and PersonalWorkspace fake scheme")
	}
	h := newAPIHarness(t, secret)
	h.db = db
	h.cfg.namespace = secret.Namespace
	h.cfg.credentialsSecret = secret.Name
	h.cfg.aepBaseURL = required("PORTAL_BROWSER_AEP_BASE_URL")
	h.cfg.aepDeploymentID = required("PORTAL_BROWSER_AEP_DEPLOYMENT_ID")
	h.cfg.adminUsers = map[string]bool{required("PORTAL_BROWSER_ADMIN_USER_ID"): true}
	h.cfg.adminRoles = map[string]bool{"platform-admin": true}
	h.cfg.paperclipBaseURL = required("PORTAL_BROWSER_PAPERCLIP_BASE_URL")
	h.cfg.paperclipToken = required("PORTAL_BROWSER_PAPERCLIP_TOKEN")
	h.aep = &aepAuthenticator{baseURL: h.cfg.aepBaseURL, http: &http.Client{Timeout: 8 * time.Second}}
	h.k8s = fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(secret).Build()
	clearSharedSpaceAdminTokenCache(t)

	port := 18500
	if raw := os.Getenv("PORTAL_BROWSER_PORT"); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1024 || parsed > 65535 {
			t.Fatal("PORTAL_BROWSER_PORT must be a valid local TCP port")
		}
		port = parsed
	}
	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: h.routes(), ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	select {
	case err = <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			t.Fatalf("local portal failed to start: %v", err)
		}
		return
	case <-time.After(400 * time.Millisecond):
	}
	t.Logf("PORTAL_BROWSER_HARNESS_READY http://127.0.0.1:%d", port)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case <-signals:
	case err = <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			t.Fatalf("local portal stopped: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
