package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type knowledgeGrantMap map[string]map[string]bool

func (m knowledgeGrantMap) hasKnowledgeGrant(_ context.Context, deployment, tenant, knowledgeBase string, subjects []knowledgeGrantSubject) (bool, error) {
	key := deployment + "/" + tenant + "/" + knowledgeBase
	for _, subject := range subjects {
		if m[key][subject.Type+":"+subject.ID] {
			return true, nil
		}
	}
	return false, nil
}

func TestEffectiveKnowledgeBasesRequiresAllScopes(t *testing.T) {
	requester := identity{UserID: "user-1", DeploymentID: "dep-1", Teams: []string{"team-1"}}
	grants := knowledgeGrantMap{
		"dep-1/tenant-1/kb-user":             {"user:user-1": true},
		"dep-1/tenant-1/kb-team":             {"team:team-1": true},
		"dep-1/tenant-1/kb-other":            {"user:user-2": true},
		"dep-2/tenant-1/kb-other-deployment": {"user:user-1": true},
		"dep-1/tenant-2/kb-other-tenant":     {"user:user-1": true},
	}

	got, err := effectiveKnowledgeBases(context.Background(), requester, "tenant-1",
		[]string{"kb-user", "kb-team", "kb-other", "kb-other-deployment", "kb-other-tenant"},
		[]string{"kb-user", "kb-team", "kb-other", "kb-other-deployment", "kb-other-tenant", "forged"}, grants)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"kb-user", "kb-team"}, got)
}

func TestEffectiveKnowledgeBasesEmptyOrMissingScopeDenies(t *testing.T) {
	requester := identity{UserID: "user-1", DeploymentID: "dep-1"}
	grants := knowledgeGrantMap{"dep-1/tenant-1/kb-1": {"user:user-1": true}}
	tests := []struct {
		name      string
		tenant    string
		bindings  []string
		requested []string
		requester identity
	}{
		{name: "nil bindings", tenant: "tenant-1", bindings: nil, requested: []string{"kb-1"}, requester: requester},
		{name: "empty bindings", tenant: "tenant-1", bindings: []string{}, requested: []string{"kb-1"}, requester: requester},
		{name: "nil caller selection", tenant: "tenant-1", bindings: []string{"kb-1"}, requested: nil, requester: requester},
		{name: "empty caller selection", tenant: "tenant-1", bindings: []string{"kb-1"}, requested: []string{}, requester: requester},
		{name: "no requester", tenant: "tenant-1", bindings: []string{"kb-1"}, requested: []string{"kb-1"}, requester: identity{DeploymentID: "dep-1"}},
		{name: "no deployment", tenant: "tenant-1", bindings: []string{"kb-1"}, requested: []string{"kb-1"}, requester: identity{UserID: "user-1"}},
		{name: "no tenant", bindings: []string{"kb-1"}, requested: []string{"kb-1"}, requester: requester},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := effectiveKnowledgeBases(context.Background(), tt.requester, tt.tenant, tt.bindings, tt.requested, grants)
			require.NoError(t, err)
			assert.Empty(t, got)
		})
	}
}

func TestEffectiveKnowledgeBasesGrantLookupFailureFailsClosed(t *testing.T) {
	lookup := failingKnowledgeGrantLookup{}
	got, err := effectiveKnowledgeBases(context.Background(), identity{UserID: "u", DeploymentID: "d"}, "t", []string{"kb"}, []string{"kb"}, lookup)
	require.Error(t, err)
	assert.Nil(t, got)
}

