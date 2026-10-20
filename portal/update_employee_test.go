package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// PATCH /api/v1/employees/{name} coverage: partial updates validate only
// the fields they carry, keep untouched configuration (and never consult
// an upstream for it), and enforce the same privilege rules as apply.

// configuredCR is a fully-configured employee matching the fake catalogs in
// newApplyHarness (owner u-owner, team sales-dept, kb-1, report-writer 1.2.0,
// restricted to sales-dept).
func configuredCR(name string) *dev1alpha1.DigitalEmployee {
	return &dev1alpha1.DigitalEmployee{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "digital-employees"},
		Spec: dev1alpha1.DigitalEmployeeSpec{
			DisplayName: "全字段员工",
			Description: "整理销售资料。",
			Team:        "sales-dept",
			Owner:       "u-owner",
			OwnerName:   "李四",
			Knowledge: &dev1alpha1.KnowledgeSpec{
				Type:  "weknora",
				Bases: []dev1alpha1.KnowledgeBaseRef{{ID: "kb-1", Name: "销售知识库"}},
			},
			Skills:     []dev1alpha1.SkillRef{{ID: "report-writer", Name: "报告撰写", Version: "1.2.0"}},
			Visibility: &dev1alpha1.VisibilitySpec{Mode: "restricted", Teams: []dev1alpha1.SubjectRef{{ID: "sales-dept", Name: "销售部"}}, Users: []dev1alpha1.SubjectRef{}},
			Governance: dev1alpha1.GovernanceSpec{DefaultAEPUser: "u-owner"},
		},
		Status: dev1alpha1.DigitalEmployeeStatus{Phase: dev1alpha1.PhaseReady},
	}
}

func patchEmployee(t *testing.T, h *apiTestPortal, name, body string, ident identity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/employees/"+name, strings.NewReader(body))
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	h.updateEmployee(w, req, ident)
	return w
}

func getCR(t *testing.T, h *apiTestPortal, name string) *dev1alpha1.DigitalEmployee {
	t.Helper()
	item := &dev1alpha1.DigitalEmployee{}
	require.NoError(t, h.k8s.Get(context.Background(), objectKey(name), item))
	return item
}

func TestUpdateEmployeeModelSwitch(t *testing.T) {
	h := newApplyHarness(t)
	h.cfg.gatewayBaseURL = "http://higress-gateway.higress-system.svc.cluster.local"
	h.cfg.modelKeyEnv = "BIGMODEL_API_KEY"
	require.NoError(t, h.k8s.Create(context.Background(), configuredCR("switch-helper")))

	w := patchEmployee(t, h, "switch-helper", `{"models":["bench-qwen","bench-anthropic"]}`, adminIdentity())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	item := getCR(t, h, "switch-helper")
	require.Len(t, item.Spec.Runtime.Models, 2)
	assert.Equal(t, "bench-qwen", item.Spec.Runtime.Models[0].Name)
	assert.Equal(t, "bench-qwen", item.Spec.Runtime.Models[0].NativeModel, "model switches must retain the AEP model ID on the gateway wire")
	assert.Equal(t, "http://higress-gateway.higress-system.svc.cluster.local/v1", item.Spec.Runtime.Models[0].BaseURL)
	assert.Equal(t, "glm-5.3-flash", item.Spec.Runtime.Models[1].NativeModel, "the per-model Anthropic route retains its upstream model")
	assert.Equal(t, "http://higress-gateway.higress-system.svc.cluster.local/bench-anthropic", item.Spec.Runtime.Models[1].BaseURL)
	assert.Empty(t, item.Spec.Runtime.Model, "the singular default must not survive a model switch")
	// Untouched configuration is preserved verbatim.
	assert.Equal(t, "u-owner", item.Spec.Owner)
	require.NotNil(t, item.Spec.Knowledge)
	require.Len(t, item.Spec.Knowledge.Bases, 1)
	assert.Equal(t, "kb-1", item.Spec.Knowledge.Bases[0].ID)
	require.NotNil(t, item.Spec.Visibility)
	assert.Equal(t, "restricted", item.Spec.Visibility.Mode)

	var out struct {
		Employee map[string]any `json:"employee"`
	}
	require.NoError(t, jsonUnmarshal(w.Body.String(), &out))
	assert.Equal(t, []any{"bench-qwen", "bench-anthropic"}, out.Employee["models"])
	assert.Equal(t, "bench-qwen", out.Employee["model"])
}

