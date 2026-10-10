// de-portal: the business layer for digital employees. It is the only
// writer of DigitalEmployee CRs (besides platform admins) and owns the
// policy functions the operator deliberately does not: who may apply,
// quotas, and the approval workflow. End users never touch Kubernetes.
package main

import (
	"context"
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
	addr         string
	aepBaseURL   string
	policyMode   string
	adminUsers   map[string]bool
	adminRoles   map[string]bool
	quotaPerUser int
	namespace    string

	runtimeImage      string
	model             string
	gatewayBaseURL    string
	modelKeyEnv       string
	memoryServer      string
	memoryAccount     string
	knowledgeMCPURL   string
	knowledgeAPIURL   string
	knowledgeUIURL    string // browser-facing WeKnora URL override; default derives per request
	credentialsSecret string
	a2aImage          string

	managedKnowledge managedKnowledgeConfig

	// Web-chat entry: session cookie TTL, the AEP deployment id used by the
	// /chat login form, and where the reused DeerFlow frontend is served.
	sessionTTL      time.Duration
	aepDeploymentID string
	chatUIURL       string

	dbURL string

	// Hibernation: how long handleChat waits for a scaled-to-zero employee
	// to come back before giving up (the browser shows "waking up").
	wakeTimeout time.Duration

	// Model failover: the watcher interval, how long a classified model
	// failure must persist before an employee is moved to the first healthy
	// fallback, and how long the original must be healthy before it is
	// restored. A non-positive interval disables the watcher; an empty
	// deployment fallback chain makes every cycle a no-op.
	failoverInterval     time.Duration
	failoverMinUnhealthy time.Duration
	failoverMinHealthy   time.Duration
}

type managedKnowledgeConfig struct {
	tenantID          string
	apiKeySecretField string
	embeddingModelID  string
}

