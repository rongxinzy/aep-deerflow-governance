package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

const (
	modelTokenVolume          = "model-access-token"
	modelTokenDirectory       = "/var/run/secrets/zhiyuan/model-access"
	modelAccountUIDAnnotation = "digitalemployee.zhiyuan.io/model-service-account-uid"
)

// Empty disables identity delivery; an explicit audience is trusted operator
// configuration, never an employee-supplied model authorization policy.
func validModelTokenAudience(audience string) bool {
	return len(audience) <= 256 && strings.IndexFunc(audience, unicode.IsSpace) == -1 && strings.IndexFunc(audience, unicode.IsControl) == -1
}

func (r *DigitalEmployeeReconciler) validateModelIdentityMapping(employee *v1alpha1.DigitalEmployee) error {
	if employee.Spec.ModelAccess == nil {
		return nil
	}
	id, deployment := employee.Spec.ModelAccess.AgentID, employee.Spec.Governance.DeploymentID
	valid := func(value string, max int) bool {
		return value != "" && len(value) <= max && strings.IndexFunc(value, unicode.IsSpace) == -1 && strings.IndexFunc(value, unicode.IsControl) == -1
	}
	if !valid(id, 256) || !valid(deployment, 80) || r.ModelTokenAudience == "" || !validModelTokenAudience(r.ModelTokenAudience) {
		return fmt.Errorf("explicit employee agent identity requires a valid deployment and configured model audience")
	}
	return nil
}

// Include the immutable CR UID, not its reusable name or its human owner. No
// credentials participate in names. The 160-bit digest also fits a DNS label.
func modelAccountName(employee *v1alpha1.DigitalEmployee) string {
	digest := sha256.Sum256([]byte(employee.Namespace + "\x00" + string(employee.UID)))
	return "de-model-" + hex.EncodeToString(digest[:20])
}

func (r *DigitalEmployeeReconciler) ensureModelAccount(ctx context.Context, employee *v1alpha1.DigitalEmployee) (*corev1.ServiceAccount, error) {
	sa := &corev1.ServiceAccount{ObjectMeta: objectMeta(modelAccountName(employee), employee.Namespace)}
	key := client.ObjectKeyFromObject(sa)
	if err := r.Get(ctx, key, sa); apierrors.IsNotFound(err) {
		sa.AutomountServiceAccountToken = ptr.To(false)
		if err := controllerutil.SetControllerReference(employee, sa, r.Scheme); err != nil {
			return nil, err
		}
		// On an AlreadyExists race requeue rather than adopting the winner.
		if err := r.Create(ctx, sa); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	owner := metav1.GetControllerOf(sa)
	if sa.UID == "" || !sa.DeletionTimestamp.IsZero() || owner == nil || owner.UID != employee.UID ||
		owner.Name != employee.Name || owner.Kind != "DigitalEmployee" || owner.APIVersion != v1alpha1.GroupVersion.String() {
		return nil, fmt.Errorf("model ServiceAccount ownership or lifecycle mismatch")
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		sa.AutomountServiceAccountToken = ptr.To(false)
		if err := r.Update(ctx, sa); err != nil {
			return nil, err
		}
	}
	return sa, nil
}

// Identity delivery only: gateway access requires the trusted agent/employee/SA
// UID mapping and native verifier, which this helper deliberately does not fake.
func (r *DigitalEmployeeReconciler) prepareModelIdentity(ctx context.Context, employee *v1alpha1.DigitalEmployee, deployment *appsv1.Deployment) error {
	employee.Status.ModelIdentity = nil
	if err := r.validateModelIdentityMapping(employee); err != nil {
		return err
	}
	if r.ModelTokenAudience == "" {
		return nil
	}
	if !validModelTokenAudience(r.ModelTokenAudience) || employee.UID == "" || !employee.DeletionTimestamp.IsZero() {
		return fmt.Errorf("invalid employee model workload identity configuration")
	}
	sa, err := r.ensureModelAccount(ctx, employee)
	if err != nil {
		return err
	}
	pod := &deployment.Spec.Template.Spec
	pod.ServiceAccountName = sa.Name
	pod.AutomountServiceAccountToken = ptr.To(false)
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	// Recreated SAs invalidate the old identity. Roll the runtime so kubelet
	// supplies a token bound to the replacement SA instead of an old UID.
	deployment.Spec.Template.Annotations[modelAccountUIDAnnotation] = string(sa.UID)
	pod.Volumes = append(pod.Volumes, corev1.Volume{Name: modelTokenVolume, VolumeSource: corev1.VolumeSource{
		Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(0444)), Sources: []corev1.VolumeProjection{{
			ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience: r.ModelTokenAudience, ExpirationSeconds: ptr.To(int64(3600)), Path: "token",
			},
		}}},
	}})
	for i := range pod.Containers {
		container := &pod.Containers[i]
		if container.Name != "runtime" {
			continue
		}
		// No subPath: kubelet must be able to atomically rotate the file.
		// Init and A2A containers do not receive this mount.
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: modelTokenVolume, MountPath: modelTokenDirectory, ReadOnly: true})
		container.Env = append(container.Env, corev1.EnvVar{Name: "ZHIYUAN_MODEL_TOKEN_FILE", Value: modelTokenDirectory + "/token"})
	}
	if employee.Spec.ModelAccess != nil {
		employee.Status.ModelIdentity = &v1alpha1.ModelIdentityStatus{
			ObservedGeneration: employee.Generation, AgentID: employee.Spec.ModelAccess.AgentID,
			DeploymentID: employee.Spec.Governance.DeploymentID, EmployeeUID: employee.UID,
			ServiceAccountName: sa.Name, ServiceAccountUID: sa.UID, Audience: r.ModelTokenAudience,
		}
	}
	return nil
}