func TestEffectiveKnowledgeBasesAdminStillNeedsExplicitGrant(t *testing.T) {
	requester := identity{UserID: "admin-1", DeploymentID: "dep-1", Roles: []string{"platform-admin"}}
	got, err := effectiveKnowledgeBases(context.Background(), requester, "tenant-1", []string{"kb-1"}, []string{"kb-1"}, knowledgeGrantMap{})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestSQLKnowledgeGrantLookupScopesByDeploymentTenantAndRegisteredKB(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		mock.ExpectClose()
		require.NoError(t, db.Close())
	})
	mock.ExpectQuery(`(?s)SELECT EXISTS \(.*FROM knowledge_base_registry r.*JOIN knowledge_base_grants g.*r\.deployment_id=\$1.*r\.weknora_tenant_id=\$2.*r\.knowledge_base_id=\$3.*g\.subject_type=\$4 AND g\.subject_id=\$5`).
		WithArgs("dep-1", "tenant-1", "kb-1", "team", "team-1").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	found, err := (sqlKnowledgeGrantLookup{db: db}).hasKnowledgeGrant(context.Background(), "dep-1", "tenant-1", "kb-1", []knowledgeGrantSubject{{Type: "team", ID: "team-1"}})
	require.NoError(t, err)
	assert.True(t, found)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestKnowledgeBaseGrantsHandlerRequiresAdminEvenWithoutRouteWrapper(t *testing.T) {
	h := newAPIHarness(t)
	h.cfg.managedKnowledge.tenantID = "tenant-1"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/managed/bases/kb-1/grants", nil)
	req.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.handleKnowledgeBaseGrants(w, req, identity{UserID: "ordinary", DeploymentID: "dep-1"})
	require.Equal(t, http.StatusForbidden, w.Code)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestKnowledgeBaseGrantsPutExplicitEmptyRevokesAll(t *testing.T) {
	h := newAPIHarness(t)
	h.cfg.aepDeploymentID = "demo"
	h.cfg.managedKnowledge.tenantID = "tenant-1"
	request := httptest.NewRequest(http.MethodPut, "/api/v1/knowledge/managed/bases/kb-1/grants", strings.NewReader(`{"grants":[]}`))
	request.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.dbmock.ExpectQuery(`SELECT knowledge_base_id FROM knowledge_base_registry WHERE deployment_id=\$1 AND weknora_tenant_id=\$2 AND knowledge_base_id=\$3`).
		WithArgs("demo", "tenant-1", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"knowledge_base_id"}).AddRow("kb-1"))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "tenant-1", "admin-user", "replace_grants", "knowledge_base", "kb-1", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectBegin()
	h.dbmock.ExpectExec(`DELETE FROM knowledge_base_grants WHERE deployment_id=\$1 AND weknora_tenant_id=\$2 AND knowledge_base_id=\$3`).
		WithArgs("demo", "tenant-1", "kb-1").WillReturnResult(sqlmock.NewResult(0, 4))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET outcome='succeeded',status_code=\$2 WHERE operation_id=\$1`).
		WithArgs(sqlmock.AnyArg(), http.StatusOK).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectCommit()
	h.handleKnowledgeBaseGrants(w, request, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"grants":[]`)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestKnowledgeBaseGrantsPutRejectsExtraFields(t *testing.T) {
	h := newAPIHarness(t)
	h.cfg.aepDeploymentID = "demo"
	h.cfg.managedKnowledge.tenantID = "tenant-1"
	request := httptest.NewRequest(http.MethodPut, "/api/v1/knowledge/managed/bases/kb-1/grants", strings.NewReader(`{"grants":[],"deployment_id":"other"}`))
	request.SetPathValue("baseID", "kb-1")
	w := httptest.NewRecorder()
	h.dbmock.ExpectQuery(`SELECT knowledge_base_id FROM knowledge_base_registry WHERE deployment_id=\$1 AND weknora_tenant_id=\$2 AND knowledge_base_id=\$3`).
		WithArgs("demo", "tenant-1", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"knowledge_base_id"}).AddRow("kb-1"))
	h.handleKnowledgeBaseGrants(w, request, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

const knowledgeGrantQueryPattern = `(?s)SELECT EXISTS \(.*FROM knowledge_base_registry r.*JOIN knowledge_base_grants g.*r\.deployment_id=\$1.*r\.weknora_tenant_id=\$2.*r\.knowledge_base_id=\$3.*g\.subject_type=\$4 AND g\.subject_id=\$5`

func expectKnowledgeGrant(mock sqlmock.Sqlmock, deployment, tenant, kb, subjectType, subjectID string, allowed bool) {
	mock.ExpectQuery(knowledgeGrantQueryPattern).
		WithArgs(deployment, tenant, kb, subjectType, subjectID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(allowed))
}

func newKnowledgeAccessPortalFixture(t *testing.T, employee *dev1alpha1.DigitalEmployee, responder http.HandlerFunc) (*portal, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, mock.ExpectationsWereMet())
		mock.ExpectClose()
		require.NoError(t, db.Close())
	})
	scheme := k8sruntime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	objects := []client.Object{}
	if employee != nil {
		objects = append(objects, employee)
		objects = append(objects, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "knowledge-test", Name: memkeySecretName(employee.Name)},
			Data:       map[string][]byte{"DEER_FLOW_INTERNAL_AUTH_TOKEN": []byte("runtime-token")},
		})
	}
	server := httptest.NewServer(responder)
	t.Cleanup(server.Close)
	return &portal{
		cfg: config{namespace: "knowledge-test", managedKnowledge: managedKnowledgeConfig{tenantID: "tenant-1"}},
		db:  db,
		k8s: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		aep: &aepAuthenticator{baseURL: server.URL, http: server.Client()},
	}, mock
}