func TestUpdateEmployeeUntouchedFieldsSkipUpstreams(t *testing.T) {
	// A displayName-only edit must succeed with every catalog down: the
	// model-switch scenario (dead upstream) is exactly when renaming and
	// re-pointing matter most.
	h := newApplyHarness(t)
	h.cfg.aepBaseURL = "http://127.0.0.1:1"
	h.cfg.knowledgeAPIURL = "http://127.0.0.1:1"
	require.NoError(t, h.k8s.Create(context.Background(), configuredCR("rename-helper")))

	w := patchEmployee(t, h, "rename-helper", `{"displayName":"改名员工"}`, adminIdentity())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "改名员工", getCR(t, h, "rename-helper").Spec.DisplayName)
}

func TestUpdateEmployeeOwnerTransfer(t *testing.T) {
	h := newApplyHarness(t)
	require.NoError(t, h.k8s.Create(context.Background(), configuredCR("transfer-helper")))
	ownerIdent := identity{UserID: "u-owner", DisplayName: "李四", Kind: "human"}

	// The current (non-admin) owner cannot hand the employee to someone else.
	w := patchEmployee(t, h, "transfer-helper", `{"owner":"u-x"}`, ownerIdent)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "管理员")

	// An administrator transfers ownership; governance identity follows.
	w = patchEmployee(t, h, "transfer-helper", `{"owner":"u-x"}`, adminIdentity())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	item := getCR(t, h, "transfer-helper")
	assert.Equal(t, "u-x", item.Spec.Owner)
	assert.Equal(t, "用户甲", item.Spec.OwnerName)
	assert.Equal(t, "u-x", item.Spec.Governance.DefaultAEPUser, "the employee acts with its owner's scope")

	// Referencing a disabled account or an agent is refused with a violation.
	w = patchEmployee(t, h, "transfer-helper", `{"owner":"u-off"}`, adminIdentity())
	assert.Equal(t, http.StatusBadRequest, w.Code)
	w = patchEmployee(t, h, "transfer-helper", `{"owner":"u-agent"}`, adminIdentity())
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateEmployeeVisibilityRules(t *testing.T) {
	h := newApplyHarness(t)
	require.NoError(t, h.k8s.Create(context.Background(), configuredCR("scope-helper")))
	ownerIdent := identity{UserID: "u-owner", DisplayName: "李四", Kind: "human"}

	// Opening to everyone stays an administrator action — no approval queue
	// exists for updates.
	w := patchEmployee(t, h, "scope-helper", `{"visibility":{"mode":"all"}}`, ownerIdent)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "全员开放")

	w = patchEmployee(t, h, "scope-helper", `{"visibility":{"mode":"all"}}`, adminIdentity())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotNil(t, getCR(t, h, "scope-helper").Spec.Visibility)
	assert.Equal(t, "all", getCR(t, h, "scope-helper").Spec.Visibility.Mode)

	// Restricted scopes resolve names and require at least one subject.
	w = patchEmployee(t, h, "scope-helper", `{"visibility":{"mode":"restricted","teams":["rd-dept"],"users":["u-x"]}}`, adminIdentity())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	item := getCR(t, h, "scope-helper")
	require.Len(t, item.Spec.Visibility.Teams, 1)
	assert.Equal(t, "研发部", item.Spec.Visibility.Teams[0].Name)
	assert.Equal(t, "用户甲", item.Spec.Visibility.Users[0].Name)

	w = patchEmployee(t, h, "scope-helper", `{"visibility":{"mode":"restricted"}}`, adminIdentity())
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "至少需要包含一个团队或用户")
}

