package main

import (
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSharedLabelRules(t *testing.T) {
	for _, in := range []sharedLabelCreation{{RequestID: "r", Name: " ", Color: "#123456"}, {RequestID: "r", Name: strings.Repeat("字", 49), Color: "#123456"}, {RequestID: "r", Name: "标签", Color: "red"}, {Name: "标签", Color: "#123456"}} {
		require.Error(t, in.validate())
	}
	in := sharedLabelCreation{RequestID: "r", Name: " 标签 ", Color: "#ABCDEF"}
	require.NoError(t, in.validate())
	require.Equal(t, "标签", in.Name)
	require.Equal(t, "#abcdef", in.Color)
	task := map[string]any{"id": "task", "labels": []map[string]any{{"id": "b", "name": "标签", "color": "#abcdef", "companyId": "secret"}, {"id": "a", "name": "另一", "color": "#000000"}}}
	labels, ids := sharedTaskLabelValues(task)
	require.Len(t, labels, 2)
	require.Equal(t, []string{"a", "b"}, ids)
	token := sharedLabelsToken(task)
	task["updatedAt"] = "heartbeat"
	task["labelIds"] = []string{"b", "a", "b"}
	require.Equal(t, token, sharedLabelsToken(task))
	dto, _ := json.Marshal(sharedTaskDTO(task))
	require.NotContains(t, string(dto), "companyId")
	for _, status := range []string{"todo", "in_progress", "done", "cancelled"} {
		task["status"] = status
		task["executionRunId"] = "pi-run"
		require.Empty(t, sharedLabelsEditReason("editor", task))
	}
	require.NotEmpty(t, sharedLabelsEditReason("viewer", task))
	task["executionPolicy"] = map[string]any{}
	require.Empty(t, sharedLabelsEditReason("owner", task))
	task["executionState"] = map[string]any{"status": "pending"}
	require.NotEmpty(t, sharedLabelsEditReason("owner", task))
	delete(task, "executionState")
	task["executionPolicy"] = map[string]any{"monitor": map[string]any{}}
	require.NotEmpty(t, sharedLabelsEditReason("owner", task))
	directory := []sharedLabel{{ID: "x", Name: "标签", Color: "#ABCDEF"}}
	label, err := sharedLabelNamed(directory, "标签", "#abcdef")
	require.NoError(t, err)
	require.Equal(t, "x", label.ID)
	_, err = sharedLabelNamed(directory, "标签", "#000000")
	require.Error(t, err)
	label, err = sharedLabelNamed(directory, "另一个", "#000000")
	require.NoError(t, err)
	require.Nil(t, label)
}
func TestSharedLabelInputBoundaries(t *testing.T) {
	p := &portal{}
	for _, raw := range []string{`{"requestId":"r","name":"标签","color":"#abcdef","companyId":"foreign"}`, `{"requestId":"r","name":"标签","color":"invalid"}`} {
		require.Error(t, p.sharedCreateLabel(httptest.NewRecorder(), sharedNativeRequest("POST", "/", raw), sharedNativeIdentity))
	}
	for _, raw := range []string{`{"requestId":"r","expectedLabelsToken":"v","labelIds":[],"status":"done"}`, `{"requestId":"r","expectedLabelsToken":"v"}`, `{"requestId":"r","expectedLabelsToken":"v","labelIds":[""]}`} {
		require.Error(t, p.sharedSetTaskLabels(httptest.NewRecorder(), sharedNativeRequest("PUT", "/", raw), sharedNativeIdentity))
	}
}
func TestSharedLabelDirectoryScope(t *testing.T) {
	for _, mode := range []string{"viewer", "foreign", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			p, _ := sharedNativePortal(t, "viewer", false)
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "GET", r.Method)
				require.Equal(t, "/companies/company/labels", r.URL.Path)
				if mode == "unavailable" {
					w.WriteHeader(503)
					return
				}
				company := "company"
				if mode == "foreign" {
					company = "other"
				}
				writeJSON(w, 200, []map[string]any{{"id": "x", "name": "标签", "color": "#abcdef", "companyId": company}})
			})
			w := httptest.NewRecorder()
			err := p.sharedLabels(w, sharedNativeRequest("GET", "/", ""), sharedNativeIdentity)
			if mode == "viewer" {
				require.NoError(t, err)
				require.Contains(t, w.Body.String(), `"scope":"company"`)
				require.Contains(t, w.Body.String(), `"createLabel":false`)
				require.NotContains(t, w.Body.String(), "companyId")
			} else {
				require.Error(t, err)
			}
		})
	}
}
func TestSharedLabelReceipts(t *testing.T) {
	for _, mode := range []string{"absent", "applied", "actor-conflict", "action-conflict", "hash-conflict", "read-error", "unknown", "finish-error", "database-error"} {
		t.Run(mode, func(t *testing.T) {
			db, m, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			defer func() { require.NoError(t, m.ExpectationsWereMet()) }()
			actor, action, hash, state := "actor", "task_labels", "hash", "unknown"
			switch mode {
			case "applied":
				state = "applied"
			case "actor-conflict":
				actor = "other"
			case "action-conflict":
				action = "other"
			case "hash-conflict":
				hash = "other"
			}
			q := m.ExpectQuery("SELECT actor_id,action")
			if mode == "database-error" {
				q.WillReturnError(errors.New("receipt unavailable"))
			} else {
				rows := sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"})
				if mode != "absent" {
					rows.AddRow(actor, action, hash, state, `{"state":"applied"}`)
				}
				q.WillReturnRows(rows)
			}
			if mode == "unknown" || mode == "finish-error" {
				m.ExpectBegin()
				exec := m.ExpectExec("UPDATE shared_space_commands SET state='applied'")
				if mode == "finish-error" {
					exec.WillReturnError(errors.New("receipt unavailable"))
					m.ExpectRollback()
				} else {
					exec.WillReturnResult(sqlmock.NewResult(0, 1))
					m.ExpectCommit()
				}
			}
			c, err := db.Conn(t.Context())
			require.NoError(t, err)
			defer func() { _ = c.Close() }()
			reads := 0
			raw, ok, err := sharedLabelReceipt(t.Context(), c, "space", "r", "actor", "task_labels", "hash", func() (map[string]any, error) {
				reads++
				if mode == "read-error" {
					return nil, sharedFail(423, "unverified")
				}
				return map[string]any{"labelsToken": "token"}, nil
			})
			switch mode {
			case "absent":
				require.NoError(t, err)
				require.False(t, ok)
				require.Zero(t, reads)
			case "applied":
				require.NoError(t, err)
				require.True(t, ok)
				require.Zero(t, reads)
			case "unknown":
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, 1, reads)
				require.Contains(t, string(raw), `"state":"applied"`)
			default:
				require.Error(t, err)
			}
		})
	}
}

