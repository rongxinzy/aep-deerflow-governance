package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSharedRuntimeResources(t *testing.T) {
	for _, mode := range []string{"list", "document", "binary", "stale", "private-document", "wrong-document", "wrong-attachment", "unknown-file"} {
		t.Run(mode, func(t *testing.T) {
			h := collaborationHarness(t)
			b := runtimeTestBinding()
			action := "space-resources"
			extra := ""
			switch mode {
			case "document", "private-document", "wrong-document":
				action = "space-document"
				extra = "&key=zhiyuan-reference-guide"
			case "binary", "wrong-attachment", "unknown-file":
				action = "space-attachment"
				extra = "&attachmentId=file"
			}
			if mode == "private-document" {
				extra = "&key=shared-space-result"
			}
			generation := "1"
			if mode == "stale" {
				generation = "0"
			} else {
				runtimeExpectSpace(h, "editor")
			}
			sharedNativeServer(t, h.portal, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer synthetic", r.Header.Get("Authorization"))
				doc := map[string]any{"id": "doc", "key": "zhiyuan-reference-guide", "issueId": b.TaskID, "companyId": "company", "title": "资料", "body": "共享正文", "format": "markdown", "latestRevisionId": "revision"}
				switch r.URL.Path {
				case "/issues/task/documents":
					writeJSON(w, 200, []any{doc, map[string]any{"key": "private-notes", "issueId": b.TaskID, "companyId": "company", "body": "不应出现"}})
				case "/issues/task/documents/zhiyuan-reference-guide":
					if mode == "wrong-document" {
						doc["issueId"] = "other"
					}
					writeJSON(w, 200, doc)
				case "/issues/task/attachments":
					issue := b.TaskID
					if mode == "wrong-attachment" {
						issue = "other"
					}
					id := "file"
					if mode == "unknown-file" {
						id = "else"
					}
					writeJSON(w, 200, []any{map[string]any{"id": id, "issueId": issue, "originalFilename": "资料.bin", "contentType": "application/octet-stream", "byteSize": 4}})
				case "/attachments/file/content":
					w.Header().Set("Content-Type", "application/octet-stream")
					_, _ = w.Write([]byte{0, 1, 2, 255})
				default:
					t.Errorf("unexpected native read %s", r.URL.Path)
					w.WriteHeader(500)
				}
			})
			r := httptest.NewRequest("GET", "/?bindingId=binding&piTaskId=pi-task&generation="+generation+"&executionId=execution"+extra, nil)
			r.SetPathValue("action", action)
			w := httptest.NewRecorder()
			err := h.sharedRuntimeResource(w, r, b)
			switch mode {
			case "list":
				require.NoError(t, err)
				require.Contains(t, w.Body.String(), "zhiyuan-reference-guide")
				require.NotContains(t, w.Body.String(), "private-notes")
				require.NotContains(t, w.Body.String(), "contentPath")
				require.NotContains(t, w.Body.String(), "共享正文")
			case "document":
				require.NoError(t, err)
				require.Contains(t, w.Body.String(), "共享正文")
				require.Contains(t, w.Body.String(), "revisionId")
			case "binary":
				require.NoError(t, err)
				require.Equal(t, []byte{0, 1, 2, 255}, w.Body.Bytes())
				require.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
			default:
				require.Error(t, err)
			}
		})
	}
}

func TestSharedRuntimeResourceActions(t *testing.T) {
	require.True(t, sharedRuntimeResourceAction("space-resources"))
	require.True(t, sharedRuntimeResourceAction("space-document"))
	require.True(t, sharedRuntimeResourceAction("space-attachment"))
	require.False(t, sharedRuntimeResourceAction("space-submit"))

}

func TestSharedRuntimeResourcesRequireCurrentBinding(t *testing.T) {
	for _, mode := range []string{"active", "retired", "epoch", "companion", "revoked", "stale-tuple"} {
		t.Run(mode, func(t *testing.T) {
			h := collaborationHarness(t)
			ws := runtimeWorkspace(t, h)
			b := runtimeTestBinding()
			if mode == "retired" {
				b.BindingState = "retired"
			}
			runtimeExpectBinding(h, b)
			companion := "assistant"
			if mode == "companion" {
				companion = "other"
			} else {
				epoch := b.AssignmentEpoch
				if mode == "epoch" {
					epoch++
				}
				runtimeExpectTaskEpoch(h, b.SpaceID, b.TaskID, epoch)
				if mode != "retired" && mode != "epoch" {
					runtimeExpectSpace(h, "editor")
					if mode == "revoked" {
						h.dbmock.ExpectQuery("SELECT id,kind,target,name,owner_id,resource_uid,role,state,version,agent_id,registration_state FROM shared_space_participants").WillReturnError(sql.ErrNoRows)
					} else {
						runtimeExpectParticipant(h, b)
					}
				}
			}
			if mode == "active" {
				runtimeExpectSpace(h, "editor")
			}
			sharedNativeServer(t, h.portal, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/issues/task" {
					writeJSON(w, 200, map[string]any{"id": "task", "companyId": "company", "projectId": "project", "assigneeAgentId": "agent"})
					return
				}
				require.Equal(t, "active", mode)
				writeJSON(w, 200, []any{})
			})
			execution := "execution"
			if mode == "stale-tuple" {
				execution = "old"
			}
			r := httptest.NewRequest("GET", "/?bindingId=binding&piTaskId=pi-task&generation=1&executionId="+execution, nil)
			r.SetPathValue("action", "space-resources")
			w := httptest.NewRecorder()
			err := h.sharedRuntimeAgent(w, r, identity{UserID: "u-x"}, ws, companion)
			if mode == "active" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
