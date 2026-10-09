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

func TestDeploymentNameRequiresAuthoritativeMetadata(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantError string
	}{
		{name: "upstream error", status: http.StatusBadGateway, wantError: "returned 502"},
		{name: "invalid json", body: `{`, wantError: "decode"},
		{name: "missing deployment", body: `{}`, wantError: "missing or mismatched"},
		{name: "missing name", body: `{"deployment":{"id":"enterprise-a"}}`, wantError: "missing or mismatched"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "enterprise-a", r.URL.Query().Get("deploymentHint"))
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				}
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			p := &portal{cfg: config{aepBaseURL: server.URL, aepDeploymentID: "enterprise-a"}}
			_, err := p.deploymentName(context.Background())
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestDeploymentNamePropagatesTransportErrors(t *testing.T) {
	p := &portal{cfg: config{aepBaseURL: "://invalid", aepDeploymentID: "enterprise-a"}}
	_, err := p.deploymentName(context.Background())
	require.Error(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"deployment":{"id":"enterprise-a","name":"Example"}}`))
	}))
	p.cfg.aepBaseURL = server.URL
	server.Close()
	_, err = p.deploymentName(context.Background())
	require.ErrorContains(t, err, "AEP metadata")
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
	spec.Spec.Governance.DeploymentID = "deployment-acme"
	h.cfg.aepBaseURL = "://invalid"
	require.Error(t, h.refreshApprovalIdentity(context.Background(), spec))
}

func TestApprovalRejectsStaleDirectoryReferences(t *testing.T) {
	h := newApplyHarness(t)
	base := dev1alpha1.DigitalEmployeeSpec{
		Team: "sales-dept", Owner: "u-owner",
		Governance: dev1alpha1.GovernanceSpec{DeploymentID: "deployment-acme"},
	}
	tests := []struct {
		name      string
		change    func(*dev1alpha1.DigitalEmployeeSpec)
		wantError string
	}{
		{name: "team removed", change: func(s *dev1alpha1.DigitalEmployeeSpec) { s.Team = "missing-team" }, wantError: "team no longer exists"},
		{name: "team lookup outage", change: func(s *dev1alpha1.DigitalEmployeeSpec) { s.Team = "boom" }, wantError: "500"},
		{name: "owner removed", change: func(s *dev1alpha1.DigitalEmployeeSpec) { s.Owner = "missing-user" }, wantError: "active human"},
		{name: "owner disabled", change: func(s *dev1alpha1.DigitalEmployeeSpec) { s.Owner = "u-off" }, wantError: "active human"},
		{name: "owner is agent", change: func(s *dev1alpha1.DigitalEmployeeSpec) { s.Owner = "u-agent" }, wantError: "active human"},
		{name: "owner lookup outage", change: func(s *dev1alpha1.DigitalEmployeeSpec) { s.Owner = "boom" }, wantError: "500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := &dev1alpha1.DigitalEmployee{Spec: *base.DeepCopy()}
			tt.change(&spec.Spec)
			require.ErrorContains(t, h.refreshApprovalIdentity(context.Background(), spec), tt.wantError)
		})
	}
}

func TestApprovalUsesUsernameWhenDisplayNameIsMissing(t *testing.T) {
	h := newApplyHarness(t)
	spec := &dev1alpha1.DigitalEmployee{Spec: dev1alpha1.DigitalEmployeeSpec{
		Team: "sales-dept", Owner: "u-no-display",
		Governance: dev1alpha1.GovernanceSpec{DeploymentID: "deployment-acme"},
	}}
	require.NoError(t, h.refreshApprovalIdentity(context.Background(), spec))
	assert.Equal(t, "account-name", spec.Spec.OwnerName)
}
