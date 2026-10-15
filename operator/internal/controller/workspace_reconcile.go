// Child rendering for PersonalWorkspace: the companions manifest ConfigMap
// (the gateway's roster input and the rollout checksum source), the workspace
// volume, the gateway Deployment (P0: single pi-companion form), and the
// gateway Service.
package controller

import (
	"context"
	"encoding/json"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// modelAPIKeySecretKey is the provider key inside the platform credentials
// secret the workspace reuses (same key the standalone pi-companion
// deployment consumes; the AEP data-plane JWT path replaces it later).
const modelAPIKeySecretKey = "BIGMODEL_API_KEY"

// defaultOpenVikingBaseURL is the in-cluster shared OpenViking service. The
// rendered OPENVIKING_BASE_URL prefers the CR's spec.memory.serverURL (the
// portal always sets it); this constant covers hand-written CRs without a
// memory block.
const defaultOpenVikingBaseURL = "http://openviking.memory-system.svc.cluster.local:1933"

// companionEnvName renders PW_<UPPER_SNAKE(name)>_<KEY>: the same rule the
// workspace-agent config disk applies (workspace-agent/internal/render/
// config.go envName), so pod and microvm forms deliver identical
// per-companion env names.
func companionEnvName(companion, key string) string {
	up := strings.ToUpper(strings.ReplaceAll(companion, "-", "_"))
	return "PW_" + up + "_" + key
}

func (r *PersonalWorkspaceReconciler) ensureWorkspaceChildren(ctx context.Context, workspace *v1alpha1.PersonalWorkspace) error {
	// The manifest ConfigMap is shared by both runtimes: the pod form rolls
	// its Deployment via the checksum, the microvm node agent copies it
	// verbatim into the guest config disk (single roster source).
	if err := ensureWorkspaceManifest(ctx, r.Client, r.Scheme, workspace); err != nil {
		return err
	}

	if err := r.ensureWorkspacePVC(ctx, workspace); err != nil {
		return errWrap("workspace pvc: ", err)
	}

	deployment := r.renderWorkspaceDeployment(workspace, configChecksum(renderCompanionsManifest(workspace), ""))
	if err := controllerutil.SetControllerReference(workspace, deployment, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, deployment); err != nil {
		return errWrap("workspace deployment: ", err)
	}

	service := renderWorkspaceService(workspace)
	if err := controllerutil.SetControllerReference(workspace, service, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, service); err != nil {
		return errWrap("workspace service: ", err)
	}
	return nil
}

// ensureWorkspaceManifest upserts the companions.json ConfigMap (the
// gateway's roster input, the pod rollout checksum source, and the node
// agent's config-disk source for microvm workspaces).
func ensureWorkspaceManifest(ctx context.Context, c client.Client, scheme *runtime.Scheme, workspace *v1alpha1.PersonalWorkspace) error {
	manifest := renderCompanionsManifest(workspace)
	manifestMap := &corev1.ConfigMap{ObjectMeta: objectMeta(workspaceConfigName(workspace), workspace.Namespace)}
	manifestMap.Data = map[string]string{"companions.json": manifest}
	if err := controllerutil.SetControllerReference(workspace, manifestMap, scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, c, manifestMap); err != nil {
		return errWrap("companions config map: ", err)
	}
	return nil
}

// companionManifestEntry is one companion's wire form in companions.json:
// the gateway consumes this roster (routing key, model route, memory secret,
// sharing scopes) instead of watching the CR.
type companionManifestEntry struct {
	Name         string   `json:"name"`
	DisplayName  string   `json:"displayName,omitempty"`
	Model        string   `json:"model"`
	Persona      string   `json:"persona,omitempty"`
	Tools        []string `json:"tools,omitempty"`
	MemkeySecret string   `json:"memkeySecret,omitempty"`
	MemoryScopes []string `json:"memoryScopes,omitempty"`
	MemoryLimit  string   `json:"memoryLimit,omitempty"`
	CPULimit     string   `json:"cpuLimit,omitempty"`
}

type companionsManifest struct {
	PortalURL  string                   `json:"portalURL"`
	Owner      string                   `json:"owner"`
	Workspace  string                   `json:"workspace"`
	Companions []companionManifestEntry `json:"companions"`
}

// renderCompanionsManifest serializes the roster the gateway boots from.
// Companion add/remove/model edits change this payload, and its checksum
// rolls the Deployment — the manifest is the single source of truth.
func renderCompanionsManifest(workspace *v1alpha1.PersonalWorkspace) string {
	manifest := companionsManifest{
		PortalURL:  "http://de-portal." + workspace.Namespace + ".svc:8090",
		Owner:      workspace.Spec.Owner,
		Workspace:  workspace.Name,
		Companions: make([]companionManifestEntry, 0, len(workspace.Spec.Companions)),
	}
	for i := range workspace.Spec.Companions {
		c := &workspace.Spec.Companions[i]
		entry := companionManifestEntry{
			Name:         c.Name,
			DisplayName:  c.DisplayName,
			Model:        c.Model,
			Persona:      c.Persona,
			Tools:        c.Tools,
			MemkeySecret: companionMemkeySecretName(workspace, c.Name),
			MemoryLimit:  c.Resources.Memory,
			CPULimit:     c.Resources.CPU,
		}
		if c.Memory != nil {
			entry.MemoryScopes = c.Memory.Scopes
		}
		manifest.Companions = append(manifest.Companions, entry)
	}
	encoded, _ := json.MarshalIndent(manifest, "", "  ")
	return string(encoded)
}

// renderWorkspaceDeployment renders the P0 gateway: a single pi-companion
// container shaped by companions[0] (multi-instance supervision arrives with
// the workspace-gateway image; the manifest ConfigMap is already its input).
func (r *PersonalWorkspaceReconciler) renderWorkspaceDeployment(workspace *v1alpha1.PersonalWorkspace, checksum string) *appsv1.Deployment {
	spec := workspace.Spec
	image := spec.Image
	if image == "" {
		image = r.WorkspaceImage
	}
	modelBaseURL := r.ModelBaseURL
	if modelBaseURL == "" {
		modelBaseURL = defaultModelBaseURL
	}
	primary := spec.Companions[0]
	memoryLimit := spec.Resources.Memory
	if _, err := resource.ParseQuantity(memoryLimit); err != nil {
		memoryLimit = "512Mi"
	}
	replicas := int32(1)

	var envFrom []corev1.EnvFromSource
	// companions[0]'s memkey keeps riding envFrom unprefixed: the existing
	// single-companion runtime reads OPENVIKING_API_KEY directly, and the
	// per-companion env below is additive (the gateway maps PW_<NAME>_* down
	// to each child process).
	if name := companionMemkeySecretName(workspace, primary.Name); name != "" {
		envFrom = append(envFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}})
	}
	// Rooms employee channel: agent credentials envFrom + the AEP the rooms
	// agent logs in to. Both unset = channel stays off inside the gateway.
	if r.RoomsAgentSecret != "" {
		envFrom = append(envFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: r.RoomsAgentSecret}}})
	}
	var env []corev1.EnvVar
	if spec.Credentials.Name != "" {
		env = append(env, corev1.EnvVar{
			Name: "BENCH_API_KEY",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: spec.Credentials.Name},
				Key:                  modelAPIKeySecretKey,
			}},
		})
	}
	// Per-companion secrets: one PW_<NAME>_OPENVIKING_API_KEY and one
	// PW_<NAME>_WORKSPACE_INTERNAL_TOKEN per roster entry, each pointing at
	// that companion's memkey secret — the same delivery contract as the
	// microvm config disk. The token authenticates collaboration/chat calls
	// (companion server reads WORKSPACE_INTERNAL_TOKEN via the gateway's
	// PW_<NAME>_* mapping); without it non-primary companions collaborate
	// under no identity while the primary's unprefixed token leaks to every
	// child through process.env.
	for i := range spec.Companions {
		c := &spec.Companions[i]
		if name := companionMemkeySecretName(workspace, c.Name); name != "" {
			for _, key := range []string{workspaceMemkeyKey, workspaceTokenKey} {
				env = append(env, corev1.EnvVar{
					Name: companionEnvName(c.Name, key),
					ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: name},
						Key:                  key,
					}},
				})
			}
		}
	}
	// OPENVIKING_BASE_URL: companions dial OpenViking directly. Prefers the
	// CR's spec.memory.serverURL; falls back to the shared in-cluster service.
	memoryBaseURL := defaultOpenVikingBaseURL
	if spec.Memory != nil && spec.Memory.ServerURL != "" {
		memoryBaseURL = spec.Memory.ServerURL
	}
	env = append(env, corev1.EnvVar{Name: "OPENVIKING_BASE_URL", Value: memoryBaseURL})
	if r.RoomsAEPBaseURL != "" {
		env = append(env, corev1.EnvVar{Name: "AEP_BASE_URL", Value: r.RoomsAEPBaseURL})
	}
	env = append(env,
		corev1.EnvVar{Name: "PORT", Value: "8080"},
		corev1.EnvVar{Name: "COMPANION_STORAGE", Value: "/data/companion.sqlite"},
		corev1.EnvVar{Name: "BENCH_BASE_URL", Value: modelBaseURL},
		corev1.EnvVar{Name: "BENCH_MODEL", Value: primary.Model},
	)

	container := corev1.Container{
		Name:    "companion",
		Image:   image,
		Ports:   []corev1.ContainerPort{{ContainerPort: 8080, Name: "http"}},
		Env:     env,
		EnvFrom: envFrom,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/data"},
			{Name: "companions", MountPath: "/etc/workspace", ReadOnly: true},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)}},
			InitialDelaySeconds: 3,
			PeriodSeconds:       10,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:        corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)}},
			InitialDelaySeconds: 10,
			PeriodSeconds:       30,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse(memoryLimit),
			},
		},
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceDeploymentName(workspace), Namespace: workspace.Namespace, Labels: workspaceLabels(workspace)},
		Spec: appsv1.DeploymentSpec{
			// The workspace volume is RWO and the companion's SQLite is
			// single-writer: one replica, no rolling double-mount.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: workspaceLabels(workspace)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: workspaceLabels(workspace),
					Annotations: map[string]string{
						configHashAnnotation: checksum,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
					Volumes: []corev1.Volume{
						{Name: "data", VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: workspacePVCName(workspace)},
						}},
						{Name: "companions", VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: workspaceConfigName(workspace)}},
						}},
					},
				},
			},
		},
	}
}

