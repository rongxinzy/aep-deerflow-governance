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

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func collaborationHarness(t *testing.T) *apiTestPortal {
	h := newApplyHarness(t)
	for _, ws := range []*dev.PersonalWorkspace{runningWorkspace("ws-source", "u-x", "assistant"), runningWorkspace("ws-target", "u-owner", "assistant", "researcher")} {
		initializeContacts(ws, nil)
		require.NoError(t, h.k8s.Create(t.Context(), ws))
		require.NoError(t, h.k8s.Create(t.Context(), workspaceMemkey(ws.Name, "assistant")))
	}
	return h
}
func collaborationRequest(method, path, body string) *http.Request {
	return httptest.NewRequest(method, path, strings.NewReader(body))
}
func threadRows(source collaborationSource) *sqlmock.Rows {
	target, _ := json.Marshal(colleagueCompanion{ID: "ws-target/assistant", Name: "assistant", OwnerID: "u-owner", OwnerName: "李四", DisplayName: "助手"})
	origin, _ := json.Marshal(source)
	return sqlmock.NewRows([]string{"id", "requester", "owner_id", "target", "source", "created_at"}).AddRow("thread", "u-x", "u-owner", string(target), string(origin), time.Now())
}
func messageRows(state string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "text", "reply", "owner_question", "owner_reply", "state", "returned", "error", "created_at"}).AddRow("message", "请问时间", "已收到", "请确认", "", state, "", "", time.Now())
}
func expectThread(h *apiTestPortal, source collaborationSource) {
	h.dbmock.ExpectQuery("SELECT id,requester,owner_id,target,source,created_at").WillReturnRows(threadRows(source))
}

