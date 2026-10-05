package controller

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// fullEmployee returns a CR with every spec field populated; the deepcopy
// tests mutate clones to prove no aliasing leaks back into the original.
func fullEmployee() *v1alpha1.DigitalEmployee {
	on := true
	three := int32(3)
	return &v1alpha1.DigitalEmployee{
		ObjectMeta: metav1.ObjectMeta{Name: "full", Namespace: "ns", Labels: map[string]string{"k": "v"}},
		Spec: v1alpha1.DigitalEmployeeSpec{
			DisplayName: "完整",
			Team:        "rd-dept",
			Runtime: v1alpha1.RuntimeSpec{
				Type: "deerflow", Image: "img", Model: "m",
				Provider: "anthropic", NativeModel: "glm", ApiKeyEnv: "K",
				Models: []v1alpha1.RuntimeModel{
					{Name: "bench-anthropic", Provider: "anthropic", NativeModel: "glm-5.3-flash", BaseURL: "http://gw/bench-anthropic", ApiKeyEnv: "BIGMODEL_API_KEY"},
					{Name: "bench-qwen", Provider: "openai", NativeModel: "Qwen3.6-35B-A3B", BaseURL: "http://gw/bench-qwen"},
				},
			},
			Memory: &v1alpha1.MemorySpec{
				Type: "openviking", ServerURL: "http://ov", AccountID: "acc",
				SingleTenantInstance: true, TopK: 8, ScoreThreshold: 0.25, MaxInjectionChars: 12000,
			},
			Knowledge: &v1alpha1.KnowledgeSpec{
				Type: "weknora", MCPURL: "http://mcp",
				Bases: []v1alpha1.KnowledgeBaseRef{{ID: "kb-1", Name: "销售知识库"}},
			},
			Channels:          &v1alpha1.ChannelsSpec{Wecom: true, WecomName: "机器人", Feishu: true},
			Persistence:       v1alpha1.PersistenceSpec{Enabled: true, Size: "4Gi"},
			ConversationStore: v1alpha1.ConversationStoreSpec{Backend: "postgres"},
			Governance: v1alpha1.GovernanceSpec{
				AEPBaseURL: "http://aep", DeploymentID: "demo", DefaultAEPUser: "u",
				IdentitySource: "wecom", GovernedTools: []string{"t"}, TeamNames: map[string]string{"rd": "研发"},
			},
			Credentials: v1alpha1.CredentialsRef{Name: "creds"},
			A2A:         &v1alpha1.A2ASpec{Image: "a2a", MaxDepth: three, NodePort: 30100},
			Replicas:    1,
			Owner:       "u1",
			OwnerName:   "用户一",
			Description: "整理资料",
			Skills:      []v1alpha1.SkillRef{{ID: "report-writer", Name: "报告撰写", Version: "1.2.0"}},
			Visibility: &v1alpha1.VisibilitySpec{
				Mode:  "restricted",
				Teams: []v1alpha1.SubjectRef{{ID: "sales", Name: "销售团队"}},
				Users: []v1alpha1.SubjectRef{{ID: "u-9", Name: "李四"}},
			},
			NodePort:    30200,
			Hibernation: &v1alpha1.HibernationSpec{Enabled: &on, IdleTimeout: "30m"},
		},
		Status: v1alpha1.DigitalEmployeeStatus{
			Phase:          v1alpha1.PhaseReady,
			Message:        "ok",
			MemoryUser:     "full",
			RuntimeAccount: v1alpha1.RuntimeAccountPhase("ready"),
		},
	}
}

