package controller

import (
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// ParseRuntimeResources decodes the operator-wide default compute envelope
// for runtime containers (RUNTIME_DEFAULT_RESOURCES, a JSON-encoded
// corev1.ResourceRequirements). Empty input means "no default" — the
// rendered container keeps the legacy no-requests/no-limits shape.
func ParseRuntimeResources(raw string) (*corev1.ResourceRequirements, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var resources corev1.ResourceRequirements
	if err := json.Unmarshal([]byte(raw), &resources); err != nil {
		return nil, fmt.Errorf("RUNTIME_DEFAULT_RESOURCES: %w", err)
	}
	return &resources, nil
}

// ParseRuntimeSecurityContext decodes the operator-wide container
// securityContext applied to runtime containers (RUNTIME_SECURITY_CONTEXT,
// a JSON-encoded corev1.SecurityContext). Empty input keeps the legacy
// behaviour (no securityContext).
func ParseRuntimeSecurityContext(raw string) (*corev1.SecurityContext, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var securityContext corev1.SecurityContext
	if err := json.Unmarshal([]byte(raw), &securityContext); err != nil {
		return nil, fmt.Errorf("RUNTIME_SECURITY_CONTEXT: %w", err)
	}
	return &securityContext, nil
}

// runtimeResources picks the CR override first, then the operator default.
func (r *DigitalEmployeeReconciler) runtimeResources(employee *v1alpha1.DigitalEmployee) corev1.ResourceRequirements {
	if employee.Spec.Resources != nil {
		return *employee.Spec.Resources.DeepCopy()
	}
	if r.RuntimeDefaultResources != nil {
		return *r.RuntimeDefaultResources.DeepCopy()
	}
	return corev1.ResourceRequirements{}
}

// runtimeSecurityContext returns a private copy of the operator default.
func (r *DigitalEmployeeReconciler) runtimeSecurityContext() *corev1.SecurityContext {
	if r.RuntimeSecurityContext == nil {
		return nil
	}
	return r.RuntimeSecurityContext.DeepCopy()
}

// initSecurityContext is the runtime default minus the identity fields:
// the db-init container runs a stock psql image whose user the runtime
// default (e.g. runAsNonRoot for a non-root runtime build) does not
// describe. Capability, privilege-escalation and seccomp hardening apply.
func (r *DigitalEmployeeReconciler) initSecurityContext() *corev1.SecurityContext {
	securityContext := r.runtimeSecurityContext()
	if securityContext == nil {
		return nil
	}
	securityContext.RunAsNonRoot = nil
	securityContext.RunAsUser = nil
	securityContext.RunAsGroup = nil
	return securityContext
}

// componentLabel marks what an employee pod is, for NetworkPolicy
// selectors (the chart isolates runtimes from each other and from AEP
// /internal; a2a adapters are the sanctioned way to reach a runtime).
// It rides on the pod template only: Deployment selectors are immutable,
// so it must never join labels()/a2aLabels().
const (
	componentLabel   = "digitalemployee.zhiyuan.io/component"
	componentRuntime = "runtime"
	componentA2A     = "a2a"
)

func podTemplateLabels(selector map[string]string, component string) map[string]string {
	merged := make(map[string]string, len(selector)+1)
	for key, value := range selector {
		merged[key] = value
	}
	merged[componentLabel] = component
	return merged
}