func knowledgeAccessEmployee(bases []dev1alpha1.KnowledgeBaseRef, visibility *dev1alpha1.VisibilitySpec) *dev1alpha1.DigitalEmployee {
	return &dev1alpha1.DigitalEmployee{
		ObjectMeta: metav1.ObjectMeta{Namespace: "knowledge-test", Name: "employee-1"},
		Spec: dev1alpha1.DigitalEmployeeSpec{
			Owner: "someone-else", Visibility: visibility,
			Knowledge: &dev1alpha1.KnowledgeSpec{Bases: bases},
		},
	}
}

func knowledgeAEPResponse(teams []string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{
			"user":  map[string]any{"id": "user-1", "displayName": "User", "kind": "human", "teamIds": teams},
			"roles": []string{}, "deploymentId": "dep-1",
		})
		_, _ = w.Write(body)
	}
}

func TestResolveEmployeeKnowledgeAccessRevalidatesAndDeniesRevokedTeamGrantNextRequest(t *testing.T) {
	var aepCalls atomic.Int32
	responder := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		teams := []string{"team-1"}
		if aepCalls.Add(1) > 1 {
			teams = []string{}
		}
		body, _ := json.Marshal(map[string]any{
			"user":  map[string]any{"id": "user-1", "displayName": "User", "kind": "human", "teamIds": teams},
			"roles": []string{}, "deploymentId": "dep-1",
		})
		_, _ = w.Write(body)
	})
	p, mock := newKnowledgeAccessPortalFixture(t, knowledgeAccessEmployee(
		[]dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}, &dev1alpha1.VisibilitySpec{Mode: "all"},
	), responder)
	// KB visibility is not enough: the user has no direct grant, and receives
	// access on the first call only through the currently verified team grant.
	expectKnowledgeGrant(mock, "dep-1", "tenant-1", "kb-1", "user", "user-1", false)
	expectKnowledgeGrant(mock, "dep-1", "tenant-1", "kb-1", "team", "team-1", true)
	expectKnowledgeGrant(mock, "dep-1", "tenant-1", "kb-1", "user", "user-1", false)
	session := identity{Token: "stored-token", UserID: "user-1", DeploymentID: "dep-1", Teams: []string{"team-1"}}
	first, err := p.resolveEmployeeKnowledgeAccess(context.Background(), session, "employee-1", []string{"kb-1"})
	require.NoError(t, err)
	assert.Equal(t, []string{"kb-1"}, first)
	second, err := p.resolveEmployeeKnowledgeAccess(context.Background(), session, "employee-1", []string{"kb-1"})
	require.NoError(t, err)
	assert.Empty(t, second)
	assert.EqualValues(t, 2, aepCalls.Load(), "the cached session team must never replace current AEP membership")
}

