package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func expectKnowledgeBindingLock(mock sqlmock.Sqlmock, id, state string) {
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT lifecycle_state FROM knowledge_base_registry.*FOR UPDATE`).WithArgs("demo", "10000", id).WillReturnRows(sqlmock.NewRows([]string{"lifecycle_state"}).AddRow(state))
}

func TestKnowledgeBindingsRejectDeletingAndMissingBases(t *testing.T) {
	for _, state := range []string{"active", "deleting", "missing"} {
		t.Run(state, func(t *testing.T) {
			h := newServicesHarness(t, newServicesStubs(t))
			configureManagedTest(t, h, "10000")
			expectKnowledgeBindingLock(h.dbmock, "kb-1", state)
			h.dbmock.ExpectRollback()
			tx, err := h.lockKnowledgeBindings(context.Background(), "demo", []string{"kb-1", "kb-1"})
			if state == "active" {
				require.NoError(t, err)
				require.NoError(t, tx.Rollback())
			} else {
				require.ErrorIs(t, err, errKnowledgeBaseInactive)
				require.Nil(t, tx)
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestKnowledgeBindingsLockSortedAndFailClosed(t *testing.T) {
	h := newServicesHarness(t, newServicesStubs(t))
	configureManagedTest(t, h, "10000")
	h.dbmock.ExpectBegin()
	h.dbmock.ExpectQuery(`SELECT lifecycle_state.*FOR UPDATE`).WithArgs("demo", "10000", "a").WillReturnRows(sqlmock.NewRows([]string{"lifecycle_state"}).AddRow("active"))
	h.dbmock.ExpectQuery(`SELECT lifecycle_state.*FOR UPDATE`).WithArgs("demo", "10000", "b").WillReturnError(errors.New("database unavailable"))
	h.dbmock.ExpectRollback()
	tx, err := h.lockKnowledgeBindings(context.Background(), "demo", []string{"b", "a"})
	require.Error(t, err)
	require.Nil(t, tx)
	_, err = h.lockKnowledgeBindings(context.Background(), "foreign", []string{"a"})
	require.Error(t, err)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestKnowledgeAuditIsScopedAndPagedWithoutUpstream(t *testing.T) {
	h := newServicesHarness(t, newServicesStubs(t))
	configureManagedTest(t, h, "10000")
	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"id", "operation_id", "actor_user_id", "action", "resource_type", "resource_id", "parent_resource_id", "outcome", "status_code", "created_at"}).AddRow(12, "kbop-1", "admin-user", "delete", "knowledge-base", "kb-1", "", "uncertain", 502, now).AddRow(11, "kbop-2", "admin-user", "create", "knowledge-base", "kb-2", "", "succeeded", 201, now)
	h.dbmock.ExpectQuery(`SELECT id,operation_id.*WHERE deployment_id=\$1 AND weknora_tenant_id=\$2 AND operation_id=\$3 ORDER BY id DESC LIMIT \$4`).WithArgs("demo", "10000", "kbop-1", 2).WillReturnRows(rows)
	w := httptest.NewRecorder()
	h.handleKnowledgeAudit(w, httptest.NewRequest("GET", "/?operationId=kbop-1&limit=1", nil), adminIdentityForManagedTest())
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"nextCursor":"12"`)
	require.Contains(t, w.Body.String(), `"outcome":"uncertain"`)
	require.NotContains(t, w.Body.String(), "kbop-2")
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

