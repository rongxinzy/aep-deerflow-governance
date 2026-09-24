// de-portal: the business layer for digital employees. It is the only
// writer of DigitalEmployee CRs (besides platform admins) and owns the
// policy functions the operator deliberately does not: who may apply,
// quotas, and the approval workflow. End users never touch Kubernetes.
package main

import (
	"database/sql"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

type config struct {
	addr        string
	aepBaseURL  string
	policyMode  string
	adminUsers  map[string]bool
	adminRoles  map[string]bool
	quotaPerUser int
	namespace   string

	runtimeImage     string
	model            string
	memoryServer     string
	memoryAccount    string
	knowledgeMCPURL  string
	credentialsSecret string
	a2aImage         string

	dbURL string
}

func loadConfig() config {
	return config{
		addr:        env("PORTAL_ADDR", ":8090"),
		aepBaseURL:  strings.TrimRight(env("PORTAL_AEP_BASE_URL", "http://aep-control-service.aep-system.svc:8080"), "/"),
		policyMode:  env("PORTAL_POLICY_MODE", "approval"),
		adminUsers:  set(env("PORTAL_ADMIN_USERS", "")),
		adminRoles:  set(env("PORTAL_ADMIN_ROLES", "admin,platform-admin")),
		quotaPerUser: envInt("PORTAL_QUOTA_PER_USER", 2),
		namespace:   env("PORTAL_NAMESPACE", "digital-employees"),

		runtimeImage:      env("PORTAL_RUNTIME_IMAGE", "172.18.5.251:8081/zhiyuan/deerflow-gov:v8"),
		model:             env("PORTAL_MODEL", "bench-glm"),
		memoryServer:      env("PORTAL_MEMORY_SERVER", "http://openviking.memory-system.svc.cluster.local:1933"),
		memoryAccount:     env("PORTAL_MEMORY_ACCOUNT", "zhiyuan"),
		knowledgeMCPURL:   env("PORTAL_KNOWLEDGE_MCP_URL", "http://weknora-mcp.weknora.svc.cluster.local:8830/mcp"),
		credentialsSecret: env("PORTAL_CREDENTIALS_SECRET", "de-platform-creds"),
		a2aImage:          env("PORTAL_A2A_IMAGE", "172.18.5.251:8081/zhiyuan/a2a-adapter:0.1.0"),

		dbURL: env("PORTAL_DB_URL", ""),
	}
}

func main() {
	flag.Parse()
	cfg := loadConfig()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if cfg.dbURL == "" {
		slog.Error("PORTAL_DB_URL is required")
		os.Exit(1)
	}

	db, err := sql.Open("pgx", cfg.dbURL)
	if err != nil {
		slog.Error("db open failed", "error", err)
		os.Exit(1)
	}
	db.SetMaxOpenConns(4)
	for attempt := 1; attempt <= 10; attempt++ {
		if err := db.Ping(); err == nil {
			break
		} else if attempt == 10 {
			slog.Error("db unreachable", "error", err)
			os.Exit(1)
		}
		time.Sleep(3 * time.Second)
	}
	if err := migrate(db); err != nil {
		slog.Error("migrate failed", "error", err)
		os.Exit(1)
	}

	scheme := clientgoscheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		slog.Error("scheme failed", "error", err)
		os.Exit(1)
	}
	k8sClient, err := client.New(ctrlGetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		slog.Error("k8s client failed", "error", err)
		os.Exit(1)
	}

	portal := &portal{
		cfg:    cfg,
		db:     db,
		k8s:    k8sClient,
		aep:    &aepAuthenticator{baseURL: cfg.aepBaseURL, http: &http.Client{Timeout: 10 * time.Second}},
		logger: logger,
	}

	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           portal.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	slog.Info("de-portal listening", "addr", cfg.addr, "policy", cfg.policyMode, "namespace", cfg.namespace)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS requests (
	id            TEXT PRIMARY KEY,
	employee_name TEXT NOT NULL,
	owner_id      TEXT NOT NULL,
	owner_name    TEXT NOT NULL,
	display_name  TEXT NOT NULL DEFAULT '',
	spec_json     TEXT NOT NULL DEFAULT '{}',
	state         TEXT NOT NULL,
	decision_reason TEXT NOT NULL DEFAULT '',
	decided_by    TEXT NOT NULL DEFAULT '',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	decided_at    TIMESTAMPTZ
)`)
	return err
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := 0
	for _, c := range env(key, "") {
		if c < '0' || c > '9' {
			return fallback
		}
		value = value*10 + int(c-'0')
	}
	if value == 0 {
		return fallback
	}
	return value
}

func set(raw string) map[string]bool {
	out := map[string]bool{}
	for _, item := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out[trimmed] = true
		}
	}
	return out
}