func TestAuthorizeEmployeeKnowledgeBaseRechecksCurrentGrantForEachRead(t *testing.T) {
	var aepCalls atomic.Int32
	p, mock := newKnowledgeAccessPortalFixture(t, knowledgeAccessEmployee(
		[]dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}, &dev1alpha1.VisibilitySpec{Mode: "all"},
	), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		teams := []string{"team-1"}
		if aepCalls.Add(1) > 1 {
			teams = nil
		}
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{
			"user":  map[string]any{"id": "user-1", "kind": "human", "teamIds": teams},
			"roles": []string{}, "deploymentId": "dep-1",
		})
		_, _ = w.Write(body)
	}))
	session := identity{Token: "stored-token", UserID: "user-1", DeploymentID: "dep-1"}
	expectKnowledgeGrant(mock, "dep-1", "tenant-1", "kb-1", "user", "user-1", false)
	expectKnowledgeGrant(mock, "dep-1", "tenant-1", "kb-1", "team", "team-1", true)
	require.NoError(t, p.authorizeEmployeeKnowledgeBase(context.Background(), session, "employee-1", "kb-1"))
	expectKnowledgeGrant(mock, "dep-1", "tenant-1", "kb-1", "user", "user-1", false)
	err := p.authorizeEmployeeKnowledgeBase(context.Background(), session, "employee-1", "kb-1")
	require.ErrorIs(t, err, errKnowledgeAccessDenied)
	require.ErrorIs(t, p.authorizeEmployeeKnowledgeBase(context.Background(), session, "employee-1", ""), errKnowledgeAccessDenied)
	assert.EqualValues(t, 2, aepCalls.Load())
}

