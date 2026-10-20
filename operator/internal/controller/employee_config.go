package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// renderModelEntry shapes one route for the runtime config: the wire name
// (Name) stays stable for agents and threads; provider/nativeModel/apiKeyEnv
// shape the actual client behind it.
func renderModelEntry(route v1alpha1.RuntimeModel) map[string]any {
	base := route.BaseURL
	if base == "" {
		base = defaultModelBaseURL
	}
	entry := map[string]any{
		"name":         route.Name,
		"display_name": route.Name,
		"model":        route.Name,
		"api_key":      "$DEER_BENCH_API_KEY",
		"base_url":     base,
	}
	if route.NativeModel != "" {
		entry["model"] = route.NativeModel
	}
	if route.ApiKeyEnv != "" {
		entry["api_key"] = "$" + route.ApiKeyEnv
	}
	// ChatAnthropic's constructor takes `timeout`; ChatOpenAI's is
	// `request_timeout`.
	if route.Provider == "anthropic" {
		entry["use"] = "langchain_anthropic:ChatAnthropic"
		entry["timeout"] = 300.0
	} else {
		entry["use"] = "langchain_openai:ChatOpenAI"
		entry["request_timeout"] = 300.0
	}
	entry["max_retries"] = 2
	// A declared reasoning block keeps DeerFlow from downgrading thinking
	// models to non-thinking (it infers support only from config).
	if route.Reasoning != nil {
		reasoning := map[string]any{}
		if route.Reasoning.Thinking != "" {
			reasoning["thinking"] = route.Reasoning.Thinking
		}
		if route.Reasoning.History != "" {
			reasoning["history"] = route.Reasoning.History
		}
		if len(reasoning) > 0 {
			entry["reasoning"] = reasoning
		}
	}
	return entry
}

func databaseConfigWithURL(backend, conversationURLKey string) map[string]any {
	if backend == "postgres" {
		return map[string]any{
			"backend":      "postgres",
			"postgres_url": conversationURLKey,
		}
	}
	// Default: per-employee SQLite on the home volume.
	return map[string]any{"sqlite_dir": "/var/lib/deerflow/data"}
}

// databaseConfig keeps a backend-only shape for unit tests.
func databaseConfig(backend string) map[string]any {
	urlKey := ""
	if backend == "postgres" {
		urlKey = "$" + conversationDBKey
	}
	return databaseConfigWithURL(backend, urlKey)
}

// conversationURLKey resolves the postgres_url config value: backend=postgres
// always rides the per-employee child Secret DSN (ensureChildren rejects
// employees it cannot render one for); sqlite ignores the value.
func conversationURLKey(employee *v1alpha1.DigitalEmployee) string {
	if employee.Spec.ConversationStore.Backend == "postgres" {
		return "$" + conversationDBKey
	}
	return ""
}

