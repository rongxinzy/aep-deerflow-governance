// Package controller reconciles DigitalEmployee resources into the child
// workloads (Deployment, ConfigMaps, Secret, Service) that run one governed
// runtime instance.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func (r *DigitalEmployeeReconciler) ensureChildren(ctx context.Context, employee *v1alpha1.DigitalEmployee) error {
	// Conversation store gate, before any child is rendered: backend=postgres
	// must resolve to a per-employee database the operator can render a DSN
	// for. The silent platform-wide fallback is gone — a shared database
	// meant shared auth tables.
	database, err := conversationDatabaseName(employee)
	if err != nil {
		return fmt.Errorf("conversation store: %w", err)
	}
	if database != "" {
		if r.ConversationAdminDBURL == "" {
			return fmt.Errorf("conversation store: backend=postgres needs CONVERSATION_ADMIN_DB_URL to render the per-employee DSN for %s", database)
		}
		if err := r.ensureConversationSecret(ctx, employee, database); err != nil {
			return fmt.Errorf("conversation db: %w", err)
		}
	}

	configPayload := renderConfig(employee, conversationURLKey(employee))
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configName(employee), Namespace: employee.Namespace}}
	configMap.Data = map[string]string{"config.yaml": configPayload}
	if err := controllerutil.SetControllerReference(employee, configMap, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, configMap); err != nil {
		return fmt.Errorf("config map: %w", err)
	}

	extensionsPayload := renderExtensions(employee)
	extensions := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: extensionsName(employee), Namespace: employee.Namespace}}
	extensions.Data = map[string]string{"extensions_config.json": extensionsPayload}
	if err := controllerutil.SetControllerReference(employee, extensions, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, extensions); err != nil {
		return fmt.Errorf("extensions config map: %w", err)
	}

	if employee.Spec.Persistence.Enabled {
		if err := r.ensurePVC(ctx, employee); err != nil {
			return fmt.Errorf("home pvc: %w", err)
		}
	}

	deployment, err := r.renderDeployment(employee, configChecksum(configPayload, extensionsPayload))
	if err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(employee, deployment, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, deployment); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}

	service := renderService(employee)
	if err := controllerutil.SetControllerReference(employee, service, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, service); err != nil {
		return fmt.Errorf("service: %w", err)
	}

	if employee.Spec.A2A != nil {
		a2aDeployment := renderA2ADeployment(employee)
		if err := controllerutil.SetControllerReference(employee, a2aDeployment, r.Scheme); err != nil {
			return err
		}
		if err := safeCreateOrUpdate(ctx, r.Client, a2aDeployment); err != nil {
			return fmt.Errorf("a2a deployment: %w", err)
		}
		a2aService := renderA2AService(employee)
		if err := controllerutil.SetControllerReference(employee, a2aService, r.Scheme); err != nil {
			return err
		}
		if err := safeCreateOrUpdate(ctx, r.Client, a2aService); err != nil {
			return fmt.Errorf("a2a service: %w", err)
		}
	}

	if err := r.ensureHibernation(ctx, employee); err != nil {
		return fmt.Errorf("hibernation: %w", err)
	}
	return nil
}

// safeCreateOrUpdate upserts by fetching, comparing the rendered payload,
// and updating only on drift (avoids hot loops from last-applied churn).
func safeCreateOrUpdate(ctx context.Context, c client.Client, object client.Object) error {
	existing := object.DeepCopyObject().(client.Object)
	err := c.Get(ctx, client.ObjectKeyFromObject(object), existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, object)
	}
	if err != nil {
		return err
	}
	switch typed := object.(type) {
	case *corev1.Secret:
		previous := existing.(*corev1.Secret)
		if reflect.DeepEqual(previous.Data, typed.Data) {
			return nil
		}
		previous.Data = typed.Data
		return c.Update(ctx, previous)
	case *corev1.ConfigMap:
		previous := existing.(*corev1.ConfigMap)
		if reflect.DeepEqual(previous.Data, typed.Data) {
			return nil
		}
		previous.Data = typed.Data
		return c.Update(ctx, previous)
	case *appsv1.Deployment:
		previous := existing.(*appsv1.Deployment)
		// The pod template annotations carry the config checksum: comparing
		// them too is what turns a rendered-config change into a rollout
		// (the runtime loads config at boot; a hot-updated ConfigMap alone
		// would leave the old policy live forever).
		if reflect.DeepEqual(previous.Spec.Template.Spec, typed.Spec.Template.Spec) &&
			reflect.DeepEqual(previous.Spec.Template.Annotations, typed.Spec.Template.Annotations) {
			return nil
		}
		previous.Spec.Template.Spec = typed.Spec.Template.Spec
		previous.Spec.Template.Annotations = typed.Spec.Template.Annotations
		return c.Update(ctx, previous)
	case *corev1.Service:
		previous := existing.(*corev1.Service)
		if reflect.DeepEqual(previous.Spec.Ports, typed.Spec.Ports) && previous.Spec.Type == typed.Spec.Type {
			return nil
		}
		// ClusterIP is immutable: keep whatever is assigned.
		previous.Spec.Type = typed.Spec.Type
		previous.Spec.Ports = typed.Spec.Ports
		return c.Update(ctx, previous)
	case *KedaScaledObject:
		previous := existing.(*KedaScaledObject)
		if reflect.DeepEqual(previous.Spec, typed.Spec) {
			return nil
		}
		previous.Spec = typed.Spec
		return c.Update(ctx, previous)
	case *KedaTriggerAuthentication:
		previous := existing.(*KedaTriggerAuthentication)
		if reflect.DeepEqual(previous.Spec, typed.Spec) {
			return nil
		}
		previous.Spec = typed.Spec
		return c.Update(ctx, previous)
	}
	return fmt.Errorf("unsupported type %T", object)
}

