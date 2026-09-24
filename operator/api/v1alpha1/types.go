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
	Type string `json:"type"`
	// Image is the container image for the runtime (must embed the
	// governance middleware and MCP tooling).
	Image string `json:"image"`
	// Model is the wire model id routed by the Higress model gateway
	// (e.g. bench-glm). Never a provider-native model name.
	Model string `json:"model"`
	// ModelBaseURL is the OpenAI-compatible base URL. Defaults to the
	// in-cluster Higress gateway when empty.
	ModelBaseURL string `json:"modelBaseURL,omitempty"`
}

// MemorySpec configures long-term memory persistence.
type MemorySpec struct {
	// Type of memory backend. Only "openviking" is implemented.
	Type string `json:"type"`
	// ServerURL of the OpenViking service.
	ServerURL string `json:"serverURL"`
	// AccountID is the OpenViking account the employee user lives in.
	AccountID string `json:"accountID"`
	// SingleTenantInstance maps the synthetic default runtime user to the
	// per-employee owner (auth-disabled single-tenant semantics).
	SingleTenantInstance bool `json:"singleTenantInstance,omitempty"`
	// Retrieval tuning; zero values fall back to the backend defaults.
	TopK              int32   `json:"topK,omitempty"`
	ScoreThreshold    float64 `json:"scoreThreshold,omitempty"`
	MaxInjectionChars int32   `json:"maxInjectionChars,omitempty"`
}

// KnowledgeSpec wires the enterprise knowledge base (WeKnora) via MCP.
type KnowledgeSpec struct {
	// Type of knowledge integration. Only "weknora" is implemented.
	Type string `json:"type"`
	// MCPURL of the WeKnora MCP bridge endpoint.
	MCPURL string `json:"mcpURL"`
}

// GovernanceSpec carries AEP governance middleware parameters.
type GovernanceSpec struct {
	AEPBaseURL      string            `json:"aepBaseURL"`
	DeploymentID    string            `json:"deploymentID"`
	DefaultAEPUser  string            `json:"defaultAEPUser"`
	IdentitySource  string            `json:"identitySource,omitempty"`
	GovernedTools   []string          `json:"governedTools,omitempty"`
	TeamNames       map[string]string `json:"teamNames,omitempty"`
	APIServiceCreds string            `json:"apiServiceCreds,omitempty"`
}

// CredentialsRef names the platform Secret holding shared credentials
// (DEER model key, WeKnora MCP bearer, OpenViking root key, AEP service
// credentials). Secrets are never embedded in the CR.
type CredentialsRef struct {
	// Name of the Secret in the employee namespace.
	Name string `json:"name"`
}

type DigitalEmployeeSpec struct {
	DisplayName string         `json:"displayName,omitempty"`
	Runtime     RuntimeSpec    `json:"runtime"`
	Memory      *MemorySpec    `json:"memory,omitempty"`
	Knowledge   *KnowledgeSpec `json:"knowledge,omitempty"`
	Governance  GovernanceSpec `json:"governance"`
	// Credentials references the shared platform secret.
	Credentials CredentialsRef `json:"credentials"`
	// Owner is the AEP user id of the employee's owner (set by the business
	// layer; the operator never interprets it).
	Owner string `json:"owner,omitempty"`
	// OwnerName is the owner's AEP username for display.
	OwnerName string `json:"ownerName,omitempty"`
	Replicas    int32          `json:"replicas,omitempty"`
	// NodePort exposes the runtime; 0 means ClusterIP only.
	NodePort int32 `json:"nodePort,omitempty"`
	// A2A optionally exposes this employee over the A2A delegation contract.
	A2A *A2ASpec `json:"a2a,omitempty"`
}

type DigitalEmployeePhase string

const (
	PhasePending DigitalEmployeePhase = "Pending"
	PhaseReady   DigitalEmployeePhase = "Ready"
	PhaseError   DigitalEmployeePhase = "Error"
)

type DigitalEmployeeStatus struct {
	Phase              DigitalEmployeePhase `json:"phase,omitempty"`
	ObservedGeneration int64                `json:"observedGeneration,omitempty"`
	// MemoryUser is the OpenViking user provisioned for this employee.
	MemoryUser string `json:"memoryUser,omitempty"`
	// Message carries the last error detail when Phase == Error.
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type DigitalEmployee struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DigitalEmployeeSpec   `json:"spec,omitempty"`
	Status            DigitalEmployeeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type DigitalEmployeeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DigitalEmployee `json:"items"`
}