func TestCollaborationDiscoveryAndScope(t *testing.T) {
	h := collaborationHarness(t)
	w := httptest.NewRecorder()
	h.colleagueDirectory(w, collaborationRequest("GET", "/?q=李四", ""), identity{UserID: "u-x"})
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "ws-target/assistant")
	require.NotContains(t, w.Body.String(), "WORKSPACE_INTERNAL_TOKEN")
	req := collaborationRequest("PUT", "/", `{"mode":"private","description":"内部工作"}`)
	req.SetPathValue("workspace", "ws-target")
	req.SetPathValue("companion", "assistant")
	w = httptest.NewRecorder()
	h.updateCompanionContact(w, req, identity{UserID: "u-x"})
	require.Equal(t, 404, w.Code)
	req = collaborationRequest("PUT", "/", `{"mode":"private","description":"内部工作"}`)
	req.SetPathValue("workspace", "ws-target")
	req.SetPathValue("companion", "assistant")
	w = httptest.NewRecorder()
	h.updateCompanionContact(w, req, ownerIdentity())
	require.Equal(t, 200, w.Code)
	_, _, err := h.collaborationTarget(t.Context(), "ws-target/assistant", identity{UserID: "u-x"}, true)
	require.Error(t, err)
	target, _, err := h.collaborationTarget(t.Context(), "ws-target/assistant", ownerIdentity(), true)
	require.NoError(t, err)
	require.Equal(t, "private", target.ContactMode)
	for _, id := range []string{"missing", "missing/name", "ws-target/missing"} {
		_, _, err = h.collaborationTarget(t.Context(), id, ownerIdentity(), true)
		require.Error(t, err)
	}
	for _, owner := range []string{"u-off", "u-agent", "unknown", "boom"} {
		ws := runningWorkspace("test-"+owner, owner, "assistant")
		initializeContacts(ws, nil)
		require.NoError(t, h.k8s.Create(t.Context(), ws))
		_, _, err = h.collaborationTarget(t.Context(), ws.Name+"/assistant", identity{UserID: "u-x"}, true)
		require.Error(t, err)
	}
	legacy := runningWorkspace("legacy", "u-x", "old")
	initializeContacts(legacy, legacy.Spec.Companions)
	require.Empty(t, contactSettings(legacy)["old"].Mode)
	legacy.Spec.Companions = append(legacy.Spec.Companions, dev.CompanionSpec{Name: "new"})
	initializeContacts(legacy, legacy.Spec.Companions[:1])
	require.Equal(t, "organization", contactSettings(legacy)["new"].Mode)
	require.Empty(t, contactSettings(legacy)["old"].Mode)
	w = httptest.NewRecorder()
	h.colleagueDirectory(w, collaborationRequest("GET", "/", ""), identity{UserID: "u-x"})
	require.Equal(t, 503, w.Code) // boom prevents presenting an incomplete identity projection as complete.
	for _, body := range []string{`{`, `{"mode":"bad"}`} {
		req = collaborationRequest("PUT", "/", body)
		req.SetPathValue("workspace", "ws-target")
		req.SetPathValue("companion", "researcher")
		w = httptest.NewRecorder()
		h.updateCompanionContact(w, req, ownerIdentity())
		require.Equal(t, 400, w.Code)
	}
	req = collaborationRequest("PUT", "/", `{}`)
	req.SetPathValue("workspace", "ws-target")
	req.SetPathValue("companion", "missing")
	w = httptest.NewRecorder()
	h.updateCompanionContact(w, req, ownerIdentity())
	require.Equal(t, 404, w.Code)
}
func TestCollaborationSendAndIdempotency(t *testing.T) {
	for _, scenario := range []string{"ok", "duplicate-conflict", "db-error", "commit-error", "source"} {
		t.Run(scenario, func(t *testing.T) {
			h := collaborationHarness(t)
			body := `{"targetId":"ws-target/assistant","message":"你好","requestId":"same"}`
			if scenario == "source" {
				body = `{"targetId":"ws-target/assistant","message":"你好","requestId":"same","source":{"workspace":"ws-source","companion":"assistant","conversationId":23}}`
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, "/assistant/api/conversations/23", r.URL.Path)
					writeJSON(w, 200, map[string]any{"entries": []any{}})
				}))
				defer up.Close()
				h.workspaceChatOverride = up.URL
			}
			if scenario == "db-error" {
				h.dbmock.ExpectBegin().WillReturnError(errors.New("db"))
			} else {
				h.dbmock.ExpectBegin()
				h.dbmock.ExpectExec("INSERT INTO companion_collaborations").WillReturnResult(sqlmock.NewResult(0, 1))
				h.dbmock.ExpectExec("INSERT INTO companion_collaboration_messages").WillReturnResult(sqlmock.NewResult(0, 1))
				text := "你好"
				if scenario == "duplicate-conflict" {
					text = "different"
				}
				h.dbmock.ExpectQuery("SELECT text").WillReturnRows(sqlmock.NewRows([]string{"text"}).AddRow(text))
				if scenario == "duplicate-conflict" {
					h.dbmock.ExpectRollback()
				} else if scenario == "commit-error" {
					h.dbmock.ExpectCommit().WillReturnError(errors.New("db"))
				} else {
					h.dbmock.ExpectCommit()
				}
			}
			w := httptest.NewRecorder()
			h.sendCollaboration(w, collaborationRequest("POST", "/", body), identity{UserID: "u-x"}, nil)
			expected := 202
			if scenario == "duplicate-conflict" {
				expected = 409
			}
			if scenario == "db-error" || scenario == "commit-error" {
				expected = 503
			}
			require.Equal(t, expected, w.Code, w.Body.String())
			require.NoError(t, h.dbmock.ExpectationsWereMet())
		})
	}
	h := collaborationHarness(t)
	for _, tc := range []struct {
		body string
		code int
	}{{`{`, 400}, {`{"message":"你好","requestId":"1","targetId":"missing"}`, 409}, {`{"message":"你好","requestId":"1","source":{"workspace":"ws-target","companion":"assistant","conversationId":1}}`, 403}, {`{"message":"你好","requestId":"1","source":{"workspace":"ws-source","companion":"missing","conversationId":1}}`, 400}, {`{"message":"你好","requestId":"1","source":{"workspace":"ws-source","companion":"assistant","conversationId":1}}`, 409}} {
		w := httptest.NewRecorder()
		h.sendCollaboration(w, collaborationRequest("POST", "/", tc.body), identity{UserID: "u-x"}, nil)
		require.Equal(t, tc.code, w.Code, w.Body.String())
	}
}
func TestCollaborationReadOwnerReplyRetry(t *testing.T) {
	h := collaborationHarness(t)
	req := collaborationRequest("GET", "/", "")
	req.SetPathValue("id", "thread")
	expectThread(h, collaborationSource{})
	h.dbmock.ExpectQuery("SELECT id,text").WillReturnRows(messageRows("waiting_owner"))
	w := httptest.NewRecorder()
	h.getCollaboration(w, req, identity{UserID: "u-x"})
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), "waiting_owner")
	for _, err := range []error{sql.ErrNoRows, errors.New("db")} {
		h.dbmock.ExpectQuery("SELECT id,requester").WillReturnError(err)
		w = httptest.NewRecorder()
		h.getCollaboration(w, req, identity{UserID: "unrelated"})
		require.GreaterOrEqual(t, w.Code, 400)
	}
	expectThread(h, collaborationSource{})
	h.dbmock.ExpectQuery("SELECT id,text").WillReturnError(errors.New("db"))
	w = httptest.NewRecorder()
	h.getCollaboration(w, req, ownerIdentity())
	require.Equal(t, 503, w.Code)
	for _, tc := range []struct {
		body string
		user string
		rows int64
		code int
	}{{`{"messageId":"message","text":"周三可以"}`, "u-owner", 1, 200}, {`{"messageId":"message","text":"周三可以"}`, "u-owner", 0, 200}, {`{"messageId":"message","text":"已变化"}`, "u-owner", 0, 409}, {`{`, "u-owner", 0, 400}, {`{}`, "u-x", 0, 404}} {
		expectThread(h, collaborationSource{})
		r := collaborationRequest("POST", "/", tc.body)
		r.SetPathValue("id", "thread")
		if tc.code == 200 || tc.code == 409 {
			h.dbmock.ExpectExec("UPDATE companion_collaboration_messages SET owner_reply").WillReturnResult(sqlmock.NewResult(0, tc.rows))
			if tc.rows == 0 {
				h.dbmock.ExpectQuery("SELECT owner_reply").WillReturnRows(sqlmock.NewRows([]string{"owner_reply"}).AddRow("周三可以"))
			}
		}
		w = httptest.NewRecorder()
		h.replyCollaboration(w, r, identity{UserID: tc.user})
		require.Equal(t, tc.code, w.Code, w.Body.String())
	}
	expectThread(h, collaborationSource{})
	h.dbmock.ExpectExec("UPDATE companion_collaboration_messages SET state='pending'").WillReturnResult(sqlmock.NewResult(0, 1))
	w = httptest.NewRecorder()
	h.retryCollaboration(w, req, identity{UserID: "u-x"})
	require.Equal(t, 202, w.Code)
	expectThread(h, collaborationSource{})
	w = httptest.NewRecorder()
	h.retryCollaboration(w, req, ownerIdentity())
	require.Equal(t, 404, w.Code)
	require.NoError(t, h.dbmock.ExpectationsWereMet())
}
func TestCollaborationWorkerReturnsOriginalConversation(t *testing.T) {
	for _, stage := range []string{"pending", "waiting_owner", "owner", "target-failure", "return-failure", "no-source", "missing-source", "empty", "claim-failure"} {
		t.Run(stage, func(t *testing.T) {
			h := collaborationHarness(t)
			source := collaborationSource{Workspace: "ws-source", Companion: "assistant", ConversationID: 23}
			if stage == "no-source" {
				source = collaborationSource{}
			}
			if stage == "missing-source" {
				source.Workspace = "missing"
			}
			calls := []string{}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.URL.Path)
				require.Equal(t, "wstok", r.Header.Get(collaborationTokenHeader))
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				if strings.HasSuffix(r.URL.Path, "/respond") {
					if stage == "target-failure" {
						w.WriteHeader(503)
						return
					}
					text := "搭档回复"
					if stage == "empty" {
						text = ""
					}
					writeJSON(w, 200, map[string]string{"reply": text, "ownerQuestion": "请确认时间"})
				} else {
					require.Equal(t, float64(23), body["conversationId"])
					require.Contains(t, body["text"], "#/collaboration/thread")
					if stage == "return-failure" {
						w.WriteHeader(503)
						return
					}
					if stage == "owner" {
						require.Contains(t, body["text"], "周三可以")
					}
					writeJSON(w, 200, map[string]bool{"returned": true})
				}
			}))
			defer up.Close()
			h.workspaceChatOverride = up.URL
			claim := h.dbmock.ExpectQuery("UPDATE companion_collaboration_messages SET attempt_at")
			if stage == "claim-failure" {
				claim.WillReturnError(sql.ErrNoRows)
				require.False(t, h.processCollaboration(t.Context()))
				return
			}
			state := "pending"
			ownerReply := ""
			if stage == "waiting_owner" || stage == "owner" || stage == "missing-source" {
				state = "waiting_owner"
			}
			if stage == "owner" {
				ownerReply = "周三可以"
			}
			claim.WillReturnRows(sqlmock.NewRows([]string{"id", "thread_id", "text", "state", "reply", "owner_question", "owner_reply", "returned"}).AddRow("message", "thread", "请问时间", state, "搭档回复", "请确认时间", ownerReply, ""))
			h.dbmock.ExpectQuery("SELECT requester").WillReturnRows(sqlmock.NewRows([]string{"requester"}).AddRow("u-x"))
			expectThread(h, source)
			if state == "pending" {
				h.dbmock.ExpectQuery("SELECT id,text").WillReturnRows(messageRows("pending"))
				if stage == "target-failure" || stage == "empty" {
					h.dbmock.ExpectExec("UPDATE companion_collaboration_messages SET state='failed'").WillReturnResult(sqlmock.NewResult(0, 1))
				} else {
					h.dbmock.ExpectExec("UPDATE companion_collaboration_messages SET reply").WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			if stage == "return-failure" {
				h.dbmock.ExpectExec("UPDATE companion_collaboration_messages SET error").WillReturnResult(sqlmock.NewResult(0, 1))
			} else if stage != "target-failure" && stage != "empty" && stage != "missing-source" {
				h.dbmock.ExpectExec("UPDATE companion_collaboration_messages SET returned").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			require.True(t, h.processCollaboration(t.Context()))
			require.NoError(t, h.dbmock.ExpectationsWereMet())
			if stage == "owner" || stage == "waiting_owner" {
				require.Equal(t, []string{"/assistant/internal/collaboration/return"}, calls)
			}
		})
	}
}
func TestCollaborationAgentCredentialAndListing(t *testing.T) {
	h := collaborationHarness(t)
	for _, tc := range []struct {
		workspace, companion, token, action, method string
		code                                        int
	}{{"ws-source", "assistant", "wrong", "directory", "GET", 403}, {"missing", "assistant", "wstok", "directory", "GET", 403}, {"ws-source", "missing", "wstok", "directory", "GET", 403}, {"ws-source", "assistant", "wstok", "directory", "GET", 200}, {"ws-source", "assistant", "wstok", "bad", "GET", 404}, {"ws-source", "assistant", "wstok", "send", "POST", 400}} {
		r := collaborationRequest(tc.method, "/", `{}`)
		r.Header.Set("X-Collaboration-Workspace", tc.workspace)
		r.Header.Set("X-Collaboration-Companion", tc.companion)
		r.Header.Set(collaborationTokenHeader, tc.token)
		r.SetPathValue("action", tc.action)
		w := httptest.NewRecorder()
		h.collaborationAgent(w, r)
		require.Equal(t, tc.code, w.Code, w.Body.String())
	}
	h.dbmock.ExpectQuery("SELECT c.id").WillReturnRows(sqlmock.NewRows([]string{"id", "requester", "owner_id", "target", "source", "created_at", "waiting"}).AddRow("thread", "u-x", "u-owner", `{"name":"assistant"}`, `{}`, time.Now(), true))
	w := httptest.NewRecorder()
	h.listCollaborations(w, collaborationRequest("GET", "/", ""), ownerIdentity())
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"waitingForOwner":true`)
	h.dbmock.ExpectQuery("SELECT c.id").WillReturnError(errors.New("db"))
	w = httptest.NewRecorder()
	h.listCollaborations(w, collaborationRequest("GET", "/", ""), ownerIdentity())
	require.Equal(t, 503, w.Code)
	var ws dev.PersonalWorkspace
	require.NoError(t, h.k8s.Get(t.Context(), client.ObjectKey{Namespace: h.cfg.namespace, Name: "ws-target"}, &ws))
	ws.Spec.Companions = nil
	_, err := h.collaborationWorkspaceToken(t.Context(), &ws)
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h.runCollaborations(ctx)
}