// renderConfig produces the DeerFlow config.yaml (JSON form) from the CR.
// databaseConfig renders the DeerFlow database section for the selected
// conversation store backend.
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

func renderConfig(employee *v1alpha1.DigitalEmployee, conversationURLKey string) string {
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
	if spec.Channels != nil && (spec.Channels.Wecom || spec.Channels.Feishu || spec.Channels.Wechat) {
		providers := map[string]any{}
		if spec.Channels.Wechat {
			providers["wechat"] = map[string]any{"enabled": true}
		}
		if spec.Channels.Wecom {
			providers["wecom"] = map[string]any{"enabled": true}
		}
		if spec.Channels.Feishu {
			providers["feishu"] = map[string]any{"enabled": true}
		}
		// require_bound_identity=false: identity resolution is the platform
		// governance middleware's job (AEP identity mappings + guest mode),
		// not upstream's per-user browser binding (which would gate every
		// unbound IM sender behind a "connect from DeerFlow Settings" reply).
		config["channel_connections"] = map[string]any{"enabled": true, "require_bound_identity": false}
		for _, name := range []string{"feishu", "wecom", "wechat"} { // deterministic order
			if entry, ok := providers[name]; ok {
				config["channel_connections"].(map[string]any)[name] = entry
			}
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
// keeps it stable across reconciles.
func configChecksum(configPayload, extensionsPayload string) string {
	digest := sha256.Sum256([]byte(configPayload + "\x00" + extensionsPayload))
	return hex.EncodeToString(digest[:])[:16]
}

func (r *DigitalEmployeeReconciler) renderDeployment(employee *v1alpha1.DigitalEmployee, checksum string) (*appsv1.Deployment, error) {
	spec := employee.Spec
	replicas := spec.Replicas
	if replicas <= 0 {
		replicas = 1
	}
	// No DEER_FLOW_AUTH_DISABLED: authentication is ON. The access path is
	// the per-employee internal token (DEER_FLOW_INTERNAL_AUTH_TOKEN arrives
	// via envFrom from the memkey secret); unauthenticated callers get 401.
	environment := []corev1.EnvVar{
		{Name: "DEER_FLOW_CONFIG_PATH", Value: "/app/config.yaml"},
		{Name: "DEER_FLOW_EXTENSIONS_CONFIG_PATH", Value: "/app/extensions-config/extensions_config.json"},
		{Name: "DEER_FLOW_HOME", Value: "/var/lib/deerflow"},
	}
	// The runtime entrypoint syncs its venv from the config's extras; the
	// postgres conversation backend needs asyncpg & co. resynced or the
	// lifespan dies with ImportError on boot (observed on cicd v18.1).
	if spec.ConversationStore.Backend == "postgres" {
		environment = append(environment, corev1.EnvVar{Name: "UV_EXTRAS", Value: "postgres"})
	}
	// Channel-gateway mode: the platform websockets live in
	// de-channel-gateway; runtimes poll its inbox instead of dialing out,
	// which is what makes idle scale-to-zero safe for IM employees.
	if r.ChannelGatewayURL != "" {
		environment = append(environment,
			corev1.EnvVar{Name: "DEERFLOW_CHANNEL_GATEWAY_URL", Value: r.ChannelGatewayURL},
			corev1.EnvVar{Name: "DEERFLOW_EMPLOYEE_NAME", Value: employee.Name},
		)
	}
	var secretRefs []corev1.LocalObjectReference
	if spec.Credentials.Name != "" {
		secretRefs = append(secretRefs, corev1.LocalObjectReference{Name: spec.Credentials.Name})
	}
	if spec.Memory != nil {
		secretRefs = append(secretRefs, corev1.LocalObjectReference{Name: memkeySecretName(employee)})
	}
	// Employee mailbox(es): the secret carries EMAIL_MAILBOXES (JSON with
	// credentials); envFrom makes it runtime env, where the governance
	// middleware registers the email tools iff it is present.
	if spec.Email != nil && spec.Email.SecretName != "" {
		secretRefs = append(secretRefs, corev1.LocalObjectReference{Name: spec.Email.SecretName})
	}
	// Per-employee conversation DSN rides in its own child Secret (a key
	// distinct from any platform-wide DSN, so envFrom order is irrelevant).
	convDatabase, err := conversationDatabaseName(employee)
	if err != nil {
		return nil, err
	}
	if convDatabase != "" {
		secretRefs = append(secretRefs, corev1.LocalObjectReference{Name: conversationSecretName(employee)})
	}
	container := corev1.Container{
		Name:  "runtime",
		Image: spec.Runtime.Image,
		Ports: []corev1.ContainerPort{{ContainerPort: 8001}},
		Env:   environment,
		EnvFrom: func() []corev1.EnvFromSource {
			sources := make([]corev1.EnvFromSource, 0, len(secretRefs))
			for i := range secretRefs {
				sources = append(sources, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: secretRefs[i]}})
			}
			return sources
		}(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "config", MountPath: "/app/config.yaml", SubPath: "config.yaml"},
			{Name: "extensions", MountPath: "/app/extensions-config"},
			{Name: "home", MountPath: "/var/lib/deerflow"},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstrFromInt(8001)}},
			InitialDelaySeconds: 15,
			PeriodSeconds:       10,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstrFromInt(8001)}},
			InitialDelaySeconds: 40,
			PeriodSeconds:       30,
		},
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: deploymentName(employee), Namespace: employee.Namespace, Labels: labels(employee)},
		Spec: appsv1.DeploymentSpec{
			// The home volume is RWO when persistent: a rolling update would
			// try to double-mount it. Single-replica Recreate is the honest
			// strategy either way.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels(employee)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels(employee),
					Annotations: map[string]string{
						configHashAnnotation: checksum,
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: func() []corev1.Container {
						if init := r.conversationInitContainer(employee, convDatabase); init != nil {
							return []corev1.Container{*init}
						}
						return nil
					}(),
					Containers: []corev1.Container{container},
					Volumes:    volumes(employee),
				},
			},
		},
	}, nil
}