func TestDeepCopyIsolation(t *testing.T) {
	original := fullEmployee()

	clone := original.DeepCopy()
	require.Equal(t, original, clone)

	clone.Spec.Governance.GovernedTools[0] = "mutated"
	clone.Spec.Channels.Wecom = false
	clone.Spec.Hibernation.IdleTimeout = "1h"
	clone.Spec.Replicas = 9
	if clone.Spec.Memory != nil {
		clone.Spec.Memory.TopK = 1
	}
	clone.Spec.Skills[0].Version = "9.9.9"
	clone.Spec.Knowledge.Bases[0].ID = "kb-mutated"
	clone.Spec.Visibility.Mode = "all"
	clone.Spec.Visibility.Teams[0].ID = "mutated"
	assert.NotEqual(t, original, clone)
	assert.Equal(t, "t", original.Spec.Governance.GovernedTools[0])
	assert.True(t, original.Spec.Channels.Wecom)
	assert.Equal(t, "30m", original.Spec.Hibernation.IdleTimeout)
	assert.EqualValues(t, 1, original.Spec.Replicas)
	assert.EqualValues(t, 8, original.Spec.Memory.TopK)
	assert.Equal(t, "1.2.0", original.Spec.Skills[0].Version)
	assert.Equal(t, "kb-1", original.Spec.Knowledge.Bases[0].ID)
	assert.Equal(t, "restricted", original.Spec.Visibility.Mode)
	assert.Equal(t, "sales", original.Spec.Visibility.Teams[0].ID)

	again := clone.DeepCopyObject().(*v1alpha1.DigitalEmployee)
	assert.Equal(t, clone, again)

	list := &v1alpha1.DigitalEmployeeList{Items: []v1alpha1.DigitalEmployee{*original}}
	listClone := list.DeepCopy()
	assert.Equal(t, list, listClone)
	listClone.Items[0].Spec.DisplayName = "改名"
	assert.Equal(t, "完整", list.Items[0].Spec.DisplayName)

	listAgain := list.DeepCopyObject().(*v1alpha1.DigitalEmployeeList)
	assert.Equal(t, list, listAgain)
	assert.NotNil(t, (&v1alpha1.DigitalEmployeeList{}).DeepCopyObject())
}

func TestDeepCopyEverySpecType(t *testing.T) {
	on := true
	spec := fullEmployee().Spec
	cases := []struct {
		name string
		call func() any
	}{
		{"A2ASpec", func() any { c := spec.A2A.DeepCopy(); c.MaxDepth = 1; return c }},
		{"ChannelsSpec", func() any { c := spec.Channels.DeepCopy(); c.Wecom = false; return c }},
		{"ConversationStoreSpec", func() any { c := spec.ConversationStore.DeepCopy(); c.Backend = "sqlite"; return c }},
		{"CredentialsRef", func() any { c := spec.Credentials.DeepCopy(); c.Name = "x"; return c }},
		{"GovernanceSpec", func() any { c := spec.Governance.DeepCopy(); c.DeploymentID = "x"; return c }},
		{"HibernationSpec", func() any { c := spec.Hibernation.DeepCopy(); c.IdleTimeout = "1h"; return c }},
		{"KnowledgeSpec", func() any { c := spec.Knowledge.DeepCopy(); c.MCPURL = "x"; return c }},
		{"MemorySpec", func() any { c := spec.Memory.DeepCopy(); c.TopK = 1; return c }},
		{"PersistenceSpec", func() any { c := spec.Persistence.DeepCopy(); c.Size = "1Gi"; return c }},
		{"RuntimeSpec", func() any { c := spec.Runtime.DeepCopy(); c.Image = "x"; return c }},
		{"DigitalEmployeeSpec", func() any { c := spec.DeepCopy(); c.DisplayName = "x"; return c }},
		{"DigitalEmployeeStatus", func() any {
			st := fullEmployee().Status
			c := st.DeepCopy()
			c.Phase = v1alpha1.PhasePending
			return c
		}},
		{"HibernationEnabledPtr", func() any {
			h := &v1alpha1.HibernationSpec{Enabled: &on}
			c := h.DeepCopy()
			*c.Enabled = false
			return c
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NotNil(t, tc.call())
		})
	}
	assert.Equal(t, "deerflow", spec.Runtime.Type)
	assert.True(t, spec.Channels.Wecom)
	assert.Equal(t, "postgres", spec.ConversationStore.Backend)
}

func TestKedaTypesDeepCopy(t *testing.T) {
	so := renderScaledObject(fullEmployee(), wakeDBCoordinates{
		Host: "pg", Port: "5432", DBName: "deportal", User: "aep", Password: "p",
	}, 1800, 3)
	clone := so.DeepCopyObject().(*KedaScaledObject)
	clone.Spec.Triggers[0].Metadata["host"] = "mutated"
	assert.Equal(t, "pg", so.Spec.Triggers[0].Metadata["host"])

	auth := renderTriggerAuthentication(fullEmployee())
	authClone := auth.DeepCopyObject().(*KedaTriggerAuthentication)
	assert.Equal(t, auth, authClone)

	soList := &KedaScaledObjectList{Items: []KedaScaledObject{*so}}
	assert.Equal(t, soList, soList.DeepCopyObject().(*KedaScaledObjectList))
	authList := &KedaTriggerAuthenticationList{Items: []KedaTriggerAuthentication{*auth}}
	assert.Equal(t, authList, authList.DeepCopyObject().(*KedaTriggerAuthenticationList))
}

