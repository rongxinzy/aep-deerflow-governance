package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// The fake client does not assign UIDs. Model the API server's assignment,
// never let the production path accept a ServiceAccount without its UID.
type identityTestClient struct{ client.Client }

func (c identityTestClient) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	if _, ok := object.(*corev1.ServiceAccount); ok && object.GetUID() == "" {
		object.SetUID("service-account-uid")
	}
	return c.Client.Create(ctx, object, opts...)
}

var _ = Describe("Employee model workload identity", func() {
	var fx *fixture
	var employee *v1alpha1.DigitalEmployee
	var reconciler *DigitalEmployeeReconciler

	BeforeEach(func(ctx context.Context) {
		fx = newFixture(ctx)
		employee = fx.employeeFixture("alice-helper")
		employee.UID = "employee-first-incarnation"
		reconciler = fx.reconciler()
		reconciler.Client = identityTestClient{fx.client}
		reconciler.ModelTokenAudience = "urn:zhiyuan:model-gateway:test"
	})
	AfterEach(func() { fx.server.Close(); fx.runtimeSrv.Close() })

	deployment := func() *appsv1.Deployment {
		value, err := reconciler.renderDeployment(employee, "unchanged")
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return value
	}
	prepare := func(ctx context.Context) *appsv1.Deployment {
		value := deployment()
		ExpectWithOffset(1, reconciler.prepareModelIdentity(ctx, employee, value)).To(Succeed())
		return value
	}
	account := func(ctx context.Context, name string) *corev1.ServiceAccount {
		value := &corev1.ServiceAccount{}
		ExpectWithOffset(1, fx.client.Get(ctx, client.ObjectKey{Namespace: employee.Namespace, Name: name}, value)).To(Succeed())
		return value
	}

	It("keeps legacy deployments unchanged until explicitly enabled", func(ctx context.Context) {
		reconciler.ModelTokenAudience = ""
		value := deployment()
		before := value.DeepCopy()
		Expect(reconciler.prepareModelIdentity(ctx, employee, value)).To(Succeed())
		Expect(value).To(Equal(before))
		list := &corev1.ServiceAccountList{}
		Expect(fx.client.List(ctx, list)).To(Succeed())
		Expect(list.Items).To(BeEmpty())
	})

	It("projects only a short-lived model-audience token into the runtime", func(ctx context.Context) {
		value := prepare(ctx)
		pod := value.Spec.Template.Spec
		Expect(pod.ServiceAccountName).NotTo(BeEmpty())
		Expect(pod.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		sa := account(ctx, pod.ServiceAccountName)
		Expect(sa.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		Expect(metav1.GetControllerOf(sa).UID).To(Equal(employee.UID))
		Expect(sa.Secrets).To(BeEmpty())
		Expect(sa.ImagePullSecrets).To(BeEmpty())
		Expect(value.Spec.Template.Annotations[modelAccountUIDAnnotation]).To(Equal(string(sa.UID)))
		Expect(pod.Containers[0].Env).To(ContainElement(corev1.EnvVar{Name: "ZHIYUAN_MODEL_TOKEN_FILE", Value: modelTokenDirectory + "/token"}))
		Expect(pod.Containers[0].VolumeMounts).To(ContainElement(corev1.VolumeMount{Name: modelTokenVolume, MountPath: modelTokenDirectory, ReadOnly: true}))
		var projection *corev1.ProjectedVolumeSource
		for _, volume := range pod.Volumes {
			if volume.Name == modelTokenVolume {
				projection = volume.Projected
			}
		}
		Expect(projection).NotTo(BeNil())
		Expect(projection.Sources).To(HaveLen(1))
		Expect(projection.Sources[0].ServiceAccountToken.Audience).To(Equal(reconciler.ModelTokenAudience))
		Expect(projection.Sources[0].ServiceAccountToken.ExpirationSeconds).To(HaveValue(Equal(int64(3600))))
		Expect(projection.Sources[0].ServiceAccountToken.Path).To(Equal("token"))
		for _, container := range pod.InitContainers {
			for _, mount := range container.VolumeMounts {
				Expect(mount.Name).NotTo(Equal(modelTokenVolume))
			}
		}
	})

	It("keeps identity and the deployment stable across ordinary reconciliation", func(ctx context.Context) {
		first := prepare(ctx)
		sa := account(ctx, first.Spec.Template.Spec.ServiceAccountName)
		second := prepare(ctx)
		Expect(second).To(Equal(first))
		Expect(account(ctx, sa.Name).ResourceVersion).To(Equal(sa.ResourceVersion))
	})

	It("delivers identity through the full employee reconcile entry point", func(ctx context.Context) {
		employee.Spec.Memory, employee.Spec.A2A = nil, nil
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(employee)})
		Expect(err).NotTo(HaveOccurred())
		value := &appsv1.Deployment{}
		Expect(fx.client.Get(ctx, client.ObjectKey{Namespace: employee.Namespace, Name: deploymentName(employee)}, value)).To(Succeed())
		Expect(value.Spec.Template.Spec.ServiceAccountName).To(Equal(modelAccountName(employee)))
		Expect(value.Spec.Template.Annotations[modelAccountUIDAnnotation]).NotTo(BeEmpty())
	})

	It("does not pass model credentials to init or A2A containers", func(ctx context.Context) {
		employee.Spec.ConversationStore.Backend = "postgres"
		reconciler.ConversationAdminDBURL = "postgres://unused.invalid/conversation"
		reconciler.ConversationInitImage = "postgres:17.6-alpine"
		value := prepare(ctx)
		Expect(value.Spec.Template.Spec.InitContainers).NotTo(BeEmpty())
		for _, container := range value.Spec.Template.Spec.InitContainers {
			for _, mount := range container.VolumeMounts {
				Expect(mount.Name).NotTo(Equal(modelTokenVolume))
			}
		}
		a2a := renderA2ADeployment(employee)
		for _, volume := range a2a.Spec.Template.Spec.Volumes {
			Expect(volume.Name).NotTo(Equal(modelTokenVolume))
		}
	})

	It("does not derive workload identity from the human owner", func(ctx context.Context) {
		first := prepare(ctx)
		employee.Spec.Owner = "different-human"
		Expect(prepare(ctx)).To(Equal(first))
	})

	It("isolates employees, namespaces, and deleted/recreated employee incarnations", func(ctx context.Context) {
		first := prepare(ctx).Spec.Template.Spec.ServiceAccountName
		employee.UID = "employee-second-incarnation"
		Expect(prepare(ctx).Spec.Template.Spec.ServiceAccountName).NotTo(Equal(first))
		employee.UID = "employee-first-incarnation"
		employee.Namespace = "another-namespace"
		Expect(prepare(ctx).Spec.Template.Spec.ServiceAccountName).NotTo(Equal(first))
	})

	It("rolls the runtime template when its owned ServiceAccount is recreated", func(ctx context.Context) {
		first := prepare(ctx)
		sa := account(ctx, first.Spec.Template.Spec.ServiceAccountName)
		Expect(fx.client.Delete(ctx, sa)).To(Succeed())
		sa.ResourceVersion, sa.UID = "", "replacement-service-account"
		Expect(fx.client.Create(ctx, sa)).To(Succeed())
		second := prepare(ctx)
		Expect(second.Spec.Template.Spec.ServiceAccountName).To(Equal(sa.Name))
		Expect(second.Spec.Template.Annotations[modelAccountUIDAnnotation]).To(Equal(string(sa.UID)))
		Expect(second.Spec.Template.Annotations).NotTo(Equal(first.Spec.Template.Annotations))
	})

	It("repairs automount drift without dropping unrelated ServiceAccount configuration", func(ctx context.Context) {
		value := prepare(ctx)
		sa := account(ctx, value.Spec.Template.Spec.ServiceAccountName)
		on := true
		sa.AutomountServiceAccountToken = &on
		sa.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry-credentials"}}
		Expect(fx.client.Update(ctx, sa)).To(Succeed())
		prepare(ctx)
		sa = account(ctx, sa.Name)
		Expect(sa.AutomountServiceAccountToken).To(HaveValue(BeFalse()))
		Expect(sa.ImagePullSecrets).To(HaveLen(1))
	})

	DescribeTable("refuses to adopt a foreign identity",
		func(ctx context.Context, alter func(*corev1.ServiceAccount)) {
			value := prepare(ctx)
			sa := account(ctx, value.Spec.Template.Spec.ServiceAccountName)
			alter(sa)
			Expect(fx.client.Update(ctx, sa)).To(Succeed())
			before := account(ctx, sa.Name)
			Expect(reconciler.prepareModelIdentity(ctx, employee, deployment())).NotTo(Succeed())
			Expect(account(ctx, sa.Name)).To(Equal(before))
		},
		Entry("unowned account", func(sa *corev1.ServiceAccount) { sa.OwnerReferences = nil }),
		Entry("another employee UID", func(sa *corev1.ServiceAccount) { sa.OwnerReferences[0].UID = types.UID("foreign") }),
		Entry("another kind", func(sa *corev1.ServiceAccount) { sa.OwnerReferences[0].Kind = "PersonalWorkspace" }),
	)

	It("refuses identity creation without a real employee UID", func(ctx context.Context) {
		employee.UID = ""
		Expect(reconciler.prepareModelIdentity(ctx, employee, deployment())).NotTo(Succeed())
	})
	DescribeTable("rejects invalid configured audiences",
		func(ctx context.Context, audience string) {
			reconciler.ModelTokenAudience = audience
			Expect(reconciler.prepareModelIdentity(ctx, employee, deployment())).NotTo(Succeed())
		},
		Entry("blank", " "), Entry("newline", "gateway\nwrong"), Entry("oversized", strings.Repeat("a", 257)),
	)
})
