package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// The open-scope rule: admins and the owner always pass; an explicit
// visibility decides for everyone else (all | restricted with team/user
// subjects); nil keeps the legacy department rule; unknown modes fail
// closed.
func TestCanAccessEmployeeVisibility(t *testing.T) {
	cfg := config{adminUsers: map[string]bool{"admin-user": true}}
	admin := identity{UserID: "admin-user"}
	owner := identity{UserID: "owner-a"}
	stranger := identity{UserID: "stranger"}
	member := identity{UserID: "stranger", Teams: []string{"rd-dept"}}

	restricted := &dev1alpha1.VisibilitySpec{
		Mode:  "restricted",
		Teams: []dev1alpha1.SubjectRef{{ID: "rd-dept", Name: "研发部"}},
		Users: []dev1alpha1.SubjectRef{{ID: "friend", Name: "好友"}},
	}

	assert.True(t, admin.canAccessEmployee(cfg, "", "owner-a", restricted))
	assert.True(t, owner.canAccessEmployee(cfg, "", "owner-a", restricted))

	// Legacy nil visibility: department rule, unscoped stays closed.
	assert.True(t, member.canAccessEmployee(cfg, "rd-dept", "owner-a", nil))
	assert.False(t, stranger.canAccessEmployee(cfg, "rd-dept", "owner-a", nil))
	assert.False(t, member.canAccessEmployee(cfg, "", "owner-a", nil))

	// Explicit all-users scope.
	assert.True(t, stranger.canAccessEmployee(cfg, "", "owner-a", &dev1alpha1.VisibilitySpec{Mode: "all"}))

	// Restricted: listed team members and listed users pass, others not.
	assert.True(t, member.canAccessEmployee(cfg, "sales-dept", "owner-a", restricted))
	assert.True(t, identity{UserID: "friend"}.canAccessEmployee(cfg, "sales-dept", "owner-a", restricted))
	assert.False(t, stranger.canAccessEmployee(cfg, "sales-dept", "owner-a", restricted))
	assert.False(t, identity{UserID: "stranger", Teams: []string{"sales-dept"}}.canAccessEmployee(cfg, "sales-dept", "owner-a", restricted))

	// Empty restricted list admits nobody but admins and the owner.
	empty := &dev1alpha1.VisibilitySpec{Mode: "restricted"}
	assert.False(t, member.canAccessEmployee(cfg, "rd-dept", "owner-a", empty))
	assert.True(t, owner.canAccessEmployee(cfg, "rd-dept", "owner-a", empty))

	// Unknown mode fails closed.
	assert.False(t, member.canAccessEmployee(cfg, "rd-dept", "owner-a", &dev1alpha1.VisibilitySpec{Mode: "everyone-trust-me"}))
}
