package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func participantPermissionHarness(t *testing.T) *apiTestPortal {
	t.Helper()
	h := newApplyHarness(t)
	ws := runningWorkspace("ws-participant-owner", "u-owner", "assistant", "private-helper")
	ws.UID = types.UID("uid-ws-participant-owner")
	initializeContacts(ws, nil)
	settings := contactSettings(ws)
	settings["private-helper"] = companionContact{Mode: "private"}
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	ws.Annotations[contactAnnotation] = string(raw)
	require.NoError(t, h.k8s.Create(t.Context(), ws))

	employee := readyCR("employee-sales", "u-owner")
	employee.UID = types.UID("uid-employee-sales")
	employee.Spec.Team = "sales-dept"
	employee.Spec.Visibility = &dev.VisibilitySpec{Mode: "restricted", Teams: []dev.SubjectRef{{ID: "sales-dept"}}}
	require.NoError(t, h.k8s.Create(t.Context(), employee))
	return h
}

func TestSharedParticipantResourceEnforcesResourceIdentityAndVisibility(t *testing.T) {
	h := participantPermissionHarness(t)
	ctx := context.Background()
	owner := identity{UserID: "u-owner", DeploymentID: "deployment-acme"}
	stranger := identity{UserID: "u-x", DeploymentID: "deployment-acme"}
	manager := identity{UserID: "u-x", Teams: []string{"sales-dept"}, DeploymentID: "deployment-acme"}
	admin := identity{UserID: "u-x", Roles: []string{"platform-admin"}, DeploymentID: "deployment-acme"}
	h.cfg.adminRoles = map[string]bool{"platform-admin": true}

	_, err := h.participantResource(ctx, "companion", "ws-participant-owner/private-helper", stranger, true)
	require.Error(t, err)
	require.Equal(t, 403, err.(*sharedError).code)
	companion, err := h.participantResource(ctx, "companion", "ws-participant-owner/assistant", owner, true)
	require.NoError(t, err)
	require.Equal(t, "assistant", companion.Name)
	require.Equal(t, "u-owner", companion.OwnerID)
	require.Equal(t, "uid-ws-participant-owner", companion.UID)
	require.True(t, companion.CanApprove)

	_, err = h.participantResource(ctx, "companion", "ws-participant-owner", owner, true)
	require.Error(t, err)
	require.Equal(t, 400, err.(*sharedError).code)
	_, err = h.participantResource(ctx, "not-an-agent", "employee-sales", owner, true)
	require.Error(t, err)
	require.Equal(t, 400, err.(*sharedError).code)

	_, err = h.participantResource(ctx, "digital_employee", "employee-sales", stranger, true)
	require.Error(t, err)
	require.Equal(t, 403, err.(*sharedError).code)
	employee, err := h.participantResource(ctx, "digital_employee", "employee-sales", manager, true)
	require.NoError(t, err, "team visibility allows a manager to invite the employee")
	require.Equal(t, "employee-sales", employee.Name)
	require.False(t, employee.CanApprove, "visibility to an employee does not make the caller its manager")
	employee, err = h.participantResource(ctx, "digital_employee", "employee-sales", owner, true)
	require.NoError(t, err)
	require.True(t, employee.CanApprove, "the employee owner may confirm its invitation")
	employee, err = h.participantResource(ctx, "digital_employee", "employee-sales", admin, true)
	require.NoError(t, err)
	require.True(t, employee.CanApprove, "platform administrators may manage employee invitations")

	_, err = h.participantResource(ctx, "digital_employee", "missing-employee", admin, true)
	require.Error(t, err)
	require.Equal(t, 404, err.(*sharedError).code)
}

