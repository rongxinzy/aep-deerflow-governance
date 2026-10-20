package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

var _ = Describe("Employee native model runtime", func() {
	var fx *fixture
	var employee *v1alpha1.DigitalEmployee
	var r *DigitalEmployeeReconciler

	BeforeEach(func(ctx context.Context) {
		fx = newFixture(ctx)
		employee = fx.employeeFixture("native-helper")
		employee.UID, employee.Generation = "employee-native-uid", 1
		employee.Spec.Memory, employee.Spec.A2A = nil, nil
		employee.Spec.ModelAccess = &v1alpha1.ModelAccessSpec{AgentID: "native-agent"}
		r = fx.reconciler()
		r.Client = identityTestClient{fx.client}
		r.ModelTokenAudience = "urn:zhiyuan:model-gateway:test"
		r.ModelGatewayURL = "http://gateway.test/v1"
		d, err := r.renderDeployment(employee, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(r.prepareModelIdentity(ctx, employee, d)).To(Succeed())
	})
	AfterEach(func() { fx.server.Close(); fx.runtimeSrv.Close() })

	It("delivers a token-file client, exact routing hint and stable wire model without static credentials", func() {
		employee.Spec.Runtime.Models = []v1alpha1.RuntimeModel{
			{Name: "chat", Provider: "openai", Reasoning: &v1alpha1.RuntimeReasoning{Thinking: "required", History: "preserve"}},
			{Name: "reasoning", Provider: "openai"},
		}
		config := renderConfigData(employee, "")
		Expect(r.configureModelRuntime(employee, config)).To(Succeed())
		for _, model := range config["models"].([]any) {
			entry := model.(map[string]any)
			Expect(entry).NotTo(HaveKey("api_key"))
			Expect(entry).To(HaveKeyWithValue("use", "deerflow.models.token_file:TokenFileChatOpenAI"))
			Expect(entry).To(HaveKeyWithValue("model", entry["name"]))
			Expect(entry).To(HaveKeyWithValue("base_url", r.ModelGatewayURL))
			Expect(entry).To(HaveKeyWithValue("api_key_file", modelTokenDirectory+"/token"))
			Expect(entry["default_headers"]).To(Equal(map[string]string{"x-aep-access-binding": modelRuntimeBinding(employee)}))
		}
		Expect(config["models"].([]any)[0].(map[string]any)["reasoning"]).To(Equal(map[string]any{"thinking": "required", "history": "preserve"}))
		// The owner and runtime account are unrelated to model workload identity.
		before := modelRuntimeBinding(employee)
		employee.Spec.Owner, employee.Spec.Governance.DefaultAEPUser = "other-owner", "other-default-user"
		Expect(modelRuntimeBinding(employee)).To(Equal(before))
		employee.Status.ModelIdentity.ServiceAccountUID = "replacement-sa-uid"
		Expect(modelRuntimeBinding(employee)).NotTo(Equal(before))
	})

	DescribeTable("rejects ambiguous native routes before writing runtime configuration",
		func(change func(*v1alpha1.DigitalEmployee)) {
			change(employee)
			Expect(r.validateModelRuntime(employee)).NotTo(Succeed())
			Expect(r.validateModelAPIKeyEnvs(context.Background(), employee)).NotTo(Succeed())
		},
		Entry("static key env", func(e *v1alpha1.DigitalEmployee) { e.Spec.Runtime.ApiKeyEnv = "PROVIDER_KEY" }),
		Entry("route key env", func(e *v1alpha1.DigitalEmployee) {
			e.Spec.Runtime.Models = []v1alpha1.RuntimeModel{{Name: "chat", Provider: "openai", ApiKeyEnv: "PROVIDER_KEY"}}
		}),
		Entry("provider-native alias", func(e *v1alpha1.DigitalEmployee) { e.Spec.Runtime.NativeModel = "upstream-model" }),
		Entry("public provider URL", func(e *v1alpha1.DigitalEmployee) { e.Spec.Runtime.ModelBaseURL = "https://provider.invalid/v1" }),
		Entry("unsupported protocol", func(e *v1alpha1.DigitalEmployee) { e.Spec.Runtime.Provider = "anthropic" }),
		Entry("invalid wire model", func(e *v1alpha1.DigitalEmployee) { e.Spec.Runtime.Model = "chat/provider" }),
		Entry("duplicate model", func(e *v1alpha1.DigitalEmployee) {
			e.Spec.Runtime.Models = []v1alpha1.RuntimeModel{{Name: "chat"}, {Name: "chat"}}
		}),
		Entry("oversized model list", func(e *v1alpha1.DigitalEmployee) { e.Spec.Runtime.Models = make([]v1alpha1.RuntimeModel, 513) }),
	)

	DescribeTable("rejects malformed trusted gateway URLs", func(value string) {
		Expect(validModelGatewayURL(value)).To(BeFalse())
	},
		Entry("relative", "/v1"), Entry("provider default path", "https://gateway.test"),
		Entry("credentials", "http://user:password@gateway.test/v1"), Entry("query", "http://gateway.test/v1?key=test"),
		Entry("fragment", "http://gateway.test/v1#extra"), Entry("protocol", "file:///v1"),
		Entry("empty fragment", "http://gateway.test/v1#"),
		Entry("whitespace", " http://gateway.test/v1"), Entry("encoded path", "http://gateway.test/v%31"),
	)

	DescribeTable("rejects stale or foreign identity before rendering a routing hint", func(change func()) {
		change()
		Expect(r.configureModelRuntime(employee, renderConfigData(employee, ""))).NotTo(Succeed())
	},
		Entry("missing", func() { employee.Status.ModelIdentity = nil }),
		Entry("generation", func() { employee.Generation++ }),
		Entry("agent", func() { employee.Status.ModelIdentity.AgentID = "foreign" }),
		Entry("deployment", func() { employee.Status.ModelIdentity.DeploymentID = "foreign" }),
		Entry("CR UID", func() { employee.Status.ModelIdentity.EmployeeUID = "foreign" }),
		Entry("missing SA UID", func() { employee.Status.ModelIdentity.ServiceAccountUID = "" }),
		Entry("audience", func() { employee.Status.ModelIdentity.Audience = "kubernetes" }),
	)

	It("preserves the old model configuration while either opt-in is absent", func() {
		for _, disabled := range []string{"gateway", "mapping"} {
			copy := employee.DeepCopy()
			if disabled == "mapping" {
				copy.Spec.ModelAccess = nil
			} else {
				r.ModelGatewayURL = ""
			}
			config := renderConfigData(copy, "")
			before := marshalConfig(config)
			Expect(r.configureModelRuntime(copy, config)).To(Succeed())
			Expect(marshalConfig(config)).To(Equal(before))
			r.ModelGatewayURL = "http://gateway.test/v1"
		}
	})

	It("rejects legacy per-user model tools instead of borrowing a conversation owner's model identity", func() {
		r.ModelTools.GatewayURL = "http://legacy-tools.test/v1"
		Expect(r.validateModelRuntime(employee)).NotTo(Succeed())
	})

	It("renders the real child config and rolls it consistently on an owned SA replacement", func(ctx context.Context) {
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		step := func() (string, string) {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(employee)})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			var cm corev1.ConfigMap
			ExpectWithOffset(1, fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, &cm)).To(Succeed())
			var d appsv1.Deployment
			ExpectWithOffset(1, fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: deploymentName(employee)}, &d)).To(Succeed())
			ExpectWithOffset(1, d.Spec.Template.Spec.ServiceAccountName).To(Equal(employee.Status.ModelIdentity.ServiceAccountName))
			return cm.Data["config.yaml"], d.Spec.Template.Annotations[configHashAnnotation]
		}
		payload, hash := step()
		Expect(payload).To(ContainSubstring("TokenFileChatOpenAI"))
		Expect(payload).To(ContainSubstring("api_key_file"))
		Expect(payload).To(ContainSubstring("x-aep-access-binding"))
		Expect(strings.Contains(payload, "DEER_BENCH_API_KEY")).To(BeFalse())
		again, sameHash := step()
		Expect(again).To(Equal(payload))
		Expect(sameHash).To(Equal(hash))
		var sa corev1.ServiceAccount
		Expect(fx.client.Get(ctx, client.ObjectKey{Namespace: employee.Namespace, Name: employee.Status.ModelIdentity.ServiceAccountName}, &sa)).To(Succeed())
		Expect(fx.client.Delete(ctx, &sa)).To(Succeed())
		sa.UID, sa.ResourceVersion = "replacement-sa-uid", ""
		Expect(fx.client.Create(ctx, &sa)).To(Succeed())
		changed, changedHash := step()
		Expect(changed).NotTo(Equal(payload))
		Expect(changedHash).NotTo(Equal(hash))
	})
})
