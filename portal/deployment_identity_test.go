package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func TestDeploymentNameRejectsAnotherEnterprise(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"deployment":{"id":"enterprise-b","name":"Other enterprise"}}`))
	}))
	defer server.Close()
	p := &portal{cfg: config{aepBaseURL: server.URL, aepDeploymentID: "enterprise-a"}}
	name, err := p.deploymentName(context.Background())
	assert.Empty(t, name)
	require.ErrorContains(t, err, "mismatched")
}

func TestDeploymentNameRejectsRuntimeInvalidLabel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"deployment":{"id":"enterprise-a","name":"` + strings.Repeat("名", 201) + `"}}`))
	}))
	defer server.Close()
	p := &portal{cfg: config{aepBaseURL: server.URL, aepDeploymentID: "enterprise-a"}}
	_, err := p.deploymentName(context.Background())
	require.ErrorContains(t, err, "exceeds 200")
}

func TestApprovalRefreshesBusinessIdentity(t *testing.T) {
	h := newApplyHarness(t)
	spec := &dev1alpha1.DigitalEmployee{Spec: dev1alpha1.DigitalEmployeeSpec{
		Team: "sales-dept", Owner: "u-owner",
		Governance: dev1alpha1.GovernanceSpec{DeploymentID: "deployment-acme", DeploymentName: "Stale enterprise"},
	}}
	require.NoError(t, h.refreshApprovalIdentity(context.Background(), spec))
	assert.Equal(t, "deployment-acme", spec.Spec.Governance.DeploymentID)
	assert.Equal(t, "示例企业", spec.Spec.Governance.DeploymentName)
	assert.Equal(t, "销售部", spec.Spec.TeamName)
	assert.Equal(t, "李四", spec.Spec.OwnerName)
	spec.Spec.Governance.DeploymentID = "other-enterprise"
	require.ErrorContains(t, h.refreshApprovalIdentity(context.Background(), spec), "another AEP deployment")
}