func TestSharedParticipantDecorateSeparatesManagerAndSpaceCapabilities(t *testing.T) {
	h := participantPermissionHarness(t)
	ctx := context.Background()
	base := sharedParticipant{
		ID: "participant", Kind: "companion", Target: "ws-participant-owner/assistant",
		Name: "Assistant", OwnerID: "u-owner", UID: "uid-ws-participant-owner",
		Role: "editor", State: "active", Version: 2, AgentID: "registered-agent", Registration: "ready",
	}

	owner := h.participantDecorate(ctx, sharedSpace{Role: "editor"}, base, identity{UserID: "u-owner"})
	require.True(t, owner.ResourceValid)
	require.True(t, owner.CanAssign, "editors can assign a valid participant")
	require.True(t, owner.CanClaim, "the resource owner can claim with its participant")

	manager := h.participantDecorate(ctx, sharedSpace{Role: "editor"}, base, identity{UserID: "u-x"})
	require.True(t, manager.ResourceValid)
	require.True(t, manager.CanAssign, "space editor assignment does not require employee ownership")
	require.False(t, manager.CanClaim, "claiming is reserved to the resource manager")

	viewer := h.participantDecorate(ctx, sharedSpace{Role: "viewer"}, base, identity{UserID: "u-owner"})
	require.True(t, viewer.ResourceValid)
	require.False(t, viewer.CanAssign)
	require.False(t, viewer.CanClaim)

	grantViewer := base
	grantViewer.Role = "viewer"
	viewOnlyAgent := h.participantDecorate(ctx, sharedSpace{Role: "owner"}, grantViewer, identity{UserID: "u-owner"})
	require.False(t, viewOnlyAgent.CanAssign, "a viewer grant never becomes assignable through owner space rights")
	require.False(t, viewOnlyAgent.CanClaim)

	invalid := base
	invalid.UID = "stale-resource-uid"
	invalidated := h.participantDecorate(ctx, sharedSpace{Role: "owner"}, invalid, identity{UserID: "u-x"})
	require.False(t, invalidated.ResourceValid, "recreated resources invalidate old grants")
	require.False(t, invalidated.CanAssign)
	require.False(t, invalidated.CanClaim)
	require.True(t, invalidated.CanRevoke, "space owner can revoke an invalidated grant")

	pending := base
	pending.State = "pending"
	pending.AgentID = ""
	pending.Registration = "pending"
	pendingDecorated := h.participantDecorate(ctx, sharedSpace{Role: "viewer"}, pending, identity{UserID: "u-owner"})
	require.True(t, pendingDecorated.CanApprove, "pending invitations may be approved by the resource owner")
	require.False(t, pendingDecorated.CanAssign)
	require.False(t, pendingDecorated.CanClaim)
}