func TestKnowledgeRuntimeAuthorizeUsesOpaqueSessionAndRechecksRevocation(t *testing.T) {
	var aepCalls atomic.Int32
	p, mock := newKnowledgeAccessPortalFixture(t, knowledgeAccessEmployee(
		[]dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}, &dev1alpha1.VisibilitySpec{Mode: "all"},
	), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		teams := []string{"team-1"}
		if aepCalls.Add(1) > 1 {
			teams = nil
		}
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{
			"user":  map[string]any{"id": "user-1", "kind": "human", "teamIds": teams},
			"roles": []string{}, "deploymentId": "dep-1",
		})
		_, _ = w.Write(body)
	}))
	p.sessions = newSessionStore(time.Hour)
	sessionToken, _ := p.sessions.issue(identity{Token: "stored-aep-token", UserID: "user-1", DeploymentID: "dep-1"})
	mock.ExpectExec(`INSERT INTO knowledge_runtime_contexts \(handle_hash,session_token,employee_name,expires_at\) VALUES`).
		WithArgs(sqlmock.AnyArg(), sessionToken, "employee-1", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	contextHandle, err := p.issueKnowledgeRuntimeContext(context.Background(), sessionToken, "employee-1")
	require.NoError(t, err)
	assert.NotContains(t, contextHandle, "stored-aep-token")

	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/internal/v1/knowledge/employees/employee-1/authorize", strings.NewReader(`{"knowledge_base_ids":["kb-1"],"action":"search"}`))
		r.SetPathValue("name", "employee-1")
		r.Header.Set("X-DeerFlow-Internal-Token", "runtime-token")
		r.Header.Set(knowledgeContextHeader, contextHandle)
		w := httptest.NewRecorder()
		p.handleKnowledgeRuntimeAuthorize(w, r)
		return w
	}
	mock.ExpectQuery(`SELECT session_token FROM knowledge_runtime_contexts WHERE handle_hash=\$1 AND employee_name=\$2 AND expires_at > now\(\)`).
		WithArgs(sqlmock.AnyArg(), "employee-1").WillReturnRows(sqlmock.NewRows([]string{"session_token"}).AddRow(sessionToken))
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
		WithArgs(sessionToken).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	expectKnowledgeGrant(mock, "dep-1", "tenant-1", "kb-1", "user", "user-1", false)
	expectKnowledgeGrant(mock, "dep-1", "tenant-1", "kb-1", "team", "team-1", true)
	mock.ExpectExec(`INSERT INTO knowledge_audit_events \(operation_id,deployment_id,weknora_tenant_id,actor_user_id,action,resource_type,resource_id,parent_resource_id,outcome,status_code\) VALUES`).
		WithArgs(sqlmock.AnyArg(), "dep-1", "tenant-1", "user-1", "runtime_search", "knowledge_scope", "kb-1", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	first := request()
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	assert.Contains(t, first.Body.String(), `"knowledge_base_ids":["kb-1"]`)
	var authorized struct {
		OperationID string `json:"operation_id"`
	}
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &authorized))
	require.NotEmpty(t, authorized.OperationID)

	mock.ExpectQuery(`SELECT session_token FROM knowledge_runtime_contexts WHERE handle_hash=\$1 AND employee_name=\$2 AND expires_at > now\(\)`).
		WithArgs(sqlmock.AnyArg(), "employee-1").WillReturnRows(sqlmock.NewRows([]string{"session_token"}).AddRow(sessionToken))
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
		WithArgs(sessionToken).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE knowledge_audit_events SET outcome=\$4,status_code=\$5 WHERE operation_id=\$1 AND deployment_id=\$2 AND actor_user_id=\$3 AND outcome='pending'`).
		WithArgs(authorized.OperationID, "dep-1", "user-1", "succeeded", http.StatusOK).WillReturnResult(sqlmock.NewResult(0, 1))
	complete := httptest.NewRequest(http.MethodPost, "/internal/v1/knowledge/employees/employee-1/authorize", strings.NewReader(`{"operation_id":"`+authorized.OperationID+`","outcome":"succeeded","status_code":200}`))
	complete.SetPathValue("name", "employee-1")
	complete.Header.Set(knowledgeContextHeader, contextHandle)
	completeResponse := httptest.NewRecorder()
	p.handleKnowledgeRuntimeAuthorize(completeResponse, complete)
	require.Equal(t, http.StatusNoContent, completeResponse.Code, completeResponse.Body.String())

	mock.ExpectQuery(`SELECT session_token FROM knowledge_runtime_contexts WHERE handle_hash=\$1 AND employee_name=\$2 AND expires_at > now\(\)`).
		WithArgs(sqlmock.AnyArg(), "employee-1").WillReturnRows(sqlmock.NewRows([]string{"session_token"}).AddRow(sessionToken))
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
		WithArgs(sessionToken).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	expectKnowledgeGrant(mock, "dep-1", "tenant-1", "kb-1", "user", "user-1", false)
	second := request()
	require.Equal(t, http.StatusForbidden, second.Code, second.Body.String())
	assert.EqualValues(t, 2, aepCalls.Load(), "revocation must be rechecked for each new authorization; audit completion only records the already-performed operation")

	mock.ExpectQuery(`SELECT session_token FROM knowledge_runtime_contexts WHERE handle_hash=\$1 AND employee_name=\$2 AND expires_at > now\(\)`).
		WithArgs(sqlmock.AnyArg(), "employee-1").WillReturnRows(sqlmock.NewRows([]string{"session_token"}).AddRow(sessionToken))
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
		WithArgs(sessionToken).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
	third := request()
	require.Equal(t, http.StatusUnauthorized, third.Code, third.Body.String())
	assert.EqualValues(t, 2, aepCalls.Load(), "a deleted Portal session must be rejected before consulting the old AEP token")
}

func TestPortalRuntimeAuthorizationWithPythonWeKnoraBridge(t *testing.T) {
	python := os.Getenv("ZHIYUAN_KNOWLEDGE_TEST_PYTHON")
	if python == "" {
		candidate := `D:\rongxin\zhiyuan-knowledge-r5-test-2\Scripts\python.exe`
		if _, err := os.Stat(candidate); err == nil {
			python = candidate
		} else {
			var err error
			python, err = exec.LookPath("python")
			if err != nil {
				t.Skip("Python is unavailable; set ZHIYUAN_KNOWLEDGE_TEST_PYTHON to the locked WeKnora MCP test environment")
			}
		}
	}
	workDir := os.Getenv("ZHIYUAN_WEKNORA_MCP_SERVER_DIR")
	if workDir == "" {
		var err error
		workDir, err = filepath.Abs(filepath.Join("..", "..", "WeKnora", "mcp-server"))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(workDir, "weknora_mcp_server.py")); err != nil {
		t.Skip("WeKnora MCP source is unavailable; set ZHIYUAN_WEKNORA_MCP_SERVER_DIR")
	}

	var aepCalls atomic.Int32
	aep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		teams := []string(nil)
		if aepCalls.Add(1) <= 2 {
			teams = []string{"team-1"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user":  map[string]any{"id": "user-1", "kind": "human", "teamIds": teams},
			"roles": []string{}, "deploymentId": "dep-1",
		})
	}))
	defer aep.Close()
	p, mock := newKnowledgeAccessPortalFixture(t,
		knowledgeAccessEmployee([]dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}, &dev1alpha1.VisibilitySpec{Mode: "all"}),
		knowledgeAEPResponse(nil))
	p.aep = &aepAuthenticator{baseURL: aep.URL, http: aep.Client()}
	p.sessions = newSessionStore(time.Hour)
	sessionToken, _ := p.sessions.issue(identity{Token: "stored-aep-token", UserID: "user-1", DeploymentID: "dep-1"})
	contextHandle := strings.Repeat("A", 43)
	server := httptest.NewServer(p.routes())
	defer server.Close()

	expectAuthorization := func(knowledgeBaseID string, teamGrant bool) {
		t.Helper()
		mock.ExpectQuery(`SELECT session_token FROM knowledge_runtime_contexts WHERE handle_hash=\$1 AND employee_name=\$2 AND expires_at > now\(\)`).
			WithArgs(sqlmock.AnyArg(), "employee-1").WillReturnRows(sqlmock.NewRows([]string{"session_token"}).AddRow(sessionToken))
		mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
			WithArgs(sessionToken).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
		expectKnowledgeGrant(mock, "dep-1", "tenant-1", knowledgeBaseID, "user", "user-1", false)
		if teamGrant {
			expectKnowledgeGrant(mock, "dep-1", "tenant-1", knowledgeBaseID, "team", "team-1", true)
		}
	}
	expectOperationStart := func() {
		t.Helper()
		mock.ExpectExec(`INSERT INTO knowledge_audit_events \(operation_id,deployment_id,weknora_tenant_id,actor_user_id,action,resource_type,resource_id,parent_resource_id,outcome,status_code\) VALUES`).
			WithArgs(sqlmock.AnyArg(), "dep-1", "tenant-1", "user-1", "runtime_search", "knowledge_scope", "kb-1", "").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	expectAuthorization("kb-1", true)
	expectOperationStart()
	mock.ExpectQuery(`SELECT session_token FROM knowledge_runtime_contexts WHERE handle_hash=\$1 AND employee_name=\$2 AND expires_at > now\(\)`).
		WithArgs(sqlmock.AnyArg(), "employee-1").WillReturnRows(sqlmock.NewRows([]string{"session_token"}).AddRow(sessionToken))
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
		WithArgs(sessionToken).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE knowledge_audit_events SET outcome=\$4,status_code=\$5 WHERE operation_id=\$1 AND deployment_id=\$2 AND actor_user_id=\$3 AND outcome='pending'`).
		WithArgs(sqlmock.AnyArg(), "dep-1", "user-1", "succeeded", http.StatusOK).WillReturnResult(sqlmock.NewResult(0, 1))
	expectAuthorization("kb-1", true)
	expectOperationStart()
	mock.ExpectQuery(`SELECT session_token FROM knowledge_runtime_contexts WHERE handle_hash=\$1 AND employee_name=\$2 AND expires_at > now\(\)`).
		WithArgs(sqlmock.AnyArg(), "employee-1").WillReturnRows(sqlmock.NewRows([]string{"session_token"}).AddRow(sessionToken))
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
		WithArgs(sessionToken).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec(`UPDATE knowledge_audit_events SET outcome=\$4,status_code=\$5 WHERE operation_id=\$1 AND deployment_id=\$2 AND actor_user_id=\$3 AND outcome='pending'`).
		WithArgs(sqlmock.AnyArg(), "dep-1", "user-1", "failed", http.StatusBadGateway).WillReturnResult(sqlmock.NewResult(0, 1))
	// Current team membership is checked on every MCP call. Once it is
	// revoked, neither the previously allowed KB nor a caller-selected foreign
	// KB reaches the mocked WeKnora read.
	expectAuthorization("kb-1", false)
	// The foreign KB is rejected by the CR binding intersection before any
	// grant lookup, so there is intentionally no database expectation here.
	mock.ExpectQuery(`SELECT session_token FROM knowledge_runtime_contexts WHERE handle_hash=\$1 AND employee_name=\$2 AND expires_at > now\(\)`).
		WithArgs(sqlmock.AnyArg(), "employee-1").WillReturnRows(sqlmock.NewRows([]string{"session_token"}).AddRow(sessionToken))
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
		WithArgs(sessionToken).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// Logout/revocation is checked before AEP revalidation or grant lookup.
	mock.ExpectQuery(`SELECT session_token FROM knowledge_runtime_contexts WHERE handle_hash=\$1 AND employee_name=\$2 AND expires_at > now\(\)`).
		WithArgs(sqlmock.AnyArg(), "employee-1").WillReturnRows(sqlmock.NewRows([]string{"session_token"}).AddRow(sessionToken))
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions WHERE token=\$1 AND expires_at > now\(\)\)`).
		WithArgs(sessionToken).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	pythonScript := `import json, sys
