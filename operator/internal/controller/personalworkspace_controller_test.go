// PersonalWorkspace reconcile specs: fake client + the suite's OpenViking
// stub (Ginkgo discipline, see AGENTS.md). The rendered children carry the
// workspace gateway contract: companions manifest (gateway roster input),
// per-companion memkey secrets, single-replica Recreate deployment, and the
// gateway service.
package controller

import (
	"context"

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

// workspaceFixture returns a complete PersonalWorkspace with two companions
// pointing at the suite's OpenViking stub.
func (f *fixture) workspaceFixture(name string) *v1alpha1.PersonalWorkspace {
	return &v1alpha1.PersonalWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: v1alpha1.PersonalWorkspaceSpec{
			Owner:     "owner-1",
			Runtime:   "pod",
			Resources: v1alpha1.WorkspaceResources{Memory: "1Gi", Disk: "5Gi"},
			Companions: []v1alpha1.CompanionSpec{
				{Name: "assistant", DisplayName: "工作助手", Model: "glm-4.6", Tools: []string{"files"}},
				{Name: "researcher", DisplayName: "调研员", Model: "glm-4.6",
					Memory: &v1alpha1.CompanionMemory{Scopes: []string{"private", "person"}}},
			},
			Memory: &v1alpha1.MemorySpec{
				Type: "openviking", ServerURL: f.server.URL, AccountID: "zhiyuan",
				SingleTenantInstance: true,
			},
			Credentials: v1alpha1.CredentialsRef{Name: "creds"},
		},
	}
}

func (f *fixture) workspaceReconciler() *PersonalWorkspaceReconciler {
	return &PersonalWorkspaceReconciler{
		Client: f.client,
		Scheme: f.client.Scheme(),
		OVRootFn: func(context.Context, client.Client, string, string) (string, error) {
			return "root-key", nil
		},
		WorkspaceImage: "reg/pi-companion:0.2.1",
		ModelBaseURL:   "http://model-gateway/v1",
	}
}

