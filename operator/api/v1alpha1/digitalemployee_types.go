// Package v1alpha1 defines the DigitalEmployee custom resource.
//
// A DigitalEmployee is the declarative form of one governed agent instance:
// the CR name is the business ID that memory and knowledge are bound to.
// The operator holds full CRUD on this type; who may create one is a
// business-layer policy question and never a Kubernetes RBAC question.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RuntimeSpec selects the execution engine and the model route.
type RuntimeSpec struct {
	// Type of the execution engine. Only "deerflow" is implemented.
	// +kubebuilder:validation:Enum=deerflow
	Type string `json:"type"`

	// Image is the container image for the runtime (must embed the
	// governance middleware and MCP tooling).
	Image string `json:"image"`

	// Model is the wire model id routed by the Higress model gateway
	// (e.g. bench-glm). Never a provider-native model name.
	Model string `json:"model"`

	// ModelBaseURL is the OpenAI-compatible base URL. Defaults to the
	// in-cluster Higress gateway when empty.
	// +optional
	ModelBaseURL string `json:"modelBaseURL,omitempty"`
}

// MemorySpec configures long-term memory persistence.
type MemorySpec struct {
	// Type of memory backend. Only "openviking" is implemented.
	// +kubebuilder:validation:Enum=openviking
	Type string `json:"type"`

	// ServerURL of the OpenViking service.
	ServerURL string `json:"serverURL"`

	// AccountID is the OpenViking account the employee user lives in.
	AccountID string `json:"accountID"`

	// SingleTenantInstance maps every runtime user of this single-tenant
	// instance onto the employee's memory owner (per-request human identity
	// lives in governance, not in the memory tenant).
	// +optional
	// +kubebuilder:default=false
	SingleTenantInstance bool `json:"singleTenantInstance,omitempty"`

	// Retrieval tuning; zero values fall back to the backend defaults.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	TopK int32 `json:"topK,omitempty"`
	// +optional

	// +kubebuilder:validation:Maximum=1
	ScoreThreshold float64 `json:"scoreThreshold,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=256
	// +kubebuilder:validation:Maximum=100000
	MaxInjectionChars int32 `json:"maxInjectionChars,omitempty"`
}

// KnowledgeSpec wires the enterprise knowledge base (WeKnora) via MCP.
type KnowledgeSpec struct {
	// Type of knowledge integration. Only "weknora" is implemented.
	// +kubebuilder:validation:Enum=weknora
	Type string `json:"type"`

	// MCPURL of the WeKnora MCP bridge endpoint.
	MCPURL string `json:"mcpURL"`
}

// GovernanceSpec carries AEP governance middleware parameters.
type GovernanceSpec struct {
	AEPBaseURL     string `json:"aepBaseURL"`
	DeploymentID   string `json:"deploymentID"`
	DefaultAEPUser string `json:"defaultAEPUser"`
	// +optional
	IdentitySource string `json:"identitySource,omitempty"`
	// +optional
	GovernedTools []string `json:"governedTools,omitempty"`
	// +optional
	TeamNames map[string]string `json:"teamNames,omitempty"`
	// +optional
	APIServiceCreds string `json:"apiServiceCreds,omitempty"`
}

// CredentialsRef names the platform Secret holding shared credentials
// (DEER model key, WeKnora MCP bearer, OpenViking root key, AEP service
// credentials). Secrets are never embedded in the CR.
type CredentialsRef struct {
	// Name of the Secret in the employee namespace.
	Name string `json:"name"`
}

// A2ASpec exposes the employee over the AEP A2A delegation contract. When
// enabled the operator renders a dedicated adapter instance fronting this
// employee's runtime: peer digital employees may delegate tasks to it with
// act-as semantics, chain-depth limits, and audit.
type A2ASpec struct {
	// Image of the a2a-adapter container.
	Image string `json:"image"`
	// MaxDepth is the delegation chain depth limit (default 3).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	MaxDepth int32 `json:"maxDepth,omitempty"`
	// NodePort exposes the adapter; 0 means ClusterIP only.
	// +optional
	// +kubebuilder:validation:Minimum=30000
	// +kubebuilder:validation:Maximum=32767
	NodePort int32 `json:"nodePort,omitempty"`
}

// ChannelsSpec toggles IM channel providers on the runtime. Channel
// credentials (bot id/secret, tokens) are supplied at runtime through the
// gateway's admin channel-connections API and never appear in the CR or
// the rendered config.
type ChannelsSpec struct {
	// Wecom enables the WeCom (企业微信) smart-bot channel.
	// +optional
	Wecom bool `json:"wecom,omitempty"`
	// Feishu enables the Feishu channel.
	// +optional
	Feishu bool `json:"feishu,omitempty"`
}

// DigitalEmployeeSpec defines the desired state of a digital employee.
type DigitalEmployeeSpec struct {
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	Runtime RuntimeSpec `json:"runtime"`
	// +optional
	Memory *MemorySpec `json:"memory,omitempty"`
	// +optional
	Knowledge *KnowledgeSpec `json:"knowledge,omitempty"`

	Governance GovernanceSpec `json:"governance"`

	// Credentials references the shared platform secret.
	Credentials CredentialsRef `json:"credentials"`

	// Channels toggles IM channel providers; credentials arrive at runtime
	// via the gateway admin API, not through the CR.
	// +optional
	Channels *ChannelsSpec `json:"channels,omitempty"`

	// Owner is the AEP user id of the employee's owner (set by the business
	// layer; the operator never interprets it).
	// +optional
	Owner string `json:"owner,omitempty"`
	// OwnerName is the owner's AEP username for display.
	// +optional
	OwnerName string `json:"ownerName,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas,omitempty"`
	// NodePort exposes the runtime; 0 means ClusterIP only.
	// +optional
	// +kubebuilder:validation:Minimum=30000
	// +kubebuilder:validation:Maximum=32767
	NodePort int32 `json:"nodePort,omitempty"`
	// A2A optionally exposes this employee over the A2A delegation contract.
	// +optional
	A2A *A2ASpec `json:"a2a,omitempty"`
}

// DigitalEmployeePhase describes the reconciliation phase.
// +kubebuilder:validation:Enum=Pending;Ready;Error
type DigitalEmployeePhase string

const (
	PhasePending DigitalEmployeePhase = "Pending"
	PhaseReady   DigitalEmployeePhase = "Ready"
	PhaseError   DigitalEmployeePhase = "Error"
)

// DigitalEmployeeStatus defines the observed state of a digital employee.
type DigitalEmployeeStatus struct {
	// +optional
	Phase DigitalEmployeePhase `json:"phase,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// MemoryUser is the OpenViking user provisioned for this employee.
	// +optional
	MemoryUser string `json:"memoryUser,omitempty"`
	// Message carries the last error detail when Phase == Error.
	// +optional
	Message string `json:"message,omitempty"`
	// RuntimeAccount is the bootstrap state of the employee's runtime
	// account: the per-instance DeerFlow admin plus the owner account and
	// PAT provisioned through the runtime auth API (see the operator's
	// ensureRuntimeAccount). Ready means the PAT in the
	// de-<name>-runtime-creds Secret authenticates against the runtime.
	// +optional
	RuntimeAccount RuntimeAccountPhase `json:"runtimeAccount,omitempty"`
	// RuntimeAccountMessage carries the last runtime-account detail
	// (failure reason, upgrade hint, …).
	// +optional
	RuntimeAccountMessage string `json:"runtimeAccountMessage,omitempty"`
}

