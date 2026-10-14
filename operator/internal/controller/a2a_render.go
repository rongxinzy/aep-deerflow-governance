package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// renderA2ADeployment builds the employee's dedicated A2A adapter instance.
func renderA2ADeployment(employee *v1alpha1.DigitalEmployee) *appsv1.Deployment {
	spec := employee.Spec.A2A
	maxDepth := spec.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 3
	}
	replicas := int32(1)
	environment := []corev1.EnvVar{
		{Name: "DEERFLOW_URL", Value: "http://" + serviceName(employee) + ":8001"},
		{Name: "AEP_BASE_URL", Value: employee.Spec.Governance.AEPBaseURL},
		{Name: "A2A_PORT", Value: "8102"},
		{Name: "A2A_SELF_NAME", Value: employee.Name},
		{Name: "A2A_PUBLIC_URL", Value: "http://" + a2aServiceName(employee) + ":8102"},
		{Name: "A2A_MAX_DEPTH", Value: int32String(maxDepth)},
	}
	// The adapter calls the employee's runtime with the same per-employee
	// internal token the runtime itself gets (envFrom the memkey secret):
	// authentication is ON in production, and the internal header also exempts
	// the sidecar from the CSRF double-submit check.
	container := corev1.Container{
		Name:  "a2a",
		Image: spec.Image,
		Ports: []corev1.ContainerPort{{ContainerPort: 8102}},
		EnvFrom: []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: memkeySecretName(employee)}},
		}},
		Env: environment,
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstrFromInt(8102)}},
			InitialDelaySeconds: 3,
			PeriodSeconds:       10,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstrFromInt(8102)}},
			InitialDelaySeconds: 10,
			PeriodSeconds:       30,
		},
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: a2aDeploymentName(employee), Namespace: employee.Namespace, Labels: a2aLabels(employee)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: a2aLabels(employee)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: a2aLabels(employee)},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{container}},
			},
		},
	}
}

func renderA2AService(employee *v1alpha1.DigitalEmployee) *corev1.Service {
	serviceType := corev1.ServiceTypeClusterIP
	ports := []corev1.ServicePort{{Port: 8102, TargetPort: intstrFromInt(8102), Name: "http"}}
	if employee.Spec.A2A.NodePort > 0 {
		serviceType = corev1.ServiceTypeNodePort
		ports[0].NodePort = employee.Spec.A2A.NodePort
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: a2aServiceName(employee), Namespace: employee.Namespace, Labels: a2aLabels(employee)},
		Spec: corev1.ServiceSpec{
			Type:     serviceType,
			Selector: a2aLabels(employee),
			Ports:    ports,
		},
	}
}

func a2aDeploymentName(employee *v1alpha1.DigitalEmployee) string {
	return "de-" + employee.Name + "-a2a"
}
func a2aServiceName(employee *v1alpha1.DigitalEmployee) string { return "de-" + employee.Name + "-a2a" }

func a2aLabels(employee *v1alpha1.DigitalEmployee) map[string]string {
	return map[string]string{"app": "de-" + employee.Name + "-a2a", "digitalemployee.zhiyuan.io/name": employee.Name}
}
