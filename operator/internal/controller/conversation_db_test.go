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

// Conversation-database isolation: a shared conversation database gave every
// runtime the same `users` table, so the first employee owned the global
// admin and every other employee's credentials were rejected forever. These
// specs pin the per-employee default and the "shared" escape hatch.
var _ = Describe("conversation database isolation", func() {
	setup := func(ctx context.Context, database string, adminURL string) (*fixture, *dev1alpha1.DigitalEmployee) {
		fx := newFixture(ctx)
		fx.convAdminURL = adminURL
		employee := fx.employeeFixture("bruce-helper")
		employee.Spec.ConversationStore = dev1alpha1.ConversationStoreSpec{Backend: "postgres", Database: database}
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		Expect(err).NotTo(HaveOccurred())
		return fx, employee
	}

	fetchDeployment := func(ctx context.Context, fx *fixture, employee *dev1alpha1.DigitalEmployee) *appsv1.Deployment {
		deployment := &appsv1.Deployment{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: deploymentName(employee)}, deployment)).To(Succeed())
		return deployment
	}

	fetchConfig := func(ctx context.Context, fx *fixture, employee *dev1alpha1.DigitalEmployee) string {
		config := &corev1.ConfigMap{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: configName(employee)}, config)).To(Succeed())
		return config.Data["config.yaml"]
	}

	It("defaults to a per-employee database with secret, init container and env key", func(ctx context.Context) {
		fx, employee := setup(ctx, "", "postgres://aep:pw@pg:5432/aep")
		name, perDB := conversationDatabaseName(employee)
		Expect(perDB).To(BeTrue())
		Expect(name).To(Equal("de_bruce_helper"))

		secret := &corev1.Secret{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: conversationSecretName(employee)}, secret)).To(Succeed())
		Expect(string(secret.Data[conversationDBKey])).To(Equal("postgres://aep:pw@pg:5432/de_bruce_helper"))

		config := fetchConfig(ctx, fx, employee)
		Expect(config).To(ContainSubstring(`"$` + conversationDBKey + `"`))
		Expect(config).NotTo(ContainSubstring("$CONVERSATION_DB_URL"))

		deployment := fetchDeployment(ctx, fx, employee)
		Expect(deployment.Spec.Template.Spec.InitContainers).To(HaveLen(1))
		init := deployment.Spec.Template.Spec.InitContainers[0]
		Expect(init.Name).To(Equal("db-init"))
		Expect(init.Image).To(Equal("reg/postgres:17-alpine"))
		Expect(init.Command[2]).To(ContainSubstring(`datname='de_bruce_helper'`))
		Expect(init.Command[2]).To(ContainSubstring(`CREATE DATABASE \"de_bruce_helper\"`))
		Expect(init.Env[0].ValueFrom.SecretKeyRef.Key).To(Equal(conversationAdminURLKey))
		Expect(init.Env[0].ValueFrom.SecretKeyRef.Name).To(Equal(employee.Spec.Credentials.Name))

		names := []string{}
		for _, source := range deployment.Spec.Template.Spec.Containers[0].EnvFrom {
			if source.SecretRef != nil {
				names = append(names, source.SecretRef.Name)
			}
		}
		Expect(names).To(ContainElement(conversationSecretName(employee)))
	})

	It("converges a drifted conversation Secret instead of erroring", func(ctx context.Context) {
		fx, employee := setup(ctx, "", "postgres://aep:pw@pg:5432/aep")
		secret := &corev1.Secret{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: conversationSecretName(employee)}, secret)).To(Succeed())
		secret.Data[conversationDBKey] = []byte("postgres://wrong:wrong@elsewhere:5432/other")
		Expect(fx.client.Update(ctx, secret)).To(Succeed())

		_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		Expect(err).NotTo(HaveOccurred())

		fresh := &corev1.Secret{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: conversationSecretName(employee)}, fresh)).To(Succeed())
		Expect(string(fresh.Data[conversationDBKey])).To(Equal("postgres://aep:pw@pg:5432/de_bruce_helper"))
	})

	It("keeps the shared platform database for database=shared", func(ctx context.Context) {
		fx, employee := setup(ctx, "shared", "postgres://aep:pw@pg:5432/aep")
		_, perDB := conversationDatabaseName(employee)
		Expect(perDB).To(BeFalse())

		secret := &corev1.Secret{}
		err := fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: conversationSecretName(employee)}, secret)
		Expect(err).To(HaveOccurred())

		Expect(fetchConfig(ctx, fx, employee)).To(ContainSubstring(`"$CONVERSATION_DB_URL"`))
		Expect(fetchDeployment(ctx, fx, employee).Spec.Template.Spec.InitContainers).To(BeEmpty())
	})

	It("supports an explicit database name", func(ctx context.Context) {
		fx, employee := setup(ctx, "tenant_a_chats", "postgres://aep:pw@pg:5432/aep")
		name, _ := conversationDatabaseName(employee)
		Expect(name).To(Equal("tenant_a_chats"))
		secret := &corev1.Secret{}
		Expect(fx.client.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: conversationSecretName(employee)}, secret)).To(Succeed())
		Expect(string(secret.Data[conversationDBKey])).To(HaveSuffix("/tenant_a_chats"))
	})

	It("degrades to the shared URL when the operator lacks an admin DSN", func(ctx context.Context) {
		fx, employee := setup(ctx, "", "")
		Expect(fetchConfig(ctx, fx, employee)).To(ContainSubstring(`"$CONVERSATION_DB_URL"`))
		Expect(fetchDeployment(ctx, fx, employee).Spec.Template.Spec.InitContainers).To(BeEmpty())
	})

	It("keeps sqlite employees untouched", func(ctx context.Context) {
		fx := newFixture(ctx)
		fx.convAdminURL = "postgres://aep:pw@pg:5432/aep"
		employee := fx.employeeFixture("carol-helper")
		Expect(fx.client.Create(ctx, employee)).To(Succeed())
		_, err := fx.reconciler().Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: employee.Namespace, Name: employee.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(fetchConfig(ctx, fx, employee)).To(ContainSubstring(`"sqlite_dir"`))
	})

	Describe("conversationDSNFor", func() {
		It("swaps only the database path", func() {
			dsn, err := conversationDSNFor("postgres://u:p%40w@pg.example:5432/aep?sslmode=disable", "de_x")
			Expect(err).NotTo(HaveOccurred())
			Expect(dsn).To(Equal("postgres://u:p%40w@pg.example:5432/de_x?sslmode=disable"))
		})
		It("rejects a hostless URL", func() {
			_, err := conversationDSNFor("not-a-url", "de_x")
			Expect(err).To(HaveOccurred())
		})
	})
})