func loadConfig() config {
	return config{
		addr:       env("PORTAL_ADDR", ":8090"),
		aepBaseURL: strings.TrimRight(env("PORTAL_AEP_BASE_URL", "http://aep-control-service.aep-system.svc:8080"), "/"),
		policyMode: env("PORTAL_POLICY_MODE", "approval"),
		adminUsers: set(env("PORTAL_ADMIN_USERS", "")),
		adminRoles: set(env("PORTAL_ADMIN_ROLES", "admin,platform-admin")),
		// 0 is a meaningful value here (unlimited), which envInt rejects as
		// unset — use the non-negative parser.
		quotaPerUser: envIntNonNegative("PORTAL_QUOTA_PER_USER", 2),
		namespace:    env("PORTAL_NAMESPACE", "digital-employees"),

		runtimeImage:      env("PORTAL_RUNTIME_IMAGE", "172.18.5.251:8081/zhiyuan/deerflow-gov:v10"),
		model:             env("PORTAL_MODEL", "bench-glm"),
		gatewayBaseURL:    env("PORTAL_GATEWAY_BASE_URL", "http://higress-gateway.higress-system.svc.cluster.local"),
		modelKeyEnv:       env("PORTAL_MODEL_KEY_ENV", "BIGMODEL_API_KEY"),
		memoryServer:      env("PORTAL_MEMORY_SERVER", "http://openviking.memory-system.svc.cluster.local:1933"),
		memoryAccount:     env("PORTAL_MEMORY_ACCOUNT", "zhiyuan"),
		knowledgeMCPURL:   env("PORTAL_KNOWLEDGE_MCP_URL", "http://weknora-mcp.weknora.svc.cluster.local:8830/mcp"),
		knowledgeAPIURL:   env("PORTAL_KNOWLEDGE_API_URL", "http://app.weknora.svc.cluster.local:8080"),
		knowledgeUIURL:    env("PORTAL_KNOWLEDGE_UI_URL", ""),
		credentialsSecret: env("PORTAL_CREDENTIALS_SECRET", "de-platform-creds"),
		a2aImage:          env("PORTAL_A2A_IMAGE", "172.18.5.251:8081/zhiyuan/a2a-adapter:0.1.0"),

		managedKnowledge: managedKnowledgeConfig{
			tenantID:          env("PORTAL_KNOWLEDGE_TENANT_ID", ""),
			apiKeySecretField: env("PORTAL_KNOWLEDGE_API_KEY_SECRET_FIELD", "WEKNORA_KNOWLEDGE_API_KEY"),
			embeddingModelID:  env("PORTAL_KNOWLEDGE_EMBEDDING_MODEL_ID", ""),
		},

		sessionTTL:      time.Duration(envInt("PORTAL_SESSION_TTL_MINUTES", 720)) * time.Minute,
		aepDeploymentID: env("PORTAL_AEP_DEPLOYMENT_ID", "demo"),
		chatUIURL:       env("PORTAL_CHAT_UI_URL", ""),

		dbURL: env("PORTAL_DB_URL", ""),

		wakeTimeout: time.Duration(envInt("PORTAL_WAKE_TIMEOUT_SECONDS", 60)) * time.Second,

		failoverInterval:     time.Duration(envIntNonNegative("PORTAL_FAILOVER_INTERVAL_SECONDS", 60)) * time.Second,
		failoverMinUnhealthy: time.Duration(envInt("PORTAL_FAILOVER_MIN_UNHEALTHY_SECONDS", 300)) * time.Second,
		failoverMinHealthy:   time.Duration(envInt("PORTAL_FAILOVER_MIN_HEALTHY_SECONDS", 1200)) * time.Second,
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
		cfg:      cfg,
		db:       db,
		k8s:      k8sClient,
		aep:      &aepAuthenticator{baseURL: cfg.aepBaseURL, http: &http.Client{Timeout: 10 * time.Second}},
		logger:   logger,
		sessions: newSessionStoreWithDB(cfg.sessionTTL, db),
	}

	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           portal.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go portal.runModelFailover(context.Background())
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
);
CREATE TABLE IF NOT EXISTS sessions (
	token         TEXT PRIMARY KEY,
	identity_json TEXT NOT NULL,
	csrf          TEXT NOT NULL,
	expires_at    TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS channel_wake (
	id         BIGSERIAL PRIMARY KEY,
	employee   TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	consumed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS channel_wake_pending ON channel_wake (employee) WHERE consumed_at IS NULL;
CREATE TABLE IF NOT EXISTS employee_activity (
	employee    TEXT PRIMARY KEY,
	last_active TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS knowledge_base_registry (
	deployment_id TEXT NOT NULL,
	weknora_tenant_id TEXT NOT NULL,
	knowledge_base_id TEXT NOT NULL,
	name_snapshot TEXT NOT NULL,
	created_by TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (deployment_id, weknora_tenant_id, knowledge_base_id)
);
CREATE TABLE IF NOT EXISTS knowledge_base_grants (
	deployment_id TEXT NOT NULL,
	weknora_tenant_id TEXT NOT NULL,
	knowledge_base_id TEXT NOT NULL,
	subject_type TEXT NOT NULL CHECK (subject_type IN ('user','team')),
	subject_id TEXT NOT NULL,
	granted_by TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (deployment_id, weknora_tenant_id, knowledge_base_id, subject_type, subject_id),
	FOREIGN KEY (deployment_id, weknora_tenant_id, knowledge_base_id)
		REFERENCES knowledge_base_registry(deployment_id, weknora_tenant_id, knowledge_base_id)
		ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS knowledge_runtime_contexts (
	handle_hash TEXT PRIMARY KEY,
	session_token TEXT NOT NULL,
	employee_name TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS knowledge_runtime_contexts_expiry ON knowledge_runtime_contexts (expires_at);
CREATE TABLE IF NOT EXISTS knowledge_audit_events (
	id BIGSERIAL PRIMARY KEY,
	operation_id TEXT NOT NULL UNIQUE,
	deployment_id TEXT NOT NULL,
	weknora_tenant_id TEXT NOT NULL,
	actor_user_id TEXT NOT NULL,
	action TEXT NOT NULL,
	resource_type TEXT NOT NULL,
	resource_id TEXT NOT NULL DEFAULT '',
	parent_resource_id TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL,
	status_code INTEGER NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE knowledge_audit_events ADD COLUMN IF NOT EXISTS parent_resource_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS knowledge_audit_events_deployment_created ON knowledge_audit_events (deployment_id, created_at DESC)`)
	return err
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := envIntNonNegative(key, fallback)
	if value == 0 {
		return fallback
	}
	return value
}

// envIntNonNegative parses an integer env var where 0 is a meaningful
// setting (e.g. PORTAL_QUOTA_PER_USER=0 disables the quota). Unset or
// malformed values still fall back.
func envIntNonNegative(key string, fallback int) int {
	raw := strings.TrimSpace(env(key, ""))
	if raw == "" {
		return fallback
	}
	value := 0
	for _, c := range raw {
		if c < '0' || c > '9' {
			return fallback
		}
		value = value*10 + int(c-'0')
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