func TestUpdateEmployeeKnowledgePolicy(t *testing.T) {
	h := newApplyHarness(t)
	require.NoError(t, h.k8s.Create(context.Background(), configuredCR("kb-helper")))

	// Repoint kb-1 → kb-2 resolves the display name snapshot.
	w := patchEmployee(t, h, "kb-helper", `{"knowledgeBases":["kb-2"]}`, adminIdentity())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	bases := getCR(t, h, "kb-helper").Spec.Knowledge.Bases
	require.Len(t, bases, 1)
	assert.Equal(t, "kb-2", bases[0].ID)
	assert.Equal(t, "产品知识库", bases[0].Name)

	// Explicit deny-all is valid without a WeKnora round-trip.
	h2 := newApplyHarness(t)
	h2.cfg.knowledgeAPIURL = "http://127.0.0.1:1"
	require.NoError(t, h2.k8s.Create(context.Background(), configuredCR("deny-helper")))
	w = patchEmployee(t, h2, "deny-helper", `{"knowledgeBases":[]}`, adminIdentity())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	item := getCR(t, h2, "deny-helper")
	require.NotNil(t, item.Spec.Knowledge.Bases, "empty list must survive as an explicit deny-all")
	assert.Empty(t, item.Spec.Knowledge.Bases)

	// Unknown bases are field-anchored violations.
	w = patchEmployee(t, h, "kb-helper", `{"knowledgeBases":["kb-x"]}`, adminIdentity())
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "knowledgeBases[0]")
}

func TestUpdateEmployeeSkills(t *testing.T) {
	h := newApplyHarness(t)
	require.NoError(t, h.k8s.Create(context.Background(), configuredCR("skill-helper")))

	// A draft version pin is refused with the apply-time message.
	w := patchEmployee(t, h, "skill-helper", `{"skills":[{"id":"report-writer","version":"2.0.0"}]}`, adminIdentity())
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "skills[0].id")

	w = patchEmployee(t, h, "skill-helper", `{"skills":[{"id":"report-writer","version":"1.0.0"}]}`, adminIdentity())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, getCR(t, h, "skill-helper").Spec.Skills, 1)
	assert.Equal(t, "1.0.0", getCR(t, h, "skill-helper").Spec.Skills[0].Version)

	// An explicitly empty list unregisters every skill.
	w = patchEmployee(t, h, "skill-helper", `{"skills":[]}`, adminIdentity())
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Empty(t, getCR(t, h, "skill-helper").Spec.Skills)
}

func TestUpdateEmployeeViolationsAndFailClosed(t *testing.T) {
	h := newApplyHarness(t)
	require.NoError(t, h.k8s.Create(context.Background(), configuredCR("bad-helper")))

	w := patchEmployee(t, h, "bad-helper",
		`{"description":"","team":"nope-dept","visibility":{"mode":"restricted"}}`, adminIdentity())
	require.Equal(t, http.StatusBadRequest, w.Code)
	var out struct {
		Violations []violation `json:"violations"`
	}
	require.NoError(t, jsonUnmarshal(w.Body.String(), &out))
	fields := map[string]bool{}
	for _, item := range out.Violations {
		fields[item.Field] = true
	}
	for _, expected := range []string{"description", "team", "visibility"} {
		assert.True(t, fields[expected], "expected violation for %s, got %+v", expected, out.Violations)
	}
	assert.Equal(t, "全字段员工", getCR(t, h, "bad-helper").Spec.DisplayName, "no partial write on violations")

	// A catalog outage (boom team → 500) fails closed without writing.
	w = patchEmployee(t, h, "bad-helper", `{"team":"boom"}`, adminIdentity())
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "暂不可用")
	assert.Equal(t, "sales-dept", getCR(t, h, "bad-helper").Spec.Team)

	// Model lists: empty violates the release condition, unknown ids fail fast.
	w = patchEmployee(t, h, "bad-helper", `{"models":[]}`, adminIdentity())
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "使用模型为必填发布条件")
	w = patchEmployee(t, h, "bad-helper", `{"models":["missing-model"]}`, adminIdentity())
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "not an enabled gateway model")
}

func TestUpdateEmployeeGuards(t *testing.T) {
	h := newApplyHarness(t)
	require.NoError(t, h.k8s.Create(context.Background(), configuredCR("guard-helper")))

	// A stranger (neither owner nor admin) cannot edit anything.
	w := patchEmployee(t, h, "guard-helper", `{"displayName":"越权"}`, identity{UserID: "stranger"})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "全字段员工", getCR(t, h, "guard-helper").Spec.DisplayName)

	// The owner may edit their own employee.
	w = patchEmployee(t, h, "guard-helper", `{"displayName":"自改"}`, identity{UserID: "u-owner", Kind: "human"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "自改", getCR(t, h, "guard-helper").Spec.DisplayName)
}
