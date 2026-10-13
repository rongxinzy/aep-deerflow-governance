// PersonalWorkspace is the personal-companion counterpart of the
// DigitalEmployee: one persistent workspace per user, hosting N companion
// agents (Pi Durable) that share the user's files, browser sessions and
// login state while keeping per-companion private state (SQLite) and
// per-companion memory identity (OpenViking user keys).
//
// The companions are the user's own avatars ("分身"): the hard isolation
// boundary is the workspace (= the user), not the companion. Personal
// workspaces never scale to zero — a companion is expected to hold warm
// working context, so the runtime stays resident (single replica, Recreate).
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PersonalWorkspaceSpec defines the desired state of a personal workspace.
type PersonalWorkspaceSpec struct {
	// Owner is the AEP user id of the workspace owner: the quota and RBAC
	// subject every companion in this workspace acts on behalf of.
	Owner string `json:"owner"`

	// Runtime selects the execution substrate. "pod" renders a plain
	// Deployment; "microvm" is reserved for the Firecracker backend and is
	// rejected until that lands.
	// +optional
	// +kubebuilder:validation:Enum=pod;microvm
	// +kubebuilder:default=pod
	Runtime string `json:"runtime,omitempty"`

	// Image is the workspace container image (gateway + companion runtime).
	// Empty falls back to the operator's PERSONAL_WORKSPACE_IMAGE default.
	// +optional
	Image string `json:"image,omitempty"`

	// Resources bounds the workspace as a whole.
	// +optional
	Resources WorkspaceResources `json:"resources,omitempty"`

	// Companions is the workspace's agent roster. Every entry gets its own
	// memory identity and private storage; entries share the workspace's
	// files and sessions. The list is managed by the business layer
	// (portal); the operator renders it but never interprets tool grants.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=10
	Companions []CompanionSpec `json:"companions"`

	// Memory configures the shared OpenViking backend the companions'
	// per-companion users live in. Nil disables memory provisioning.
	// +optional
	Memory *MemorySpec `json:"memory,omitempty"`

	// Browser keeps the shared-browser policy (login state is a feature of
	// the avatar model, not a compromise). Stored for the gateway; the
	// operator does not enforce it in P0.
	// +optional
	Browser WorkspaceBrowser `json:"browser,omitempty"`

	// Security declares the workspace's egress surface. Stored for the
	// gateway/substrate; the operator does not render network policy in P0.
	// +optional
	Security WorkspaceSecurity `json:"security,omitempty"`

	// Credentials references the shared platform secret (OpenViking root
	// key, model API key). Secrets are never embedded in the CR.
	Credentials CredentialsRef `json:"credentials"`
}

// WorkspaceResources bounds one workspace.
type WorkspaceResources struct {
	// Memory is the container memory limit. Default 512Mi.
	// +optional
	// +kubebuilder:default="512Mi"
	Memory string `json:"memory,omitempty"`
	// Disk is the workspace volume size (files, sessions, per-companion
	// SQLite). Default 2Gi.
	// +optional
	// +kubebuilder:default="2Gi"
	Disk string `json:"disk,omitempty"`
	// VCPUs is reserved for the microvm substrate (P1); the pod runtime
	// ignores it.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	VCPUs int32 `json:"vcpus,omitempty"`
}

// CompanionSpec is one companion agent in the workspace roster.
type CompanionSpec struct {
	// Name identifies the companion for routing and storage namespacing
	// (DNS-label rules; unique inside the workspace).
	// +kubebuilder:validation:Pattern=`^[a-z]([a-z0-9-]{0,28}[a-z0-9])?$`
	Name string `json:"name"`
	// DisplayName is the roster label shown to the user.
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// Model is the AEP-approved model route the companion uses (wire id
	// routed by the model gateway, never a provider-native name).
	Model string `json:"model"`
	// Tools lists the capability faces the companion may use. Stored for the
	// gateway; the operator never interprets entries.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Tools []string `json:"tools,omitempty"`
	// Resources bounds this companion inside the workspace budget.
	// +optional
	Resources CompanionResources `json:"resources,omitempty"`
	// Memory declares the companion's memory sharing surface.
	// +optional
	Memory *CompanionMemory `json:"memory,omitempty"`
}