func TestConfigMapNamePredicate(t *testing.T) {
	p := configMapNamePredicate("de-hibernation-policy")
	named := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "de-hibernation-policy"}}
	assert.True(t, p.Create(event.CreateEvent{Object: named}))
	assert.False(t, p.Create(event.CreateEvent{Object: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "other"}}}))
	assert.True(t, p.Delete(event.DeleteEvent{Object: named}))
	assert.True(t, p.Update(event.UpdateEvent{ObjectNew: named}))
	assert.False(t, p.Update(event.UpdateEvent{ObjectNew: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x"}}}))
	assert.False(t, p.Generic(event.GenericEvent{}))
}

func TestDatabaseConfigBackends(t *testing.T) {
	assert.Equal(t, map[string]any{"backend": "postgres", "postgres_url": "$DE_CONVERSATION_DB_URL"}, databaseConfig("postgres"))
	assert.Equal(t, map[string]any{"sqlite_dir": "/var/lib/deerflow/data"}, databaseConfig("sqlite"))
}

func TestRenderConfigVariants(t *testing.T) {
	employee := fullEmployee()
	employee.Spec.NodePort = 30300
	cfg := renderConfig(employee, conversationURLKey(employee))
	assert.Contains(t, cfg, "ChatAnthropic")
	assert.Contains(t, cfg, "AepGovernanceMiddleware")

	rendered, err := (&DigitalEmployeeReconciler{}).renderDeployment(employee)
	assert.NoError(t, err)
	assert.EqualValues(t, 1, *rendered.Spec.Replicas)
	assert.NotNil(t, rendered.Spec.Template.Spec.Containers[0].ReadinessProbe)
	svc := renderService(employee)
	assert.Equal(t, int32(30300), svc.Spec.Ports[0].NodePort)
}

func TestRenderA2AAndVolumes(t *testing.T) {
	employee := fullEmployee()
	a2a := renderA2ADeployment(employee)
	assert.Equal(t, "de-full-a2a", a2a.Name)
	assert.NotNil(t, a2a.Spec.Template.Spec.Containers[0])
	a2aSvc := renderA2AService(employee)
	assert.Equal(t, int32(30100), a2aSvc.Spec.Ports[0].NodePort)

	noA2A := fullEmployee()
	noA2A.Spec.A2A = nil
	noA2A.Spec.NodePort = 0
	svc := renderService(noA2A)
	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)
}

func TestParseWakeDBURLErrors(t *testing.T) {
	_, err := parseWakeDBURL("://bad url")
	assert.Error(t, err)
	_, err = parseWakeDBURL("postgres://host:5432/db")
	assert.Error(t, err, "missing user must be rejected")

	coords, err := parseWakeDBURL("postgresql://u:p@h:6432/deportal")
	assert.NoError(t, err)
	assert.Equal(t, "h", coords.Host)
	assert.Equal(t, "6432", coords.Port)
	assert.Equal(t, "deportal", coords.DBName)

	coords, err = parseWakeDBURL("postgresql://u:p@h/db2")
	assert.NoError(t, err)
	assert.Equal(t, "5432", coords.Port)
}

func TestDurationParsingAndDefaults(t *testing.T) {
	secs, ok := durationToSeconds("45m")
	assert.True(t, ok)
	assert.EqualValues(t, 2700, secs)
	_, ok = durationToSeconds("9d")
	assert.False(t, ok)
	_, ok = durationToSeconds("")
	assert.False(t, ok)
	secs, _ = durationToSeconds("999h")
	assert.EqualValues(t, 24*3600, secs)
}

func TestSnippetAndEmails(t *testing.T) {
	assert.Equal(t, "short", snippet([]byte(" short ")))
	runes := []rune(snippet([]byte(strings.Repeat("x", 300))))
	assert.Len(t, runes, 201) // 200 body + ellipsis
	assert.Equal(t, "admin@full.runtime.internal", runtimeAdminEmail(fullEmployee()))
	assert.Equal(t, "de-full@runtime.internal", runtimeOwnerEmail(fullEmployee()))
}

func TestEnsureChildrenWithoutPersistenceAndChannels(t *testing.T) {
	// An employee with persistence disabled and no optional children must
	// still reconcile cleanly (empty-dir home, no PVC, no a2a, no hibernation).
	ctx := context.Background()
	fx := newFixture(ctx)
	item := fx.employeeFixture("minimal-helper")
	item.Spec.Persistence.Enabled = false
	item.Spec.Channels = nil
	item.Spec.A2A = nil
	item.Spec.Memory = nil
	require.NoError(t, fx.client.Create(ctx, item))
	_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name},
	})
	require.NoError(t, err)
}

