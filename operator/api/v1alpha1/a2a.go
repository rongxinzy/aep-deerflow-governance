package v1alpha1

// A2ASpec exposes the employee over the AEP A2A delegation contract. When
// enabled the operator renders a dedicated adapter instance fronting this
// employee's runtime: peer digital employees may delegate tasks to it with
// act-as semantics, chain-depth limits, and audit.
type A2ASpec struct {
	// Image of the a2a-adapter container.
	Image string `json:"image"`
	// MaxDepth is the delegation chain depth limit (default 3).
	MaxDepth int32 `json:"maxDepth,omitempty"`
	// NodePort exposes the adapter; 0 means ClusterIP only.
	NodePort int32 `json:"nodePort,omitempty"`
}