// renderConfigData builds the runtime config the employee's ConfigMap carries.
// The pod-template fingerprint hashes a channel-free view of this same map
// (fingerprintPayload), so both readings share one builder.
func renderConfigData(employee *v1alpha1.DigitalEmployee, conversationURLKey string) map[string]any {
	spec := employee.Spec
	// The runtime picks models by name; models[0] is the default. Legacy
	// singular fields synthesize a one-entry list so existing CRs render
	// unchanged.
	routes := spec.Runtime.Models
	if len(routes) == 0 {
		routes = []v1alpha1.RuntimeModel{{
			Name:        spec.Runtime.Model,
			Provider:    spec.Runtime.Provider,
			NativeModel: spec.Runtime.NativeModel,
			BaseURL:     spec.Runtime.ModelBaseURL,
			ApiKeyEnv:   spec.Runtime.ApiKeyEnv,
		}}
	}
	models := make([]any, 0, len(routes))
	for _, route := range routes {
		models = append(models, renderModelEntry(route))
	}
	config := map[string]any{
		"config_version": 51,
		"log_level":      "info",
		"lead_identity": map[string]any{
			"deployment_id":   spec.Governance.DeploymentID,
			"deployment_name": spec.Governance.DeploymentName,
			"employee_name":   spec.DisplayName,
			"purpose":         spec.Description,
			"team_id":         spec.Team,
			"team_name":       spec.TeamName,
			"owner_id":        spec.Owner,
			"owner_name":      spec.OwnerName,
		},
		"models": models,
		"sandbox": map[string]any{
			"use":             "deerflow.sandbox.local:LocalSandboxProvider",
			"allow_host_bash": false,
		},
		// Threads/checkpointer DB: centralized PostgreSQL enables
		// multi-replica employees and cross-employee queries; per-employee
		// SQLite on the home volume remains the default (single-writer).
		// backend=postgres always provisions one database per employee
		// (DE_CONVERSATION_DB_URL from a child Secret).
		"database": databaseConfigWithURL(spec.ConversationStore.Backend, conversationURLKey),
		// Custom agents (delegation targets) default to disabled upstream;
		// without this every get_agent/list_agents tool call 403s and the
		// agent reports "智能体服务连续调用失败" instead of answering.
		"agents_api": map[string]any{"enabled": true},
		"subagents":  map[string]any{"max_total_per_run": 6},
		"extensions": map[string]any{
			"middlewares": []any{map[string]any{
				"class": "aep_deerflow_governance:AepGovernanceMiddleware",
				"kwargs": map[string]any{
					"aep_base_url":       spec.Governance.AEPBaseURL,
					"deployment_id":      spec.Governance.DeploymentID,
					"service_username":   "$GOV_SERVICE_USERNAME",
					"service_password":   "$GOV_SERVICE_PASSWORD",
					"default_aep_user":   spec.Governance.DefaultAEPUser,
					"identity_source_id": spec.Governance.IdentitySource,
					"governed_tools":     spec.Governance.GovernedTools,
					"team_names":         spec.Governance.TeamNames,
				},
			}},
		},
	}
	// Knowledge-base whitelist: an explicit policy (Bases non-nil, even
	// empty) is rendered into the governance middleware, which denies
	// knowledge_search calls outside the list ({} denies every search).
	// A nil Bases renders no policy kwarg: the middleware treats that as a
	// configuration error and default-denies every knowledge tool —
	// migration, inventory and rollback: docs/design/knowledge-whitelist-default-deny.md.
	if spec.Knowledge != nil && spec.Knowledge.Bases != nil {
		kwargs := config["extensions"].(map[string]any)["middlewares"].([]any)[0].(map[string]any)["kwargs"].(map[string]any)
		bases := map[string]any{}
		for _, base := range spec.Knowledge.Bases {
			bases[base.ID] = base.Name
		}
		kwargs["knowledge_bases"] = bases
		kwargs["knowledge_online_authorization"] = true
	}
	// IM channels: the channel-connections feature gate plus the enabled
	// providers. Only enabled providers are listed (the runtime config
	// schema defaults every provider to disabled). Credentials never live
	// here — they are injected at runtime through the gateway's admin
	// channel-connections API and persisted in the runtime's own store.
	if spec.Channels != nil && spec.Channels.WecomName != "" {
		kwargs := config["extensions"].(map[string]any)["middlewares"].([]any)[0].(map[string]any)["kwargs"].(map[string]any)
		kwargs["im_bot_name"] = spec.Channels.WecomName
	}
	// The IM channel subsystem itself is always rendered: enabling or disabling
	// one provider is applied on the live runtime (the control service calls PUT
	// /api/channels/{provider}/enabled), so the service has to be available from
	// the employee's first boot — otherwise the very first channel would still
	// need a rollout. The per-provider entries below only keep the declared ones
	// on, because the runtime config schema defaults every provider to disabled.
	providers := map[string]any{}
	if spec.Channels != nil {
		if spec.Channels.Wechat {
			providers["wechat"] = map[string]any{"enabled": true}
		}
		if spec.Channels.Wecom {
			providers["wecom"] = map[string]any{"enabled": true}
		}
		if spec.Channels.Feishu {
			providers["feishu"] = map[string]any{"enabled": true}
		}
	}
	// require_bound_identity=false: identity resolution is the platform
	// governance middleware's job (AEP identity mappings + guest mode),
	// not upstream's per-user browser binding (which would gate every
	// unbound IM sender behind a "connect from DeerFlow Settings" reply).
	config[channelConnectionsKey] = map[string]any{"enabled": true, "require_bound_identity": false}
	for _, name := range []string{"feishu", "wecom", "wechat"} { // deterministic order
		if entry, ok := providers[name]; ok {
			config[channelConnectionsKey].(map[string]any)[name] = entry
		}
	}
	// Declarative channel keys the runtime cannot take from the runtime store:
	// the Feishu group-gate display name, and endpoint overrides that point a
	// channel at a protocol-level mock or a self-hosted gateway instead of the
	// public platform. Only these keys are rendered — `enabled` and the
	// credentials still come from the runtime store, so a CR without injected
	// credentials does not make the channel try to start.
	if spec.Channels != nil {
		if spec.Channels.Feishu {
			feishuKeys := map[string]any{}
			if spec.Channels.FeishuName != "" {
				feishuKeys["bot_name"] = spec.Channels.FeishuName
			}
			if spec.Channels.FeishuDomain != "" {
				feishuKeys["domain"] = spec.Channels.FeishuDomain
			}
			if len(feishuKeys) > 0 {
				mergeChannelConfig(config, "feishu", feishuKeys)
			}
		}
		if spec.Channels.Wecom && spec.Channels.WecomWSURL != "" {
			mergeChannelConfig(config, "wecom", map[string]any{"ws_url": spec.Channels.WecomWSURL})
		}
	}
	if spec.Memory != nil {
		retrieval := map[string]any{}
		if spec.Memory.TopK > 0 {
			retrieval["top_k"] = spec.Memory.TopK
		}
		if spec.Memory.ScoreThreshold > 0 {
			retrieval["score_threshold"] = spec.Memory.ScoreThreshold
		}
		if spec.Memory.MaxInjectionChars > 0 {
			retrieval["max_injection_chars"] = spec.Memory.MaxInjectionChars
		}
		if len(retrieval) == 0 {
			retrieval = map[string]any{"top_k": 8, "score_threshold": 0.25, "max_injection_chars": 12000}
		}
		scope := spec.Memory.MemoryScope
		if scope == "" {
			scope = "instance"
		}
		backendConfig := map[string]any{
			"base_url":               spec.Memory.ServerURL,
			"owner_user_id":          employee.Name,
			"single_tenant_instance": spec.Memory.SingleTenantInstance,
			"api_key_env":            memkeySecretKey,
			"timeout_seconds":        30,
			"default_peer_id":        employee.Name,
			"memory_scope":           scope,
			"startup_policy":         "fail_fast",
			"allow_insecure_http":    true,
			"failure_policy":         map[string]any{"read": "fail_open", "write": "log_and_drop"},
			"retrieval":              retrieval,
		}
		// shared_peer_id is only valid in requester scope; the backend rejects
		// it otherwise, and a config that fails to parse crash-loops the pod.
		if scope == "requester" && spec.Memory.SharedPeerID != "" {
			backendConfig["shared_peer_id"] = spec.Memory.SharedPeerID
		}
		config["memory"] = map[string]any{
			"enabled":                        true,
			"injection_enabled":              true,
			"shutdown_flush_timeout_seconds": 30,
			"manager_class":                  "openviking",
			"mode":                           "middleware",
			"backend_config":                 backendConfig,
		}
	}
	return config
}