func TestSetupWithManager(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, k8sscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	// Manager construction is lazy: no API server contact happens until Start.
	mgr, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	require.NoError(t, err)
	r := &DigitalEmployeeReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
	require.NoError(t, r.SetupWithManager(mgr))
}

func TestRuntimeAccountProvisionNoPAT(t *testing.T) {
	// A runtime that creates the user but returns no PAT must fail the
	// bootstrap loudly (the operator cannot recover the key later).
	ctx := context.Background()
	fx := newFixture(ctx)
	fx.runtime.glitchProvision = true
	item := fx.employeeFixture("glitch-runtime")
	require.NoError(t, fx.client.Create(ctx, item))
	for range 3 {
		_, _ = fx.reconciler().Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name},
		})
		deployment := &appsv1.Deployment{}
		if err := fx.client.Get(ctx, types.NamespacedName{Namespace: item.Namespace, Name: deploymentName(item)}, deployment); err == nil && deployment.Status.ReadyReplicas == 0 {
			deployment.Status.ObservedGeneration = deployment.Generation
			deployment.Status.ReadyReplicas = *deployment.Spec.Replicas
			require.NoError(t, fx.client.Status().Update(ctx, deployment))
		}
	}
	fresh := &v1alpha1.DigitalEmployee{}
	require.NoError(t, fx.client.Get(ctx, types.NamespacedName{Namespace: item.Namespace, Name: item.Name}, fresh))
	assert.NotEqual(t, v1alpha1.RuntimeAccountPhase("ready"), fresh.Status.RuntimeAccount)
}

func TestRuntimeAccountProvisionFailure(t *testing.T) {
	// A runtime API that answers 500 on setup-status drives provision into
	// its failure branches; the reconcile must surface the error instead of
	// marking the account ready.
	ctx := context.Background()
	fx := newFixture(ctx)
	fx.runtime.failMode = true
	item := fx.employeeFixture("broken-runtime")
	require.NoError(t, fx.client.Create(ctx, item))
	// First pass renders the children (deployment included).
	_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name},
	})
	require.NoError(t, err)
	// Mark the deployment ready so the account bootstrap path runs.
	deployment := &appsv1.Deployment{}
	require.NoError(t, fx.client.Get(ctx, types.NamespacedName{Namespace: item.Namespace, Name: deploymentName(item)}, deployment))
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.ReadyReplicas = *deployment.Spec.Replicas
	require.NoError(t, fx.client.Status().Update(ctx, deployment))

	_, _ = fx.reconciler().Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: item.Namespace, Name: item.Name},
	})
	// No panic, no ready account: the failure is contained.
	fresh := &v1alpha1.DigitalEmployee{}
	require.NoError(t, fx.client.Get(ctx, types.NamespacedName{Namespace: item.Namespace, Name: item.Name}, fresh))
	assert.NotEqual(t, v1alpha1.RuntimeAccountPhase("ready"), fresh.Status.RuntimeAccount)
}

func TestRuntimeURLDerivation(t *testing.T) {
	item := fullEmployee()
	item.Namespace = "digital-employees"
	fx := newFixture(context.Background())
	r := fx.reconciler()
	r.RuntimeURLFn = nil // fixture wires a stub; exercise the in-cluster branch
	assert.Equal(t, "http://de-full.digital-employees.svc.cluster.local:8001", r.runtimeURL(item))
	r.RuntimeURLFn = func(namespace, name string) string { return "http://stub:1/" + name }
	assert.Equal(t, "http://stub:1/full", r.runtimeURL(item))
}

