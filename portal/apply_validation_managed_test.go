package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func managedApplyHarness(t *testing.T, serve http.HandlerFunc) *apiTestPortal {
	t.Helper()
	h := newApplyHarness(t)
	h.cfg.managedKnowledge.tenantID = "10000"
	h.cfg.managedKnowledge.apiKeySecretField = "WEKNORA_KNOWLEDGE_API_KEY"
	var secret corev1.Secret
	require.NoError(t, h.k8s.Get(context.Background(), objectKey("de-platform-creds"), &secret))
	delete(secret.Data, "WEKNORA_API_KEY")
	secret.Data["WEKNORA_KNOWLEDGE_API_KEY"] = []byte("synthetic-managed-key")
	require.NoError(t, h.k8s.Update(context.Background(), &secret))
	server := httptest.NewServer(serve)
	t.Cleanup(server.Close)
	h.cfg.knowledgeAPIURL = server.URL
	return h
}

func expectManagedEmployeeCatalog(h *apiTestPortal, ids ...string) {
	rows := sqlmock.NewRows([]string{"knowledge_base_id"})
	for _, id := range ids {
		rows.AddRow(id)
	}
	h.dbmock.ExpectQuery(`SELECT knowledge_base_id FROM knowledge_base_registry WHERE deployment_id=\$1 AND weknora_tenant_id=\$2 AND lifecycle_state='active' ORDER BY knowledge_base_id`).WithArgs("deployment-acme", "10000").WillReturnRows(rows)
	for _, id := range ids {
		h.dbmock.ExpectQuery(`SELECT name_snapshot FROM knowledge_base_registry.*lifecycle_state='active'`).WithArgs("deployment-acme", "10000", id).WillReturnRows(sqlmock.NewRows([]string{"name_snapshot"}).AddRow("Registered"))
	}
}