import weknora_mcp_server as bridge

endpoint, context_handle = sys.argv[1:]
bridge.PORTAL_KNOWLEDGE_AUTH_REQUIRED = True
bridge.client.portal_knowledge_auth_url = endpoint
reads = []
def read(kb_id, query, config):
    reads.append((kb_id, query, config))
    if query == "upstream-failure":
        raise RuntimeError("mock WeKnora read failed")
    return {"knowledge_base_id": kb_id, "results": [{"content": "verified result"}]}
bridge.client.hybrid_search = read

def call(kb_id, query):
    return bridge.hybrid_search(kb_id, query, knowledge_employee_name="employee-1",
        knowledge_context_handle=context_handle)

success = call("kb-1", "allowed")
assert success["results"][0]["content"] == "verified result"
try:
    call("kb-1", "upstream-failure")
except RuntimeError as exc:
    assert str(exc) == "mock WeKnora read failed"
else:
    raise AssertionError("upstream read failure was swallowed")
for kb_id in ("kb-1", "kb-foreign"):
    try:
        call(kb_id, "must-not-read")
    except PermissionError:
        pass
    else:
        raise AssertionError("revoked or foreign KB was authorized")
try:
    call("kb-1", "revoked-session")
except PermissionError:
    pass
else:
    raise AssertionError("revoked Portal session was authorized")