func TestSharedLabelsUnknownCommandsOnlyRead(t *testing.T) {
	for _, mode := range []string{"create-pending", "create-different-color", "task-pending", "task-outside-project"} {
		t.Run(mode, func(t *testing.T) {
			p, m := sharedNativePortal(t, "editor", true)
			action := "task_labels"
			var input any
			method := "PUT"
			if strings.HasPrefix(mode, "create-") {
				action = "create_label"
				method = "POST"
				input = sharedLabelCreation{RequestID: "r", Name: "标签", Color: "#abcdef"}
			} else {
				input = sharedTaskLabelsInput{RequestID: "r", Token: "old", IDs: []string{"x"}}
			}
			hash := sharedHash(input)
			if action == "task_labels" {
				hash = sharedHash([]any{"task", input})
			}
			m.ExpectQuery("SELECT actor_id,action").WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}).AddRow("person", action, hash, "unknown", `{}`))
			sharedNativeUnlock(m)
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "GET", r.Method)
				if action == "create_label" {
					labels := []sharedCompanyLabel{}
					if mode == "create-different-color" {
						labels = append(labels, sharedCompanyLabel{sharedLabel: sharedLabel{ID: "x", Name: "标签", Color: "#000000"}, CompanyID: "company"})
					}
					writeJSON(w, 200, labels)
					return
				}
				project := "project"
				if mode == "task-outside-project" {
					project = "foreign"
				}
				writeJSON(w, 200, map[string]any{"id": "task", "companyId": "company", "projectId": project, "labelIds": []string{}})
			})
			raw, err := json.Marshal(input)
			require.NoError(t, err)
			r := sharedNativeRequest(method, "/", string(raw))
			w := httptest.NewRecorder()
			if action == "create_label" {
				err = p.sharedCreateLabel(w, r, sharedNativeIdentity)
			} else {
				err = p.sharedSetTaskLabels(w, r, sharedNativeIdentity)
			}
			require.Error(t, err)
		})
	}
}
func TestSharedTaskLabelEditingGates(t *testing.T) {
	for _, mode := range []string{"command-pending", "handoff-pending", "native-policy", "native-state", "foreign-task"} {
		t.Run(mode, func(t *testing.T) {
			p, m := sharedNativePortal(t, "editor", true)
			m.ExpectQuery("SELECT actor_id,action").WillReturnRows(sqlmock.NewRows([]string{"actor", "action", "hash", "state", "receipt"}))
			m.ExpectQuery("SELECT EXISTS.*shared_space_commands").WillReturnRows(sqlmock.NewRows([]string{"blocked"}).AddRow(mode == "command-pending"))
			if mode != "command-pending" {
				m.ExpectQuery("SELECT EXISTS.*shared_space_handoffs").WillReturnRows(sqlmock.NewRows([]string{"pending"}).AddRow(mode == "handoff-pending"))
			}
			sharedNativeUnlock(m)
			sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "GET", r.Method)
				task := map[string]any{"id": "task", "companyId": "company", "projectId": "project", "status": "in_progress"}
				switch mode {
				case "native-policy":
					task["executionPolicy"] = map[string]any{"stages": []any{"review"}}
				case "native-state":
					task["executionState"] = map[string]any{"status": "pending"}
				case "foreign-task":
					task["projectId"] = "foreign"
				default:
					t.Error("pending command or handoff must stop before native read")
				}
				writeJSON(w, 200, task)
			})
			require.Error(t, p.sharedSetTaskLabels(httptest.NewRecorder(), sharedNativeRequest("PUT", "/", `{"requestId":"r","expectedLabelsToken":"old","labelIds":[]}`), sharedNativeIdentity))
		})
	}
}
func TestSharedTaskLabelFilterRejectsOutsideDirectory(t *testing.T) {
	p, _ := sharedNativePortal(t, "viewer", false)
	sharedNativeServer(t, p, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/companies/company/labels", r.URL.Path)
		writeJSON(w, 200, []sharedCompanyLabel{})
	})
	r := sharedNativeRequest("GET", "/?labelId=foreign&q=query&limit=1&offset=2", "")
	require.Error(t, p.sharedTasks(httptest.NewRecorder(), r, sharedNativeIdentity))
}
