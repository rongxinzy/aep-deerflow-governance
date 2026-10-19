package controller

import (
	"context"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// The pod-template fingerprint (zhiyuan.io/config-hash) is what makes the
// Deployment roll, so it must cover exactly the rendered config a booting
// runtime can only read at startup. The channel block is not one of those
// things: enabling or disabling a channel is applied on the live runtime
// (the portal's PUT /api/channels/{provider}/enabled) while the CR booleans
// only record intent/audit. Everything else — knowledge, memory, governance,
// the extension payload — keeps rolling.
var _ = Describe("config checksum and the channel block", func() {
	checksum := func(ctx context.Context, fx *fixture, employee *dev1alpha1.DigitalEmployee) string {
		deployment := &appsv1.Deployment{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: deploymentName(employee)}, deployment)).To(Succeed())
		return deployment.Spec.Template.Annotations[configHashAnnotation]
	}

	renderedConfig := func(ctx context.Context, fx *fixture, employee *dev1alpha1.DigitalEmployee) string {
		config := &corev1.ConfigMap{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
		return config.Data["config.yaml"]
	}

	renderedExtensions := func(ctx context.Context, fx *fixture, employee *dev1alpha1.DigitalEmployee) string {
		extensions := &corev1.ConfigMap{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: extensionsName(employee)}, extensions)).To(Succeed())
		return extensions.Data["extensions_config.json"]
	}

	reconcile := func(ctx context.Context, fx *fixture, employee *dev1alpha1.DigitalEmployee) {
		_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		Expect(err).NotTo(HaveOccurred())
	}

	// setup reconciles a fresh employee; mutate re-reads the CR (the first
	// reconcile writes the finalizer), applies change and reconciles again.
	setup := func(ctx context.Context) (*fixture, *dev1alpha1.DigitalEmployee) {
		fx := newFixture(ctx)
		employee := fx.employeeFixture("chan-checksum")
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		reconcile(ctx, fx, employee)
		return fx, employee
	}

	mutate := func(ctx context.Context, fx *fixture, employee *dev1alpha1.DigitalEmployee, change func(*dev1alpha1.DigitalEmployee)) {
		fresh := &dev1alpha1.DigitalEmployee{}
		Expect(fx.client.Get(ctx, client.ObjectKeyFromObject(employee), fresh)).To(Succeed())
		change(fresh)
		Expect(fx.client.Update(ctx, fresh)).To(Succeed())
		reconcile(ctx, fx, employee)
	}

	It("holds the fingerprint still while channels are enabled and disabled", func(ctx context.Context) {
		fx, employee := setup(ctx)
		first := checksum(ctx, fx, employee)
		Expect(first).NotTo(BeEmpty())
		before := renderedConfig(ctx, fx, employee)
		Expect(before).NotTo(ContainSubstring(`"channel_connections"`))

		// Portal activation: only the CR booleans flip.
		mutate(ctx, fx, employee, func(e *dev1alpha1.DigitalEmployee) {
			e.Spec.Channels = &dev1alpha1.ChannelsSpec{Wecom: true, Wechat: true, Feishu: true}
		})
		// The block still lands in config.yaml, which is what a booting
		// runtime reads as its startup default …
		enabled := renderedConfig(ctx, fx, employee)
		Expect(enabled).To(ContainSubstring(`"channel_connections"`))
		Expect(enabled).To(ContainSubstring(`"wecom"`))
		Expect(enabled).NotTo(Equal(before))
		// … but the pod template stays where it was: the toggle is hot.
		Expect(checksum(ctx, fx, employee)).To(Equal(first))
		// Guard: raw payloads like these are exactly what used to roll the
		// employee, taking conversations, memory and knowledge down with it.
		Expect(configChecksum(before, "")).NotTo(Equal(configChecksum(enabled, "")))

		// Disabling one provider again keeps the fingerprint too.
		mutate(ctx, fx, employee, func(e *dev1alpha1.DigitalEmployee) {
			e.Spec.Channels = &dev1alpha1.ChannelsSpec{Wechat: true}
		})
		Expect(renderedConfig(ctx, fx, employee)).NotTo(ContainSubstring(`"wecom"`))
		Expect(checksum(ctx, fx, employee)).To(Equal(first))

		// Disabling every provider again leaves the fingerprint where it was.
		mutate(ctx, fx, employee, func(e *dev1alpha1.DigitalEmployee) { e.Spec.Channels = nil })
		Expect(renderedConfig(ctx, fx, employee)).NotTo(ContainSubstring(`"channel_connections"`))
		Expect(checksum(ctx, fx, employee)).To(Equal(first))
	})

	It("still rolls on knowledge, memory and extension changes", func(ctx context.Context) {
		fx, employee := setup(ctx)
		first := checksum(ctx, fx, employee)

		// The extension payload the runtime loads at boot (MCP servers).
		mutate(ctx, fx, employee, func(e *dev1alpha1.DigitalEmployee) {
			e.Spec.Knowledge.MCPURL = "http://weknora-mcp:8831/mcp"
		})
		Expect(renderedExtensions(ctx, fx, employee)).To(ContainSubstring("8831"))
		Expect(checksum(ctx, fx, employee)).NotTo(Equal(first))

		// The knowledge whitelist lands in the governance middleware kwargs.
		second := checksum(ctx, fx, employee)
		mutate(ctx, fx, employee, func(e *dev1alpha1.DigitalEmployee) {
			e.Spec.Knowledge.Bases = []dev1alpha1.KnowledgeBaseRef{{ID: "kb-3", Name: "财务知识库"}}
		})
		Expect(renderedConfig(ctx, fx, employee)).To(ContainSubstring(`"kb-3"`))
		Expect(checksum(ctx, fx, employee)).NotTo(Equal(second))

		// Memory scope is part of the config payload too.
		third := checksum(ctx, fx, employee)
		mutate(ctx, fx, employee, func(e *dev1alpha1.DigitalEmployee) {
			e.Spec.Memory.MemoryScope = "requester"
			e.Spec.Memory.SharedPeerID = "df-shared-finance"
		})
		Expect(checksum(ctx, fx, employee)).NotTo(Equal(third))

		// A channel change on top of a rolled config is not masked either.
		fourth := checksum(ctx, fx, employee)
		mutate(ctx, fx, employee, func(e *dev1alpha1.DigitalEmployee) {
			e.Spec.Channels = &dev1alpha1.ChannelsSpec{Wechat: true}
			e.Spec.Governance.TeamNames = map[string]string{"gov-rd": "研发部", "gov-qa": "质量部"}
		})
		Expect(checksum(ctx, fx, employee)).NotTo(Equal(fourth))
	})

	It("drops the channel key and nothing else from the fingerprinted payload", func() {
		plain := &dev1alpha1.DigitalEmployee{ObjectMeta: metav1.ObjectMeta{Name: "chan-payload", Namespace: testNamespace}}
		plain.Spec.Knowledge = &dev1alpha1.KnowledgeSpec{
			Type:   "weknora",
			MCPURL: "http://weknora-mcp:8830/mcp",
			Bases:  []dev1alpha1.KnowledgeBaseRef{{ID: "kb-1", Name: "销售知识库"}},
		}
		plain.Spec.Memory = &dev1alpha1.MemorySpec{Type: "openviking", ServerURL: "http://openviking:8080", AccountID: "zhiyuan"}
		channelled := plain.DeepCopy()
		channelled.Spec.Channels = &dev1alpha1.ChannelsSpec{Wecom: true, Wechat: true, Feishu: true}

		config := renderConfigData(channelled, "")
		payload := marshalConfig(config)
		Expect(payload).To(ContainSubstring(`"channel_connections"`), "the ConfigMap payload keeps the block: a booting runtime reads it")

		fingerprinted := fingerprintPayload(config)
		Expect(fingerprinted).NotTo(ContainSubstring(`"channel_connections"`))
		var full, kept map[string]any
		Expect(json.Unmarshal([]byte(payload), &full)).To(Succeed())
		Expect(json.Unmarshal([]byte(fingerprinted), &kept)).To(Succeed())
		delete(full, channelConnectionsKey)
		Expect(kept).To(Equal(full), "only the channel block leaves the fingerprinted config")

		// An employee with channels enabled fingerprints exactly like the same
		// employee with none, so the toggle cannot move the pod template …
		channelFree := marshalConfig(renderConfigData(plain, ""))
		Expect(fingerprinted).To(Equal(channelFree))
		Expect(configChecksum(fingerprinted, renderExtensions(channelled))).
			To(Equal(configChecksum(channelFree, renderExtensions(plain))))
		// … while the payload the runtime boots from really does move, so the
		// exclusion is not a no-op.
		Expect(payload).NotTo(Equal(channelFree))
		Expect(configChecksum(payload, renderExtensions(channelled))).
			NotTo(Equal(configChecksum(fingerprinted, renderExtensions(channelled))))
	})
})
