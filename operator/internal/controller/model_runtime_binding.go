package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// This nonsecret routing hint uses the existing AEP v1 immutable binding tuple,
// not the publication hash, owner identity or a bearer token. Field order is
// part of that wire convention; contract vectors are checked against AEP's real
// gatewayaccess.Snapshot.BindingKey implementation before release. It conveys
// no permissions: native TokenReview and grant checks remain authoritative.
func modelRuntimeBinding(employee *v1alpha1.DigitalEmployee) string {
	var tuple struct {
		DeploymentID string `json:"deploymentId"`
		Subject      struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"subject"`
		Binding struct {
			Kind     string `json:"kind"`
			Workload struct {
				Namespace          string `json:"namespace"`
				ServiceAccountName string `json:"serviceAccountName"`
				ServiceAccountUID  string `json:"serviceAccountUid"`
				EmployeeUID        string `json:"employeeUid"`
				Audience           string `json:"audience"`
			} `json:"workload"`
		} `json:"binding"`
	}
	identity := employee.Status.ModelIdentity
	tuple.DeploymentID = identity.DeploymentID
	tuple.Subject.Kind, tuple.Subject.ID = "agent", identity.AgentID
	tuple.Binding.Kind = "kubernetes"
	tuple.Binding.Workload.Namespace = employee.Namespace
	tuple.Binding.Workload.ServiceAccountName = identity.ServiceAccountName
	tuple.Binding.Workload.ServiceAccountUID = string(identity.ServiceAccountUID)
	tuple.Binding.Workload.EmployeeUID = string(identity.EmployeeUID)
	tuple.Binding.Workload.Audience = identity.Audience
	payload, _ := json.Marshal(tuple)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
