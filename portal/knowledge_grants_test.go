package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// grantsDirectoryStub is a minimal AEP admin directory: password login plus
// per-id user/team lookups, recording every lookup so a test can prove each
// subject is queried once.
type grantsDirectoryStub struct {
	server *httptest.Server
	mu     sync.Mutex
	down   bool
	// nil means "every id exists in this directory" (the regression harness
	// only cares about transaction semantics); non-nil means only the listed
	// ids exist and everything else is a 404.
	users   map[string]bool
	teams   map[string]bool
	queries []string
}

func newGrantsDirectoryStub(t *testing.T, down bool, users, teams map[string]bool) *grantsDirectoryStub {
	t.Helper()
	stub := &grantsDirectoryStub{down: down, users: users, teams: teams}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *grantsDirectoryStub) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/aep/v1/auth/password/login" {
		// Login keeps working while the directory is down, so a test that
		// wants the lookup-failure path exercises it instead of the
		// "no admin token" path.
		_, _ = w.Write([]byte(`{"accessToken":"grant-admin-token"}`))
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/aep/v1/admin/users/"):
		s.lookup(w, "user", strings.TrimPrefix(r.URL.Path, "/aep/v1/admin/users/"), s.users)
	case strings.HasPrefix(r.URL.Path, "/aep/v1/admin/teams/"):
		s.lookup(w, "team", strings.TrimPrefix(r.URL.Path, "/aep/v1/admin/teams/"), s.teams)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *grantsDirectoryStub) lookup(w http.ResponseWriter, kind, id string, known map[string]bool) {
	s.mu.Lock()
	s.queries = append(s.queries, kind+":"+id)
	down := s.down
	s.mu.Unlock()
	switch {
	case down:
		w.WriteHeader(http.StatusServiceUnavailable)
	case known != nil && !known[id]:
		w.WriteHeader(http.StatusNotFound)
	default:
		_, _ = w.Write([]byte(`{"id":"` + id + `"}`))
	}
}

func (s *grantsDirectoryStub) lookedUp() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

// newGrantsSubjectHarness wires a grant handler whose AEP directory is the
// stub. adminTokenCache is package level: it must be cleared or the login for
// this stub is skipped and whether the stub is even reached depends on test
// order.
func newGrantsSubjectHarness(t *testing.T, stub *grantsDirectoryStub) (*apiTestPortal, identity) {
	t.Helper()
	resetAdminTokenCache(t)
	t.Cleanup(func() { resetAdminTokenCache(t) })
	h := newAPIHarness(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "grant-platform-creds", Namespace: "digital-employees"},
		Data: map[string][]byte{
			"AEP_ADMIN_USER":     []byte("admin"),
			"AEP_ADMIN_PASSWORD": []byte("pw"),
		},
	})
	h.cfg.credentialsSecret = "grant-platform-creds"
	h.cfg.aepBaseURL = stub.server.URL
	h.cfg.aepDeploymentID = "demo"
	h.cfg.managedKnowledge.tenantID = "tenant-1"
	return h, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}}
}

