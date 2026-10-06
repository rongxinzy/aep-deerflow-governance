package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// Knowledge-base whitelist and business-layer fields: an explicit
// spec.knowledge.bases policy must reach the governance middleware kwargs
// (empty list = deny every search; nil = legacy unrestricted credential),
// while description/skills/visibility are stored CR fields the operator
// renders nowhere and must keep intact across reconciles.
var _ = Describe("knowledge bases and business-layer fields", func() {
	setup := func(ctx context.Context, mutate func(*dev1alpha1.DigitalEmployee)) (*fixture, *dev1alpha1.DigitalEmployee) {
		fx := newFixture(ctx)
		employee := fx.employeeFixture("kb-helper")
		if mutate != nil {
			mutate(employee)
		}
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		Expect(err).NotTo(HaveOccurred())
		return fx, employee
	}

	fetchConfig := func(ctx context.Context, fx *fixture, employee *dev1alpha1.DigitalEmployee) string {
		config := &corev1.ConfigMap{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
		return config.Data["config.yaml"]
	}

	fetchExtensions := func(ctx context.Context, fx *fixture, employee *dev1alpha1.DigitalEmployee) string {
		extensions := &corev1.ConfigMap{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: extensionsName(employee)}, extensions)).To(Succeed())
		return extensions.Data["extensions_config.json"]
	}

	It("renders an explicit whitelist into the middleware kwargs", func(ctx context.Context) {
		fx, employee := setup(ctx, nil) // employeeFixture carries kb-1/kb-2
		config := fetchConfig(ctx, fx, employee)
		Expect(config).To(ContainSubstring(`"knowledge_bases"`))
		Expect(config).To(ContainSubstring(`"kb-1": "销售知识库"`))
		Expect(config).To(ContainSubstring(`"kb-2": "产品知识库"`))
	})

	It("renders an empty whitelist as deny-all, keeping the weknora MCP server", func(ctx context.Context) {
		fx, employee := setup(ctx, func(e *dev1alpha1.DigitalEmployee) {
			e.Spec.Knowledge.Bases = []dev1alpha1.KnowledgeBaseRef{}
		})
		Expect(fetchConfig(ctx, fx, employee)).To(ContainSubstring(`"knowledge_bases": {}`))
		Expect(fetchExtensions(ctx, fx, employee)).To(ContainSubstring("weknora"))
	})

	It("omits the kwarg entirely for legacy nil bases", func(ctx context.Context) {
		fx, employee := setup(ctx, func(e *dev1alpha1.DigitalEmployee) {
			e.Spec.Knowledge.Bases = nil
		})
		Expect(fetchConfig(ctx, fx, employee)).NotTo(ContainSubstring(`"knowledge_bases"`))
	})

	It("keeps description, skills and visibility intact across reconciles", func(ctx context.Context) {
		fx, employee := setup(ctx, nil)
		for range 2 {
			_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
			Expect(err).NotTo(HaveOccurred())
		}
		fresh := &dev1alpha1.DigitalEmployee{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}, fresh)).To(Succeed())
		Expect(fresh.Spec.Description).To(Equal("整理销售资料并辅助生成周报。"))
		Expect(fresh.Spec.Skills).To(Equal([]dev1alpha1.SkillRef{{ID: "report-writer", Name: "报告撰写", Version: "1.2.0"}}))
		Expect(fresh.Spec.Visibility).To(Equal(&dev1alpha1.VisibilitySpec{
			Mode:  "restricted",
			Teams: []dev1alpha1.SubjectRef{{ID: "sales", Name: "销售团队"}},
			Users: []dev1alpha1.SubjectRef{{ID: "u-9", Name: "李四"}},
		}))
		// Nowhere in the rendered config: these are business-layer fields.
		Expect(fetchConfig(ctx, fx, employee)).NotTo(ContainSubstring("report-writer"))
		Expect(fetchConfig(ctx, fx, employee)).NotTo(ContainSubstring(`"visibility"`))
	})

	// The runtime reads its config at boot: a rendered-config change must
	// move the deployment's checksum annotation or an edited whitelist only
	// ever applies to freshly created employees.
	fetchChecksum := func(ctx context.Context, fx *fixture, employee *dev1alpha1.DigitalEmployee) string {
		deployment := &appsv1.Deployment{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: deploymentName(employee)}, deployment)).To(Succeed())
		return deployment.Spec.Template.Annotations[configHashAnnotation]
	}

	It("rolls the deployment when the rendered config changes", func(ctx context.Context) {
		fx, employee := setup(ctx, nil)
		first := fetchChecksum(ctx, fx, employee)
		Expect(first).NotTo(BeEmpty())

		// Unchanged spec → identical checksum (no rollout churn).
		_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fetchChecksum(ctx, fx, employee)).To(Equal(first))

		// Edited whitelist → new checksum on the pod template.
		fresh := &dev1alpha1.DigitalEmployee{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}, fresh)).To(Succeed())
		fresh.Spec.Knowledge.Bases = []dev1alpha1.KnowledgeBaseRef{{ID: "kb-3", Name: "财务知识库"}}
		Expect(fx.client.Update(ctx, fresh)).To(Succeed())
		_, err = fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fetchChecksum(ctx, fx, employee)).NotTo(Equal(first))
		Expect(fetchConfig(ctx, fx, employee)).To(ContainSubstring(`"kb-3": "财务知识库"`))
	})
})
