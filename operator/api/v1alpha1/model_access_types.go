package v1alpha1

import "k8s.io/apimachinery/pkg/types"

// ModelAccessSpec identifies an employee's AEP kind=agent account. The trusted
// AEP lifecycle writer must confirm its deployment and account kind before
// registration. No permissions, model allow-set or credentials live here.
// +kubebuilder:validation:XValidation:rule="self.agentId == oldSelf.agentId",message="replace the employee to change its AEP agent identity"
type ModelAccessSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	AgentID string `json:"agentId"`
}

// ModelIdentityStatus is observed by the employee Operator after verifying
// live SA ownership. Consumers must independently read the current CR and SA,
// reject deletion/stale generation and enforce immutable identity history.
// Removing/readding modelAccess must not bypass that history in AEP storage.
type ModelIdentityStatus struct {
	// +kubebuilder:validation:Minimum=1
	ObservedGeneration int64 `json:"observedGeneration"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	AgentID string `json:"agentId"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=80
	DeploymentID string `json:"deploymentId"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	EmployeeUID types.UID `json:"employeeUid"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ServiceAccountName string `json:"serviceAccountName"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ServiceAccountUID types.UID `json:"serviceAccountUid"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Audience string `json:"audience"`
}
