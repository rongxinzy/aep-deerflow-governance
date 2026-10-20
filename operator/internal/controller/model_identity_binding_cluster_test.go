package controller

import (
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

var _ = Describe("Employee explicit identity Kubernetes API", func() {
	It("preserves mapping/status through admission and enforces the identity transition", func(ctx SpecContext) {
		kubeconfig := os.Getenv("EMPLOYEE_IDENTITY_KUBECONFIG")
		if kubeconfig == "" {
			Skip("requires an explicitly selected disposable Kubernetes cluster")
		}
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		Expect(err).NotTo(HaveOccurred())
		fx := newFixture(ctx)
		DeferCleanup(func() { fx.server.Close(); fx.runtimeSrv.Close() })
		real, err := client.New(config, client.Options{Scheme: fx.client.Scheme()})
		Expect(err).NotTo(HaveOccurred())
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "employee-binding-"}}
		Expect(real.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(cleanup SpecContext) { Expect(real.Delete(cleanup, ns)).To(Succeed()) })
		employee := fx.employeeFixture("binding-probe")
		employee.Namespace, employee.Spec.Memory, employee.Spec.A2A = ns.Name, nil, nil
		employee.Spec.ModelAccess = &v1alpha1.ModelAccessSpec{AgentID: "probe-agent"}
		Expect(real.Create(ctx, employee)).To(Succeed())
		key := client.ObjectKeyFromObject(employee)
		read := func() *v1alpha1.DigitalEmployee {
			value := &v1alpha1.DigitalEmployee{}
			ExpectWithOffset(1, real.Get(ctx, key, value)).To(Succeed())
			return value
		}
		r := fx.reconciler()
		r.Client, r.ModelTokenAudience = real, "urn:zhiyuan:model-gateway:test"
		reconcile := func() *v1alpha1.DigitalEmployee {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			return read()
		}
		first := reconcile()
		Expect(first.Spec.ModelAccess.AgentID).To(Equal("probe-agent"))
		Expect(first.Status.ModelIdentity).NotTo(BeNil())
		Expect(first.Status.ModelIdentity.ObservedGeneration).To(Equal(first.Generation))
		Expect(first.Status.ModelIdentity.EmployeeUID).To(Equal(first.UID))
		Expect(first.Status.ModelIdentity.ServiceAccountUID).NotTo(BeEmpty())
		Expect(first.Spec.Owner).NotTo(Equal(first.Status.ModelIdentity.AgentID))
		Expect(reconcile().ResourceVersion).To(Equal(first.ResourceVersion))
		GinkgoWriter.Println("PASS: generated CRD preserves explicit mapping and credential-free typed status; unchanged Pending status stays stable")

		changed := read()
		changed.Spec.ModelAccess.AgentID = "different-agent"
		Expect(apierrors.IsInvalid(real.Update(ctx, changed))).To(BeTrue())
		changed = read()
		changed.Spec.Owner = "new-human-owner"
		Expect(real.Update(ctx, changed)).To(Succeed())
		fresh := reconcile()
		Expect(fresh.Generation).To(BeNumerically(">", first.Generation))
		Expect(fresh.Status.ModelIdentity.ObservedGeneration).To(Equal(fresh.Generation))
		Expect(fresh.Status.ModelIdentity.AgentID).To(Equal("probe-agent"))
		Expect(fresh.Status.ModelIdentity.ServiceAccountUID).To(Equal(first.Status.ModelIdentity.ServiceAccountUID))
		GinkgoWriter.Println("PASS: API rejects agent replacement; human owner edit preserves employee identity and updates observed generation")

		sa := &corev1.ServiceAccount{}
		Expect(real.Get(ctx, client.ObjectKey{Namespace: ns.Name, Name: fresh.Status.ModelIdentity.ServiceAccountName}, sa)).To(Succeed())
		Expect(real.Delete(ctx, sa)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(real.Get(ctx, client.ObjectKeyFromObject(sa), &corev1.ServiceAccount{}))
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
		replaced := reconcile()
		Expect(replaced.Status.Phase).To(Equal(v1alpha1.PhasePending))
		Expect(replaced.Status.ModelIdentity.ServiceAccountUID).NotTo(Equal(fresh.Status.ModelIdentity.ServiceAccountUID))
		GinkgoWriter.Println("PASS: same-phase SA recreation persists a new real API-assigned UID")

		replaced.Spec.ModelAccess = nil
		Expect(real.Update(ctx, replaced)).To(Succeed())
		Expect(reconcile().Status.ModelIdentity).To(BeNil())
		GinkgoWriter.Println("PASS: removing explicit mapping clears stored observation; future AEP writer must enforce historical fences")
	}, SpecTimeout(2*time.Minute))
})
