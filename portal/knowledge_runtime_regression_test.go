package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func runtimeRegressionFixture(t *testing.T) (*portal, sqlmock.Sqlmock, string) {
	t.Helper()
	p, mock := newKnowledgeAccessPortalFixture(t, knowledgeAccessEmployee(
		[]dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}, &dev1alpha1.VisibilitySpec{Mode: "all"},
	), knowledgeAEPResponse(nil))
	p.cfg.aepDeploymentID = "dep-1"
	p.cfg.managedKnowledge.tenantID = "10000"
	p.sessions = newSessionStore(time.Hour)
	token, _ := p.sessions.issue(identity{Token: "synthetic-access", UserID: "user-1", DeploymentID: "dep-1"})
	return p, mock, token
}

func expectRuntimeActive(mock sqlmock.Sqlmock, token string) {
	mock.ExpectQuery(`SELECT session_token FROM knowledge_runtime_contexts`).
		WithArgs(sqlmock.AnyArg(), "employee-1").WillReturnRows(sqlmock.NewRows([]string{"session_token"}).AddRow(token))
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM sessions`).WithArgs(token).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
}

func runtimeRegressionRequest(p *portal, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/internal/v1/knowledge/employees/employee-1/authorize", strings.NewReader(body))
	r.Header.Set(knowledgeContextHeader, strings.Repeat("A", 43))
	w := httptest.NewRecorder()
	p.routes().ServeHTTP(w, r)
	return w
}

func TestKnowledgeRuntimeRejectsInvalidScopeBeforeGrants(t *testing.T) {
	for _, body := range []string{
		`{`, `{"action":"write"}`, `{"action":"search"}`,
		`{"action":"search","knowledge_base_ids":[]}`,
		`{"action":"search","knowledge_base_ids":["../outside"]}`,
		`{"action":"search","knowledge_base_ids":["kb-1","kb-1"]}`,
		`{"action":"read_document","document_id":"doc-1","knowledge_base_ids":["kb-1"]}`,
		`{"operation_id":"op","outcome":"succeeded","status_code":200,"action":"search"}`,
	} {
		t.Run(body, func(t *testing.T) {
			p, mock, token := runtimeRegressionFixture(t)
			expectRuntimeActive(mock, token)
			w := runtimeRegressionRequest(p, body)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestKnowledgeRuntimeContextLookupFailsClosed(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{true: "expired handle", false: "database unavailable"}[missing], func(t *testing.T) {
			p, mock, _ := runtimeRegressionFixture(t)
			query := mock.ExpectQuery(`SELECT session_token FROM knowledge_runtime_contexts`).WithArgs(sqlmock.AnyArg(), "employee-1")
			status := http.StatusServiceUnavailable
			if missing {
				query.WillReturnRows(sqlmock.NewRows([]string{"session_token"}))
				status = http.StatusUnauthorized
			} else {
				query.WillReturnError(errors.New("synthetic unavailable"))
			}
			w := runtimeRegressionRequest(p, `{"action":"search","knowledge_base_ids":["kb-1"]}`)
			require.Equal(t, status, w.Code, w.Body.String())
		})
	}
}

func TestKnowledgeRuntimeDocumentAuthorizationKeepsAuditPending(t *testing.T) {
	for _, tc := range []struct {
		name         string
		grant        bool
		auditFailure bool
		status       int
	}{
		{name: "allowed document", grant: true, status: http.StatusOK},
		{name: "revoked document", status: http.StatusForbidden},
		{name: "audit unavailable", grant: true, auditFailure: true, status: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, mock, token := runtimeRegressionFixture(t)
			p.cfg.credentialsSecret = "knowledge-credentials"
			p.cfg.managedKnowledge.apiKeySecretField = "WEKNORA_API_KEY"
			require.NoError(t, p.k8s.Create(context.Background(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: p.cfg.namespace, Name: p.cfg.credentialsSecret},
				Data:       map[string][]byte{"WEKNORA_API_KEY": []byte("synthetic-only")},
			}))
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(`{"data":{"id":"doc-1","knowledge_base_id":"kb-1","tenant_id":10000}}`))
				case "/api/v1/knowledge-bases/kb-1":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Allowed","tenant_id":10000}}`))
				default:
					t.Errorf("unexpected upstream request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			p.cfg.knowledgeAPIURL = upstream.URL
			expectRuntimeActive(mock, token)
			mock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("dep-1", "10000", "kb-1").
				WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Allowed"))
			expectKnowledgeGrant(mock, "dep-1", "10000", "kb-1", "user", "user-1", tc.grant)
			if tc.grant {
				audit := mock.ExpectExec(`INSERT INTO knowledge_audit_events`).
					WithArgs(sqlmock.AnyArg(), "dep-1", "10000", "user-1", "runtime_read_document", "document", "doc-1", "kb-1", "employee-1", sqlmock.AnyArg())
				if tc.auditFailure {
					audit.WillReturnError(errors.New("synthetic audit unavailable"))
				} else {
					audit.WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			w := runtimeRegressionRequest(p, `{"document_id":"doc-1","action":"read_document"}`)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == http.StatusOK {
				require.Contains(t, w.Body.String(), `"knowledge_base_id":"kb-1"`)
				require.Contains(t, w.Body.String(), `"operation_id":`)
			}
			// No UPDATE is expected: authorization is not a completed read.
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestKnowledgeRuntimeDocumentRejectsUnavailableOrForeignMetadata(t *testing.T) {
	for _, tc := range []struct {
		name       string
		secret     bool
		tenantBody string
		docBody    string
		registered bool
		status     int
	}{
		{name: "missing credential", status: http.StatusServiceUnavailable},
		{name: "tenant mismatch", secret: true, tenantBody: `{"data":{"tenant":{"id":99999}}}`, status: http.StatusServiceUnavailable},
		{name: "malformed tenant", secret: true, tenantBody: `{`, status: http.StatusServiceUnavailable},
		{name: "foreign document", secret: true, tenantBody: `{"data":{"tenant":{"id":10000}}}`, docBody: `{"data":{"id":"doc-1","tenant_id":99999,"knowledge_base_id":"kb-1"}}`, status: http.StatusServiceUnavailable},
		{name: "malformed document", secret: true, tenantBody: `{"data":{"tenant":{"id":10000}}}`, docBody: `{`, status: http.StatusServiceUnavailable},
		{name: "unregistered parent", secret: true, tenantBody: `{"data":{"tenant":{"id":10000}}}`, docBody: `{"data":{"id":"doc-1","tenant_id":10000,"knowledge_base_id":"kb-1"}}`, registered: true, status: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, mock, token := runtimeRegressionFixture(t)
			p.cfg.credentialsSecret = "knowledge-credentials"
			p.cfg.managedKnowledge.apiKeySecretField = "WEKNORA_API_KEY"
			if tc.secret {
				require.NoError(t, p.k8s.Create(context.Background(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Namespace: p.cfg.namespace, Name: p.cfg.credentialsSecret},
					Data:       map[string][]byte{"WEKNORA_API_KEY": []byte("synthetic-only")},
				}))
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(tc.tenantBody))
				case "/api/v1/knowledge/doc-1":
					_, _ = w.Write([]byte(tc.docBody))
				default:
					t.Errorf("rejected document reached unexpected upstream %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			p.cfg.knowledgeAPIURL = upstream.URL
			expectRuntimeActive(mock, token)
			if tc.registered {
				mock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WithArgs("dep-1", "10000", "kb-1").
					WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}))
			}
			w := runtimeRegressionRequest(p, `{"document_id":"doc-1","action":"read_document"}`)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), `"operation_id"`)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestKnowledgeRuntimeRejectsUnknownAuditCompletion(t *testing.T) {
	p, mock, token := runtimeRegressionFixture(t)
	expectRuntimeActive(mock, token)
	mock.ExpectExec(`UPDATE knowledge_audit_events SET outcome`).
		WithArgs("unknown-operation", "dep-1", "user-1", "succeeded", 200, "10000", "employee-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	w := runtimeRegressionRequest(p, `{"operation_id":"unknown-operation","outcome":"succeeded","status_code":200}`)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKnowledgeRuntimeContextIssuanceDoesNotReturnAnUnstoredHandle(t *testing.T) {
	p, mock, token := runtimeRegressionFixture(t)
	mock.ExpectExec(`INSERT INTO knowledge_runtime_contexts`).
		WithArgs(sqlmock.AnyArg(), token, "employee-1", sqlmock.AnyArg()).
		WillReturnError(errors.New("synthetic database unavailable"))
	handle, err := p.issueKnowledgeRuntimeContext(context.Background(), token, "employee-1")
	require.ErrorIs(t, err, errKnowledgeContextUnavailable)
	require.Empty(t, handle)
	for _, input := range []struct{ token, employee string }{{"", "employee-1"}, {token, ""}} {
		handle, err = p.issueKnowledgeRuntimeContext(context.Background(), input.token, input.employee)
		require.ErrorIs(t, err, errKnowledgeContextUnavailable)
		require.Empty(t, handle)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestKnowledgeRuntimeAuditStartRejectsUnboundOrNonRuntimeIntents(t *testing.T) {
	p, mock, _ := runtimeRegressionFixture(t)
	requester := identity{UserID: "user-1", DeploymentID: "dep-1"}
	hash := strings.Repeat("a", 64)
	for _, input := range []struct {
		tenant, employee, hash, action string
	}{
		{"", "employee-1", hash, "runtime_search"},
		{"foreign", "employee-1", hash, "runtime_search"},
		{"10000", "", hash, "runtime_search"},
		{"10000", "../employee-1", hash, "runtime_search"},
		{"10000", "employee-1", "", "runtime_search"},
		{"10000", "employee-1", hash, "delete"},
	} {
		op, err := p.beginKnowledgeRuntimeAudit(context.Background(), requester, input.tenant, input.employee, input.hash, input.action, "knowledge_scope", "kb-1", "")
		require.Error(t, err)
		require.Empty(t, op)
	}
	for _, badIdentity := range []identity{{DeploymentID: "dep-1"}, {UserID: "user-1", DeploymentID: "foreign"}} {
		op, err := p.beginKnowledgeRuntimeAudit(context.Background(), badIdentity, "10000", "employee-1", hash, "runtime_search", "knowledge_scope", "kb-1", "")
		require.Error(t, err)
		require.Empty(t, op)
	}
	for _, unavailable := range []*portal{nil, {cfg: p.cfg}} {
		op, err := unavailable.beginKnowledgeRuntimeAudit(context.Background(), requester, "10000", "employee-1", hash, "runtime_search", "knowledge_scope", "kb-1", "")
		require.Error(t, err)
		require.Empty(t, op)
		require.Error(t, unavailable.completeKnowledgeRuntimeAudit(context.Background(), requester, "employee-1", hash, "operation", "succeeded", 200))
	}
	require.NoError(t, mock.ExpectationsWereMet(), "invalid intents must not touch the database")
}
