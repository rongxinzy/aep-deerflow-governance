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

func assignmentParticipantRow(mock sqlmock.Sqlmock, predicate string, id, agent, state, registration string) {
	mock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND `+predicate).
		WithArgs("space", sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "target", "name", "owner_id", "resource_uid", "role", "state", "version", "agent_id", "registration_state"}).
		AddRow(id, "companion", "ws-participant-owner/assistant", "Assistant", "u-owner", "uid-ws-participant-owner", "editor", state, 1, agent, registration))
}

func assignmentParticipantMissingByAgent(mock sqlmock.Sqlmock, agent string) {
	mock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND agent_id=\$2`).
		WithArgs("space", agent).WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "target", "name", "owner_id", "resource_uid", "role", "state", "version", "agent_id", "registration_state"}))
}

func assignmentEpochRow(mock sqlmock.Sqlmock, task string, epoch int) {
	mock.ExpectQuery(`SELECT count\(\*\) FROM shared_space_commands WHERE space_id=\$1 AND \(\(task_id=\$2 AND action IN \('assign','claim'\)\) OR action='participant' OR \(task_id=\$2 AND action='handoff'\)\)`).
		WithArgs("space", task).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(epoch))
}

func assignmentReplayMiss(mock sqlmock.Sqlmock, request string) {
	mock.ExpectQuery(`SELECT actor_id,action,payload_hash,state,receipt FROM shared_space_commands WHERE space_id=\$1 AND request_id=\$2`).
		WithArgs("space", request).WillReturnRows(sqlmock.NewRows([]string{"actor_id", "action", "payload_hash", "state", "receipt"}))
}

func assignmentParticipantActive(mock sqlmock.Sqlmock, id, agent string) {
	assignmentParticipantRow(mock, `id=\$2`, id, agent, "active", "ready")
}

func assignmentParticipantByAgent(mock sqlmock.Sqlmock, agent string) {
	assignmentParticipantRow(mock, `agent_id=\$2`, "participant", agent, "active", "ready")
}

func assignmentCommandGate(mock sqlmock.Sqlmock, task string) {
	assignmentCommandGateValue(mock, task, false)
}