var _ = Describe("PersonalWorkspace reconciler", func() {
	var fx *fixture
	var workspace *v1alpha1.PersonalWorkspace

	BeforeEach(func(ctx context.Context) {
		fx = newFixture(ctx)
		workspace = fx.workspaceFixture("zhangsan")
		Expect(fx.client.Create(ctx, workspace)).To(Succeed())
	})

	reconcile := func(ctx context.Context) ctrl.Result {
		result, err := fx.workspaceReconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return result
	}

	getWorkspace := func(ctx context.Context) *v1alpha1.PersonalWorkspace {
		fresh := &v1alpha1.PersonalWorkspace{}
		ExpectWithOffset(1, fx.client.Get(ctx, client.ObjectKeyFromObject(workspace), fresh)).To(Succeed())
		return fresh
	}

	getDeployment := func(ctx context.Context) *appsv1.Deployment {
		deployment := &appsv1.Deployment{}
		ExpectWithOffset(1, fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspaceDeploymentName(workspace)}, deployment)).To(Succeed())
		return deployment
	}

	markGatewayReady := func(ctx context.Context) {
		deployment := getDeployment(ctx)
		deployment.Status.ObservedGeneration = deployment.Generation
		deployment.Status.ReadyReplicas = *deployment.Spec.Replicas
		Expect(fx.client.Status().Update(ctx, deployment)).To(Succeed())
	}

	Describe("child rendering", func() {
		It("renders manifest, pvc, deployment and service", func(ctx context.Context) {
			reconcile(ctx)

			manifest := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspaceConfigName(workspace)}, manifest)).To(Succeed())
			Expect(manifest.Data["companions.json"]).To(ContainSubstring(`"workspace": "zhangsan"`))
			Expect(manifest.Data["companions.json"]).To(ContainSubstring(`"name": "assistant"`))
			Expect(manifest.Data["companions.json"]).To(ContainSubstring(`"memkeySecret": "pw-zhangsan-researcher-memkey"`))
			Expect(manifest.Data["companions.json"]).To(ContainSubstring(`"memoryScopes": [`))

			claim := &corev1.PersistentVolumeClaim{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspacePVCName(workspace)}, claim)).To(Succeed())
			Expect(claim.Spec.AccessModes).To(ContainElement(corev1.ReadWriteOnce))
			Expect(claim.Spec.Resources.Requests.Storage().String()).To(Equal("5Gi"))

			deployment := getDeployment(ctx)
			Expect(deployment.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
			Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("reg/pi-companion:0.2.1"))
			env := map[string]string{}
			for _, item := range deployment.Spec.Template.Spec.Containers[0].Env {
				env[item.Name] = item.Value
			}
			Expect(env["BENCH_MODEL"]).To(Equal("glm-4.6"))
			Expect(env["BENCH_BASE_URL"]).To(Equal("http://model-gateway/v1"))
			Expect(env["COMPANION_STORAGE"]).To(Equal("/data/companion.sqlite"))
			apiKeyRef := corev1.EnvVar{}
			for _, item := range deployment.Spec.Template.Spec.Containers[0].Env {
				if item.Name == "BENCH_API_KEY" {
					apiKeyRef = item
				}
			}
			Expect(apiKeyRef.ValueFrom).NotTo(BeNil(), "model key must come from the credentials secret")
			Expect(apiKeyRef.ValueFrom.SecretKeyRef.Name).To(Equal("creds"))
			Expect(apiKeyRef.ValueFrom.SecretKeyRef.Key).To(Equal("BIGMODEL_API_KEY"))
			var memkeyRef string
			for _, source := range deployment.Spec.Template.Spec.Containers[0].EnvFrom {
				if source.SecretRef != nil {
					memkeyRef = source.SecretRef.Name
				}
			}
			Expect(memkeyRef).To(Equal("pw-zhangsan-assistant-memkey"))
			dataVolume := corev1.Volume{}
			for _, v := range deployment.Spec.Template.Spec.Volumes {
				if v.Name == "data" {
					dataVolume = v
				}
			}
			Expect(dataVolume.PersistentVolumeClaim).NotTo(BeNil(), "workspace data must ride the PVC")
			Expect(dataVolume.PersistentVolumeClaim.ClaimName).To(Equal("pw-zhangsan-data"))

			service := &corev1.Service{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspaceServiceName(workspace)}, service)).To(Succeed())
			Expect(service.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
			Expect(service.Spec.Ports[0].Port).To(Equal(int32(80)))
		})

		It("falls back to the operator image default", func(ctx context.Context) {
			ws := fx.workspaceFixture("lisi")
			Expect(fx.client.Create(ctx, ws)).To(Succeed())
			ws.Spec.Image = ""
			Expect(fx.client.Update(ctx, ws)).To(Succeed())
			_, err := fx.workspaceReconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}})
			Expect(err).NotTo(HaveOccurred())
			deployment := &appsv1.Deployment{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: workspaceDeploymentName(ws)}, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("reg/pi-companion:0.2.1"))
		})

		It("rejects the microvm runtime loudly", func(ctx context.Context) {
			updated := getWorkspace(ctx)
			updated.Spec.Runtime = "microvm"
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			_, err := fx.workspaceReconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
			Expect(err).To(MatchError(ContainSubstring("microvm")))

			fresh := getWorkspace(ctx)
			Expect(string(fresh.Status.Phase)).To(Equal("Failed"))
		})
	})

	Describe("memory identities", func() {
		It("mints one memkey per companion, once", func(ctx context.Context) {
			reconcile(ctx)
			reconcile(ctx)

			Expect(fx.stub.registers).To(Equal(2), "one register per companion, no re-mint on re-reconcile")
			secret := &corev1.Secret{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: "pw-zhangsan-assistant-memkey"}, secret)).To(Succeed())
			Expect(string(secret.Data[workspaceMemkeyKey])).To(Equal("ovkey-pw-zhangsan-assistant"))
			Expect(secret.Data[workspaceTokenKey]).NotTo(BeEmpty(), "internal token minted once alongside")

			fresh := getWorkspace(ctx)
			Expect(fresh.Status.MemoryUsers).To(ConsistOf("pw-zhangsan-assistant", "pw-zhangsan-researcher"))
		})

		It("honors an externally managed memkey secret", func(ctx context.Context) {
			updated := getWorkspace(ctx)
			updated.Spec.Companions[1].Memory.UserKeySecret = "external-key"
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconcile(ctx)

			Expect(fx.stub.registers).To(Equal(1), "externally managed companions are not minted")
			manifest := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspaceConfigName(workspace)}, manifest)).To(Succeed())
			Expect(manifest.Data["companions.json"]).To(ContainSubstring(`"memkeySecret": "external-key"`))
		})

		It("deletes the companions' memory users on finalization", func(ctx context.Context) {
			reconcile(ctx)
			Expect(fx.client.Delete(ctx, getWorkspace(ctx))).To(Succeed())
			_, err := fx.workspaceReconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
			Expect(err).NotTo(HaveOccurred())

			Expect(fx.stub.deletes).To(Equal(2))
			err = fx.client.Get(ctx, client.ObjectKeyFromObject(workspace), &v1alpha1.PersonalWorkspace{})
			Expect(err).To(HaveOccurred(), "finalizer released after memory cleanup")
		})

		It("skips memory provisioning when spec.memory is absent", func(ctx context.Context) {
			updated := getWorkspace(ctx)
			updated.Spec.Memory = nil
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconcile(ctx)

			Expect(fx.stub.registers).To(Equal(0))
			fresh := getWorkspace(ctx)
			Expect(fresh.Status.MemoryUsers).To(BeEmpty())
		})
	})

	Describe("rollout and availability", func() {
		It("stays Pending until the gateway is available, then Running with the endpoint", func(ctx context.Context) {
			reconcile(ctx)
			fresh := getWorkspace(ctx)
			Expect(string(fresh.Status.Phase)).To(Equal("Pending"))

			markGatewayReady(ctx)
			reconcile(ctx)

			fresh = getWorkspace(ctx)
			Expect(string(fresh.Status.Phase)).To(Equal("Running"))
			Expect(fresh.Status.GatewayEndpoint).To(Equal("pw-zhangsan.default.svc.cluster.local"))
			Expect(fresh.Status.ObservedGeneration).To(Equal(fresh.Generation))
		})

		It("rolls the deployment when the roster changes", func(ctx context.Context) {
			reconcile(ctx)
			before := getDeployment(ctx).Spec.Template.Annotations[configHashAnnotation]
			Expect(before).NotTo(BeEmpty())

			updated := getWorkspace(ctx)
			updated.Spec.Companions = append(updated.Spec.Companions,
				v1alpha1.CompanionSpec{Name: "writer", Model: "glm-4.6"})
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconcile(ctx)

			after := getDeployment(ctx).Spec.Template.Annotations[configHashAnnotation]
			Expect(after).NotTo(Equal(before), "roster edit must roll the gateway")

			manifest := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspaceConfigName(workspace)}, manifest)).To(Succeed())
			Expect(manifest.Data["companions.json"]).To(ContainSubstring(`"name": "writer"`))
		})

		It("is idempotent across reconciles", func(ctx context.Context) {
			reconcile(ctx)
			first := getDeployment(ctx)
			Expect(fx.client.Update(ctx, first)).To(Succeed()) // bump nothing; generation unchanged
			reconcile(ctx)
			second := getDeployment(ctx)
			Expect(second.ResourceVersion).To(Equal(first.ResourceVersion), "no-op reconcile must not touch the deployment")
		})

		It("requeues while the gateway deployment is missing", func(ctx context.Context) {
			reconcile(ctx)
			Expect(fx.client.Delete(ctx, getDeployment(ctx))).To(Succeed())
			result := reconcile(ctx)
			Expect(result.RequeueAfter).NotTo(BeZero(), "a missing gateway waits with a requeue, not an error")
		})
	})

	Describe("failure paths", func() {
		It("fails reconciliation when the root key source errors", func(ctx context.Context) {
			r := fx.workspaceReconciler()
			r.OVRootFn = func(context.Context, client.Client, string, string) (string, error) {
				return "", errWrap("stub: no root key", nil)
			}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
			Expect(err).To(HaveOccurred())
			Expect(string(getWorkspace(ctx).Status.Phase)).To(Equal("Failed"))
		})

		It("finalizes without memory provisioning", func(ctx context.Context) {
			updated := getWorkspace(ctx)
			updated.Spec.Memory = nil
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconcile(ctx)
			Expect(fx.client.Delete(ctx, getWorkspace(ctx))).To(Succeed())
			_, err := fx.workspaceReconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
			Expect(err).NotTo(HaveOccurred())
			Expect(fx.stub.deletes).To(Equal(0), "no memory users were ever minted")
			err = fx.client.Get(ctx, client.ObjectKeyFromObject(workspace), &v1alpha1.PersonalWorkspace{})
			Expect(err).To(HaveOccurred(), "no finalizer to hold deletion")
		})
	})
})
