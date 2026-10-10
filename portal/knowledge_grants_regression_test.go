package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func grantRegressionHarness(t *testing.T) (*apiTestPortal, identity) {
	t.Helper()
	h := newAPIHarness(t)
	h.cfg.aepDeploymentID = "demo"
	h.cfg.managedKnowledge.tenantID = "tenant-1"
	return h, identity{UserID: "admin-user", DeploymentID: "demo", Roles: []string{"admin"}}
}

func expectGrantRegistry(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT knowledge_base_id FROM knowledge_base_registry`).WithArgs("demo", "tenant-1", "kb-1").WillReturnRows(sqlmock.NewRows([]string{"knowledge_base_id"}).AddRow("kb-1"))
}

func grantRequest(method, body string) *http.Request {
	r := httptest.NewRequest(method, "/api/v1/knowledge/managed/bases/kb-1/grants", strings.NewReader(body))
	r.SetPathValue("baseID", "kb-1")
	return r
}

func TestGrantRegistryAndInputFailuresCannotMutate(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"absent grants", `{}`}, {"null grants", `{"grants":null}`},
		{"invalid JSON", `{`}, {"invalid subject", `{"grants":[{"type":"all","id":"user"}]}`},
		{"empty subject", `{"grants":[{"type":"user","id":"  "}]}`},
		{"duplicate after trim", `{"grants":[{"type":"team","id":"team"},{"type":"team","id":" team "}]}`},
		{"nul subject", `{"grants":[{"type":"user","id":"user\u0000"}]}`},
		{"spoofed grant actor", `{"grants":[{"type":"user","id":"user","granted_by":"other-user"}]}`},
		{"spoofed creation date", `{"grants":[{"type":"user","id":"user","created_at":"2026-10-10T00:00:00Z"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ident := grantRegressionHarness(t)
			expectGrantRegistry(h.dbmock)
			w := httptest.NewRecorder()
			h.handleKnowledgeBaseGrants(w, grantRequest(http.MethodPut, tc.body), ident)
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.NoError(t, h.dbmock.ExpectationsWereMet(), "rejected input must not start a mutation or create a success audit")
		})
	}
	for _, failure := range []string{"deployment", "storage", "id", "unregistered", "registry error", "method"} {
		t.Run(failure, func(t *testing.T) {
			h, ident := grantRegressionHarness(t)
			r := grantRequest(http.MethodGet, "")
			status := http.StatusServiceUnavailable
			switch failure {
			case "deployment":
				ident.DeploymentID = "foreign"
				status = http.StatusForbidden
			case "storage":
				h.cfg.managedKnowledge.tenantID = ""
			case "id":
				r.SetPathValue("baseID", "../foreign")
				status = http.StatusBadRequest
			case "unregistered":
				h.dbmock.ExpectQuery(`SELECT knowledge_base_id`).WillReturnError(sql.ErrNoRows)
				status = http.StatusNotFound
			case "registry error":
				h.dbmock.ExpectQuery(`SELECT knowledge_base_id`).WillReturnError(errors.New("db unavailable"))
			case "method":
				expectGrantRegistry(h.dbmock)
				r.Method = http.MethodPatch
				status = http.StatusMethodNotAllowed
			}
			w := httptest.NewRecorder()
			h.handleKnowledgeBaseGrants(w, r, ident)
			require.Equal(t, status, w.Code)
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestGrantReadReturnsScopedSubjectsOrFailsClosed(t *testing.T) {
	for _, stage := range []string{"success", "query", "scan", "rows", "audit"} {
		t.Run(stage, func(t *testing.T) {
			h, ident := grantRegressionHarness(t)
			expectGrantRegistry(h.dbmock)
			query := h.dbmock.ExpectQuery(`SELECT subject_type,subject_id,granted_by,created_at`).WithArgs("demo", "tenant-1", "kb-1")
			rows := sqlmock.NewRows([]string{"subject_type", "subject_id", "granted_by", "created_at"})
			switch stage {
			case "query":
				query.WillReturnError(errors.New("read unavailable"))
			case "scan":
				query.WillReturnRows(rows.AddRow("user", "user-1", "operator", "invalid timestamp"))
			case "rows":
				query.WillReturnRows(rows.AddRow("user", "user-1", "operator", time.Now()).RowError(0, errors.New("iteration failed")))
			default:
				query.WillReturnRows(rows.AddRow("user", "user-1", "operator", time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)))
				audit := h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "tenant-1", "admin-user", "list_grants", "knowledge_base", "kb-1", "")
				if stage == "audit" {
					audit.WillReturnError(errors.New("audit unavailable"))
				} else {
					audit.WillReturnResult(sqlmock.NewResult(0, 1))
					h.dbmock.ExpectExec(`UPDATE knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "kb-1", "succeeded", http.StatusOK).WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			w := httptest.NewRecorder()
			h.handleKnowledgeBaseGrants(w, grantRequest(http.MethodGet, ""), ident)
			if stage == "success" {
				require.Equal(t, http.StatusOK, w.Code)
				var payload struct {
					Grants []knowledgeGrantDTO `json:"grants"`
					Tenant string              `json:"tenant_id"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &payload))
				require.Equal(t, "tenant-1", payload.Tenant)
				require.Len(t, payload.Grants, 1)
				require.Equal(t, "user-1", payload.Grants[0].ID)
			} else {
				require.Equal(t, http.StatusServiceUnavailable, w.Code)
				require.NotContains(t, w.Body.String(), "user-1")
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestGrantReplaceUsesActorAndAtomicFailureSemantics(t *testing.T) {
	for _, stage := range []string{"success", "intent", "begin", "delete", "insert", "audit", "commit"} {
		t.Run(stage, func(t *testing.T) {
			h, ident := grantRegressionHarness(t)
			expectGrantRegistry(h.dbmock)
			intent := h.dbmock.ExpectExec(`INSERT INTO knowledge_audit_events`).WithArgs(sqlmock.AnyArg(), "demo", "tenant-1", "admin-user", "replace_grants", "knowledge_base", "kb-1", "")
			failure := errors.New("synthetic storage failure")
			if stage == "intent" {
				intent.WillReturnError(failure)
			} else {
				intent.WillReturnResult(sqlmock.NewResult(0, 1))
				begin := h.dbmock.ExpectBegin()
				if stage == "begin" {
					begin.WillReturnError(failure)
				} else {
					begin.WillReturnError(nil)
					del := h.dbmock.ExpectExec(`DELETE FROM knowledge_base_grants`).WithArgs("demo", "tenant-1", "kb-1")
					if stage == "delete" {
						del.WillReturnError(failure)
					} else {
						del.WillReturnResult(sqlmock.NewResult(0, 3))
						insert := h.dbmock.ExpectExec(`INSERT INTO knowledge_base_grants`).WithArgs("demo", "tenant-1", "kb-1", "team", "a-team", "admin-user")
						if stage == "insert" {
							insert.WillReturnError(failure)
						} else {
							insert.WillReturnResult(sqlmock.NewResult(0, 1))
							h.dbmock.ExpectExec(`INSERT INTO knowledge_base_grants`).WithArgs("demo", "tenant-1", "kb-1", "user", "z-user", "admin-user").WillReturnResult(sqlmock.NewResult(0, 1))
							audit := h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET outcome='succeeded'`).WithArgs(sqlmock.AnyArg(), http.StatusOK)
							if stage == "audit" {
								audit.WillReturnError(failure)
							} else {
								audit.WillReturnResult(sqlmock.NewResult(0, 1))
								commit := h.dbmock.ExpectCommit()
								if stage == "commit" {
									commit.WillReturnError(failure)
								} else {
									commit.WillReturnError(nil)
								}
							}
						}
					}
				}
				if stage == "begin" || stage == "delete" || stage == "insert" || stage == "commit" {
					outcome := "failed"
					if stage == "commit" {
						outcome = "uncertain"
					}
					h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET resource_id`).WithArgs(sqlmock.AnyArg(), "kb-1", outcome, http.StatusServiceUnavailable).WillReturnResult(sqlmock.NewResult(0, 1))
				}
				if stage == "delete" || stage == "insert" || stage == "audit" {
					h.dbmock.ExpectRollback()
				}
			}
			w := httptest.NewRecorder()
			h.handleKnowledgeBaseGrants(w, grantRequest(http.MethodPut, `{"grants":[{"type":"user","id":" z-user "},{"type":"team","id":"a-team"}]}`), ident)
			if stage == "success" {
				require.Equal(t, http.StatusOK, w.Code)
				require.NotContains(t, w.Body.String(), `"id":" z-user "`)
				require.Contains(t, w.Body.String(), `"granted_by":"admin-user"`)
			} else {
				require.Equal(t, http.StatusServiceUnavailable, w.Code)
				require.NotContains(t, w.Body.String(), `"grants"`)
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestRuntimeAuditCompletionCannotSpoofOutcomeOrCompleteAnotherActor(t *testing.T) {
	h, ident := grantRegressionHarness(t)
	for _, tc := range []struct {
		operation, outcome string
		status             int
	}{{"", "succeeded", 200}, {"operation", "pending", 200}, {"operation", "failed", 199}, {"operation", "failed", 600}} {
		require.Error(t, h.completeKnowledgeRuntimeAudit(context.Background(), ident, tc.operation, tc.outcome, tc.status))
	}
	for _, failure := range []string{"query", "rows", "missing"} {
		exec := h.dbmock.ExpectExec(`UPDATE knowledge_audit_events SET outcome`).WithArgs("operation", "demo", "admin-user", "succeeded", 200)
		switch failure {
		case "query":
			exec.WillReturnError(errors.New("audit unavailable"))
		case "rows":
			exec.WillReturnResult(sqlmock.NewErrorResult(errors.New("rows unavailable")))
		case "missing":
			exec.WillReturnResult(sqlmock.NewResult(0, 0))
		}
		require.Error(t, h.completeKnowledgeRuntimeAudit(context.Background(), ident, "operation", "succeeded", 200))
	}
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}