// CompanionResources bounds one companion.
type CompanionResources struct {
	// Memory is the companion's process memory cap inside the workspace.
	// +optional
	Memory string `json:"memory,omitempty"`
	// CPU is the companion's CPU cap (Kubernetes quantity).
	// +optional
	CPU string `json:"cpu,omitempty"`
}

// CompanionMemory declares the companion's memory identity and sharing.
type CompanionMemory struct {
	// UserKeySecret optionally names an externally managed Secret (key
	// OPENVIKING_API_KEY) instead of an operator-minted per-companion user.
	// +optional
	UserKeySecret string `json:"userKeySecret,omitempty"`
	// Scopes is the declared sharing surface (e.g. private, person, org).
	// The OpenViking ACL enforcement reads it via the gateway; the operator
	// only stores it.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	Scopes []string `json:"scopes,omitempty"`
}

// WorkspaceBrowser is the shared-browser policy of the workspace.
type WorkspaceBrowser struct {
	// SharedProfile keeps one shared browser profile (login state) across
	// the workspace's companions — the handoff-without-reconfiguration
	// feature of the avatar model. Default true.
	// +optional
	// +kubebuilder:default=true
	SharedProfile bool `json:"sharedProfile,omitempty"`
	// MaxTabs bounds concurrent browser sessions per workspace.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	MaxTabs int32 `json:"maxTabs,omitempty"`
}

// WorkspaceSecurity declares the workspace's egress surface.
type WorkspaceSecurity struct {
	// EgressAllow lists the destination classes the workspace may reach
	// (model-gateway, memory, registry). Stored for the substrate; the
	// whitelist itself is rendered by the gateway/node agent, not P0.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	EgressAllow []string `json:"egressAllow,omitempty"`
}

// PersonalWorkspacePhase describes the reconciliation phase.
// +kubebuilder:validation:Enum=Pending;Running;Failed;Terminating
type PersonalWorkspacePhase string

const (
	// WorkspacePhasePending: children are rendered but the gateway pod is
	// not available yet.
	WorkspacePhasePending PersonalWorkspacePhase = "Pending"
	// WorkspacePhaseRunning: the workspace gateway is available.
	WorkspacePhaseRunning PersonalWorkspacePhase = "Running"
	// WorkspacePhaseFailed: reconciliation error (status.message carries
	// the detail).
	WorkspacePhaseFailed PersonalWorkspacePhase = "Failed"
	// WorkspacePhaseTerminating: deletion in progress (memory users being
	// cleaned up).
	WorkspacePhaseTerminating PersonalWorkspacePhase = "Terminating"
)

// PersonalWorkspaceStatus defines the observed state of a workspace.
type PersonalWorkspaceStatus struct {
	// +optional
	Phase PersonalWorkspacePhase `json:"phase,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Message carries the last error detail when Phase == Failed.
	// +optional
	Message string `json:"message,omitempty"`
	// GatewayEndpoint is the in-cluster gateway service DNS name callers
	// (portal chat proxy) route to.
	// +optional
	GatewayEndpoint string `json:"gatewayEndpoint,omitempty"`
	// MemoryUsers lists the OpenViking user ids provisioned for the
	// companions (one per companion with memory enabled).
	// +optional
	MemoryUsers []string `json:"memoryUsers,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.spec.runtime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=pw

// PersonalWorkspace is one user's persistent companion environment.
type PersonalWorkspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PersonalWorkspaceSpec   `json:"spec,omitempty"`
	Status PersonalWorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PersonalWorkspaceList contains a list of PersonalWorkspace.
type PersonalWorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PersonalWorkspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PersonalWorkspace{}, &PersonalWorkspaceList{})
}
