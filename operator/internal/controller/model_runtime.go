package controller

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

var runtimeWireModel = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Empty preserves identity-delivery-only/legacy operation. The gateway address
// is trusted operator configuration, never supplied by an employee or a caller.
func validModelGatewayURL(value string) bool {
	if value == "" {
		return true
	}
	u, err := url.Parse(value)
	return err == nil && strings.TrimSpace(value) == value && (u.Scheme == "http" || u.Scheme == "https") &&
		u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && !strings.Contains(value, "#") &&
		(u.Path == "/v1" || u.Path == "/v1/") && u.RawPath == ""
}

func (r *DigitalEmployeeReconciler) nativeModelRuntime(employee *v1alpha1.DigitalEmployee) bool {
	return r.ModelGatewayURL != "" && employee.Spec.ModelAccess != nil
}

func (r *DigitalEmployeeReconciler) validateModelRuntime(employee *v1alpha1.DigitalEmployee) error {
	if !r.nativeModelRuntime(employee) {
		return nil
	}
	if !validModelGatewayURL(r.ModelGatewayURL) || r.ModelTokenAudience == "" {
		return fmt.Errorf("native model runtime requires a trusted /v1 gateway URL and model audience")
	}
	if r.ModelTools.GatewayURL != "" {
		return fmt.Errorf("native model runtime cannot enable legacy per-user model tools")
	}
	routes := runtimeModelRoutes(employee.Spec.Runtime)
	if len(routes) > 512 || employee.Spec.Runtime.ApiKeyEnv != "" {
		return fmt.Errorf("native model runtime rejects static credentials or oversized model lists")
	}
	seen := map[string]bool{}
	for _, route := range routes {
		if !runtimeWireModel.MatchString(route.Name) || seen[route.Name] || route.Provider != "" && route.Provider != "openai" ||
			route.ApiKeyEnv != "" || route.NativeModel != "" && route.NativeModel != route.Name ||
			route.BaseURL != "" && strings.TrimSuffix(route.BaseURL, "/") != strings.TrimSuffix(r.ModelGatewayURL, "/") {
			return fmt.Errorf("native model runtime requires unique AEP model IDs, OpenAI-compatible routes and no credential or destination overrides")
		}
		seen[route.Name] = true
	}
	return nil
}

func runtimeModelRoutes(spec v1alpha1.RuntimeSpec) []v1alpha1.RuntimeModel {
	if len(spec.Models) > 0 {
		return spec.Models
	}
	return []v1alpha1.RuntimeModel{{Name: spec.Model, Provider: spec.Provider, NativeModel: spec.NativeModel,
		BaseURL: spec.ModelBaseURL, ApiKeyEnv: spec.ApiKeyEnv}}
}

// Called only after prepareModelIdentity has read the live, controller-owned
// ServiceAccount. Keep selection in the existing runtime model list; selection
// is not authorization. DB role-derived grants are still enforced at the gateway.
func (r *DigitalEmployeeReconciler) configureModelRuntime(employee *v1alpha1.DigitalEmployee, config map[string]any) error {
	if !r.nativeModelRuntime(employee) {
		return nil
	}
	if err := r.validateModelRuntime(employee); err != nil {
		return err
	}
	i := employee.Status.ModelIdentity
	if i == nil || i.ObservedGeneration != employee.Generation || i.AgentID != employee.Spec.ModelAccess.AgentID ||
		i.DeploymentID != employee.Spec.Governance.DeploymentID || i.EmployeeUID != employee.UID ||
		i.ServiceAccountName == "" || i.ServiceAccountUID == "" || i.Audience != r.ModelTokenAudience {
		return fmt.Errorf("native model runtime requires fresh owned workload identity")
	}
	models, ok := config["models"].([]any)
	if !ok || len(models) != len(runtimeModelRoutes(employee.Spec.Runtime)) {
		return fmt.Errorf("native model runtime received invalid model configuration")
	}
	key := modelRuntimeBinding(employee)
	for _, model := range models {
		entry, ok := model.(map[string]any)
		if !ok {
			return fmt.Errorf("native model runtime received invalid model entry")
		}
		delete(entry, "api_key")
		entry["use"] = "deerflow.models.token_file:TokenFileChatOpenAI"
		entry["model"] = entry["name"] // Always the AEP wire ID, never a provider-native alias.
		entry["base_url"] = r.ModelGatewayURL
		entry["api_key_file"] = modelTokenDirectory + "/token"
		entry["default_headers"] = map[string]string{"x-aep-access-binding": key}
	}
	return nil
}
