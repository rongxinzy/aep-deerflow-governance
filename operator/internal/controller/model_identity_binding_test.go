package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

var _ = Describe("Employee explicit AEP agent mapping", func() {
	var fx *fixture
	var employee *v1alpha1.DigitalEmployee
	var r *DigitalEmployeeReconciler

	BeforeEach(func(ctx context.Context) {
		fx = newFixture(ctx)
		employee = fx.employeeFixture("bound-helper")
		employee.UID, employee.Generation = "employee-cr-uid", 1
		employee.Spec.Memory, employee.Spec.A2A = nil, nil
		employee.Spec.ModelAccess = &v1alpha1.ModelAccessSpec{AgentID: "employee-agent"}
		r = fx.reconciler()
		r.Client = identityTestClient{fx.client}
		r.ModelTokenAudience = "urn:zhiyuan:model-gateway:test"
	})
	AfterEach(func() { fx.server.Close(); fx.runtimeSrv.Close() })

	prepare := func(ctx context.Context) *appsv1.Deployment {
		d, err := r.renderDeployment(employee, "unchanged")
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, r.prepareModelIdentity(ctx, employee, d)).To(Succeed())
		return d
	}
	read := func(ctx context.Context) *v1alpha1.DigitalEmployee {
		value := &v1alpha1.DigitalEmployee{}
		ExpectWithOffset(1, fx.client.Get(ctx, client.ObjectKeyFromObject(employee), value)).To(Succeed())
		return value
	}
	step := func(ctx context.Context) *v1alpha1.DigitalEmployee {
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(employee)})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return read(ctx)
	}

	It("observes the exact owned SA, CR incarnation and explicit agent without using its owner", func(ctx context.Context) {
		d := prepare(ctx)
		sa := &corev1.ServiceAccount{}
		Expect(fx.client.Get(ctx, client.ObjectKey{Namespace: employee.Namespace, Name: d.Spec.Template.Spec.ServiceAccountName}, sa)).To(Succeed())
		Expect(employee.Status.ModelIdentity).To(Equal(&v1alpha1.ModelIdentityStatus{
			ObservedGeneration: 1, AgentID: "employee-agent", DeploymentID: employee.Spec.Governance.DeploymentID,
			EmployeeUID: employee.UID, ServiceAccountName: sa.Name, ServiceAccountUID: sa.UID, Audience: r.ModelTokenAudience,
		}))
		before := employee.Status.ModelIdentity.DeepCopy()
		employee.Spec.Owner, employee.Spec.Governance.DefaultAEPUser = "another-human", "another-owner"
		prepare(ctx)
		Expect(employee.Status.ModelIdentity).To(Equal(before))
	})

	It("clears an old observation when the explicit mapping is absent", func(ctx context.Context) {
		prepare(ctx)
		employee.Spec.ModelAccess = nil
		prepare(ctx)
		Expect(employee.Status.ModelIdentity).To(BeNil())
	})

	DescribeTable("rejects invalid explicit identity before provisioning",
		func(ctx context.Context, change func()) {
			change()
			Expect(fx.client.Create(ctx, employee)).To(Succeed())
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(employee)})
			Expect(err).To(HaveOccurred())
			stored := read(ctx)
			Expect(stored.Status.Phase).To(Equal(v1alpha1.PhaseError))
			Expect(stored.Status.ModelIdentity).To(BeNil())
			sa := &corev1.ServiceAccountList{}
			Expect(fx.client.List(ctx, sa)).To(Succeed())
			Expect(sa.Items).To(BeEmpty())
		},
		Entry("missing agent", func() { employee.Spec.ModelAccess.AgentID = "" }),
		Entry("oversized agent", func() { employee.Spec.ModelAccess.AgentID = strings.Repeat("x", 257) }),
		Entry("agent whitespace", func() { employee.Spec.ModelAccess.AgentID = "agent two" }),
		Entry("agent control", func() { employee.Spec.ModelAccess.AgentID = "agent\x00" }),
		Entry("missing deployment", func() { employee.Spec.Governance.DeploymentID = "" }),
		Entry("oversized deployment", func() { employee.Spec.Governance.DeploymentID = strings.Repeat("x", 81) }),
		Entry("deployment whitespace", func() { employee.Spec.Governance.DeploymentID = "deployment\n" }),
		Entry("disabled delivery", func() { r.ModelTokenAudience = "" }),
		Entry("invalid audience", func() { r.ModelTokenAudience = "aud\x00" }),
	)

	It("persists an SA replacement while phase and message stay Pending without a status loop", func(ctx context.Context) {
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		first := step(ctx)
		Expect(first.Status.Phase).To(Equal(v1alpha1.PhasePending))
		Expect(first.Status.ModelIdentity).NotTo(BeNil())
		sa := &corev1.ServiceAccount{}
		Expect(fx.client.Get(ctx, client.ObjectKey{Namespace: employee.Namespace, Name: first.Status.ModelIdentity.ServiceAccountName}, sa)).To(Succeed())
		Expect(fx.client.Delete(ctx, sa)).To(Succeed())
		sa.ResourceVersion, sa.UID = "", "replacement-sa-uid"
		Expect(fx.client.Create(ctx, sa)).To(Succeed())
		second := step(ctx)
		Expect(second.Status.Phase).To(Equal(first.Status.Phase))
		Expect(second.Status.ModelIdentity.ServiceAccountUID).To(Equal(sa.UID))
		Expect(second.ResourceVersion).NotTo(Equal(first.ResourceVersion))
		Expect(step(ctx).ResourceVersion).To(Equal(second.ResourceVersion))
	})

	It("persists a removed mapping and clears the previous observation", func(ctx context.Context) {
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		stored := step(ctx)
		stored.Spec.ModelAccess = nil
		Expect(fx.client.Update(ctx, stored)).To(Succeed())
		Expect(step(ctx).Status.ModelIdentity).To(BeNil())
	})

	It("does not rewrite a Ready observation after runtime bootstrap converges", func(ctx context.Context) {
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		step(ctx)
		d := &appsv1.Deployment{}
		Expect(fx.client.Get(ctx, client.ObjectKey{Namespace: employee.Namespace, Name: deploymentName(employee)}, d)).To(Succeed())
		d.Status.ObservedGeneration, d.Status.ReadyReplicas = d.Generation, *d.Spec.Replicas
		Expect(fx.client.Status().Update(ctx, d)).To(Succeed())
		first := step(ctx)
		Expect(first.Status.Phase).To(Equal(v1alpha1.PhaseReady))
		Expect(first.Status.RuntimeAccount).To(Equal(v1alpha1.RuntimeAccountReady))
		Expect(step(ctx).ResourceVersion).To(Equal(first.ResourceVersion))
	})

	It("clears a previous observation on reconciliation failure", func(ctx context.Context) {
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		stored := step(ctx)
		stored.Spec.Governance.DeploymentID = ""
		Expect(fx.client.Update(ctx, stored)).To(Succeed())
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(employee)})
		Expect(err).To(HaveOccurred())
		Expect(read(ctx).Status.ModelIdentity).To(BeNil())
	})

	It("does not overwrite concurrent spec changes through an old status object", func(ctx context.Context) {
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		stale := read(ctx)
		current := read(ctx)
		current.Spec.DisplayName = "concurrent edit"
		Expect(fx.client.Update(ctx, current)).To(Succeed())
		err := r.setStatus(ctx, stale, v1alpha1.PhasePending, "new observation")
		Expect(apierrors.IsConflict(err)).To(BeTrue())
		Expect(read(ctx).Spec.DisplayName).To(Equal("concurrent edit"))
	})

	It("does not recreate status after the employee has been deleted", func(ctx context.Context) {
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		stale := read(ctx)
		Expect(fx.client.Delete(ctx, stale)).To(Succeed())
		Expect(apierrors.IsNotFound(r.setStatus(ctx, stale, v1alpha1.PhasePending, ""))).To(BeTrue())
	})
})