func renderWorkspaceService(workspace *v1alpha1.PersonalWorkspace) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: workspaceServiceName(workspace), Namespace: workspace.Namespace, Labels: workspaceLabels(workspace)},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: workspaceLabels(workspace),
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080), Name: "http"}},
		},
	}
}

// ensureWorkspacePVC creates the workspace volume when absent; existing
// claims are immutable in practice (same discipline as the employee home).
func (r *PersonalWorkspaceReconciler) ensureWorkspacePVC(ctx context.Context, workspace *v1alpha1.PersonalWorkspace) error {
	size := workspace.Spec.Resources.Disk
	if _, err := resource.ParseQuantity(size); err != nil {
		size = "2Gi"
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: objectMeta(workspacePVCName(workspace), workspace.Namespace),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	if err := controllerutil.SetControllerReference(workspace, claim, r.Scheme); err != nil {
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

func workspaceDeploymentName(workspace *v1alpha1.PersonalWorkspace) string {
	return "pw-" + workspace.Name
}
func workspaceServiceName(workspace *v1alpha1.PersonalWorkspace) string {
	return "pw-" + workspace.Name
}
func workspaceConfigName(workspace *v1alpha1.PersonalWorkspace) string {
	return "pw-" + workspace.Name + "-companions"
}
func workspacePVCName(workspace *v1alpha1.PersonalWorkspace) string {
	return "pw-" + workspace.Name + "-data"
}

func workspaceLabels(workspace *v1alpha1.PersonalWorkspace) map[string]string {
	return map[string]string{"app": "pw-" + workspace.Name, "digitalemployee.zhiyuan.io/workspace": workspace.Name}
}
