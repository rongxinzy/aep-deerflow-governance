package main

import (
	"encoding/json"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSharedMetadataInput(t *testing.T) {
	valid := `{"requestId":"r","expectedMetadataToken":"v","title":" 标题 ","description":"正文","priority":"high"}`
	for _, extra := range []string{"status", "assigneeUserId", "assigneeAgentId", "parentId", "blockedBy", "blockedByIssueIds"} {
		var in sharedMetadataInput
		require.Error(t, sharedDecode(httptest.NewRecorder(), httptest.NewRequest("PATCH", "/", strings.NewReader(strings.TrimSuffix(valid, "}")+`,"`+extra+`":"x"}`)), &in))
	}
	for _, raw := range []string{valid, strings.Replace(valid, `"description":"正文",`, "", 1), strings.Replace(valid, `"priority":"high"`, `"priority":""`, 1), strings.Replace(valid, "标题", strings.Repeat("字", 241), 1)} {
		var in sharedMetadataInput
		require.NoError(t, json.Unmarshal([]byte(raw), &in))
		if raw == valid {
			require.NoError(t, in.validate())
			require.Equal(t, "标题", *in.Title)
			require.True(t, in.matches(in.body()))
		} else {
			require.Error(t, in.validate())
		}
	}
	task := map[string]any{"id": "t", "title": "one", "description": "", "priority": "low"}
	token := sharedMetadataToken(task)
	task["title"] = "two"
	require.NotEqual(t, token, sharedMetadataToken(task))
}

func TestSharedMetadataReceiptRecovery(t *testing.T) {
	for _, mode := range []string{"absent", "applied", "conflict", "unknown-match", "unknown-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			defer func() { require.NoError(t, m.ExpectationsWereMet()) }()
			p := &portal{db: db}
			title, desc, priority := "title", "body", "high"
			in := sharedMetadataInput{RequestID: "r", Token: "token", Title: &title, Description: &desc, Priority: &priority}
			state, hash := "unknown", "hash"
			if mode == "applied" {
				state = "applied"
			}
			if mode == "conflict" {
				hash = "other"
			}
			rows := sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"})
			if mode != "absent" {
				rows.AddRow("actor", "task_metadata", hash, state, `{"state":"applied"}`)
			}
			m.ExpectQuery("SELECT actor_id,action").WillReturnRows(rows)
			if strings.HasPrefix(mode, "unknown-") {
				sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, "GET", r.Method)
					task := in.body()
					task["id"] = "task"
					task["companyId"] = "company"
					task["projectId"] = "project"
					if mode == "unknown-mismatch" {
						task["title"] = "other"
					}
					writeJSON(w, 200, task)
				})
				if mode == "unknown-match" {
					m.ExpectBegin()
					m.ExpectExec("UPDATE shared_space_commands SET state='applied'").WillReturnResult(sqlmock.NewResult(0, 1))
					m.ExpectCommit()
				}
			}
			c, err := db.Conn(t.Context())
			require.NoError(t, err)
			defer c.Close()
			raw, ok, err := p.sharedMetadataReceipt(t.Context(), c, sharedSpace{ID: "space", CompanyID: "company", ProjectID: "project"}, "task", "actor", "hash", in)
			if mode == "conflict" || mode == "unknown-mismatch" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, mode != "absent", ok)
				if ok {
					require.Contains(t, string(raw), "applied")
				}
			}
		})
	}
}
