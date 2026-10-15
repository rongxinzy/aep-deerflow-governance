// PersonalWorkspace reconcile specs: fake client + the suite's OpenViking
// stub (Ginkgo discipline, see AGENTS.md). The rendered children carry the
// workspace gateway contract: companions manifest (gateway roster input),
// per-companion memkey secrets, single-replica Recreate deployment, and the
// gateway service.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
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

// roomsChannelReconciler is the workspace reconciler with the employee A2A
// channel armed (agent credentials secret + AEP base URL).
func (f *fixture) roomsChannelReconciler() *PersonalWorkspaceReconciler {
	r := f.workspaceReconciler()
	r.RoomsAgentSecret = "rooms-agent-creds"
	r.RoomsAEPBaseURL = "http://aep-control.aep-system.svc.cluster.local:8080"
	return r
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
		It("persists persona and refreshes the gateway configuration when it changes", func(ctx context.Context) {
			reconcile(ctx)
			before := getDeployment(ctx).Spec.Template.Annotations
			fresh := getWorkspace(ctx)
			fresh.Spec.Companions[0].Persona = "用中文回答，协助整理工作。"
			Expect(fx.client.Update(ctx, fresh)).To(Succeed())
			reconcile(ctx)
			Expect(getWorkspace(ctx).Spec.Companions[0].Persona).To(Equal(fresh.Spec.Companions[0].Persona))
			manifest := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspaceConfigName(workspace)}, manifest)).To(Succeed())
			var roster companionsManifest
			Expect(json.Unmarshal([]byte(manifest.Data["companions.json"]), &roster)).To(Succeed())
			Expect(roster.Companions[0].Persona).To(Equal(fresh.Spec.Companions[0].Persona))
			Expect(roster.Companions[1].Persona).To(BeEmpty())
			Expect(getDeployment(ctx).Spec.Template.Annotations).NotTo(Equal(before))
			fresh = getWorkspace(ctx)
			fresh.Spec.Companions[0].Persona = ""
			Expect(fx.client.Update(ctx, fresh)).To(Succeed())
			reconcile(ctx)
			Expect(fx.client.Get(ctx, client.ObjectKeyFromObject(manifest), manifest)).To(Succeed())
			Expect(manifest.Data["companions.json"]).NotTo(ContainSubstring(`"persona"`))
		})

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

		It("injects the rooms employee channel when armed, stays off otherwise", func(ctx context.Context) {
			// Channel armed: credentials secret rides envFrom, AEP base URL is explicit.
			_, err := fx.roomsChannelReconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
			Expect(err).NotTo(HaveOccurred())
			deployment := getDeployment(ctx)
			var hasRoomsSecret bool
			for _, source := range deployment.Spec.Template.Spec.Containers[0].EnvFrom {
				if source.SecretRef != nil && source.SecretRef.Name == "rooms-agent-creds" {
					hasRoomsSecret = true
				}
			}
			Expect(hasRoomsSecret).To(BeTrue(), "rooms agent credentials secret must ride envFrom")
			var aepBase string
			for _, item := range deployment.Spec.Template.Spec.Containers[0].Env {
				if item.Name == "AEP_BASE_URL" {
					aepBase = item.Value
				}
			}
			Expect(aepBase).To(Equal("http://aep-control.aep-system.svc.cluster.local:8080"))

			// Channel off (default): neither the secret nor the env appears.
			off := fx.workspaceFixture("lisi")
			off.Spec.Image = "reg/pi-companion:0.2.1"
			Expect(fx.client.Create(ctx, off)).To(Succeed())
			_, err = fx.workspaceReconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: off.Namespace, Name: off.Name}})
			Expect(err).NotTo(HaveOccurred())
			offDeployment := &appsv1.Deployment{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: off.Namespace, Name: workspaceDeploymentName(off)}, offDeployment)).To(Succeed())
			for _, source := range offDeployment.Spec.Template.Spec.Containers[0].EnvFrom {
				if source.SecretRef != nil && source.SecretRef.Name == "rooms-agent-creds" {
					Fail("rooms agent secret must not ride the deployment when the channel is off")
				}
			}
			for _, item := range offDeployment.Spec.Template.Spec.Containers[0].Env {
				if item.Name == "AEP_BASE_URL" {
					Fail("AEP_BASE_URL must not be set when the channel is off")
				}
			}
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

		It("passes the companion persona through to the manifest, omitting empty ones", func(ctx context.Context) {
			updated := getWorkspace(ctx)
			updated.Spec.Companions[0].Persona = "你是用户的工作助手，语气简洁专业。"
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconcile(ctx)

			manifest := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspaceConfigName(workspace)}, manifest)).To(Succeed())
			Expect(manifest.Data["companions.json"]).To(ContainSubstring(`"persona": "你是用户的工作助手，语气简洁专业。"`))
			Expect(strings.Count(manifest.Data["companions.json"], `"persona":`)).To(Equal(1),
				"an empty persona must not appear as a key (researcher carries none)")
		})

		It("injects per-companion OpenViking env from each memkey secret plus the global base URL", func(ctx context.Context) {
			reconcile(ctx)

			deployment := getDeployment(ctx)
			env := map[string]corev1.EnvVar{}
			for _, item := range deployment.Spec.Template.Spec.Containers[0].Env {
				env[item.Name] = item
			}
			assistantKey := env["PW_ASSISTANT_OPENVIKING_API_KEY"]
			Expect(assistantKey.ValueFrom).NotTo(BeNil(), "per-companion key must come from the memkey secret")
			Expect(assistantKey.ValueFrom.SecretKeyRef.Name).To(Equal("pw-zhangsan-assistant-memkey"))
			Expect(assistantKey.ValueFrom.SecretKeyRef.Key).To(Equal("OPENVIKING_API_KEY"))
			researcherKey := env["PW_RESEARCHER_OPENVIKING_API_KEY"]
			Expect(researcherKey.ValueFrom).NotTo(BeNil(), "every roster entry gets its own env, not just companions[0]")
			Expect(researcherKey.ValueFrom.SecretKeyRef.Name).To(Equal("pw-zhangsan-researcher-memkey"))
			Expect(researcherKey.ValueFrom.SecretKeyRef.Key).To(Equal("OPENVIKING_API_KEY"))

			Expect(env["OPENVIKING_BASE_URL"].Value).To(Equal(fx.server.URL), "spec.memory.serverURL wins")

			var unprefixed bool
			for _, source := range deployment.Spec.Template.Spec.Containers[0].EnvFrom {
				if source.SecretRef != nil && source.SecretRef.Name == "pw-zhangsan-assistant-memkey" {
					unprefixed = true
				}
			}
			Expect(unprefixed).To(BeTrue(), "companions[0]'s unprefixed envFrom stays for the single-companion runtime")
		})

		It("falls back to the shared OpenViking service URL when spec.memory is absent", func(ctx context.Context) {
			updated := getWorkspace(ctx)
			updated.Spec.Memory = nil
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconcile(ctx)

			deployment := getDeployment(ctx)
			var baseURL string
			for _, item := range deployment.Spec.Template.Spec.Containers[0].Env {
				if item.Name == "OPENVIKING_BASE_URL" {
					baseURL = item.Value
				}
			}
			Expect(baseURL).To(Equal(defaultOpenVikingBaseURL))
		})

		It("assigns the microvm runtime to a pool node without rendering pod children", func(ctx context.Context) {
			updated := getWorkspace(ctx)
			updated.Spec.Runtime = "microvm"
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconciler := fx.workspaceReconciler()
			reconciler.MicrovmNodes = []string{"node-a", "node-b"}
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
			Expect(err).NotTo(HaveOccurred())

			fresh := getWorkspace(ctx)
			Expect(string(fresh.Status.Phase)).To(Equal("Pending"))
			Expect(fresh.Status.NodeName).To(Equal("node-a")) // pool order wins ties
			Expect(fresh.Status.VMSubnet).To(Equal("192.168.77.0/30"))

			// No pod-form children; the shared manifest ConfigMap still renders
			// (the node agent's config-disk source), and memkeys still mint.
			deployment := &appsv1.Deployment{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspaceDeploymentName(workspace)}, deployment)).NotTo(Succeed())
			service := &corev1.Service{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspaceServiceName(workspace)}, service)).NotTo(Succeed())
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspacePVCName(workspace)}, pvc)).NotTo(Succeed())
			manifest := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspaceConfigName(workspace)}, manifest)).To(Succeed())
			Expect(manifest.Data["companions.json"]).To(ContainSubstring("assistant"))
			Expect(fx.stub.registers).To(Equal(2)) // memkeys mint for both companions, same as the pod path
		})

		It("keeps the assignment sticky across reconciles", func(ctx context.Context) {
			updated := getWorkspace(ctx)
			updated.Spec.Runtime = "microvm"
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconciler := fx.workspaceReconciler()
			reconciler.MicrovmNodes = []string{"node-a", "node-b"}
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
			Expect(err).NotTo(HaveOccurred())
			first := getWorkspace(ctx)

			// A second reconcile (and even a pool reorder) never reassigns.
			reconciler.MicrovmNodes = []string{"node-b", "node-a"}
			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
			Expect(err).NotTo(HaveOccurred())
			fresh := getWorkspace(ctx)
			Expect(fresh.Status.NodeName).To(Equal(first.Status.NodeName))
			Expect(fresh.Status.VMSubnet).To(Equal(first.Status.VMSubnet))
		})

		It("carves a unique subnet and balances the next microvm workspace", func(ctx context.Context) {
			updated := getWorkspace(ctx)
			updated.Spec.Runtime = "microvm"
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconciler := fx.workspaceReconciler()
			reconciler.MicrovmNodes = []string{"node-a", "node-b"}
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
			Expect(err).NotTo(HaveOccurred())

			second := fx.workspaceFixture("lisi")
			second.Spec.Runtime = "microvm"
			Expect(fx.client.Create(ctx, second)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: second.Namespace, Name: second.Name}})
			Expect(err).NotTo(HaveOccurred())

			freshSecond := &v1alpha1.PersonalWorkspace{}
			Expect(fx.client.Get(ctx, client.ObjectKeyFromObject(second), freshSecond)).To(Succeed())
			Expect(freshSecond.Status.VMSubnet).To(Equal("192.168.77.4/30")) // next free /30
			Expect(freshSecond.Status.NodeName).To(Equal("node-b"))          // 1-vs-0 balance
		})

		It("fails the microvm runtime loudly when no node pool is configured", func(ctx context.Context) {
			updated := getWorkspace(ctx)
			updated.Spec.Runtime = "microvm"
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			_, err := fx.workspaceReconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
			Expect(err).To(MatchError(ContainSubstring("WORKSPACE_MICROVM_NODES")))

			fresh := getWorkspace(ctx)
			Expect(string(fresh.Status.Phase)).To(Equal("Failed"))
		})

		It("rejects duplicate companion names", func(ctx context.Context) {
			updated := getWorkspace(ctx)
			updated.Spec.Companions = []v1alpha1.CompanionSpec{
				{Name: "assistant", Model: "glm-4.6"},
				{Name: "assistant", Model: "glm-4.6"},
			}
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			_, err := fx.workspaceReconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
			Expect(err).To(MatchError(ContainSubstring("duplicate companion name")))

			Expect(string(getWorkspace(ctx).Status.Phase)).To(Equal("Failed"))
			Expect(fx.stub.registers).To(Equal(0), "a rejected roster must not mint identities")
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

		It("deletes a removed companion's memory user immediately", func(ctx context.Context) {
			reconcile(ctx)
			updated := getWorkspace(ctx)
			updated.Spec.Companions = updated.Spec.Companions[:1] // drop "researcher"
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconcile(ctx)

			Expect(fx.stub.deletes).To(Equal(1), "roster removal cleans up the minted user at once")
			Expect(getWorkspace(ctx).Status.MemoryUsers).To(ConsistOf("pw-zhangsan-assistant"))
		})

		It("refuses to drop spec.memory after provisioning", func(ctx context.Context) {
			reconcile(ctx)
			updated := getWorkspace(ctx)
			updated.Spec.Memory = nil
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			_, err := fx.workspaceReconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}})
			Expect(err).To(MatchError(ContainSubstring("spec.memory cannot be removed")))
			Expect(fx.stub.deletes).To(Equal(0), "refusal must not silently delete identities")
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
				return "", fmt.Errorf("stub: no root key")
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