// 「查看本库操作记录」：库级动作写在 resource_id、文档级动作写在 parent_resource_id，
// 按库筛选必须两者都算；非法库 ID 直接拒绝，不用空列表伪装成功。
func TestKnowledgeAuditFiltersByKnowledgeBase(t *testing.T) {
	h := newServicesHarness(t, newServicesStubs(t))
	configureManagedTest(t, h, "10000")
	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"id", "operation_id", "actor_user_id", "action", "resource_type", "resource_id", "parent_resource_id", "outcome", "status_code", "created_at"}).
		AddRow(21, "kbop-3", "admin-user", "upload_file", "document", "doc-1", "kb-1", "succeeded", 202, now)
	h.dbmock.ExpectQuery(regexp.QuoteMeta(`SELECT id,operation_id,actor_user_id,action,resource_type,resource_id,parent_resource_id,outcome,status_code,created_at FROM knowledge_audit_events WHERE deployment_id=$1 AND weknora_tenant_id=$2 AND `+knowledgeAuditBasePredicate(3)+` ORDER BY id DESC LIMIT $4`)).
		WithArgs("demo", "10000", "kb-1", 51).WillReturnRows(rows)
	w := httptest.NewRecorder()
	h.handleKnowledgeAudit(w, httptest.NewRequest("GET", "/?knowledgeBaseId=kb-1", nil), adminIdentityForManagedTest())
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"parentResourceId":"kb-1"`)
	require.NoError(t, h.dbmock.ExpectationsWereMet())

	bad := httptest.NewRecorder()
	h.handleKnowledgeAudit(bad, httptest.NewRequest("GET", "/?knowledgeBaseId=kb%2F1", nil), adminIdentityForManagedTest())
	require.Equal(t, 400, bad.Code)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestKnowledgeAuditRejectsInvalidFiltersAndIdentity(t *testing.T) {
	for _, query := range []string{"tenantId=other", "cursor=-1", "limit=101", "actorUserId=a&actorUserId=b"} {
		h := newServicesHarness(t, newServicesStubs(t))
		configureManagedTest(t, h, "10000")
		w := httptest.NewRecorder()
		h.handleKnowledgeAudit(w, httptest.NewRequest("GET", "/?"+query, nil), adminIdentityForManagedTest())
		require.Equal(t, 400, w.Code)
		require.NoError(t, h.dbmock.ExpectationsWereMet())
	}
	h := newServicesHarness(t, newServicesStubs(t))
	configureManagedTest(t, h, "10000")
	w := httptest.NewRecorder()
	h.handleKnowledgeAudit(w, httptest.NewRequest("GET", "/", nil), identity{UserID: "member", DeploymentID: "demo"})
	require.Equal(t, 403, w.Code)
}

func TestManagedFAQCreateAndUnsupportedType(t *testing.T) {
	h := newServicesHarness(t, newServicesStubs(t))
	configureManagedTest(t, h, "10000")
	h.cfg.managedKnowledge.embeddingModelID = "embed-1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/me":
			_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
		case "/api/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"embed-1","type":"embedding"}]}`))
		case "/api/v1/knowledge-bases":
			var input map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
			require.Equal(t, "faq", input["type"])
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"data":{"id":"faq-1","name":"FAQ","tenant_id":10000,"type":"faq"}}`))
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	h.cfg.knowledgeAPIURL = server.URL
	h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id=\$2 WHERE`).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectBegin()
	h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET outcome='succeeded'`).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectExec(`INSERT INTO knowledge_base_registry`).WillReturnResult(sqlmock.NewResult(0, 1))
	h.dbmock.ExpectCommit()
	w := httptest.NewRecorder()
	h.handleManagedKnowledgeCreateBase(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"FAQ","type":"faq"}`)), adminIdentityForManagedTest())
	require.Equal(t, 201, w.Code, w.Body.String())
	w = httptest.NewRecorder()
	h.handleManagedKnowledgeCreateBase(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"name":"Wiki","type":"wiki"}`)), adminIdentityForManagedTest())
	require.Equal(t, 400, w.Code)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}

func TestKnowledgeRunRejectsNullObject(t *testing.T) {
	r := httptest.NewRequest("POST", "/", strings.NewReader("null"))
	require.Error(t, injectKnowledgeRunContext(r, "threads/t/runs", "opaque", "employee-1"))
}

func TestKnowledgeDeletePersistsBarrierBeforeUpstreamAndFailsClosed(t *testing.T) {
	for _, failure := range []string{"references", "scan", "state", "commit", "network", "audit-result"} {
		t.Run(failure, func(t *testing.T) {
			h := newServicesHarness(t, newServicesStubs(t))
			configureManagedTest(t, h, "10000")
			var deleteCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/auth/me" {
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
					return
				}
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"data":{"id":"kb-1","name":"Docs","tenant_id":10000}}`))
					return
				}
				deleteCalls.Add(1)
				if failure == "network" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			h.cfg.knowledgeAPIURL = server.URL
			h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry`).WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Docs"))
			expectKnowledgeBindingLock(h.dbmock, "kb-1", "active")
			rows := sqlmock.NewRows([]string{"spec_json"})
			if failure == "references" {
				rows.AddRow(`{"spec":{"knowledge":{"bases":[{"id":"kb-1"}]}}}`)
			}
			if failure == "scan" {
				rows.AddRow(`invalid-json`)
			}
			h.dbmock.ExpectQuery(`SELECT spec_json FROM requests`).WillReturnRows(rows)
			if failure == "references" || failure == "scan" {
				h.dbmock.ExpectRollback()
			} else {
				h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WillReturnResult(sqlmock.NewResult(0, 1))
				state := h.dbmock.ExpectExec(`UPDATE knowledge_base_registry SET lifecycle_state='deleting'`)
				if failure == "state" {
					state.WillReturnError(errors.New("write failed"))
					h.dbmock.ExpectRollback()
				} else {
					state.WillReturnResult(sqlmock.NewResult(0, 1))
					if failure == "commit" {
						h.dbmock.ExpectCommit().WillReturnError(errors.New("commit failed"))
					} else {
						h.dbmock.ExpectCommit()
						if failure == "network" {
							h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "kb-1", "uncertain", 502).WillReturnResult(sqlmock.NewResult(0, 1))
						} else {
							h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WillReturnError(errors.New("audit failed"))
						}
					}
				}
			}
			r := httptest.NewRequest("DELETE", "/", nil)
			r.SetPathValue("baseID", "kb-1")
			w := httptest.NewRecorder()
			h.handleManagedKnowledgeDeleteBase(w, r, adminIdentityForManagedTest())
			switch failure {
			case "references":
				require.Equal(t, 409, w.Code)
			case "network":
				require.Equal(t, 502, w.Code)
			default:
				require.Equal(t, 503, w.Code, w.Body.String())
			}
			if failure == "network" || failure == "audit-result" {
				require.Equal(t, int64(1), deleteCalls.Load())
			} else {
				require.Zero(t, deleteCalls.Load())
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestKnowledgeAuditCursorAndStorageFailures(t *testing.T) {
	for _, failure := range []string{"query", "scan", "rows", "empty"} {
		t.Run(failure, func(t *testing.T) {
			h := newServicesHarness(t, newServicesStubs(t))
			configureManagedTest(t, h, "10000")
			query := h.dbmock.ExpectQuery(`SELECT id,operation_id.*id<\$3.*ORDER BY id DESC LIMIT \$4`).WithArgs("demo", "10000", int64(12), 51)
			rows := sqlmock.NewRows([]string{"id", "operation_id", "actor_user_id", "action", "resource_type", "resource_id", "parent_resource_id", "outcome", "status_code", "created_at"})
			switch failure {
			case "query":
				query.WillReturnError(errors.New("private database detail"))
			case "scan":
				rows.AddRow(11, "op", "user", "read", "document", "doc", "kb", "succeeded", 200, "bad time")
				query.WillReturnRows(rows)
			case "rows":
				rows.AddRow(11, "op", "user", "read", "document", "doc", "kb", "succeeded", 200, time.Now()).RowError(0, errors.New("private database detail"))
				query.WillReturnRows(rows)
			default:
				query.WillReturnRows(rows)
			}
			w := httptest.NewRecorder()
			h.handleKnowledgeAudit(w, httptest.NewRequest("GET", "/?cursor=12", nil), adminIdentityForManagedTest())
			if failure == "empty" {
				require.Equal(t, 200, w.Code)
				require.Contains(t, w.Body.String(), `"items":[]`)
			} else {
				require.Equal(t, 503, w.Code)
				require.NotContains(t, w.Body.String(), "private database detail")
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
