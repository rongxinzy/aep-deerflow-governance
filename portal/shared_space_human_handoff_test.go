package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestSharedHumanHandoffUsesNativeIdentityAndRetiresExecution(t *testing.T) {
	for _, mode := range []string{"agent-to-human", "human-to-human", "unknown-patch", "target-revoked", "source-changed"} {
		t.Run(mode, func(t *testing.T) {
			h := newApplyHarness(t)
			s := sharedSpace{ID: "space", DeploymentID: "deployment-acme", CompanyID: "company", ProjectID: "project", Role: "owner"}
			handoff := sharedHandoff{RequestID: "handoff", Actor: "u-x", TaskID: "task", Phase: "patch_pending", SourceAgentID: "agent", TargetUserID: "u-owner", TargetNativeUserID: "native-owner", SourceBindingID: "source", BindingID: "source"}
			task := map[string]any{"id": "task", "companyId": "company", "projectId": "project", "assigneeAgentId": "agent", "status": "in_progress"}
			if mode == "human-to-human" {
				handoff.SourceAgentID = ""
				handoff.SourceUserID = "u-x"
				handoff.SourceNativeUserID = "native-x"
				handoff.BindingID = ""
				task["assigneeAgentId"] = nil
				task["assigneeUserId"] = "native-x"
			}
			if mode == "source-changed" {
				task["assigneeAgentId"] = "other"
			}
			runtimeExpectMemberSpace(h, "owner")
			humanRole(h.dbmock, "u-owner", "editor")
			humanMappingRow(h.dbmock, "u-owner", "native-owner")
			patches := 0
			sharedNativeServer(t, h.portal, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/companies/company/user-directory" {
					users := []any{}
					if mode != "target-revoked" {
						users = append(users, map[string]any{"principalId": "native-owner", "status": "active", "user": map[string]any{"id": "native-owner", "name": "同事"}})
					}
					writeJSON(w, 200, map[string]any{"users": users})
					return
				}
				if r.Method == "PATCH" {
					patches++
					var body map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					require.Equal(t, "native-owner", body["assigneeUserId"])
					require.Nil(t, body["assigneeAgentId"])
					if mode == "unknown-patch" {
						w.WriteHeader(503)
						return
					}
					for k, v := range body {
						task[k] = v
					}
				}
				writeJSON(w, 200, task)
			})
			if mode != "target-revoked" && mode != "source-changed" {
				h.dbmock.ExpectExec("UPDATE shared_space_handoffs SET phase").WillReturnResult(sqlmock.NewResult(0, 1))
				humanRole(h.dbmock, "u-owner", "editor")
				humanMappingRow(h.dbmock, "u-owner", "native-owner")
				if mode != "unknown-patch" {
					h.dbmock.ExpectBegin()
					if handoff.BindingID != "" {
						h.dbmock.ExpectExec("UPDATE shared_space_runtime_bindings SET binding_state='retired'").WillReturnResult(sqlmock.NewResult(0, 1))
					}
					h.dbmock.ExpectExec("UPDATE shared_space_handoffs SET phase='applied'").WillReturnResult(sqlmock.NewResult(0, 1))
					h.dbmock.ExpectExec("UPDATE shared_space_commands").WillReturnResult(sqlmock.NewResult(0, 1))
					h.dbmock.ExpectCommit()
				}
			}
			c, err := h.db.Conn(t.Context())
			require.NoError(t, err)
			defer func() { _ = c.Close() }()
			h.processHandoffLocked(t.Context(), c, s, &handoff)
			switch mode {
			case "agent-to-human", "human-to-human":
				require.Equal(t, "applied", handoff.Phase)
				require.Equal(t, 1, patches)
			case "unknown-patch":
				require.Equal(t, "patch_unknown", handoff.Phase)
				require.Equal(t, 1, patches)
			default:
				require.Equal(t, "patch_pending", handoff.Phase)
				require.Zero(t, patches)
			}
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
}

func TestSharedHumanHandoffHistoryHidesNativePrincipal(t *testing.T) {
	raw, err := json.Marshal(sharedHandoff{Phase: "applied", SourceUserID: "aep-a", SourceNativeUserID: "native-a", SourceName: "甲", TargetUserID: "aep-b", TargetNativeUserID: "native-b", TargetName: "乙"})
	require.NoError(t, err)
	require.Contains(t, string(raw), `"kind":"human"`)
	require.Contains(t, string(raw), "aep-b")
	require.NotContains(t, string(raw), "native-")
}
