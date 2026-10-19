/* Copyright 2026.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

var _ = Describe("DigitalEmployee reconciler", func() {
	var fx *fixture
	var employee *v1alpha1.DigitalEmployee

	BeforeEach(func(ctx context.Context) {
		fx = newFixture(ctx)
		employee = fx.employeeFixture("alice-helper")
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
	})

	reconcile := func(ctx context.Context) ctrl.Result {
		result, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return result
	}

	getEmployee := func(ctx context.Context) *v1alpha1.DigitalEmployee {
		fresh := &v1alpha1.DigitalEmployee{}
		ExpectWithOffset(1, fx.client.Get(ctx, client.ObjectKeyFromObject(employee), fresh)).To(Succeed())
		return fresh
	}

	markDeploymentReady := func(ctx context.Context) {
		deployment := &appsv1.Deployment{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: deploymentName(employee)}, deployment)).To(Succeed())
		deployment.Status.ObservedGeneration = deployment.Generation
		deployment.Status.ReadyReplicas = *deployment.Spec.Replicas
		Expect(fx.client.Status().Update(ctx, deployment)).To(Succeed())
	}

	Describe("child rendering", func() {
		It("renders config, extensions, deployment, service and the a2a adapter", func(ctx context.Context) {
			reconcile(ctx)

			config := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
			Expect(config.Data["config.yaml"]).To(ContainSubstring(`"manager_class": "openviking"`))
			Expect(config.Data["config.yaml"]).To(ContainSubstring(`"owner_user_id": "alice-helper"`))
			Expect(config.Data["config.yaml"]).To(ContainSubstring(`"single_tenant_instance": true`))
			Expect(config.Data["config.yaml"]).To(ContainSubstring(`"aep_base_url": "http://aep:8080"`))

			extensions := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: extensionsName(employee)}, extensions)).To(Succeed())
			Expect(extensions.Data["extensions_config.json"]).To(ContainSubstring(`"weknora"`))
			Expect(extensions.Data["extensions_config.json"]).To(ContainSubstring(`"X-MCP-Auth-Token"`))

			runtimeDeployment := &appsv1.Deployment{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: deploymentName(employee)}, runtimeDeployment)).To(Succeed())
			Expect(runtimeDeployment.Spec.Template.Spec.Containers[0].Image).To(Equal("reg/deerflow:v7"))
			env := map[string]string{}
			for _, item := range runtimeDeployment.Spec.Template.Spec.Containers[0].Env {
				env[item.Name] = item.Value
			}
			Expect(env).NotTo(HaveKey("DEER_FLOW_AUTH_DISABLED"), "authentication must stay on")

			service := &corev1.Service{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: serviceName(employee)}, service)).To(Succeed())

			a2aDeployment := &appsv1.Deployment{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: a2aDeploymentName(employee)}, a2aDeployment)).To(Succeed())
			a2aEnv := map[string]string{}
			for _, item := range a2aDeployment.Spec.Template.Spec.Containers[0].Env {
				a2aEnv[item.Name] = item.Value
			}
			Expect(a2aEnv["A2A_SELF_NAME"]).To(Equal("alice-helper"))
			Expect(a2aEnv["DEERFLOW_URL"]).To(Equal("http://de-alice-helper:8001"))
		})

		It("renders the email secret envFrom into the runtime when spec.email is set", func(ctx context.Context) {
			updated := &v1alpha1.DigitalEmployee{}
			Expect(fx.client.Get(ctx, client.ObjectKeyFromObject(employee), updated)).To(Succeed())
			updated.Spec.Email = &v1alpha1.EmailSpec{SecretName: "de-alice-helper-email"}
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconcile(ctx)

			runtimeDeployment := &appsv1.Deployment{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: deploymentName(employee)}, runtimeDeployment)).To(Succeed())
			names := []string{}
			for _, source := range runtimeDeployment.Spec.Template.Spec.Containers[0].EnvFrom {
				if source.SecretRef != nil {
					names = append(names, source.SecretRef.Name)
				}
			}
			Expect(names).To(ContainElement("de-alice-helper-email"), "mailbox credentials must ride envFrom into the runtime")
		})

		It("renders a durable home PVC and Recreate strategy when persistence is enabled", func(ctx context.Context) {
			updated := &v1alpha1.DigitalEmployee{}
			Expect(fx.client.Get(ctx, client.ObjectKeyFromObject(employee), updated)).To(Succeed())
			updated.Spec.Persistence = v1alpha1.PersistenceSpec{Enabled: true, Size: "3Gi"}
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconcile(ctx)

			claim := &corev1.PersistentVolumeClaim{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: "de-alice-helper-home"}, claim)).To(Succeed())
			Expect(claim.Spec.AccessModes).To(ContainElement(corev1.ReadWriteOnce))
			Expect(claim.Spec.Resources.Requests.Storage().String()).To(Equal("3Gi"))

			deployment := &appsv1.Deployment{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: deploymentName(employee)}, deployment)).To(Succeed())
			Expect(deployment.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
			home := corev1.Volume{}
			for _, v := range deployment.Spec.Template.Spec.Volumes {
				if v.Name == "home" {
					home = v
				}
			}
			Expect(home.PersistentVolumeClaim).NotTo(BeNil(), "home volume must reference the PVC")
			Expect(home.PersistentVolumeClaim.ClaimName).To(Equal("de-alice-helper-home"))
		})

		It("keeps the home volume ephemeral when persistence is disabled", func(ctx context.Context) {
			reconcile(ctx)
			deployment := &appsv1.Deployment{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: deploymentName(employee)}, deployment)).To(Succeed())
			home := corev1.Volume{}
			for _, v := range deployment.Spec.Template.Spec.Volumes {
				if v.Name == "home" {
					home = v
				}
			}
			Expect(home.EmptyDir).NotTo(BeNil())
		})

		It("defaults memory scope to instance without a shared bucket", func(ctx context.Context) {
			reconcile(ctx)

			config := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
			Expect(config.Data["config.yaml"]).To(ContainSubstring(`"memory_scope": "instance"`))
			Expect(config.Data["config.yaml"]).NotTo(ContainSubstring("shared_peer_id"))
		})

		It("renders requester-scoped memory with a curated shared bucket", func(ctx context.Context) {
			employee.Spec.Memory.MemoryScope = "requester"
			employee.Spec.Memory.SharedPeerID = "df-shared-finance"
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			reconcile(ctx)

			config := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
			Expect(config.Data["config.yaml"]).To(ContainSubstring(`"memory_scope": "requester"`))
			Expect(config.Data["config.yaml"]).To(ContainSubstring(`"shared_peer_id": "df-shared-finance"`))
		})

		It("renders channel-connections only for enabled IM channels", func(ctx context.Context) {
			employee.Spec.Channels = &v1alpha1.ChannelsSpec{Wecom: true, Wechat: true}
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			reconcile(ctx)

			config := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
			Expect(config.Data["config.yaml"]).To(ContainSubstring(`"channel_connections"`))
			Expect(config.Data["config.yaml"]).To(ContainSubstring(`"wecom"`))
			Expect(config.Data["config.yaml"]).To(ContainSubstring(`"wechat"`))
			Expect(config.Data["config.yaml"]).NotTo(ContainSubstring(`"feishu"`), "disabled providers stay unlisted")

			// The first reconcile adds the memory finalizer; re-read before
			// the second mutation.
			updated := &v1alpha1.DigitalEmployee{}
			Expect(fx.client.Get(ctx, client.ObjectKeyFromObject(employee), updated)).To(Succeed())
			updated.Spec.Channels = nil
			Expect(fx.client.Update(ctx, updated)).To(Succeed())
			reconcile(ctx)
			fresh := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, fresh)).To(Succeed())
			// The subsystem block stays rendered (a hot enable needs a live
			// channel service to start a leg on); only the declared provider
			// entries disappear.
			Expect(fresh.Data["config.yaml"]).To(ContainSubstring(`"channel_connections"`))
			Expect(fresh.Data["config.yaml"]).To(ContainSubstring(`"enabled": true`))
			Expect(fresh.Data["config.yaml"]).NotTo(ContainSubstring(`"wecom"`))
			Expect(fresh.Data["config.yaml"]).NotTo(ContainSubstring(`"wechat"`))
		})

		It("renders the feishu bot name for the group mention gate", func(ctx context.Context) {
			employee.Spec.Channels = &v1alpha1.ChannelsSpec{Feishu: true, FeishuName: "数字员工"}
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			reconcile(ctx)

			config := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
			rendered := config.Data["config.yaml"]
			Expect(rendered).To(ContainSubstring(`"bot_name": "数字员工"`))
			// Only the display name is rendered here; `enabled` and the
			// credentials still come from the runtime store.
			Expect(rendered).To(ContainSubstring(`"channels"`))
		})

		It("omits the feishu bot name when the CR does not set one", func(ctx context.Context) {
			employee.Spec.Channels = &v1alpha1.ChannelsSpec{Feishu: true}
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			reconcile(ctx)

			config := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
			Expect(config.Data["config.yaml"]).NotTo(ContainSubstring("bot_name"))
		})

		It("renders channel endpoint overrides so a mock or self-hosted gateway can be used", func(ctx context.Context) {
			employee.Spec.Channels = &v1alpha1.ChannelsSpec{
				Feishu:       true,
				FeishuDomain: "http://mock-feishu:18086",
				Wecom:        true,
				WecomWSURL:   "wss://channel-gateway:8899",
			}
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			reconcile(ctx)

			config := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
			rendered := config.Data["config.yaml"]
			Expect(rendered).To(ContainSubstring(`"domain": "http://mock-feishu:18086"`))
			Expect(rendered).To(ContainSubstring(`"ws_url": "wss://channel-gateway:8899"`))
			// The endpoints are declarative only; `enabled` and the credentials
			// still come from the runtime store.
			Expect(rendered).NotTo(ContainSubstring(`"app_id"`))
		})

		It("omits channel endpoint overrides when the CR does not set them", func(ctx context.Context) {
			employee.Spec.Channels = &v1alpha1.ChannelsSpec{Feishu: true, Wecom: true}
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			reconcile(ctx)

			config := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
			rendered := config.Data["config.yaml"]
			Expect(rendered).NotTo(ContainSubstring(`"domain"`))
			Expect(rendered).NotTo(ContainSubstring(`"ws_url"`))
		})

		It("keeps the spec.owner field intact across reconciles", func(ctx context.Context) {
			reconcile(ctx)
			markDeploymentReady(ctx)
			reconcile(ctx)
			Expect(getEmployee(ctx).Spec.Owner).To(Equal("owner-1"))
		})
	})

	Describe("memory credential lifecycle", func() {
		It("mints the memory key and internal token exactly once", func(ctx context.Context) {
			reconcile(ctx)
			reconcile(ctx)
			reconcile(ctx)

			secret := &corev1.Secret{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: memkeySecretName(employee)}, secret)).To(Succeed())
			Expect(secret.Data).To(HaveKey("OPENVIKING_API_KEY"))
			Expect(secret.Data).To(HaveKey(internalTokenKey))
			Expect(fx.stub.registers).To(Equal(1))
			Expect(fx.stub.regens).To(Equal(0), "an existing user with a persisted key must never be regenerated")
		})

		It("recovers a lost key by regenerating without rotating the token", func(ctx context.Context) {
			reconcile(ctx)
			secret := &corev1.Secret{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: memkeySecretName(employee)}, secret)).To(Succeed())
			token := secret.Data[internalTokenKey]

			// Simulate key loss: strip only the memory key.
			patch := secret.DeepCopy()
			delete(patch.Data, memkeySecretKey)
			Expect(fx.client.Update(context.Background(), patch)).To(Succeed())

			reconcile(ctx)
			after := &corev1.Secret{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: memkeySecretName(employee)}, after)).To(Succeed())
			Expect(after.Data[memkeySecretKey]).NotTo(BeEmpty())
			Expect(after.Data[internalTokenKey]).To(Equal(token), "the internal token must never rotate")
			Expect(fx.stub.regens).To(Equal(1))
		})

		It("deletes the OpenViking user on finalization", func(ctx context.Context) {
			reconcile(ctx)
			Expect(fx.client.Delete(ctx, employee)).To(Succeed())
			reconcile(ctx)

			err := fx.client.Get(ctx, client.ObjectKeyFromObject(employee), &v1alpha1.DigitalEmployee{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
			Expect(fx.stub.deletes).To(Equal(1))
		})
	})

	Describe("status transitions", func() {
		It("goes Pending while the deployment rolls out and Ready once available", func(ctx context.Context) {
			result := reconcile(ctx)
			Expect(result.RequeueAfter).To(BeNumerically(">", time.Duration(0)), "a not-yet-available deployment requeues")
			Expect(getEmployee(ctx).Status.Phase).To(Equal(v1alpha1.PhasePending))

			markDeploymentReady(ctx)
			reconcile(ctx)
			fresh := getEmployee(ctx)
			Expect(fresh.Status.Phase).To(Equal(v1alpha1.PhaseReady))
			Expect(fresh.Status.MemoryUser).To(Equal("alice-helper"))
		})

		It("reports Error when the OpenViking admin API is unreachable", func(ctx context.Context) {
			employee.Spec.Memory.ServerURL = "http://127.0.0.1:1"
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
			Expect(err).To(HaveOccurred())
			Expect(getEmployee(ctx).Status.Phase).To(Equal(v1alpha1.PhaseError))
		})
	})
})

var _ = Describe("DigitalEmployee hibernation", func() {
	var fx *fixture
	var employee *v1alpha1.DigitalEmployee

	enable := func(on bool, idle string) *v1alpha1.HibernationSpec {
		return &v1alpha1.HibernationSpec{Enabled: &on, IdleTimeout: idle}
	}

	reconcile := func(ctx context.Context) (ctrl.Result, error) {
		return fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
	}

	getEmployee := func(ctx context.Context) *v1alpha1.DigitalEmployee {
		fresh := &v1alpha1.DigitalEmployee{}
		ExpectWithOffset(1, fx.client.Get(ctx, client.ObjectKeyFromObject(employee), fresh)).To(Succeed())
		return fresh
	}

	getScaledObject := func(ctx context.Context) (*KedaScaledObject, bool) {
		so := &KedaScaledObject{}
		err := fx.client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: deploymentName(employee) + "-wake"}, so)
		return so, err == nil
	}

	BeforeEach(func(ctx context.Context) {
		fx = newFixture(ctx)
		fx.wakeDBURL = "postgresql://wake:wak3pass@pg.db.svc:5432/deportal"
		employee = fx.employeeFixture("alice-helper")
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
	})

	Describe("scaled-object rendering", func() {
		It("renders the KEDA objects for an opted-in employee", func(ctx context.Context) {
			employee.Spec.Hibernation = enable(true, "45m")
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			_, err := reconcile(ctx)
			Expect(err).NotTo(HaveOccurred())

			so, found := getScaledObject(ctx)
			Expect(found).To(BeTrue())
			Expect(so.Spec.ScaleTargetRef.Name).To(Equal(deploymentName(employee)))
			Expect(*so.Spec.MinReplicaCount).To(Equal(int32(0)))
			Expect(*so.Spec.MaxReplicaCount).To(Equal(int32(1)))
			Expect(*so.Spec.CooldownPeriod).To(Equal(int32(45 * 60)))
			Expect(so.Spec.Triggers).To(HaveLen(1))
			trigger := so.Spec.Triggers[0]
			Expect(trigger.Type).To(Equal("postgresql"))
			Expect(trigger.Metadata["host"]).To(Equal("pg.db.svc"))
			Expect(trigger.Metadata["port"]).To(Equal("5432"))
			Expect(trigger.Metadata["dbName"]).To(Equal("deportal"))
			Expect(trigger.Metadata["userName"]).To(Equal("wake"))
			Expect(trigger.Metadata["query"]).To(ContainSubstring("channel_wake WHERE employee='alice-helper'"))
			Expect(trigger.Metadata["query"]).To(ContainSubstring("employee_activity WHERE employee='alice-helper'"))
			Expect(trigger.AuthenticationRef.Name).To(Equal("de-wake-db-auth"))

			secret := &corev1.Secret{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "de-wake-db"}, secret)).To(Succeed())
			Expect(string(secret.Data["DB_PASSWORD"])).To(Equal("wak3pass"))
			auth := &KedaTriggerAuthentication{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "de-wake-db-auth"}, auth)).To(Succeed())
			Expect(auth.Spec.SecretTargetRef).To(HaveLen(1))
		})

		It("falls back to the operator default idle window", func(ctx context.Context) {
			employee.Spec.Hibernation = enable(true, "")
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			_, err := reconcile(ctx)
			Expect(err).NotTo(HaveOccurred())
			so, found := getScaledObject(ctx)
			Expect(found).To(BeTrue())
			Expect(*so.Spec.CooldownPeriod).To(Equal(int32(1800)))
		})

		It("renders nothing when hibernation is disabled", func(ctx context.Context) {
			employee.Spec.Hibernation = enable(false, "")
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			_, err := reconcile(ctx)
			Expect(err).NotTo(HaveOccurred())
			_, found := getScaledObject(ctx)
			Expect(found).To(BeFalse())
		})

		It("refuses employees with the WeCom channel and reports why", func(ctx context.Context) {
			employee.Spec.Hibernation = enable(true, "")
			employee.Spec.Channels = &v1alpha1.ChannelsSpec{Wecom: true, WecomName: "数字员工"}
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			_, err := reconcile(ctx)
			Expect(err).To(HaveOccurred())
			Expect(getEmployee(ctx).Status.Phase).To(Equal(v1alpha1.PhaseError))
			Expect(getEmployee(ctx).Status.Message).To(ContainSubstring("WeCom"))
			_, found := getScaledObject(ctx)
			Expect(found).To(BeFalse())
		})
	})

	Describe("sleeping phase", func() {
		It("reports Sleeping when KEDA scales the runtime to zero", func(ctx context.Context) {
			employee.Spec.Hibernation = enable(true, "")
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			_, err := reconcile(ctx)
			Expect(err).NotTo(HaveOccurred())

			deployment := &appsv1.Deployment{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: deploymentName(employee)}, deployment)).To(Succeed())
			deployment.Status.ObservedGeneration = deployment.Generation
			deployment.Status.ReadyReplicas = *deployment.Spec.Replicas
			Expect(fx.client.Status().Update(ctx, deployment)).To(Succeed())
			_, err = reconcile(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(getEmployee(ctx).Status.Phase).To(Equal(v1alpha1.PhaseReady))

			zero := int32(0)
			deployment.Spec.Replicas = &zero
			deployment.Status.ReadyReplicas = 0
			deployment.Status.ObservedGeneration = deployment.Generation
			Expect(fx.client.Update(ctx, deployment)).To(Succeed())
			Expect(fx.client.Status().Update(ctx, deployment)).To(Succeed())

			_, err = reconcile(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(getEmployee(ctx).Status.Phase).To(Equal(v1alpha1.PhaseSleeping))
		})
	})
})

var _ = Describe("DigitalEmployee hibernation P2", func() {
	var fx *fixture
	var employee *v1alpha1.DigitalEmployee

	enable := func() *v1alpha1.HibernationSpec {
		on := true
		return &v1alpha1.HibernationSpec{Enabled: &on}
	}

	reconcile := func(ctx context.Context) (ctrl.Result, error) {
		return fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
	}

	getSO := func(ctx context.Context) *KedaScaledObject {
		so := &KedaScaledObject{}
		if err := fx.client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: deploymentName(employee) + "-wake"}, so); err != nil {
			return nil
		}
		return so
	}

	BeforeEach(func(ctx context.Context) {
		fx = newFixture(ctx)
		fx.wakeDBURL = "postgresql://wake:wak3pass@pg.db.svc:5432/deportal"
		employee = fx.employeeFixture("wecom-helper")
		employee.Spec.Channels = &v1alpha1.ChannelsSpec{Wecom: true, WecomName: "数字员工"}
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
	})

	It("does not lift the WeCom refusal merely because a gateway URL is configured", func(ctx context.Context) {
		fx.gatewayURL = "http://de-channel-gateway.digital-employees.svc.cluster.local:8899"
		employee.Spec.Hibernation = enable()
		Expect(fx.client.Update(ctx, employee)).To(Succeed())
		_, err := reconcile(ctx)
		Expect(err).To(HaveOccurred())
		Expect(getSO(ctx)).To(BeNil())
	})

	It("still refuses WeCom without a gateway", func(ctx context.Context) {
		employee.Spec.Hibernation = enable()
		Expect(fx.client.Update(ctx, employee)).To(Succeed())
		_, err := reconcile(ctx)
		Expect(err).To(HaveOccurred())
	})

	It("merges the policy ConfigMap: team override beats global default", func(ctx context.Context) {
		fx.gatewayURL = "http://gw:8899"
		employee.Spec.Channels = nil
		employee.Spec.Team = "rd-dept"
		employee.Spec.Hibernation = enable()
		Expect(fx.client.Update(ctx, employee)).To(Succeed())
		Expect(fx.client.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "de-hibernation-policy", Namespace: testNamespace},
			Data: map[string]string{
				"defaultIdleSeconds":       "900",
				"keepWarm":                 "3",
				"team.rd-dept.idleSeconds": "2400",
			},
		})).To(Succeed())
		_, err := reconcile(ctx)
		Expect(err).NotTo(HaveOccurred())
		so := getSO(ctx)
		Expect(so).NotTo(BeNil())
		Expect(*so.Spec.CooldownPeriod).To(Equal(int32(2400)))
		Expect(so.Spec.Triggers[0].Metadata["query"]).To(ContainSubstring("LIMIT 3"))
	})

	It("falls back to the policy default when the team is unset", func(ctx context.Context) {
		fx.gatewayURL = "http://gw:8899"
		employee.Spec.Channels = nil
		employee.Spec.Hibernation = enable()
		Expect(fx.client.Update(ctx, employee)).To(Succeed())
		Expect(fx.client.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "de-hibernation-policy", Namespace: testNamespace},
			Data:       map[string]string{"defaultIdleSeconds": "900"},
		})).To(Succeed())
		_, err := reconcile(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(*getSO(ctx).Spec.CooldownPeriod).To(Equal(int32(900)))
	})

	It("injects gateway env into the runtime deployment", func(ctx context.Context) {
		fx.gatewayURL = "http://gw:8899"
		_, err := reconcile(ctx)
		Expect(err).NotTo(HaveOccurred())
		deployment := &appsv1.Deployment{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: deploymentName(employee)}, deployment)).To(Succeed())
		env := map[string]string{}
		for _, e := range deployment.Spec.Template.Spec.Containers[0].Env {
			env[e.Name] = e.Value
		}
		Expect(env["DEERFLOW_CHANNEL_GATEWAY_URL"]).To(Equal("http://gw:8899"))
		Expect(env["DEERFLOW_EMPLOYEE_NAME"]).To(Equal("wecom-helper"))
	})
})

var _ = Describe("DigitalEmployee runtime model list", func() {
	var fx *fixture

	BeforeEach(func(ctx context.Context) {
		fx = newFixture(ctx)
	})

	reconcileAndGetConfig := func(ctx context.Context, employee *v1alpha1.DigitalEmployee) string {
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		Expect(err).NotTo(HaveOccurred())
		config := &corev1.ConfigMap{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
		return config.Data["config.yaml"]
	}

	It("renders every entry in order and shapes each provider family", func(ctx context.Context) {
		employee := fx.employeeFixture("multi-model-helper")
		employee.Spec.Runtime.Models = []v1alpha1.RuntimeModel{
			{Name: "bench-anthropic", Provider: "anthropic", NativeModel: "glm-5.3-flash", BaseURL: "http://higress-gateway.higress-system.svc.cluster.local/bench-anthropic", ApiKeyEnv: "BIGMODEL_API_KEY"},
			{Name: "bench-qwen", Provider: "openai", NativeModel: "Qwen3.6-35B-A3B", BaseURL: "http://higress-gateway.higress-system.svc.cluster.local/v1", ApiKeyEnv: "DEER_BENCH_API_KEY", Reasoning: &v1alpha1.RuntimeReasoning{Thinking: "optional", History: "preserve"}},
		}
		employee.Spec.Runtime.Model = ""

		config := reconcileAndGetConfig(ctx, employee)
		var parsed struct {
			Models []map[string]any `json:"models"`
		}
		Expect(json.Unmarshal([]byte(config), &parsed)).To(Succeed())
		Expect(parsed.Models).To(HaveLen(2))
		// models[0] is the runtime default and carries the anthropic client
		// shape (timeout, not request_timeout) with the native wire id.
		Expect(parsed.Models[0]).To(SatisfyAll(
			HaveKeyWithValue("name", "bench-anthropic"),
			HaveKeyWithValue("model", "glm-5.3-flash"),
			HaveKeyWithValue("use", "langchain_anthropic:ChatAnthropic"),
			HaveKeyWithValue("timeout", 300.0),
			HaveKeyWithValue("api_key", "$BIGMODEL_API_KEY"),
			HaveKeyWithValue("base_url", "http://higress-gateway.higress-system.svc.cluster.local/bench-anthropic"),
		))
		Expect(parsed.Models[0]).NotTo(HaveKey("request_timeout"))
		Expect(parsed.Models[0]).NotTo(HaveKey("reasoning"), "undeclared models stay thinking-off (runtime default)")
		Expect(parsed.Models[1]["reasoning"]).To(SatisfyAll(
			HaveKeyWithValue("thinking", "optional"),
			HaveKeyWithValue("history", "preserve"),
		))
		Expect(parsed.Models[1]).To(SatisfyAll(
			HaveKeyWithValue("name", "bench-qwen"),
			HaveKeyWithValue("model", "Qwen3.6-35B-A3B"),
			HaveKeyWithValue("use", "langchain_openai:ChatOpenAI"),
			HaveKeyWithValue("request_timeout", 300.0),
		))
		Expect(parsed.Models[1]).NotTo(HaveKey("timeout"))
	})

	It("synthesizes a one-entry list from the legacy singular fields", func(ctx context.Context) {
		employee := fx.employeeFixture("legacy-model-helper")
		employee.Spec.Runtime.Provider = "anthropic"
		employee.Spec.Runtime.NativeModel = "glm-5.3-flash"

		config := reconcileAndGetConfig(ctx, employee)
		Expect(config).To(ContainSubstring(`"name": "bench-glm"`))
		Expect(config).To(ContainSubstring(`"use": "langchain_anthropic:ChatAnthropic"`))
		Expect(config).To(ContainSubstring(`"model": "glm-5.3-flash"`))
	})
})