// marshalConfig is the one encoder for the rendered config: map-key-sorted
// MarshalIndent, so equal maps always produce equal payloads.
func marshalConfig(config map[string]any) string {
	encoded, _ := json.MarshalIndent(config, "", "  ")
	return string(encoded)
}

func renderExtensions(employee *v1alpha1.DigitalEmployee) string {
	servers := map[string]any{}
	if employee.Spec.Knowledge != nil {
		servers["weknora"] = map[string]any{
			"enabled": true,
			"type":    "http",
			"url":     employee.Spec.Knowledge.MCPURL,
			// Whole-string $VAR resolution only (no "Bearer $VAR" prefixes).
			"headers": map[string]any{"X-MCP-Auth-Token": "$WEKNORA_MCP_BEARER"},
		}
	}
	encoded, _ := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	return string(encoded)
}

// configHashAnnotation keys the rendered-config checksum on the pod
// template. A change rolls the deployment (Recreate) so the runtime boots
// with the new governance kwargs — without it, PATCHed knowledge
// whitelists would only ever apply to freshly created employees.
const configHashAnnotation = "zhiyuan.io/config-hash"

// configChecksum fingerprints both rendered payloads; map-key-sorted JSON
// keeps it stable across reconciles. Callers hand in the payload they want
// fingerprinted: employee_reconcile drops the channel block first (see
// fingerprintPayload), workspace_reconcile hashes its manifest as is.
func configChecksum(configPayload, extensionsPayload string) string {
	digest := sha256.Sum256([]byte(configPayload + "\x00" + extensionsPayload))
	return hex.EncodeToString(digest[:])[:16]
}

// channelConnectionsKey is the config key renderConfigData renders the IM
// channel block under (feature gate plus the enabled providers).
const channelConnectionsKey = "channel_connections"

// fingerprintPayload renders the config the pod-template fingerprint covers:
// the ConfigMap payload minus the channel block.
//
// Channel enable/disable is a hot operation on the runtime itself — the
// portal flips the CR booleans as intent/audit and applies them through the
// runtime's admin API (PUT /api/channels/{provider}/enabled). Fingerprinting
// the block instead rolled the whole employee on every toggle (Deployment
// Recreate), so conversations, memory and knowledge went unreachable for a
// change the runtime takes live.
//
// The rendered payload itself keeps channel_connections: a booting runtime
// reads it as its startup default. Every other key is fingerprinted exactly
// as rendered, and a channel-free employee hashes the very payload it did
// before. That includes the declarative channel keys the runtime can only
// read at boot — the `channels` block mergeChannelConfig writes (bot_name,
// domain, ws_url) and the middleware's im_bot_name: editing those still rolls
// the employee so the edit takes effect.
func fingerprintPayload(config map[string]any) string {
	fingerprinted := make(map[string]any, len(config))
	for key, value := range config {
		fingerprinted[key] = value
	}
	delete(fingerprinted, channelConnectionsKey)
	return marshalConfig(fingerprinted)
}

// mergeChannelConfig merges declarative keys into config["channels"][provider],
// creating the maps when the config has none yet.
func mergeChannelConfig(config map[string]any, provider string, values map[string]any) {
	channels, ok := config["channels"].(map[string]any)
	if !ok {
		channels = map[string]any{}
		config["channels"] = channels
	}
	entry, ok := channels[provider].(map[string]any)
	if !ok {
		entry = map[string]any{}
		channels[provider] = entry
	}
	for key, value := range values {
		entry[key] = value
	}
}