func TestGrantReplaceRejectsUnknownSubjects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		unknown []string
	}{
		{"unknown user", `{"grants":[{"type":"user","id":"ghost-user"},{"type":"team","id":"team-1"}]}`, []string{"user:ghost-user"}},
		{"unknown team", `{"grants":[{"type":"team","id":"ghost-team"}]}`, []string{"team:ghost-team"}},
		// 未知清单按排序后的授权顺序给出（team 在 user 之前），便于控制台稳定展示。
		{"both unknown", `{"grants":[{"type":"user","id":"ghost-user"},{"type":"team","id":"ghost-team"}]}`, []string{"team:ghost-team", "user:ghost-user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newGrantsDirectoryStub(t, false, map[string]bool{"u-1": true}, map[string]bool{"team-1": true})
			h, ident := newGrantsSubjectHarness(t, stub)
			expectGrantRegistry(h.dbmock)
			// The DELETE is registered but deliberately left unfulfilled: it
			// must still be pending when the request ends, which proves the
			// check ran before the mutation and existing grants survive. A
			// "delete first, then 400" regression would fulfil it.
			h.dbmock.ExpectExec(`DELETE FROM knowledge_base_grants`).WithArgs("demo", "tenant-1", "kb-1")
			w := httptest.NewRecorder()
			h.handleKnowledgeBaseGrants(w, grantRequest(http.MethodPut, tc.body), ident)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			var payload struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
				Unknown []string `json:"unknown_subjects"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
			require.Equal(t, tc.unknown, payload.Unknown)
			require.Equal(t, "KNOWLEDGE_GRANT_SUBJECT_UNKNOWN", payload.Error.Code)
			for _, subject := range tc.unknown {
				require.Contains(t, payload.Error.Message, subject, "错误消息必须点名未知主体")
			}
			require.ErrorContains(t, h.dbmock.ExpectationsWereMet(), "DELETE FROM knowledge_base_grants",
				"未知主体必须在删除既有授权之前被拦下")
		})
	}
}

func TestGrantReplacePersistsExistingSubjectsAfterOneLookupEach(t *testing.T) {
	stub := newGrantsDirectoryStub(t, false, map[string]bool{"u-1": true}, map[string]bool{"team-1": true})
	h, ident := newGrantsSubjectHarness(t, stub)
	expectGrantRegistry(h.dbmock)
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
		WithArgs(sqlmock.AnyArg(), "demo", "tenant-1", "admin-user", "replace_grants", "knowledge_base", "kb-1", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectBegin()
	h.dbmock.ExpectExec(`DELETE FROM knowledge_base_grants`).WithArgs("demo", "tenant-1", "kb-1").WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_base_grants`).WithArgs("demo", "tenant-1", "kb-1", "team", "team-1", "admin-user").WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_base_grants`).WithArgs("demo", "tenant-1", "kb-1", "user", "u-1", "admin-user").WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET outcome='succeeded'`).WithArgs(sqlmock.AnyArg(), http.StatusOK).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectCommit()
	w := httptest.NewRecorder()
	h.handleKnowledgeBaseGrants(w, grantRequest(http.MethodPut, `{"grants":[{"type":"user","id":"u-1"},{"type":"team","id":"team-1"}]}`), ident)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"granted_by":"admin-user"`)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
	// Sorted grant order, one directory lookup per subject.
	require.Equal(t, []string{"team:team-1", "user:u-1"}, stub.lookedUp())
}

func TestGrantReplaceFailsClosedWhenSubjectDirectoryUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name          string
		adminSecretOK bool
	}{
		{"directory error", true},
		{"admin credentials missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newGrantsDirectoryStub(t, true, nil, nil)
			h, ident := newGrantsSubjectHarness(t, stub)
			if !tc.adminSecretOK {
				h.cfg.credentialsSecret = "absent-secret"
			}
			expectGrantRegistry(h.dbmock)
			h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).
				WithArgs(sqlmock.AnyArg(), "demo", "tenant-1", "admin-user", "replace_grants", "knowledge_base", "kb-1", "").
				WillReturnResult(sqlmock.NewResult(0, 1))
			h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2,outcome=\$3,status_code=\$4 WHERE operation_id=\$1`).
				WithArgs(sqlmock.AnyArg(), "kb-1", "failed", http.StatusBadGateway).
				WillReturnResult(sqlmock.NewResult(0, 1))
			// Upstream trouble must not touch existing grants either.
			h.dbmock.ExpectExec(`DELETE FROM knowledge_base_grants`).WithArgs("demo", "tenant-1", "kb-1")
			w := httptest.NewRecorder()
			h.handleKnowledgeBaseGrants(w, grantRequest(http.MethodPut, `{"grants":[{"type":"user","id":"u-1"}]}`), ident)
			require.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "暂不可用")
			require.ErrorContains(t, h.dbmock.ExpectationsWereMet(), "DELETE FROM knowledge_base_grants",
				"fail closed 不得进入删除阶段")
		})
	}
}
