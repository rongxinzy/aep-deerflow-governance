package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestParseRuntimePodDefaults(t *testing.T) {
	if got, err := ParseRuntimeResources("  "); err != nil || got != nil {
		t.Fatalf("empty resources: got %v, %v", got, err)
	}
	if got, err := ParseRuntimeSecurityContext(""); err != nil || got != nil {
		t.Fatalf("empty securityContext: got %v, %v", got, err)
	}
	if _, err := ParseRuntimeResources("{not json"); err == nil {
		t.Fatal("malformed resources must fail")
	}
	if _, err := ParseRuntimeSecurityContext("[1]"); err == nil {
		t.Fatal("malformed securityContext must fail")
	}
	resources, err := ParseRuntimeResources(`{"requests":{"cpu":"250m","memory":"512Mi"},"limits":{"memory":"2Gi"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !resources.Requests.Cpu().Equal(resource.MustParse("250m")) || !resources.Limits.Memory().Equal(resource.MustParse("2Gi")) {
		t.Fatalf("unexpected resources %v", resources)
	}
	securityContext, err := ParseRuntimeSecurityContext(`{"allowPrivilegeEscalation":false,"runAsNonRoot":true,"runAsUser":1000,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if securityContext.AllowPrivilegeEscalation == nil || *securityContext.AllowPrivilegeEscalation {
		t.Fatal("allowPrivilegeEscalation not decoded")
	}
}

func TestRenderDeploymentRuntimePodDefaults(t *testing.T) {
	employee := fullEmployee()
	employee.Spec.Resources = nil

	legacy := &DigitalEmployeeReconciler{}
	deployment, err := legacy.renderDeployment(employee, "h")
	if err != nil {
		t.Fatal(err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if len(container.Resources.Requests) != 0 || len(container.Resources.Limits) != 0 || container.SecurityContext != nil {
		t.Fatalf("no defaults must keep the legacy shape, got %+v %+v", container.Resources, container.SecurityContext)
	}

	defaults, _ := ParseRuntimeResources(`{"requests":{"cpu":"250m","memory":"512Mi"},"limits":{"memory":"2Gi"}}`)
	securityContext, _ := ParseRuntimeSecurityContext(`{"allowPrivilegeEscalation":false,"runAsNonRoot":true,"runAsUser":1000,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}`)
	hardened := &DigitalEmployeeReconciler{
		RuntimeDefaultResources: defaults,
		RuntimeSecurityContext:  securityContext,
		ConversationAdminDBURL:  "postgres://admin:pw@postgres:5432/postgres",
		ConversationInitImage:   "postgres:17.6-alpine",
	}
	deployment, err = hardened.renderDeployment(employee, "h")
	if err != nil {
		t.Fatal(err)
	}
	container = deployment.Spec.Template.Spec.Containers[0]
	if !container.Resources.Requests.Memory().Equal(resource.MustParse("512Mi")) {
		t.Fatalf("operator default not applied: %+v", container.Resources)
	}
	if container.SecurityContext == nil || container.SecurityContext.Capabilities.Drop[0] != "ALL" || container.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("securityContext not applied: %+v", container.SecurityContext)
	}
	// The default must be copied, never aliased into rendered objects.
	container.SecurityContext.Capabilities.Drop[0] = "MUTATED"
	if securityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatal("securityContext default aliased into the rendered pod")
	}
	inits := deployment.Spec.Template.Spec.InitContainers
	if len(inits) != 1 || inits[0].SecurityContext == nil {
		t.Fatalf("db-init must be hardened: %+v", inits)
	}
	if inits[0].SecurityContext.RunAsNonRoot != nil || inits[0].SecurityContext.RunAsUser != nil {
		t.Fatal("db-init must not inherit the runtime identity fields")
	}
	if inits[0].SecurityContext.AllowPrivilegeEscalation == nil || *inits[0].SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("db-init must keep privilege-escalation hardening")
	}

	override := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}}
	employee.Spec.Resources = &override
	deployment, err = hardened.renderDeployment(employee, "h")
	if err != nil {
		t.Fatal(err)
	}
	got := deployment.Spec.Template.Spec.Containers[0].Resources
	if !got.Requests.Cpu().Equal(resource.MustParse("1")) || len(got.Limits) != 0 {
		t.Fatalf("CR override must replace the default wholesale: %+v", got)
	}
}

func TestPodTemplateComponentLabels(t *testing.T) {
	employee := fullEmployee()
	reconciler := &DigitalEmployeeReconciler{}
	deployment, err := reconciler.renderDeployment(employee, "h")
	if err != nil {
		t.Fatal(err)
	}
	if got := deployment.Spec.Template.Labels[componentLabel]; got != componentRuntime {
		t.Fatalf("runtime pod template component = %q", got)
	}
	if _, ok := deployment.Spec.Selector.MatchLabels[componentLabel]; ok {
		t.Fatal("component label must stay out of the immutable selector")
	}
	for key, value := range deployment.Spec.Selector.MatchLabels {
		if deployment.Spec.Template.Labels[key] != value {
			t.Fatalf("selector label %s not on the pod template", key)
		}
	}
}

func TestA2APodTemplateComponentLabel(t *testing.T) {
	deployment := renderA2ADeployment(fullEmployee())
	if got := deployment.Spec.Template.Labels[componentLabel]; got != componentA2A {
		t.Fatalf("a2a pod template component = %q", got)
	}
	if _, ok := deployment.Spec.Selector.MatchLabels[componentLabel]; ok {
		t.Fatal("component label must stay out of the a2a selector")
	}
}
