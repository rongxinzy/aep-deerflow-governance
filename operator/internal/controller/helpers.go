package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ovclient "github.com/rongxinzy/aep-deerflow-governance/operator/internal/openviking"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func (r *DigitalEmployeeReconciler) fail(ctx context.Context, employee *v1alpha1.DigitalEmployee, err error) error {
	log.FromContext(ctx).Error(err, "reconcile failed", "employee", employee.Name)
	if statusErr := r.setStatus(ctx, employee, v1alpha1.PhaseError, err.Error()); statusErr != nil {
		return statusErr
	}
	return err
}

func (r *DigitalEmployeeReconciler) setStatus(ctx context.Context, employee *v1alpha1.DigitalEmployee, phase v1alpha1.DigitalEmployeePhase, message string) error {
	if employee.Status.Phase == phase && employee.Status.Message == message && phase != v1alpha1.PhaseReady {
		return nil
	}
	employee.Status.Phase = phase
	employee.Status.Message = message
	return r.Status().Update(ctx, employee)
}

func (r *DigitalEmployeeReconciler) ovRootKey(ctx context.Context, employee *v1alpha1.DigitalEmployee) (string, error) {
	if r.OVRootFn == nil {
		return "", fmt.Errorf("no openviking root key source configured")
	}
	return r.OVRootFn(ctx, r.Client, employee.Namespace, employee.Spec.Credentials.Name)
}

func ovNew(baseURL, rootKey string) *ovclient.Client { return ovclient.New(baseURL, rootKey) }

func objectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace}
}

func errWrap(prefix string, err error) error { return fmt.Errorf("%s%w", prefix, err) }

func randomToken() string {
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

var _ client.Client = (client.Client)(nil)
var _ = corev1.Secret{}
