package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// deploymentName reads the authoritative AEP directory instance name. A mismatched
// deployment ID is an error: never stamp another enterprise on a CR.
func (p *portal) deploymentName(ctx context.Context) (string, error) {
	endpoint := strings.TrimRight(p.cfg.aepBaseURL, "/") + "/aep/v1/auth/methods?deploymentHint=" + url.QueryEscape(p.cfg.aepDeploymentID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-AEP-Protocol-Version", "1.0")
	resp, err := serviceProbeClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("AEP metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("AEP metadata returned %d", resp.StatusCode)
	}
	var metadata struct {
		Deployment struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"deployment"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return "", fmt.Errorf("AEP metadata decode: %w", err)
	}
	if metadata.Deployment.ID != p.cfg.aepDeploymentID || strings.TrimSpace(metadata.Deployment.Name) == "" {
		return "", fmt.Errorf("AEP deployment identity is missing or mismatched")
	}
	if len([]rune(metadata.Deployment.Name)) > 200 {
		return "", fmt.Errorf("AEP deployment name exceeds 200 characters")
	}
	return metadata.Deployment.Name, nil
}

// refreshApprovalIdentity prevents a pending application's identity labels
// from becoming stale or crossing deployments before an administrator approves it.
func (p *portal) refreshApprovalIdentity(ctx context.Context, spec *dev1alpha1.DigitalEmployee) error {
	if spec.Spec.Governance.DeploymentID != p.cfg.aepDeploymentID {
		return fmt.Errorf("pending request belongs to another AEP deployment")
	}
	name, err := p.deploymentName(ctx)
	if err != nil {
		return err
	}
	resolver := newReferenceResolver(p, ctx)
	team, found, err := resolver.lookupTeam(spec.Spec.Team)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("owning team no longer exists")
	}
	owner, found, err := resolver.lookupUser(spec.Spec.Owner)
	if err != nil {
		return err
	}
	if !found || owner.Status == "disabled" || (owner.Kind != "" && owner.Kind != "human") {
		return fmt.Errorf("owner is no longer an active human user")
	}
	spec.Spec.Governance.DeploymentID = p.cfg.aepDeploymentID
	spec.Spec.Governance.DeploymentName = name
	spec.Spec.TeamName = team.Name
	spec.Spec.OwnerName = owner.DisplayName
	if spec.Spec.OwnerName == "" {
		spec.Spec.OwnerName = owner.Username
	}
	return nil
}
