/* Copyright 2026.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// credentialsSecret is the platform secret a CR points at with
// spec.credentials.name; the runtime loads its keys through envFrom.
func credentialsSecret(name string, data map[string]string) *corev1.Secret {
	secret := &corev1.Secret{ObjectMeta: objectMeta(name, testNamespace), Data: map[string][]byte{}}
	for key, value := range data {
		secret.Data[key] = []byte(value)
	}
	return secret
}

var _ = Describe("DigitalEmployee model credentials", func() {
	var fx *fixture
	var employee *v1alpha1.DigitalEmployee

	BeforeEach(func(ctx context.Context) {
		fx = newFixture(ctx)
		employee = fx.employeeFixture("alice-helper")
		employee.Spec.Runtime.Models = []v1alpha1.RuntimeModel{{
			Name:        "bigmodel",
			Provider:    "openai",
			NativeModel: "GLM-4.6",
			BaseURL:     "http://higress-gateway.higress-system.svc.cluster.local/v1",
			ApiKeyEnv:   "BIGMODEL_API_KEY",
		}}
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
	})

	reconcile := func(ctx context.Context) error {
		_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		return err
	}

	getEmployee := func(ctx context.Context) *v1alpha1.DigitalEmployee {
		fresh := &v1alpha1.DigitalEmployee{}
		ExpectWithOffset(1, fx.client.Get(ctx, client.ObjectKeyFromObject(employee), fresh)).To(Succeed())
		return fresh
	}

	It("rejects a CR whose declared apiKeyEnv the credentials secret cannot supply", func(ctx context.Context) {
		Expect(fx.client.Create(ctx, credentialsSecret(employee.Spec.Credentials.Name, map[string]string{"OTHER_KEY": "x"}))).To(Succeed())

		Expect(reconcile(ctx)).To(HaveOccurred())

		fresh := getEmployee(ctx)
		Expect(fresh.Status.Phase).To(Equal(v1alpha1.PhaseError))
		Expect(fresh.Status.Message).To(ContainSubstring("BIGMODEL_API_KEY"), "the message must name the missing variable")
		Expect(fx.stub.registers).To(Equal(0), "a configuration error is rejected before any provisioning")
	})

	It("rejects a CR that declares an env but names no credentials secret", func(ctx context.Context) {
		updated := getEmployee(ctx)
		updated.Spec.Credentials.Name = ""
		Expect(fx.client.Update(ctx, updated)).To(Succeed())

		Expect(reconcile(ctx)).To(HaveOccurred())
		Expect(getEmployee(ctx).Status.Message).To(ContainSubstring("BIGMODEL_API_KEY"))
	})

	It("accepts the CR once the credentials secret carries the declared env", func(ctx context.Context) {
		Expect(fx.client.Create(ctx, credentialsSecret(employee.Spec.Credentials.Name, map[string]string{"BIGMODEL_API_KEY": "secret"}))).To(Succeed())

		Expect(reconcile(ctx)).To(Succeed())
		Expect(getEmployee(ctx).Status.Phase).NotTo(Equal(v1alpha1.PhaseError))
	})

	It("ignores route envs that only the legacy singular field declares", func(ctx context.Context) {
		// No env can be resolved from an empty declaration: the runtime falls
		// back to its platform default, which the platform secret carries.
		updated := getEmployee(ctx)
		updated.Spec.Runtime.Models = nil
		Expect(fx.client.Update(ctx, updated)).To(Succeed())

		Expect(reconcile(ctx)).To(Succeed())
	})
})

var _ = Describe("DigitalEmployee department memory account", func() {
	var fx *fixture
	var employee *v1alpha1.DigitalEmployee

	BeforeEach(func(ctx context.Context) {
		fx = newFixture(ctx)
		employee = fx.employeeFixture("whz-test")
		employee.Spec.Team = "all-users"
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
	})

	It("creates the department account when OpenViking reports it missing", func(ctx context.Context) {
		fx.stub.missingAccounts["tenant-all-users"] = true

		_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		Expect(err).NotTo(HaveOccurred())

		Expect(fx.stub.accountCreates).To(Equal([]string{"tenant-all-users"}), "the missing account is created through the admin API")

		secret := &corev1.Secret{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: memkeySecretName(employee)}, secret)).To(Succeed())
		Expect(secret.Data[memkeySecretKey]).NotTo(BeEmpty(), "the register retry mints the key")
	})

	It("surfaces an actionable error when the account cannot be created", func(ctx context.Context) {
		fx.stub.missingAccounts["tenant-all-users"] = true
		fx.stub.failAccountCreate = true

		_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("tenant-all-users"))
		Expect(err.Error()).To(ContainSubstring("Account not found"))
	})
})
