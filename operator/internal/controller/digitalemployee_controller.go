// Package controller reconciles DigitalEmployee resources into the child
// workloads (Deployment, ConfigMaps, Secret, Service) that run one governed
// runtime instance.
package controller

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

const (
	// memkeySecretKey is the OpenViking USER key handed to the runtime.
	memkeySecretKey = "OPENVIKING_API_KEY"
	// internalTokenKey is the per-employee gateway internal-auth token.
	internalTokenKey = "DEER_FLOW_INTERNAL_AUTH_TOKEN"
	// memoryFinalizer guards the OpenViking user lifecycle.
	memoryFinalizer = "digitalemployee.zhiyuan.io/memory-user"

	// defaultModelBaseURL is the in-cluster Higress model gateway.
	defaultModelBaseURL = "http://higress-gateway.higress-system.svc.cluster.local/v1"
)

// DigitalEmployeeReconciler reconciles a DigitalEmployee object.
type DigitalEmployeeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	OVRootFn func(ctx context.Context, c client.Client, namespace, secretName string) (string, error)
	// RuntimeURLFn overrides how the runtime auth API base URL is computed
	// (specs inject an httptest server). Nil → the in-cluster service DNS.
	RuntimeURLFn func(namespace, name string) string
}

// +kubebuilder:rbac:groups=digitalemployee.zhiyuan.io,resources=digitalemployees,verbs=*
// +kubebuilder:rbac:groups=digitalemployee.zhiyuan.io,resources=digitalemployees/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=digitalemployee.zhiyuan.io,resources=digitalemployees/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=*
// +kubebuilder:rbac:groups=core,resources=configmaps;secrets;services;pods;events;persistentvolumeclaims,verbs=*

// Reconcile is part of the main kubernetes reconciliation loop.
func (r *DigitalEmployeeReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var employee v1alpha1.DigitalEmployee
	if err := r.Get(ctx, request.NamespacedName, &employee); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !employee.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &employee)
	}

	// 1. Memory user + key + internal access token (idempotent; persisted
	// in a child Secret — the key is minted once, the token never rotates).
	if err := r.ensureMemorySecret(ctx, &employee); err != nil {
		return ctrl.Result{}, r.fail(ctx, &employee, err)
	}

	// 2. Child resources.
	if err := r.ensureChildren(ctx, &employee); err != nil {
		return ctrl.Result{}, r.fail(ctx, &employee, err)
	}

	// 3. Wait for availability.
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: deploymentName(&employee)}, deployment); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, r.fail(ctx, &employee, err)
	}
	available := deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.ReadyReplicas == *deployment.Spec.Replicas
	if !available {
		if err := r.setStatus(ctx, &employee, v1alpha1.PhasePending, ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	employee.Status.Phase = v1alpha1.PhaseReady
	employee.Status.ObservedGeneration = employee.Generation
	employee.Status.Message = ""

	// 4. Runtime account bootstrap (instance admin + owner PAT). The HTTP
	// API is up by now (deployment available); outcomes land in
	// status.runtimeAccount and the requeue merges into one return.
	account, err := r.ensureRuntimeAccount(ctx, &employee)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, &employee, err)
	}
	if err := r.Status().Update(ctx, &employee); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("digital employee ready", "replicas", deployment.Status.ReadyReplicas, "runtimeAccount", employee.Status.RuntimeAccount)
	return account, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *DigitalEmployeeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.DigitalEmployee{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Named("digitalemployee").
		Complete(r)
}

// ensureMemorySecret registers an OpenViking user for the employee and
// persists its key plus the gateway internal-auth token. Existing values
// are authoritative: the key is minted once, the token never rotates.
func (r *DigitalEmployeeReconciler) ensureMemorySecret(ctx context.Context, employee *v1alpha1.DigitalEmployee) error {
	if employee.Spec.Memory == nil {
		return nil
	}
	// Memory user cleanup needs the OpenViking admin API after the CR is
	// gone, so guard the whole lifecycle with a finalizer.
	if !controllerutil.ContainsFinalizer(employee, memoryFinalizer) {
		controllerutil.AddFinalizer(employee, memoryFinalizer)
		if err := r.Update(ctx, employee); err != nil {
			return err
		}
	}
	name := memkeySecretName(employee)
	existing := &corev1.Secret{ObjectMeta: objectMeta(name, employee.Namespace)}
	if err := r.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil && !errors.IsNotFound(err) {
		return err
	}
	haveMemoryKey := len(existing.Data[memkeySecretKey]) > 0
	haveToken := len(existing.Data[internalTokenKey]) > 0
	if haveMemoryKey && haveToken {
		employee.Status.MemoryUser = employee.Name
		return nil
	}

	rootKey, err := r.ovRootKey(ctx, employee)
	if err != nil {
		return errWrap("openviking root key: ", err)
	}
	ov := ovNew(employee.Spec.Memory.ServerURL, rootKey)
	// Department-scoped memory: when spec.team is set, the employee's
	// memories live in the department's OV account (provisioned by the
	// portal's department orchestration), not the shared default.
	accountID := employee.Spec.Memory.AccountID
	if employee.Spec.Team != "" {
		accountID = "tenant-" + employee.Spec.Team
	}
	key := string(existing.Data[memkeySecretKey])
	if !haveMemoryKey {
		minted, found, err := ov.RegisterUser(ctx, accountID, employee.Name)
		if err != nil {
			return errWrap("register memory user: ", err)
		}
		if found || minted == "" {
			minted, err = ov.RegenerateKey(ctx, accountID, employee.Name)
			if err != nil {
				return errWrap("regenerate memory user key: ", err)
			}
		}
		key = minted
	}
	token := string(existing.Data[internalTokenKey])
	if !haveToken {
		token = randomToken()
	}
	// Data (not StringData): the fake client used by the specs does not
	// convert StringData to Data the way the API server does, and reading
	// existing.Data back is the operator's own idempotency check — write
	// the canonical form directly.
	secret := &corev1.Secret{
		ObjectMeta: objectMeta(name, employee.Namespace),
		Data:       map[string][]byte{memkeySecretKey: []byte(key), internalTokenKey: []byte(token)},
	}
	if err := controllerutil.SetControllerReference(employee, secret, r.Scheme); err != nil {
		return err
	}
	switch err := r.Create(ctx, secret); {
	case err == nil:
	case errors.IsAlreadyExists(err):
		existing.Data = secret.Data
		if err := r.Update(ctx, existing); err != nil {
			return errWrap("persist memory credentials: ", err)
		}
	default:
		return errWrap("persist memory credentials: ", err)
	}
	employee.Status.MemoryUser = employee.Name
	return nil
}

func (r *DigitalEmployeeReconciler) finalize(ctx context.Context, employee *v1alpha1.DigitalEmployee) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(employee, memoryFinalizer) {
		return ctrl.Result{}, nil
	}
	if employee.Spec.Memory != nil {
		rootKey, err := r.ovRootKey(ctx, employee)
		if err == nil {
			_ = ovNew(employee.Spec.Memory.ServerURL, rootKey).DeleteUser(ctx, employee.Spec.Memory.AccountID, employee.Name)
		}
	}
	controllerutil.RemoveFinalizer(employee, memoryFinalizer)
	if err := r.Update(ctx, employee); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