assert len(reads) == 2, reads
print(json.dumps({"successful_result": success, "weknora_reads": len(reads)}))
`
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", pythonScript, server.URL+"/internal/v1/knowledge/employees", contextHandle)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), fmt.Sprintf("PYTHONPATH=%s", workDir), "PORTAL_KNOWLEDGE_AUTH_REQUIRED=true")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "Python WeKnora bridge failed: %s", output)
	assert.Contains(t, string(output), `"weknora_reads": 2`)
	assert.EqualValues(t, 4, aepCalls.Load(), "AEP team membership must be revalidated on each active-session authorization")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestResolveEmployeeKnowledgeAccessFailsClosedForMissingEmployeeScopeOrInfrastructure(t *testing.T) {
	goodResponder := knowledgeAEPResponse(nil)
	tests := []struct {
		name       string
		employee   *dev1alpha1.DigitalEmployee
		wantErr    bool
		k8sFailure bool
		dbFailure  bool
	}{
		{name: "employee missing"},
		{name: "visibility denied", employee: knowledgeAccessEmployee([]dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}, &dev1alpha1.VisibilitySpec{Mode: "restricted"})},
		{name: "nil bases deny", employee: knowledgeAccessEmployee(nil, &dev1alpha1.VisibilitySpec{Mode: "all"})},
		{name: "empty bases deny", employee: knowledgeAccessEmployee([]dev1alpha1.KnowledgeBaseRef{}, &dev1alpha1.VisibilitySpec{Mode: "all"})},
		{name: "Kubernetes failure", employee: knowledgeAccessEmployee([]dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}, &dev1alpha1.VisibilitySpec{Mode: "all"}), wantErr: true, k8sFailure: true},
		{name: "grant database failure", employee: knowledgeAccessEmployee([]dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}, &dev1alpha1.VisibilitySpec{Mode: "all"}), wantErr: true, dbFailure: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, mock := newKnowledgeAccessPortalFixture(t, tt.employee, goodResponder)
			if tt.k8sFailure {
				p.k8s = knowledgeAccessK8sErrorClient{Client: p.k8s, err: errors.New("kubernetes unavailable")}
			}
			if tt.dbFailure {
				mock.ExpectQuery(knowledgeGrantQueryPattern).
					WithArgs("dep-1", "tenant-1", "kb-1", "user", "user-1").
					WillReturnError(errors.New("database unavailable"))
			}
			session := identity{Token: "stored-token", UserID: "user-1", DeploymentID: "dep-1"}
			got, err := p.resolveEmployeeKnowledgeAccess(context.Background(), session, "employee-1", []string{"kb-1"})
			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, got)
			} else {
				require.NoError(t, err)
				assert.Empty(t, got)
			}
		})
	}
}

type knowledgeAccessK8sErrorClient struct {
	client.Client
	err error
}

func (c knowledgeAccessK8sErrorClient) Get(context.Context, k8stypes.NamespacedName, client.Object, ...client.GetOption) error {
	return c.err
}

type failingKnowledgeGrantLookup struct{}

func (failingKnowledgeGrantLookup) hasKnowledgeGrant(context.Context, string, string, string, []knowledgeGrantSubject) (bool, error) {
	return false, errors.New("database unavailable")
}

func TestVerifyKnowledgeRequesterRevalidatesCurrentAEPIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer stored-aep-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"user-1","displayName":"User","kind":"human","teamIds":[]},"roles":[],"deploymentId":"dep-1"}`))
	}))
	defer server.Close()
	p := &portal{aep: &aepAuthenticator{baseURL: server.URL, http: server.Client()}}
	got, err := p.verifyKnowledgeRequester(context.Background(), identity{Token: "stored-aep-token", UserID: "user-1", DeploymentID: "dep-1"})
	require.NoError(t, err)
	assert.Equal(t, "user-1", got.UserID)
	assert.Empty(t, got.Teams)
}

