package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// The fixture intentionally models a lost response after native checkout has
// applied. Happy-path acceptances must opt out rather than weakening readback.
func TestSharedDependencyNativeCheckoutAcknowledgementContract(t *testing.T) {
	for _, lose := range []bool{false, true} {
		name := "acknowledged"
		if lose {
			name = "lost acknowledgement"
		}
		t.Run(name, func(t *testing.T) {
			f := &dependencyPGFixture{tasks: map[string]map[string]any{"a": {"id": "a", "companyId": "company", "projectId": "project", "assigneeAgentId": "agent", "assigneeUserId": nil, "status": "todo"}}, loseCheckout: lose}
			r := httptest.NewRequest("POST", "/issues/a/checkout", nil)
			w := httptest.NewRecorder()
			f.paperclip(t, w, r)
			require.Equal(t, "in_progress", f.tasks["a"]["status"])
			require.Equal(t, "agent", f.tasks["a"]["assigneeAgentId"])
			require.Nil(t, f.tasks["a"]["assigneeUserId"])
			require.Equal(t, 1, f.checkoutPosts)
			if lose {
				require.Equal(t, 503, w.Code)
				require.False(t, f.loseCheckout)
			} else {
				require.Equal(t, 200, w.Code)
				var task map[string]any
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &task))
				require.Equal(t, "agent", task["assigneeAgentId"])
				require.Equal(t, "in_progress", task["status"])
				require.Nil(t, task["assigneeUserId"])
			}
		})
	}
}
