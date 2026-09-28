/* Copyright 2026.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

		It("renders channel-connections only for enabled IM channels", func(ctx context.Context) {
			employee.Spec.Channels = &v1alpha1.ChannelsSpec{Wecom: true}
			Expect(fx.client.Update(ctx, employee)).To(Succeed())
			reconcile(ctx)

			config := &corev1.ConfigMap{}
			Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
			Expect(config.Data["config.yaml"]).To(ContainSubstring(`"channel_connections"`))
			Expect(config.Data["config.yaml"]).To(ContainSubstring(`"wecom"`))
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
			Expect(fresh.Data["config.yaml"]).NotTo(ContainSubstring(`"channel_connections"`))
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