func TestVerifyKnowledgeRequesterObservesTeamRevocationOnNextCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"user":{"id":"user-1","displayName":"User","kind":"human","teamIds":["team-1"]},"roles":[],"deploymentId":"dep-1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"user":{"id":"user-1","displayName":"User","kind":"human","teamIds":[]},"roles":[],"deploymentId":"dep-1"}`))
	}))
	defer server.Close()
	p := &portal{aep: &aepAuthenticator{baseURL: server.URL, http: server.Client()}}
	sessionIdentity := identity{Token: "stored-token", UserID: "user-1", DeploymentID: "dep-1", Teams: []string{"team-1"}}
	first, err := p.verifyKnowledgeRequester(context.Background(), sessionIdentity)
	require.NoError(t, err)
	assert.Equal(t, []string{"team-1"}, first.Teams)
	second, err := p.verifyKnowledgeRequester(context.Background(), sessionIdentity)
	require.NoError(t, err)
	assert.Empty(t, second.Teams)
	assert.EqualValues(t, 2, calls.Load())
}

func TestVerifyKnowledgeRequesterRejectsUnavailableOrChangedIdentity(t *testing.T) {
	t.Run("no stored AEP token", func(t *testing.T) {
		p := &portal{aep: &aepAuthenticator{baseURL: "http://127.0.0.1", http: http.DefaultClient}}
		_, err := p.verifyKnowledgeRequester(context.Background(), identity{UserID: "user-1", DeploymentID: "dep-1"})
		require.ErrorIs(t, err, errKnowledgeIdentityUnavailable)
	})
	t.Run("subject changed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"user":{"id":"user-2","displayName":"User","kind":"human","teamIds":[]},"roles":[],"deploymentId":"dep-1"}`))
		}))
		defer server.Close()
		p := &portal{aep: &aepAuthenticator{baseURL: server.URL, http: server.Client()}}
		_, err := p.verifyKnowledgeRequester(context.Background(), identity{Token: "token", UserID: "user-1", DeploymentID: "dep-1"})
		require.ErrorIs(t, err, errKnowledgeIdentityUnavailable)
	})
	t.Run("AEP rejects token", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
		defer server.Close()
		p := &portal{aep: &aepAuthenticator{baseURL: server.URL, http: server.Client()}}
		_, err := p.verifyKnowledgeRequester(context.Background(), identity{Token: "revoked", UserID: "user-1", DeploymentID: "dep-1"})
		require.Error(t, err)
	})
}
