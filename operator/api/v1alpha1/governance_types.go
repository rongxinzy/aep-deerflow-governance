package v1alpha1

// GovernanceSpec carries AEP governance middleware parameters.
type GovernanceSpec struct {
	AEPBaseURL   string `json:"aepBaseURL"`
	DeploymentID string `json:"deploymentID"`
	// DeploymentName is the AEP deployment's verified display name.
	// +optional
	DeploymentName string `json:"deploymentName,omitempty"`
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
