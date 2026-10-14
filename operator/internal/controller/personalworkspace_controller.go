// PersonalWorkspace reconciliation: lifecycle (memory identities + finalizer)
// lives here; child rendering lives in workspace_reconcile.go, mirroring the
// digitalemployee_controller.go / employee_reconcile.go split.
package controller

import (
	"context"
	"fmt"
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
	// workspaceMemkeyKey is the OpenViking USER key inside a companion's
	// memkey Secret (same key name as the employee memkey: the companion's
	// OpenViking integration reads the same env var).
	workspaceMemkeyKey = "OPENVIKING_API_KEY"
	// workspaceTokenKey is the per-companion internal-auth token the chat
	// proxy will present to the gateway (minted once, never rotated).
	workspaceTokenKey = "WORKSPACE_INTERNAL_TOKEN"
	// workspaceMemoryFinalizer guards the companions' OpenViking users.
	workspaceMemoryFinalizer = "digitalemployee.zhiyuan.io/workspace-memory"
)

// errMicrovmPoolEmpty fails a microvm workspace loudly instead of parking it
// silently when no KVM node pool is configured.
var errMicrovmPoolEmpty = fmt.Errorf("microvm runtime requested but no KVM node pool is configured (WORKSPACE_MICROVM_NODES)")

// PersonalWorkspaceReconciler reconciles a PersonalWorkspace object.
type PersonalWorkspaceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	OVRootFn func(ctx context.Context, c client.Client, namespace, secretName string) (string, error)
	// WorkspaceImage is the default workspace container image (spec.image
	// overrides per CR).
	WorkspaceImage string
	// ModelBaseURL is the model gateway route companions call (defaults to
	// the in-cluster Higress gateway).
	ModelBaseURL string
	// RoomsAgentSecret, when set, is a Secret (keys ROOMS_AGENT_USER /
	// ROOMS_AGENT_PASSWORD) injected envFrom into every workspace gateway so
	// the room orchestrator can speak A2A to digital employees under its own
	// agent identity. Empty disables the employee channel.
	RoomsAgentSecret string
	// RoomsAEPBaseURL is the AEP control plane the rooms agent logs in to
	// (AEP_BASE_URL in the gateway). Empty disables the employee channel.
	RoomsAEPBaseURL string
	// MicrovmNodes is the eligible KVM node pool (WORKSPACE_MICROVM_NODES);
	// order is the assignment tie-break (first pool entry wins ties, so the
	// cicd pool lists the preferred host first).
	MicrovmNodes []string
	// MicrovmSubnet is the /24 pool /30s are carved from
	// (WORKSPACE_MICROVM_SUBNET, default 192.168.77.0/24).
	MicrovmSubnet string
}

// +kubebuilder:rbac:groups=digitalemployee.zhiyuan.io,resources=personalworkspaces,verbs=*
// +kubebuilder:rbac:groups=digitalemployee.zhiyuan.io,resources=personalworkspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=digitalemployee.zhiyuan.io,resources=personalworkspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=*
// +kubebuilder:rbac:groups=core,resources=configmaps;secrets;services;pods;events;persistentvolumeclaims,verbs=*

