// Package controller reconciles DigitalEmployee resources into the child
// workloads (Deployment, ConfigMaps, Secret, Service) that run one governed
// runtime instance.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func (r *DigitalEmployeeReconciler) ensureChildren(ctx context.Context, employee *v1alpha1.DigitalEmployee) error {
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configName(employee), Namespace: employee.Namespace}}
	configMap.Data = map[string]string{"config.yaml": renderConfig(employee)}
	if err := controllerutil.SetControllerReference(employee, configMap, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, configMap); err != nil {
		return fmt.Errorf("config map: %w", err)
	}

	extensions := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: extensionsName(employee), Namespace: employee.Namespace}}
	extensions.Data = map[string]string{"extensions_config.json": renderExtensions(employee)}
	if err := controllerutil.SetControllerReference(employee, extensions, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, extensions); err != nil {
		return fmt.Errorf("extensions config map: %w", err)
	}

	deployment := renderDeployment(employee)
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
	case *corev1.ConfigMap:
		previous := existing.(*corev1.ConfigMap)
		if reflect.DeepEqual(previous.Data, typed.Data) {
			return nil
		}
		previous.Data = typed.Data
		return c.Update(ctx, previous)
	case *appsv1.Deployment:
		previous := existing.(*appsv1.Deployment)
		if reflect.DeepEqual(previous.Spec.Template.Spec, typed.Spec.Template.Spec) {
			return nil
		}
		previous.Spec.Template.Spec = typed.Spec.Template.Spec
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
	}
	return fmt.Errorf("unsupported type %T", object)
}

// renderConfig produces the DeerFlow config.yaml (JSON form) from the CR.
func renderConfig(employee *v1alpha1.DigitalEmployee) string {
	spec := employee.Spec
	modelBase := spec.Runtime.ModelBaseURL
	if modelBase == "" {
		modelBase = defaultModelBaseURL
	}
	config := map[string]any{
		"config_version": 46,
		"log_level":      "info",
		"models": []any{map[string]any{
			"name":            spec.Runtime.Model,
			"display_name":    spec.Runtime.Model,
			"use":             "langchain_openai:ChatOpenAI",
			"model":           spec.Runtime.Model,
			"api_key":         "$DEER_BENCH_API_KEY",
			"base_url":        modelBase,
			"request_timeout": 300.0,
			"max_retries":     2,
		}},
		"sandbox": map[string]any{
			"use":             "deerflow.sandbox.local:LocalSandboxProvider",
			"allow_host_bash": false,
		},
		"subagents": map[string]any{"max_total_per_run": 6},
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
	// IM channels: the channel-connections feature gate plus the enabled
	// providers. Only enabled providers are listed (the runtime config
	// schema defaults every provider to disabled). Credentials never live
	// here — they are injected at runtime through the gateway's admin
	// channel-connections API and persisted in the runtime's own store.
	if spec.Channels != nil && (spec.Channels.Wecom || spec.Channels.Feishu) {
		providers := map[string]any{}
		if spec.Channels.Wecom {
			providers["wecom"] = map[string]any{"enabled": true}
		}
		if spec.Channels.Feishu {
			providers["feishu"] = map[string]any{"enabled": true}
		}
		config["channel_connections"] = map[string]any{"enabled": true}
		for _, name := range []string{"feishu", "wecom"} { // deterministic order
			if entry, ok := providers[name]; ok {
				config["channel_connections"].(map[string]any)[name] = entry
			}
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
		config["memory"] = map[string]any{
			"enabled":                        true,
			"injection_enabled":              true,
			"shutdown_flush_timeout_seconds": 30,
			"manager_class":                  "openviking",
			"mode":                           "middleware",
			"backend_config": map[string]any{
				"base_url":               spec.Memory.ServerURL,
				"owner_user_id":          employee.Name,
				"single_tenant_instance": spec.Memory.SingleTenantInstance,
				"api_key_env":            memkeySecretKey,
				"timeout_seconds":        30,
				"default_peer_id":        employee.Name,
				"startup_policy":         "fail_fast",
				"allow_insecure_http":    true,
				"failure_policy":         map[string]any{"read": "fail_open", "write": "log_and_drop"},
				"retrieval":              retrieval,
			},
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

func renderDeployment(employee *v1alpha1.DigitalEmployee) *appsv1.Deployment {
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
	var secretRefs []corev1.LocalObjectReference
	if spec.Credentials.Name != "" {
		secretRefs = append(secretRefs, corev1.LocalObjectReference{Name: spec.Credentials.Name})
	}
	if spec.Memory != nil {
		secretRefs = append(secretRefs, corev1.LocalObjectReference{Name: memkeySecretName(employee)})
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
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels(employee)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels(employee)},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
					Volumes: []corev1.Volume{
						{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: configName(employee)}}}},
						{Name: "extensions", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: extensionsName(employee)}}}},
						{Name: "home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
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