// RuntimeAccountPhase tracks the runtime-account bootstrap.
// +kubebuilder:validation:Enum=Pending;Ready;Failed;Unsupported
type RuntimeAccountPhase string

const (
	// RuntimeAccountPending: the runtime is not reachable yet; the
	// controller requeues and retries.
	RuntimeAccountPending RuntimeAccountPhase = "Pending"
	// RuntimeAccountReady: admin + owner account + PAT provisioned and the
	// PAT verifies against the runtime.
	RuntimeAccountReady RuntimeAccountPhase = "Ready"
	// RuntimeAccountFailed: manual intervention required (rejected
	// credentials, orphaned account, …).
	RuntimeAccountFailed RuntimeAccountPhase = "Failed"
	// RuntimeAccountUnsupported: the runtime image predates the provision
	// API; upgrade the image to get runtime accounts.
	RuntimeAccountUnsupported RuntimeAccountPhase = "Unsupported"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.spec.runtime.type`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.runtime.model`
// +kubebuilder:printcolumn:name="MemoryUser",type=string,JSONPath=`.status.memoryUser`
// +kubebuilder:printcolumn:name="Account",type=string,JSONPath=`.status.runtimeAccount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=de

// DigitalEmployee is one governed agent instance; the CR name is the
// business ID that memory and knowledge bind to.
type DigitalEmployee struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DigitalEmployeeSpec   `json:"spec,omitempty"`
	Status DigitalEmployeeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DigitalEmployeeList contains a list of DigitalEmployee.
type DigitalEmployeeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DigitalEmployee `json:"items"`
}