// Reconcile renders the workspace children and tracks gateway availability.
// Workspaces never scale to zero: a companion is expected to hold warm
// working context, so there is no hibernation branch here.
func (r *PersonalWorkspaceReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var workspace v1alpha1.PersonalWorkspace
	if err := r.Get(ctx, request.NamespacedName, &workspace); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !workspace.DeletionTimestamp.IsZero() {
		return r.finalizeWorkspace(ctx, &workspace)
	}
	if err := validateCompanionNames(&workspace); err != nil {
		return ctrl.Result{}, r.failWorkspace(ctx, &workspace, err)
	}

	// 1. Per-companion memory identities (idempotent; keys minted once,
	// internal tokens never rotate).
	if err := r.ensureCompanionMemkeys(ctx, &workspace); err != nil {
		return ctrl.Result{}, r.failWorkspace(ctx, &workspace, err)
	}

	// Microvm path: the operator assigns node + subnet once and renders the
	// manifest ConfigMap (single roster source the node agent copies into
	// the guest config disk), then hands phase/gatewayEndpoint over to the
	// workspace-agent — no Deployment/PVC/Service, no availability gate.
	if workspace.Spec.Runtime == "microvm" {
		if err := ensureWorkspaceManifest(ctx, r.Client, r.Scheme, &workspace); err != nil {
			return ctrl.Result{}, r.failWorkspace(ctx, &workspace, err)
		}
		if err := r.ensureMicrovmAssignment(ctx, &workspace); err != nil {
			return ctrl.Result{}, r.failWorkspace(ctx, &workspace, err)
		}
		logger.Info("personal workspace assigned to node",
			"node", workspace.Status.NodeName, "subnet", workspace.Status.VMSubnet)
		return ctrl.Result{}, nil
	}

	// 2. Children.
	if err := r.ensureWorkspaceChildren(ctx, &workspace); err != nil {
		return ctrl.Result{}, r.failWorkspace(ctx, &workspace, err)
	}

	// 3. Wait for gateway availability.
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspaceDeploymentName(&workspace)}, deployment); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, r.failWorkspace(ctx, &workspace, err)
	}
	available := deployment.Status.ObservedGeneration >= deployment.Generation &&
		deployment.Spec.Replicas != nil && deployment.Status.ReadyReplicas == *deployment.Spec.Replicas
	if !available {
		if err := r.setWorkspaceStatus(ctx, &workspace, v1alpha1.WorkspacePhasePending, ""); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	workspace.Status.Phase = v1alpha1.WorkspacePhaseRunning
	workspace.Status.ObservedGeneration = workspace.Generation
	workspace.Status.Message = ""
	workspace.Status.GatewayEndpoint = workspaceServiceName(&workspace) + "." + workspace.Namespace + ".svc.cluster.local"
	if err := r.Status().Update(ctx, &workspace); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("personal workspace running", "companions", len(workspace.Spec.Companions))
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the manager.
func (r *PersonalWorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PersonalWorkspace{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Named("personalworkspace").
		Complete(r)
}

// companionUserID is the OpenViking user id for one companion: workspace
// namespaced so all of a user's companions live in the platform account
// under a stable, collision-free id.
func companionUserID(workspace *v1alpha1.PersonalWorkspace, companion string) string {
	return "pw-" + workspace.Name + "-" + companion
}

// companionMemkeySecretName names the Secret holding one companion's memory
// key and internal token (or the externally managed secret the CR named).
func companionMemkeySecretName(workspace *v1alpha1.PersonalWorkspace, companion string) string {
	for i := range workspace.Spec.Companions {
		c := &workspace.Spec.Companions[i]
		if c.Name == companion && c.Memory != nil && c.Memory.UserKeySecret != "" {
			return c.Memory.UserKeySecret
		}
	}
	return "pw-" + workspace.Name + "-" + companion + "-memkey"
}

// validateCompanionNames rejects duplicate roster entries: two companions
// sharing a name would silently share one memkey secret and one routing key.
func validateCompanionNames(workspace *v1alpha1.PersonalWorkspace) error {
	seen := make(map[string]bool, len(workspace.Spec.Companions))
	for i := range workspace.Spec.Companions {
		name := workspace.Spec.Companions[i].Name
		if seen[name] {
			return fmt.Errorf("duplicate companion name %q: companion names must be unique inside the workspace", name)
		}
		seen[name] = true
	}
	return nil
}

// ensureCompanionMemkeys mints one OpenViking user per companion (unless the
// CR pins an externally managed secret) and persists key + internal token.
// Existing values are authoritative: keys are minted once, tokens never
// rotate. Removing a companion from the roster deletes its memory user
// immediately (the identity is dead the moment the roster drops it);
// status.memoryUsers tracks the live set so finalization cleans up exactly
// what was minted, not what the spec happens to say at deletion time.
func (r *PersonalWorkspaceReconciler) ensureCompanionMemkeys(ctx context.Context, workspace *v1alpha1.PersonalWorkspace) error {
	previous := append([]string(nil), workspace.Status.MemoryUsers...)
	if workspace.Spec.Memory == nil {
		if len(previous) > 0 {
			// Dropping spec.memory would orphan the minted users: the
			// server URL and account live in the spec, so deletion-time
			// cleanup would be impossible. Refuse loudly instead of
			// leaking; remove companions or delete the workspace.
			return fmt.Errorf("spec.memory cannot be removed after provisioning (%d memory users minted); delete the workspace or remove companions instead", len(previous))
		}
		workspace.Status.MemoryUsers = nil
		return nil
	}
	if !controllerutil.ContainsFinalizer(workspace, workspaceMemoryFinalizer) {
		controllerutil.AddFinalizer(workspace, workspaceMemoryFinalizer)
		if err := r.Update(ctx, workspace); err != nil {
			return err
		}
	}
	rootKey, err := r.workspaceOVRootKey(ctx, workspace)
	if err != nil {
		return errWrap("openviking root key: ", err)
	}
	ov := ovNew(workspace.Spec.Memory.ServerURL, rootKey)
	accountID := workspace.Spec.Memory.AccountID

	memoryUsers := make([]string, 0, len(workspace.Spec.Companions))
	for i := range workspace.Spec.Companions {
		companion := &workspace.Spec.Companions[i]
		if companion.Memory != nil && companion.Memory.UserKeySecret != "" {
			continue // externally managed identity
		}
		userID := companionUserID(workspace, companion.Name)
		name := "pw-" + workspace.Name + "-" + companion.Name + "-memkey"
		existing := &corev1.Secret{ObjectMeta: objectMeta(name, workspace.Namespace)}
		if err := r.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil && !errors.IsNotFound(err) {
			return err
		}
		key, token := string(existing.Data[workspaceMemkeyKey]), string(existing.Data[workspaceTokenKey])
		if key == "" {
			minted, found, err := ov.RegisterUser(ctx, accountID, userID)
			if err != nil {
				return errWrap("register memory user: ", err)
			}
			if found || minted == "" {
				minted, err = ov.RegenerateKey(ctx, accountID, userID)
				if err != nil {
					return errWrap("regenerate memory user key: ", err)
				}
			}
			key = minted
		}
		if token == "" {
			token = randomToken()
		}
		// Data (not StringData): reading existing.Data back is the
		// operator's own idempotency check (fake client discipline).
		secret := &corev1.Secret{
			ObjectMeta: objectMeta(name, workspace.Namespace),
			Data:       map[string][]byte{workspaceMemkeyKey: []byte(key), workspaceTokenKey: []byte(token)},
		}
		if err := controllerutil.SetControllerReference(workspace, secret, r.Scheme); err != nil {
			return err
		}
		switch err := r.Create(ctx, secret); {
		case err == nil:
		case errors.IsAlreadyExists(err):
			existing.Data = secret.Data
			if err := r.Update(ctx, existing); err != nil {
				return errWrap("persist companion credentials: ", err)
			}
		default:
			return errWrap("persist companion credentials: ", err)
		}
		memoryUsers = append(memoryUsers, userID)
	}
	// Companion removed from the roster: its minted identity is gone with
	// it — delete now rather than at workspace deletion.
	for _, userID := range previous {
		stillPresent := false
		for _, live := range memoryUsers {
			if live == userID {
				stillPresent = true
				break
			}
		}
		if !stillPresent {
			if err := ov.DeleteUser(ctx, accountID, userID); err != nil {
				return errWrap("delete removed companion memory user: ", err)
			}
		}
	}
	workspace.Status.MemoryUsers = memoryUsers
	// Persist the minted set the moment it changes: the later phase write
	// no-ops when phase and message are unchanged, which would otherwise
	// swallow a roster-shrink update on a steady-state workspace.
	if !sameStringSet(previous, memoryUsers) {
		if err := r.Status().Update(ctx, workspace); err != nil {
			return errWrap("persist memory users: ", err)
		}
	}
	return nil
}

// sameStringSet compares two id lists ignoring order (both derive from
// roster order, which the API does not guarantee stable).
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, item := range a {
		counts[item]++
	}
	for _, item := range b {
		counts[item]--
		if counts[item] < 0 {
			return false
		}
	}
	return true
}

func (r *PersonalWorkspaceReconciler) finalizeWorkspace(ctx context.Context, workspace *v1alpha1.PersonalWorkspace) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(workspace, workspaceMemoryFinalizer) {
		return ctrl.Result{}, nil
	}
	// Clean up what was actually minted (status), not what the spec says at
	// deletion time — a companion removed in the same window must not
	// double-delete, and one added but never minted must not be missed.
	if workspace.Spec.Memory != nil {
		if rootKey, err := r.workspaceOVRootKey(ctx, workspace); err == nil {
			ov := ovNew(workspace.Spec.Memory.ServerURL, rootKey)
			for _, userID := range workspace.Status.MemoryUsers {
				_ = ov.DeleteUser(ctx, workspace.Spec.Memory.AccountID, userID)
			}
		}
	}
	controllerutil.RemoveFinalizer(workspace, workspaceMemoryFinalizer)
	if err := r.Update(ctx, workspace); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *PersonalWorkspaceReconciler) workspaceOVRootKey(ctx context.Context, workspace *v1alpha1.PersonalWorkspace) (string, error) {
	if r.OVRootFn == nil {
		return "", fmt.Errorf("no openviking root key source configured")
	}
	return r.OVRootFn(ctx, r.Client, workspace.Namespace, workspace.Spec.Credentials.Name)
}

func (r *PersonalWorkspaceReconciler) failWorkspace(ctx context.Context, workspace *v1alpha1.PersonalWorkspace, err error) error {
	log.FromContext(ctx).Error(err, "workspace reconcile failed", "workspace", workspace.Name)
	_ = r.setWorkspaceStatus(ctx, workspace, v1alpha1.WorkspacePhaseFailed, err.Error())
	return err
}

func (r *PersonalWorkspaceReconciler) setWorkspaceStatus(ctx context.Context, workspace *v1alpha1.PersonalWorkspace, phase v1alpha1.PersonalWorkspacePhase, message string) error {
	if workspace.Status.Phase == phase && workspace.Status.Message == message && phase != v1alpha1.WorkspacePhaseRunning {
		return nil
	}
	workspace.Status.Phase = phase
	workspace.Status.Message = message
	return r.Status().Update(ctx, workspace)
}