func TestRuntimeClientCSRFToken(t *testing.T) {
	client, err := newRuntimeClient("http://runtime.test:8001")
	require.NoError(t, err)

	// No cookies yet: the token is empty, never an error.
	assert.Empty(t, client.csrfToken())

	parsed, err := url.Parse("http://runtime.test:8001")
	require.NoError(t, err)
	client.jar.SetCookies(parsed, []*http.Cookie{{Name: "csrf_token", Value: "csrf-1"}})
	assert.Equal(t, "csrf-1", client.csrfToken())

	// An unparseable base degrades to an empty token.
	client.base = "://bad url"
	assert.Empty(t, client.csrfToken())
}

// controller-gen emits a nil guard on every DeepCopy and standalone
// DeepCopyInto/DeepCopy pairs for the value-ref types (slices of them are
// copied element-wise with copy(), so their methods are never reached via
// the CR graph). Sweep both so new types keep the coverage floor honest.
func TestDeepCopyNilReceiversAndRefTypes(t *testing.T) {
	for name, call := range map[string]func() any{
		"ChannelsSpec":          func() any { return (*v1alpha1.ChannelsSpec)(nil).DeepCopy() },
		"ConversationStoreSpec": func() any { return (*v1alpha1.ConversationStoreSpec)(nil).DeepCopy() },
		"CredentialsRef":        func() any { return (*v1alpha1.CredentialsRef)(nil).DeepCopy() },
		"DigitalEmployee":       func() any { return (*v1alpha1.DigitalEmployee)(nil).DeepCopy() },
		"DigitalEmployeeList":   func() any { return (*v1alpha1.DigitalEmployeeList)(nil).DeepCopy() },
		"DigitalEmployeeSpec":   func() any { return (*v1alpha1.DigitalEmployeeSpec)(nil).DeepCopy() },
		"DigitalEmployeeStatus": func() any { return (*v1alpha1.DigitalEmployeeStatus)(nil).DeepCopy() },
		"GovernanceSpec":        func() any { return (*v1alpha1.GovernanceSpec)(nil).DeepCopy() },
		"HibernationSpec":       func() any { return (*v1alpha1.HibernationSpec)(nil).DeepCopy() },
		"KnowledgeBaseRef":      func() any { return (*v1alpha1.KnowledgeBaseRef)(nil).DeepCopy() },
		"KnowledgeSpec":         func() any { return (*v1alpha1.KnowledgeSpec)(nil).DeepCopy() },
		"MemorySpec":            func() any { return (*v1alpha1.MemorySpec)(nil).DeepCopy() },
		"PersistenceSpec":       func() any { return (*v1alpha1.PersistenceSpec)(nil).DeepCopy() },
		"RuntimeModel":          func() any { return (*v1alpha1.RuntimeModel)(nil).DeepCopy() },
		"RuntimeSpec":           func() any { return (*v1alpha1.RuntimeSpec)(nil).DeepCopy() },
		"SkillRef":              func() any { return (*v1alpha1.SkillRef)(nil).DeepCopy() },
		"SubjectRef":            func() any { return (*v1alpha1.SubjectRef)(nil).DeepCopy() },
		"VisibilitySpec":        func() any { return (*v1alpha1.VisibilitySpec)(nil).DeepCopy() },
	} {
		assert.Nil(t, call(), "%s DeepCopy must return nil for a nil receiver", name)
	}

	base := v1alpha1.KnowledgeBaseRef{ID: "kb-1", Name: "销售知识库"}
	baseInto := v1alpha1.KnowledgeBaseRef{}
	base.DeepCopyInto(&baseInto)
	assert.Equal(t, base, baseInto)
	assert.Equal(t, base, *base.DeepCopy())

	skill := v1alpha1.SkillRef{ID: "report-writer", Name: "报告撰写", Version: "1.2.0"}
	skillInto := v1alpha1.SkillRef{}
	skill.DeepCopyInto(&skillInto)
	assert.Equal(t, skill, skillInto)
	assert.Equal(t, skill, *skill.DeepCopy())

	subject := v1alpha1.SubjectRef{ID: "sales", Name: "销售团队"}
	subjectInto := v1alpha1.SubjectRef{}
	subject.DeepCopyInto(&subjectInto)
	assert.Equal(t, subject, subjectInto)
	assert.Equal(t, subject, *subject.DeepCopy())
}