func TestApplyManagedKnowledgeUsesConfiguredKeyAndActiveRegistry(t *testing.T) {
	for _, selected := range []string{"kb-active", "kb-unregistered", "kb-deleting"} {
		t.Run(selected, func(t *testing.T) {
			var broadListing atomic.Int32
			h := managedApplyHarness(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "synthetic-managed-key", r.Header.Get("X-API-Key"))
				switch r.URL.Path {
				case "/api/v1/auth/me":
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
				case "/api/v1/knowledge-bases/kb-active":
					_, _ = w.Write([]byte(`{"data":{"id":"kb-active","name":"Live registered name","tenant_id":10000}}`))
				default:
					broadListing.Add(1)
					http.NotFound(w, r)
				}
			})
			expectManagedEmployeeCatalog(h, "kb-active")
			if selected == "kb-active" {
				h.dbmock.ExpectBegin()
				h.dbmock.ExpectQuery(`SELECT lifecycle_state FROM knowledge_base_registry.*FOR UPDATE`).WithArgs("deployment-acme", "10000", "kb-active").WillReturnRows(sqlmock.NewRows([]string{"lifecycle_state"}).AddRow("active"))
				h.dbmock.ExpectExec(`INSERT INTO requests`).WillReturnResult(sqlmock.NewResult(1, 1))
				h.dbmock.ExpectRollback()
			}
			body := `{"name":"managed-helper","description":"Managed knowledge employee","team":"sales-dept","models":["bench-qwen"],"knowledgeBases":["` + selected + `"],"visibility":{"mode":"restricted","teams":["sales-dept"]}}`
			w := httptest.NewRecorder()
			ident := adminIdentity()
			ident.DeploymentID = "deployment-acme"
			h.applyForEmployee(w, httptest.NewRequest(http.MethodPost, "/api/v1/employees", strings.NewReader(body)), ident)
			if selected == "kb-active" {
				require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
				var employee dev1alpha1.DigitalEmployee
				require.NoError(t, h.k8s.Get(context.Background(), objectKey("managed-helper"), &employee))
				require.Equal(t, []dev1alpha1.KnowledgeBaseRef{{ID: "kb-active", Name: "Live registered name"}}, employee.Spec.Knowledge.Bases)
			} else {
				require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
				assert.Contains(t, w.Body.String(), "knowledgeBases[0]")
			}
			assert.Zero(t, broadListing.Load(), "must not enumerate unregistered upstream resources")
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestManagedEmployeeCatalogFailsClosed(t *testing.T) {
	for _, failure := range []string{"tenant", "upstream tenant", "registry", "scan", "rows", "missing key", "missing selector", "no db", "no deployment", "upstream failure"} {
		t.Run(failure, func(t *testing.T) {
			h := managedApplyHarness(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/auth/me" {
					if failure == "tenant" {
						_, _ = w.Write([]byte(`{"data":{"tenant":{"id":20000}}}`))
					} else {
						_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
					}
					return
				}
				if failure == "upstream failure" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte(`{"data":{"id":"kb-active","name":"Foreign","tenant_id":20000}}`))
			})
			switch failure {
			case "registry":
				h.dbmock.ExpectQuery(`SELECT knowledge_base_id FROM knowledge_base_registry`).WithArgs("deployment-acme", "10000").WillReturnError(errors.New("sensitive storage details"))
			case "scan":
				h.dbmock.ExpectQuery(`SELECT knowledge_base_id FROM knowledge_base_registry`).WithArgs("deployment-acme", "10000").WillReturnRows(sqlmock.NewRows([]string{"knowledge_base_id"}).AddRow(nil))
			case "rows":
				h.dbmock.ExpectQuery(`SELECT knowledge_base_id FROM knowledge_base_registry`).WithArgs("deployment-acme", "10000").WillReturnRows(sqlmock.NewRows([]string{"knowledge_base_id"}).AddRow("kb-active").RowError(0, errors.New("sensitive storage details")))
			case "upstream tenant", "upstream failure":
				expectManagedEmployeeCatalog(h, "kb-active")
			case "missing key":
				h.cfg.managedKnowledge.apiKeySecretField = "MISSING"
			case "missing selector":
				h.cfg.managedKnowledge.apiKeySecretField = ""
			case "no db":
				h.db = nil
			case "no deployment":
				h.cfg.aepDeploymentID = ""
			}
			bases, err := h.listWeKnoraBases(context.Background())
			require.Error(t, err)
			assert.Nil(t, bases)
			assert.NotContains(t, err.Error(), "sensitive storage details")
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestUpdateManagedKnowledgeUsesConfiguredKeyAndPreservesDenyAll(t *testing.T) {
	for _, selected := range []string{"kb-active", "kb-unregistered", ""} {
		t.Run(selected, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			h := managedApplyHarness(t, func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				assert.Equal(t, "synthetic-managed-key", r.Header.Get("X-API-Key"))
				if r.URL.Path == "/api/v1/auth/me" {
					_, _ = w.Write([]byte(`{"data":{"tenant":{"id":10000}}}`))
					return
				}
				assert.Equal(t, "/api/v1/knowledge-bases/kb-active", r.URL.Path)
				_, _ = w.Write([]byte(`{"data":{"id":"kb-active","name":"Live","tenant_id":10000}}`))
			})
			employee := readyCR("managed-helper", "admin-user")
			require.NoError(t, h.k8s.Create(context.Background(), employee))
			body := `{"knowledgeBases":[]}`
			if selected != "" {
				body = `{"knowledgeBases":["` + selected + `"]}`
				expectManagedEmployeeCatalog(h, "kb-active")
			}
			if selected == "kb-active" {
				h.dbmock.ExpectBegin()
				h.dbmock.ExpectQuery(`SELECT lifecycle_state FROM knowledge_base_registry.*FOR UPDATE`).WithArgs("deployment-acme", "10000", "kb-active").WillReturnRows(sqlmock.NewRows([]string{"lifecycle_state"}).AddRow("active"))
				h.dbmock.ExpectRollback()
			}
			ident := adminIdentity()
			ident.DeploymentID = "deployment-acme"
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/employees/managed-helper", strings.NewReader(body))
			req.SetPathValue("name", "managed-helper")
			w := httptest.NewRecorder()
			h.updateEmployee(w, req, ident)
			if selected == "kb-unregistered" {
				require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			} else {
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				require.NoError(t, h.k8s.Get(context.Background(), objectKey("managed-helper"), employee))
				require.NotNil(t, employee.Spec.Knowledge.Bases)
				if selected == "" {
					assert.Empty(t, employee.Spec.Knowledge.Bases)
					assert.Zero(t, upstreamCalls.Load())
				} else {
					assert.Equal(t, []dev1alpha1.KnowledgeBaseRef{{ID: "kb-active", Name: "Live"}}, employee.Spec.Knowledge.Bases)
				}
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}