func TestSharedParticipantListDecoratesPersistedGrants(t *testing.T) {
	h := participantPermissionHarness(t)
	h.dbmock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 ORDER BY name,id`).
		WithArgs("space").WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "target", "name", "owner_id", "resource_uid", "role", "state", "version", "agent_id", "registration_state"}).
		AddRow("participant-a", "companion", "ws-participant-owner/assistant", "Assistant", "u-owner", "uid-ws-participant-owner", "editor", "active", 2, "registered-agent", "ready").
		AddRow("participant-b", "companion", "ws-participant-owner/assistant", "Assistant", "u-owner", "stale-uid", "editor", "active", 1, "old-agent", "ready"))

	participants, err := h.participantList(t.Context(), sharedSpace{ID: "space", Role: "editor"}, identity{UserID: "u-owner"})
	require.NoError(t, err)
	require.Len(t, participants, 2)
	require.True(t, participants[0].ResourceValid)
	require.True(t, participants[0].CanAssign)
	require.True(t, participants[0].CanClaim)
	require.False(t, participants[1].ResourceValid)
	require.False(t, participants[1].CanAssign)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestSharedAgentCandidatesApplyVisibilityAndOwnerRole(t *testing.T) {
	for _, tc := range []struct {
		name string
		user string
		role string
		want int
	}{
		{name: "owner sees only candidates open to them", user: "space-owner", role: "owner", want: http.StatusOK},
		{name: "editor cannot invite", user: "u-x", role: "editor", want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := participantPermissionHarness(t)
			h.dbmock.ExpectQuery(`SELECT s.id,s.deployment_id,s.company_id,s.project_id,s.name,m.role FROM shared_spaces s JOIN shared_space_members m ON m.space_id=s.id WHERE s.id=\$1 AND s.deployment_id=\$2 AND m.user_id=\$3 AND m.role<>'revoked'`).
				WithArgs("space", "deployment-acme", tc.user).
				WillReturnRows(sqlmock.NewRows([]string{"id", "deployment_id", "company_id", "project_id", "name", "role"}).AddRow("space", "deployment-acme", "company", "project", "Team Space", tc.role))

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.SetPathValue("space", "space")
			w := httptest.NewRecorder()
			err := h.sharedAgentCandidates(w, r, identity{UserID: tc.user, Teams: []string{"sales-dept"}, DeploymentID: "deployment-acme"})
			if err != nil {
				sharedWriteError(w, err)
			}
			require.Equal(t, tc.want, w.Code, w.Body.String())
			if tc.want == http.StatusOK {
				var out struct {
					Candidates []sharedParticipant `json:"candidates"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
				require.Len(t, out.Candidates, 2, "private companions are omitted while the owner-visible companion and employee remain eligible")
				require.Equal(t, []string{"assistant", "employee-sales"}, []string{out.Candidates[0].Name, out.Candidates[1].Name})
				require.False(t, out.Candidates[1].CanApprove, "team manager may see the employee candidate without becoming its resource owner")
				require.NotContains(t, w.Body.String(), "private-helper")
			} else {
				require.True(t, strings.Contains(w.Body.String(), "仅空间负责人可邀请"))
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestSharedInvitationsAreVisibleToCurrentResourceManager(t *testing.T) {
	for _, tc := range []struct {
		name string
		user string
		want int
	}{
		{name: "resource owner receives pending invitation", user: "u-owner", want: 1},
		{name: "unrelated member cannot discover invitation", user: "u-x", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := participantPermissionHarness(t)
			h.cfg.aepDeploymentID = "deployment-acme"
			h.dbmock.ExpectQuery(`SELECT id,space_id FROM shared_space_participants WHERE state IN \('pending','active'\) ORDER BY id`).
				WillReturnRows(sqlmock.NewRows([]string{"id", "space_id"}).AddRow("participant-pending", "space"))
			h.dbmock.ExpectQuery(`SELECT id,deployment_id,company_id,project_id,name FROM shared_spaces WHERE id=\$1 AND deployment_id=\$2`).
				WithArgs("space", "deployment-acme").
				WillReturnRows(sqlmock.NewRows([]string{"id", "deployment_id", "company_id", "project_id", "name"}).AddRow("space", "deployment-acme", "company", "project", "Team Space"))
			h.dbmock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND id=\$2`).
				WithArgs("space", "participant-pending").
				WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "target", "name", "owner_id", "resource_uid", "role", "state", "version", "agent_id", "registration_state"}).
					AddRow("participant-pending", "companion", "ws-participant-owner/private-helper", "Private Helper", "u-owner", "uid-ws-participant-owner", "editor", "pending", 1, "", "pending"))

			w := httptest.NewRecorder()
			err := h.sharedInvitations(w, httptest.NewRequest(http.MethodGet, "/", nil), identity{UserID: tc.user, DeploymentID: "deployment-acme"})
			require.NoError(t, err)
			var out struct {
				Invitations []map[string]any `json:"invitations"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
			require.Len(t, out.Invitations, tc.want)
			if tc.want > 0 {
				participant := out.Invitations[0]["participant"].(map[string]any)
				require.Equal(t, "space", out.Invitations[0]["spaceId"])
				require.True(t, participant["canApprove"].(bool))
			} else {
				require.NotContains(t, w.Body.String(), "Private Helper")
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestSharedAssignmentLockedRejectsViewerBeforeTaskLookup(t *testing.T) {
	h := newApplyHarness(t)
	c, err := h.db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	s := sharedSpace{ID: "space", Role: "viewer"}

	_, err = h.sharedAssignmentLocked(t.Context(), c, s, "task", identity{UserID: "u-x"}, sharedAssignmentInput{}, false)
	require.Error(t, err)
	require.Equal(t, 400, err.(*sharedError).code, "incomplete requests fail before permission checks")

	_, err = h.sharedAssignmentLocked(t.Context(), c, s, "task", identity{UserID: "u-x"}, sharedAssignmentInput{
		RequestID: "viewer-assign", ExpectedAssignmentToken: "token", ParticipantID: "participant",
	}, false)
	require.Error(t, err)
	require.Equal(t, 403, err.(*sharedError).code, "read-only membership cannot reach replay, Paperclip, or task mutation")
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}
