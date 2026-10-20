package controller

import (
	"context"
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
	// Governed asset tools: the middleware registers asset_list /
	// asset_ssh_exec iff both env vars are present. The subject is the
	// employee name — the same identifier the AEP asset catalog's
	// de_employee bindings key on.
	if spec.Assets != nil && spec.Assets.Enabled {
		environment = append(environment,
			corev1.EnvVar{Name: "ASSET_TOOLS_ENABLED", Value: "1"},
			corev1.EnvVar{Name: "ASSET_SUBJECT_ID", Value: employee.Name},
		)
	}
	// Governed multimodal model tools: platform-wide switchboard from the
	// operator env (chart values). Each capability registers only when its
	// catalog model id is set; per-call authorization is per-user (M2M
	// model token issued against the resolved requester).
	if r.ModelTools.GatewayURL != "" {
		environment = append(environment, corev1.EnvVar{Name: "MODEL_TOOLS_ENABLED", Value: "1"},
			corev1.EnvVar{Name: "MODEL_TOOLS_GATEWAY_URL", Value: r.ModelTools.GatewayURL})
		if r.ModelTools.Image != "" {
			environment = append(environment, corev1.EnvVar{Name: "MODEL_TOOLS_IMAGE", Value: r.ModelTools.Image})
		}
		if r.ModelTools.Speech != "" {
			environment = append(environment, corev1.EnvVar{Name: "MODEL_TOOLS_SPEECH", Value: r.ModelTools.Speech})
		}
		if r.ModelTools.Video != "" {
			environment = append(environment, corev1.EnvVar{Name: "MODEL_TOOLS_VIDEO", Value: r.ModelTools.Video})
		}
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
		Resources:       r.runtimeResources(employee),
		SecurityContext: r.runtimeSecurityContext(),
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
					Labels: podTemplateLabels(labels(employee), componentRuntime),
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
