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
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

var _ = Describe("runtime account provisioning", func() {
	var fx *fixture
	var employee *v1alpha1.DigitalEmployee

	reconcile := func(ctx context.Context) ctrl.Result {
		result, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return result
	}

	reconcileReady := func(ctx context.Context) ctrl.Result {
		reconcile(ctx)
		deployment := &appsv1.Deployment{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: deploymentName(employee)}, deployment)).To(Succeed())
		deployment.Status.ObservedGeneration = deployment.Generation
		deployment.Status.ReadyReplicas = *deployment.Spec.Replicas
		Expect(fx.client.Status().Update(ctx, deployment)).To(Succeed())
		return reconcile(ctx)
	}

	BeforeEach(func(ctx context.Context) {
		fx = newFixture(ctx)
		employee = fx.employeeFixture("bravo-helper")
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		// Rollout done: reconcile once to render children, mark the
		// deployment available, then the account bootstrap runs.
		reconcileReady(ctx)
	})

	getEmployee := func(ctx context.Context) *v1alpha1.DigitalEmployee {
		fresh := &v1alpha1.DigitalEmployee{}
		ExpectWithOffset(1, fx.client.Get(ctx, client.ObjectKeyFromObject(employee), fresh)).To(Succeed())
		return fresh
	}

	getCreds := func(ctx context.Context) *corev1.Secret {
		secret := &corev1.Secret{}
		ExpectWithOffset(1, fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: runtimeCredsSecretName(employee)}, secret)).To(Succeed())
		return secret
	}

	It("bootstraps the instance admin and owner PAT into the Secret", func(ctx context.Context) {
		Expect(getEmployee(ctx).Status.RuntimeAccount).To(Equal(v1alpha1.RuntimeAccountReady))
		Expect(fx.runtime.initializations).To(Equal(1))
		Expect(fx.runtime.provisions).To(Equal(1))

		creds := getCreds(ctx)
		Expect(string(creds.Data[runtimeAdminEmailKey])).To(Equal("admin@bravo-helper.runtime.internal"))
		Expect(creds.Data[runtimeAdminPasswordKey]).NotTo(BeEmpty())
		Expect(string(creds.Data[runtimeOwnerEmailKey])).To(Equal("de-bravo-helper@runtime.internal"))
		Expect(creds.Data[runtimeOwnerUserIDKey]).NotTo(BeEmpty())
		Expect(string(creds.Data[runtimeOwnerPATKey])).To(Equal(fx.runtime.accounts["de-bravo-helper@runtime.internal"]),
			"the Secret must carry the show-once PAT the runtime issued")
	})

	It("is idempotent: a PAT that still authenticates means zero runtime writes", func(ctx context.Context) {
		pat := string(getCreds(ctx).Data[runtimeOwnerPATKey])

		reconcile(ctx)

		Expect(fx.runtime.provisions).To(Equal(1), "no re-provision when the PAT validates")
		Expect(fx.runtime.initializations).To(Equal(1))
		Expect(string(getCreds(ctx).Data[runtimeOwnerPATKey])).To(Equal(pat), "the PAT must not rotate")
		Expect(getEmployee(ctx).Status.RuntimeAccount).To(Equal(v1alpha1.RuntimeAccountReady))
	})

	It("stays Pending with a requeue while the runtime auth API is unreachable", func(ctx context.Context) {
		fx.runtimeURL = "http://127.0.0.1:1"
		result := reconcile(ctx)

		Expect(result.RequeueAfter).To(BeNumerically(">", time.Duration(0)))
		fresh := getEmployee(ctx)
		Expect(fresh.Status.RuntimeAccount).To(Equal(v1alpha1.RuntimeAccountPending))
		Expect(fresh.Status.Phase).To(Equal(v1alpha1.PhaseReady), "workload readiness is independent of the account sub-state")
	})

	It("rebuilds the account after the runtime DB is wiped (pod restart)", func(ctx context.Context) {
		oldPAT := string(getCreds(ctx).Data[runtimeOwnerPATKey])
		adminPassword := string(getCreds(ctx).Data[runtimeAdminPasswordKey])
		fx.runtime.resetDB()

		reconcile(ctx)

		creds := getCreds(ctx)
		Expect(fx.runtime.initializations).To(Equal(2), "the fresh DB is re-initialized")
		Expect(fx.runtime.provisions).To(Equal(2), "the owner is re-provisioned with a new PAT")
		Expect(string(creds.Data[runtimeAdminPasswordKey])).To(Equal(adminPassword),
			"the stored password is reused so restarts converge instead of locking the operator out")
		Expect(string(creds.Data[runtimeOwnerPATKey])).NotTo(Equal(oldPAT))
		Expect(string(creds.Data[runtimeOwnerPATKey])).To(Equal(fx.runtime.accounts["de-bravo-helper@runtime.internal"]))
		Expect(getEmployee(ctx).Status.RuntimeAccount).To(Equal(v1alpha1.RuntimeAccountReady))
	})

	It("reports Failed when the stored admin password was rotated out-of-band", func(ctx context.Context) {
		fx.runtime.rotateAdminPassword("someone-else-set-this")
		reconcile(ctx)

		fresh := getEmployee(ctx)
		Expect(fresh.Status.RuntimeAccount).To(Equal(v1alpha1.RuntimeAccountFailed))
		Expect(fresh.Status.RuntimeAccountMessage).To(ContainSubstring("rejected the stored admin credentials"))
	})

	It("reports Failed when the Secret is lost but the runtime stays initialized", func(ctx context.Context) {
		Expect(fx.client.Delete(ctx, getCreds(ctx))).To(Succeed())
		reconcile(ctx)

		fresh := getEmployee(ctx)
		Expect(fresh.Status.RuntimeAccount).To(Equal(v1alpha1.RuntimeAccountFailed))
		Expect(fresh.Status.RuntimeAccountMessage).To(ContainSubstring("holds no admin credentials"))
	})

	It("reports Unsupported when the runtime image predates the auth API", func(ctx context.Context) {
		fx2 := newFixture(ctx)
		fx2.runtime.hideAuthAPI = true
		fx2.runtimeURL = fx2.runtimeSrv.URL
		employee2 := fx2.employeeFixture("charlie-helper")
		Expect(fx2.client.Create(ctx, employee2)).To(Succeed())
		_, err := fx2.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee2.Namespace, Name: employee2.Name}})
		Expect(err).NotTo(HaveOccurred())
		markReady := &appsv1.Deployment{}
		Expect(fx2.client.Get(ctx, types.NamespacedName{Namespace: employee2.Namespace, Name: deploymentName(employee2)}, markReady)).To(Succeed())
		markReady.Status.ObservedGeneration = markReady.Generation
		markReady.Status.ReadyReplicas = *markReady.Spec.Replicas
		Expect(fx2.client.Status().Update(ctx, markReady)).To(Succeed())
		result, err := fx2.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee2.Namespace, Name: employee2.Name}})
		Expect(err).NotTo(HaveOccurred())

		Expect(result.RequeueAfter).To(BeNumerically(">", time.Duration(0)), "upgrade self-heal needs a retry")
		fresh := &v1alpha1.DigitalEmployee{}
		Expect(fx2.client.Get(ctx, client.ObjectKeyFromObject(employee2), fresh)).To(Succeed())
		Expect(fresh.Status.RuntimeAccount).To(Equal(v1alpha1.RuntimeAccountUnsupported))
		Expect(fresh.Status.RuntimeAccountMessage).To(ContainSubstring("upgrade the runtime image"))
	})
})
