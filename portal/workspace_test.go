// Workspace API specs (testify + fake client, mirroring api_test.go).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// newWorkspaceHarness wires a portal with the workspace config defaults and
// the PersonalWorkspace status subresource registered on the fake client.
func newWorkspaceHarness(t *testing.T, objects ...runtime.Object) *apiTestPortal {
	t.Helper()
	h := newAPIHarness(t, objects...)
	// Rebuild the fake client with the PersonalWorkspace status subresource
	// registered (newAPIHarness predates workspaces).
	scheme := h.k8s.Scheme()
	existing := &dev1alpha1.DigitalEmployeeList{}
	_ = h.k8s.List(t.Context(), existing)
	objs := make([]runtime.Object, 0, len(objects))
	objs = append(objs, objects...)
	h.k8s = fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).
		WithStatusSubresource(&dev1alpha1.DigitalEmployee{}, &dev1alpha1.PersonalWorkspace{}).Build()
	h.cfg.workspaceModel = "glm-4.6"
	h.cfg.workspaceQuotaPerUser = 1
	h.cfg.memoryServer = "http://ov:1933"
	h.cfg.memoryAccount = "zhiyuan"
	h.cfg.credentialsSecret = "de-platform-creds"
	return h
}

func runningWorkspace(name, owner string, companions ...string) *dev1alpha1.PersonalWorkspace {
	specs := make([]dev1alpha1.CompanionSpec, 0, len(companions))
	for _, c := range companions {
		specs = append(specs, dev1alpha1.CompanionSpec{Name: c, Model: "glm-4.6"})
	}
	return &dev1alpha1.PersonalWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "digital-employees"},
		Spec: dev1alpha1.PersonalWorkspaceSpec{
			Owner: owner, Runtime: "pod", Companions: specs,
			Memory:      &dev1alpha1.MemorySpec{Type: "openviking", ServerURL: "http://ov:1933", AccountID: "zhiyuan"},
			Credentials: dev1alpha1.CredentialsRef{Name: "de-platform-creds"},
		},
		Status: dev1alpha1.PersonalWorkspaceStatus{Phase: dev1alpha1.WorkspacePhaseRunning, GatewayEndpoint: "pw-" + name + ".digital-employees.svc.cluster.local"},
	}
}

func ownerIdentity() identity { return identity{UserID: "u-owner", Username: "zhangsan"} }

func postWorkspace(t *testing.T, h *apiTestPortal, body string, ident identity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.createWorkspace(w, req, ident)
	return w
}