func renderService(employee *v1alpha1.DigitalEmployee) *corev1.Service {
	serviceType := corev1.ServiceTypeClusterIP
	var ports []corev1.ServicePort
	if employee.Spec.NodePort > 0 {
		serviceType = corev1.ServiceTypeNodePort
		ports = []corev1.ServicePort{{Port: 8001, TargetPort: intstrFromInt(8001), NodePort: employee.Spec.NodePort, Name: "http"}}
	} else {
		ports = []corev1.ServicePort{{Port: 8001, TargetPort: intstrFromInt(8001), Name: "http"}}
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceName(employee), Namespace: employee.Namespace, Labels: labels(employee)},
		Spec: corev1.ServiceSpec{
			Type:     serviceType,
			Selector: labels(employee),
			Ports:    ports,
		},
	}
}

func deploymentName(employee *v1alpha1.DigitalEmployee) string { return "de-" + employee.Name }
func serviceName(employee *v1alpha1.DigitalEmployee) string    { return "de-" + employee.Name }
func configName(employee *v1alpha1.DigitalEmployee) string     { return "de-" + employee.Name + "-config" }

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

func extensionsName(employee *v1alpha1.DigitalEmployee) string {
	return "de-" + employee.Name + "-extensions"
}
func memkeySecretName(employee *v1alpha1.DigitalEmployee) string {
	return "de-" + employee.Name + "-memkey"
}

func intstrFromInt(v int32) intstr.IntOrString { return intstr.FromInt32(v) }

func int32String(v int32) string { return strconv.FormatInt(int64(v), 10) }

func labels(employee *v1alpha1.DigitalEmployee) map[string]string {
	return map[string]string{"app": "de-" + employee.Name, "digitalemployee.zhiyuan.io/name": employee.Name}
}

// volumes renders the pod volumes: config/extensions from ConfigMaps, and
// the runtime home from a PVC when persistence is enabled (threads, channel
// credentials, and the auth DB survive restarts) or an EmptyDir otherwise.
func volumes(employee *v1alpha1.DigitalEmployee) []corev1.Volume {
	home := corev1.Volume{Name: "home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	if employee.Spec.Persistence.Enabled {
		home = corev1.Volume{Name: "home", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: homePVCName(employee)},
		}}
	}
	return []corev1.Volume{
		{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configName(employee)}}}},
		{Name: "extensions", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: extensionsName(employee)}}}},
		home,
	}
}

func homePVCName(employee *v1alpha1.DigitalEmployee) string { return "de-" + employee.Name + "-home" }

func renderPVC(employee *v1alpha1.DigitalEmployee) *corev1.PersistentVolumeClaim {
	size := employee.Spec.Persistence.Size
	if _, err := resource.ParseQuantity(size); err != nil {
		size = "2Gi"
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: objectMeta(homePVCName(employee), employee.Namespace),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
}

// ensurePVC creates the home claim when absent. Existing claims are left
// untouched: PVC specs are largely immutable, and shrinking an in-use claim
// would only produce a reconcile error loop.
func (r *DigitalEmployeeReconciler) ensurePVC(ctx context.Context, employee *v1alpha1.DigitalEmployee) error {
	claim := renderPVC(employee)
	if err := controllerutil.SetControllerReference(employee, claim, r.Scheme); err != nil {
		return err
	}
	existing := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(claim), existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}
	return r.Create(ctx, claim)
}