func assignmentCommandGateValue(mock sqlmock.Sqlmock, task string, blocked bool) {
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM shared_space_commands WHERE space_id=\$1 AND task_id=\$2 AND state<>'applied'\)`).
		WithArgs("space", task).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(blocked))
}

func assignmentServer(t *testing.T, current *map[string]any) (*httptest.Server, *int, *int) {
	t.Helper()
	patches, checkouts := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-paperclip" {
			t.Errorf("unexpected Paperclip authorization header")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/issues/task" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, *current)
		case http.MethodPatch:
			patches++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode assignment patch: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if body["assigneeAgentId"] != "agent-a" || body["assigneeUserId"] != nil || body["status"] != "todo" {
				t.Errorf("unexpected assignment patch body: %#v", body)
				http.Error(w, "bad assignment", http.StatusBadRequest)
				return
			}
			(*current)["assigneeAgentId"] = body["assigneeAgentId"]
			(*current)["assigneeUserId"] = nil
			(*current)["status"] = body["status"]
			(*current)["updatedAt"] = "v2"
			writeJSON(w, http.StatusOK, *current)
		case http.MethodPost:
			checkouts++
			http.Error(w, "unexpected checkout", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &patches, &checkouts
}

func assignmentTask(status string) map[string]any {
	return map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": status, "updatedAt": "v1"}
}

func TestSharedAssignmentLockedWritesOnlyFreshAuthorizedAssignment(t *testing.T) {
	t.Run("editor assignment patches and verifies the final owner", func(t *testing.T) {
		h := participantPermissionHarness(t)
		current := assignmentTask("todo")
		server, patches, checkouts := assignmentServer(t, &current)
		h.cfg.paperclipBaseURL = server.URL
		h.cfg.paperclipToken = "synthetic-paperclip"
		c, err := h.db.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		input := sharedAssignmentInput{RequestID: "assign-one", ExpectedAssignmentToken: sharedAssignmentToken(current, 0), ParticipantID: "participant"}
		assignmentReplayMiss(h.dbmock, input.RequestID)
		assignmentEpochRow(h.dbmock, "task", 0)
		assignmentParticipantActive(h.dbmock, "participant", "agent-a")
		assignmentCommandGate(h.dbmock, "task")
		h.dbmock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM shared_space_runtime_bindings WHERE space_id=\$1 AND task_id=\$2 AND binding_state='active'\)`).
			WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		assignmentCommandGate(h.dbmock, "task")
		h.dbmock.ExpectExec(`INSERT INTO shared_space_commands`).
			WithArgs("space", "assign-one", "task", "u-owner", "assign", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
		// The assignment command is counted when the fresh Paperclip readback
		// is converted to its response DTO.
		assignmentParticipantByAgent(h.dbmock, "agent-a")
		assignmentEpochRow(h.dbmock, "task", 1)
		h.dbmock.ExpectBegin()
		h.dbmock.ExpectExec(`UPDATE shared_space_commands SET state='applied',receipt=\$3 WHERE space_id=\$1 AND request_id=\$2`).
			WithArgs("space", "assign-one", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
		h.dbmock.ExpectCommit()

		out, err := h.sharedAssignmentLocked(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project", Role: "editor"}, "task", identity{UserID: "u-owner"}, input, false)
		require.NoError(t, err)
		encoded, err := json.Marshal(out)
		require.NoError(t, err)
		require.Contains(t, string(encoded), `"assigneeAgentId":"agent-a"`)
		require.Equal(t, 1, *patches)
		require.Zero(t, *checkouts)
		require.Equal(t, "agent-a", current["assigneeAgentId"], "the response comes from a fresh Paperclip readback")
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("stale token refuses before selecting a participant", func(t *testing.T) {
		h := participantPermissionHarness(t)
		current := assignmentTask("todo")
		server, patches, checkouts := assignmentServer(t, &current)
		h.cfg.paperclipBaseURL = server.URL
		h.cfg.paperclipToken = "synthetic-paperclip"
		c, err := h.db.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		input := sharedAssignmentInput{RequestID: "assign-stale", ExpectedAssignmentToken: "stale-token", ParticipantID: "participant"}
		assignmentReplayMiss(h.dbmock, input.RequestID)
		assignmentEpochRow(h.dbmock, "task", 0)
		_, err = h.sharedAssignmentLocked(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project", Role: "owner"}, "task", identity{UserID: "u-owner"}, input, false)
		require.Error(t, err)
		require.Equal(t, 409, err.(*sharedError).code)
		require.Zero(t, *patches)
		require.Zero(t, *checkouts)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("external owner refuses even with a current token", func(t *testing.T) {
		h := participantPermissionHarness(t)
		current := assignmentTask("todo")
		current["assigneeAgentId"] = "outside-agent"
		server, patches, checkouts := assignmentServer(t, &current)
		h.cfg.paperclipBaseURL = server.URL
		h.cfg.paperclipToken = "synthetic-paperclip"
		c, err := h.db.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		input := sharedAssignmentInput{RequestID: "assign-external", ExpectedAssignmentToken: sharedAssignmentToken(current, 0), ParticipantID: "participant"}
		assignmentReplayMiss(h.dbmock, input.RequestID)
		assignmentParticipantMissingByAgent(h.dbmock, "outside-agent")
		assignmentEpochRow(h.dbmock, "task", 0)
		_, err = h.sharedAssignmentLocked(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project", Role: "owner"}, "task", identity{UserID: "u-owner"}, input, false)
		require.Error(t, err)
		require.Equal(t, 409, err.(*sharedError).code)
		require.Zero(t, *patches)
		require.Zero(t, *checkouts)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	for _, tc := range []struct {
		name   string
		status string
		runID  string
		grant  string
		code   int
	}{
		{name: "in progress cannot be reassigned", status: "in_progress", code: 409},
		{name: "native run cannot be taken over", status: "todo", runID: "native-run", code: 409},
		{name: "pending participant has no assignment authority", status: "todo", grant: "pending", code: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := participantPermissionHarness(t)
			current := assignmentTask(tc.status)
			if tc.runID != "" {
				current["executionRunId"] = tc.runID
			}
			server, patches, checkouts := assignmentServer(t, &current)
			h.cfg.paperclipBaseURL = server.URL
			h.cfg.paperclipToken = "synthetic-paperclip"
			c, err := h.db.Conn(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			input := sharedAssignmentInput{RequestID: "assign-" + strings.ReplaceAll(tc.name, " ", "-"), ExpectedAssignmentToken: sharedAssignmentToken(current, 0), ParticipantID: "participant"}
			assignmentReplayMiss(h.dbmock, input.RequestID)
			assignmentEpochRow(h.dbmock, "task", 0)
			state, registration, agent := "active", "ready", "agent-a"
			if tc.grant == "pending" {
				state, registration, agent = "pending", "pending", ""
			}
			assignmentParticipantRow(h.dbmock, `id=\$2`, "participant", agent, state, registration)
			if tc.runID == "" && tc.grant != "pending" {
				assignmentCommandGate(h.dbmock, "task")
				h.dbmock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM shared_space_runtime_bindings WHERE space_id=\$1 AND task_id=\$2 AND binding_state='active'\)`).
					WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
			}
			_, err = h.sharedAssignmentLocked(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project", Role: "owner"}, "task", identity{UserID: "u-owner"}, input, false)
			require.Error(t, err)
			require.Equal(t, tc.code, err.(*sharedError).code)
			require.Zero(t, *patches)
			require.Zero(t, *checkouts)
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}

	for _, tc := range []struct {
		name          string
		status        string
		gateBlocked   bool
		runtimeBound  bool
		wantCode      int
		needsGate     bool
		needsBoundRow bool
	}{
		{name: "completed task is immutable", status: "done", wantCode: 409},
		{name: "cancelled task is immutable", status: "cancelled", wantCode: 409},
		{name: "unknown task stage is rejected", status: "review", wantCode: 409, needsGate: true},
		{name: "pending command blocks assignment", status: "todo", gateBlocked: true, wantCode: 423, needsGate: true},
		{name: "runtime binding blocks assignment", status: "todo", runtimeBound: true, wantCode: 409, needsGate: true, needsBoundRow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := participantPermissionHarness(t)
			current := assignmentTask(tc.status)
			server, patches, checkouts := assignmentServer(t, &current)
			h.cfg.paperclipBaseURL = server.URL
			h.cfg.paperclipToken = "synthetic-paperclip"
			c, err := h.db.Conn(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			input := sharedAssignmentInput{RequestID: "assign-" + strings.ReplaceAll(tc.name, " ", "-"), ExpectedAssignmentToken: sharedAssignmentToken(current, 0), ParticipantID: "participant"}
			assignmentReplayMiss(h.dbmock, input.RequestID)
			assignmentEpochRow(h.dbmock, "task", 0)
			assignmentParticipantActive(h.dbmock, "participant", "agent-a")
			if tc.needsGate {
				assignmentCommandGateValue(h.dbmock, "task", tc.gateBlocked)
			}
			if tc.needsBoundRow {
				h.dbmock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM shared_space_runtime_bindings WHERE space_id=\$1 AND task_id=\$2 AND binding_state='active'\)`).
					WithArgs("space", "task").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(tc.runtimeBound))
			}
			_, err = h.sharedAssignmentLocked(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project", Role: "owner"}, "task", identity{UserID: "u-owner"}, input, false)
			require.Error(t, err)
			require.Equal(t, tc.wantCode, err.(*sharedError).code)
			require.Zero(t, *patches)
			require.Zero(t, *checkouts)
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}

	t.Run("applied request replays its original receipt", func(t *testing.T) {
		h := participantPermissionHarness(t)
		c, err := h.db.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		input := sharedAssignmentInput{RequestID: "assign-replay", ExpectedAssignmentToken: "token", ParticipantID: "participant"}
		hash := sharedHash([]any{"task", input})
		receipt := `{"state":"applied","requestId":"assign-replay"}`
		h.dbmock.ExpectQuery(`SELECT actor_id,action,payload_hash,state,receipt FROM shared_space_commands WHERE space_id=\$1 AND request_id=\$2`).
			WithArgs("space", input.RequestID).WillReturnRows(sqlmock.NewRows([]string{"actor_id", "action", "payload_hash", "state", "receipt"}).
			AddRow("u-owner", "assign", hash, "applied", []byte(receipt)))
		out, err := h.sharedAssignmentLocked(t.Context(), c, sharedSpace{ID: "space", Role: "owner"}, "task", identity{UserID: "u-owner"}, input, false)
		require.NoError(t, err)
		encoded, marshalErr := json.Marshal(out)
		require.NoError(t, marshalErr)
		require.JSONEq(t, receipt, string(encoded))
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("request id conflicts cannot cross actors or payloads", func(t *testing.T) {
		h := participantPermissionHarness(t)
		c, err := h.db.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		input := sharedAssignmentInput{RequestID: "assign-conflict", ExpectedAssignmentToken: "token", ParticipantID: "participant"}
		hash := sharedHash([]any{"task", input})
		h.dbmock.ExpectQuery(`SELECT actor_id,action,payload_hash,state,receipt FROM shared_space_commands WHERE space_id=\$1 AND request_id=\$2`).
			WithArgs("space", input.RequestID).WillReturnRows(sqlmock.NewRows([]string{"actor_id", "action", "payload_hash", "state", "receipt"}).
			AddRow("other-user", "assign", hash, "applied", []byte(`{"state":"applied"}`)))
		_, err = h.sharedAssignmentLocked(t.Context(), c, sharedSpace{ID: "space", Role: "owner"}, "task", identity{UserID: "u-owner"}, input, false)
		require.Error(t, err)
		require.Equal(t, 409, err.(*sharedError).code)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("task service read failure prevents any assignment write", func(t *testing.T) {
		h := participantPermissionHarness(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "missing task", http.StatusNotFound)
		}))
		t.Cleanup(server.Close)
		h.cfg.paperclipBaseURL = server.URL
		h.cfg.paperclipToken = "synthetic-paperclip"
		c, err := h.db.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		input := sharedAssignmentInput{RequestID: "assign-upstream-read-failed", ExpectedAssignmentToken: "token", ParticipantID: "participant"}
		assignmentReplayMiss(h.dbmock, input.RequestID)
		_, err = h.sharedAssignmentLocked(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project", Role: "owner"}, "task", identity{UserID: "u-owner"}, input, false)
		require.Error(t, err)
		require.Equal(t, 502, err.(*sharedError).code)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("missing participant is rejected after the current task read", func(t *testing.T) {
		h := participantPermissionHarness(t)
		current := assignmentTask("todo")
		server, patches, checkouts := assignmentServer(t, &current)
		h.cfg.paperclipBaseURL = server.URL
		h.cfg.paperclipToken = "synthetic-paperclip"
		c, err := h.db.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		input := sharedAssignmentInput{RequestID: "assign-missing", ExpectedAssignmentToken: sharedAssignmentToken(current, 0), ParticipantID: "missing-participant"}
		assignmentReplayMiss(h.dbmock, input.RequestID)
		assignmentEpochRow(h.dbmock, "task", 0)
		h.dbmock.ExpectQuery(`SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants WHERE space_id=\$1 AND id=\$2`).
			WithArgs("space", "missing-participant").WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "target", "name", "owner_id", "resource_uid", "role", "state", "version", "agent_id", "registration_state"}))
		_, err = h.sharedAssignmentLocked(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project", Role: "owner"}, "task", identity{UserID: "u-owner"}, input, false)
		require.Error(t, err)
		require.Equal(t, 404, err.(*sharedError).code)
		require.Zero(t, *patches)
		require.Zero(t, *checkouts)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})

	t.Run("same-agent claim persists a no-op receipt without checkout", func(t *testing.T) {
		h := participantPermissionHarness(t)
		current := assignmentTask("in_progress")
		current["assigneeAgentId"] = "agent-a"
		server, patches, checkouts := assignmentServer(t, &current)
		h.cfg.paperclipBaseURL = server.URL
		h.cfg.paperclipToken = "synthetic-paperclip"
		c, err := h.db.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		input := sharedAssignmentInput{RequestID: "claim-noop", ExpectedAssignmentToken: sharedAssignmentToken(current, 0), ParticipantID: "participant"}
		assignmentReplayMiss(h.dbmock, input.RequestID)
		assignmentParticipantByAgent(h.dbmock, "agent-a")
		assignmentEpochRow(h.dbmock, "task", 0)
		assignmentParticipantActive(h.dbmock, "participant", "agent-a")
		assignmentCommandGate(h.dbmock, "task")
		h.dbmock.ExpectBegin()
		assignmentCommandGate(h.dbmock, "task")
		h.dbmock.ExpectExec(`INSERT INTO shared_space_commands`).
			WithArgs("space", "claim-noop", "task", "u-owner", "claim", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
		assignmentEpochRow(h.dbmock, "task", 1)
		h.dbmock.ExpectExec(`UPDATE shared_space_commands SET state='applied',receipt=\$3 WHERE space_id=\$1 AND request_id=\$2`).
			WithArgs("space", "claim-noop", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
		h.dbmock.ExpectCommit()

		out, err := h.sharedAssignmentLocked(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project", Role: "editor"}, "task", identity{UserID: "u-owner"}, input, true)
		require.NoError(t, err)
		require.Zero(t, *patches)
		require.Zero(t, *checkouts, "a no-op claim must never POST a second checkout")
		receipt, marshalErr := json.Marshal(out)
		require.NoError(t, marshalErr)
		require.Contains(t, string(receipt), `"state":"applied"`)
		require.Contains(t, string(receipt), `"requestId":"claim-noop"`)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	})
}