func TestWorkspaceCreate(t *testing.T) {
	t.Run("creates with default name and per-companion models", func(t *testing.T) {
		h := newWorkspaceHarness(t)
		w := postWorkspace(t, h, `{"companions":[{"name":"assistant"},{"name":"researcher","model":"glm-5"}]}`, ownerIdentity())
		require.Equal(t, http.StatusCreated, w.Code)
		var out struct {
			Workspace map[string]any
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		assert.Equal(t, "ws-zhangsan-"+strings.Split(out.Workspace["name"].(string), "-")[2], out.Workspace["name"])

		var created dev1alpha1.PersonalWorkspace
		require.NoError(t, h.k8s.Get(t.Context(), client.ObjectKey{Namespace: "digital-employees", Name: out.Workspace["name"].(string)}, &created))
		assert.Equal(t, "u-owner", created.Spec.Owner)
		assert.Equal(t, "glm-4.6", created.Spec.Companions[0].Model, "empty model falls back to platform default")
		assert.Equal(t, "glm-5", created.Spec.Companions[1].Model)
		assert.Equal(t, "http://ov:1933", created.Spec.Memory.ServerURL)
		assert.Equal(t, "de-platform-creds", created.Spec.Credentials.Name)
	})

	t.Run("rejects invalid companion names and duplicates", func(t *testing.T) {
		h := newWorkspaceHarness(t)
		for _, body := range []string{
			`{"companions":[]}`,
			`{"companions":[{"name":"Bad_Name"}]}`,
			`{"companions":[{"name":"a"},{"name":"a"}]}`,
		} {
			w := postWorkspace(t, h, body, ownerIdentity())
			assert.Equal(t, http.StatusBadRequest, w.Code, body)
		}
	})

	t.Run("enforces one workspace per user", func(t *testing.T) {
		h := newWorkspaceHarness(t, runningWorkspace("ws-existing", "u-owner", "assistant"))
		w := postWorkspace(t, h, `{"companions":[{"name":"assistant"}]}`, ownerIdentity())
		assert.Equal(t, http.StatusConflict, w.Code)
	})

	t.Run("admin quota is per-owner, not per-admin", func(t *testing.T) {
		h := newWorkspaceHarness(t, runningWorkspace("ws-existing", "u-owner", "assistant"))
		w := postWorkspace(t, h, `{"companions":[{"name":"assistant"}]}`, adminIdentity())
		assert.Equal(t, http.StatusCreated, w.Code, "another user's quota does not block the admin's own workspace")
	})
}

func TestWorkspaceListAndGet(t *testing.T) {
	t.Run("non-admin sees only their own", func(t *testing.T) {
		h := newWorkspaceHarness(t,
			runningWorkspace("ws-mine", "u-owner", "assistant"),
			runningWorkspace("ws-other", "u-other", "assistant"))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces", nil)
		w := httptest.NewRecorder()
		h.listWorkspaces(w, req, ownerIdentity())
		require.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "ws-mine")
		assert.NotContains(t, w.Body.String(), "ws-other")
	})

	t.Run("get rejects non-owner", func(t *testing.T) {
		h := newWorkspaceHarness(t, runningWorkspace("ws-mine", "u-owner", "assistant"))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-mine", nil)
		req.SetPathValue("name", "ws-mine")
		w := httptest.NewRecorder()
		h.getWorkspace(w, req, identity{UserID: "u-stranger"})
		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

func TestWorkspacePersona(t *testing.T) {
	t.Run("create stores the persona on the CR", func(t *testing.T) {
		h := newWorkspaceHarness(t)
		w := postWorkspace(t, h, `{"companions":[{"name":"assistant","persona":"你是工作助手"}]}`, ownerIdentity())
		require.Equal(t, http.StatusCreated, w.Code)
		var out struct {
			Workspace map[string]any
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))

		var created dev1alpha1.PersonalWorkspace
		require.NoError(t, h.k8s.Get(t.Context(), client.ObjectKey{Namespace: "digital-employees", Name: out.Workspace["name"].(string)}, &created))
		assert.Equal(t, "你是工作助手", created.Spec.Companions[0].Persona)
	})

	t.Run("patch updates the persona (roster replacement semantics)", func(t *testing.T) {
		h := newWorkspaceHarness(t, runningWorkspace("ws-mine", "u-owner", "assistant"))
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/workspaces/ws-mine",
			strings.NewReader(`{"companions":[{"name":"assistant","persona":"改成调研员人格"}]}`))
		req.SetPathValue("name", "ws-mine")
		w := httptest.NewRecorder()
		h.updateWorkspace(w, req, ownerIdentity())
		require.Equal(t, http.StatusOK, w.Code)

		var updated dev1alpha1.PersonalWorkspace
		require.NoError(t, h.k8s.Get(t.Context(), client.ObjectKey{Namespace: "digital-employees", Name: "ws-mine"}, &updated))
		assert.Equal(t, "改成调研员人格", updated.Spec.Companions[0].Persona)
	})

	t.Run("rejects a persona beyond 2000 characters", func(t *testing.T) {
		h := newWorkspaceHarness(t)
		body := `{"companions":[{"name":"assistant","persona":"` + strings.Repeat("格", 2001) + `"}]}`
		w := postWorkspace(t, h, body, ownerIdentity())
		assert.Equal(t, http.StatusBadRequest, w.Code)

		ok := `{"companions":[{"name":"assistant","persona":"` + strings.Repeat("格", 2000) + `"}]}`
		w = postWorkspace(t, h, ok, ownerIdentity())
		assert.Equal(t, http.StatusCreated, w.Code, "exactly 2000 runes is the CRD MaxLength boundary")
	})

	t.Run("summary echoes persona and omits it when empty", func(t *testing.T) {
		ws := runningWorkspace("ws-mine", "u-owner", "assistant", "researcher")
		ws.Spec.Companions[0].Persona = "你是工作助手"
		h := newWorkspaceHarness(t, ws)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-mine", nil)
		req.SetPathValue("name", "ws-mine")
		w := httptest.NewRecorder()
		h.getWorkspace(w, req, ownerIdentity())
		require.Equal(t, http.StatusOK, w.Code)

		var out struct {
			Workspace struct {
				Companions []map[string]any
			}
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		require.Len(t, out.Workspace.Companions, 2)
		assert.Equal(t, "你是工作助手", out.Workspace.Companions[0]["persona"])
		_, present := out.Workspace.Companions[1]["persona"]
		assert.False(t, present, "an empty persona must not appear in the summary")
	})
}

func TestWorkspacePatchAndDelete(t *testing.T) {
	t.Run("patch replaces the roster and rolls via operator", func(t *testing.T) {
		h := newWorkspaceHarness(t, runningWorkspace("ws-mine", "u-owner", "assistant"))
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/workspaces/ws-mine",
			strings.NewReader(`{"companions":[{"name":"assistant"},{"name":"writer"}]}`))
		req.SetPathValue("name", "ws-mine")
		w := httptest.NewRecorder()
		h.updateWorkspace(w, req, ownerIdentity())
		require.Equal(t, http.StatusOK, w.Code)

		var updated dev1alpha1.PersonalWorkspace
		require.NoError(t, h.k8s.Get(t.Context(), client.ObjectKey{Namespace: "digital-employees", Name: "ws-mine"}, &updated))
		assert.Len(t, updated.Spec.Companions, 2)
		assert.Equal(t, "writer", updated.Spec.Companions[1].Name)
	})

	t.Run("delete is owner-only", func(t *testing.T) {
		h := newWorkspaceHarness(t, runningWorkspace("ws-mine", "u-owner", "assistant"))
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/workspaces/ws-mine", nil)
		req.SetPathValue("name", "ws-mine")
		w := httptest.NewRecorder()
		h.deleteWorkspace(w, req, identity{UserID: "u-stranger"})
		assert.Equal(t, http.StatusForbidden, w.Code)

		w = httptest.NewRecorder()
		h.deleteWorkspace(w, req, ownerIdentity())
		assert.Equal(t, http.StatusOK, w.Code)
	})
}
