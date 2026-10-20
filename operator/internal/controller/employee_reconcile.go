package controller

import (
	"context"
	"fmt"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

func (r *DigitalEmployeeReconciler) ensureChildren(ctx context.Context, employee *v1alpha1.DigitalEmployee) error {
	// Conversation store gate, before any child is rendered: backend=postgres
	// must resolve to a per-employee database the operator can render a DSN
	// for. The silent platform-wide fallback is gone — a shared database
	// meant shared auth tables.
	database, err := conversationDatabaseName(employee)
	if err != nil {
		return fmt.Errorf("conversation store: %w", err)
	}
	if database != "" {
		if r.ConversationAdminDBURL == "" {
			return fmt.Errorf("conversation store: backend=postgres needs CONVERSATION_ADMIN_DB_URL to render the per-employee DSN for %s", database)
		}
		if err := r.ensureConversationSecret(ctx, employee, database); err != nil {
			return fmt.Errorf("conversation db: %w", err)
		}
	}

	// Verify the live SA before generating the runtime's routing hint. Identity
	// delivery and config share this exact observation, including SA replacement.
	deployment, err := r.renderDeployment(employee, "")
	if err != nil {
		return err
	}
	if err := r.prepareModelIdentity(ctx, employee, deployment); err != nil {
		return fmt.Errorf("model workload identity: %w", err)
	}
	configData := renderConfigData(employee, conversationURLKey(employee))
	if err := r.configureModelRuntime(employee, configData); err != nil {
		return err
	}
	configPayload := marshalConfig(configData)
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configName(employee), Namespace: employee.Namespace}}
	configMap.Data = map[string]string{"config.yaml": configPayload}
	if err := controllerutil.SetControllerReference(employee, configMap, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, configMap); err != nil {
		return fmt.Errorf("config map: %w", err)
	}

	extensionsPayload := renderExtensions(employee)
	extensions := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: extensionsName(employee), Namespace: employee.Namespace}}
	extensions.Data = map[string]string{"extensions_config.json": extensionsPayload}
	if err := controllerutil.SetControllerReference(employee, extensions, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, extensions); err != nil {
		return fmt.Errorf("extensions config map: %w", err)
	}

	if employee.Spec.Persistence.Enabled {
		if err := r.ensurePVC(ctx, employee); err != nil {
			return fmt.Errorf("home pvc: %w", err)
		}
	}

	// The channel block stays out of the pod-template fingerprint: enabling
	// or disabling a channel is applied on the live runtime (the portal's
	// PUT /api/channels/{provider}/enabled) and the CR booleans only record
	// intent/audit, so a toggle must not roll the employee — the rollout
	// takes conversations, memory and knowledge down with it.
	deployment.Spec.Template.Annotations[configHashAnnotation] = configChecksum(fingerprintPayload(configData), extensionsPayload)
	if err := controllerutil.SetControllerReference(employee, deployment, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, deployment); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}

	service := renderService(employee)
	if err := controllerutil.SetControllerReference(employee, service, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, service); err != nil {
		return fmt.Errorf("service: %w", err)
	}

	if employee.Spec.A2A != nil {
		a2aDeployment := renderA2ADeployment(employee)
		if err := controllerutil.SetControllerReference(employee, a2aDeployment, r.Scheme); err != nil {
			return err
		}
		if err := safeCreateOrUpdate(ctx, r.Client, a2aDeployment); err != nil {
			return fmt.Errorf("a2a deployment: %w", err)
		}
		a2aService := renderA2AService(employee)
		if err := controllerutil.SetControllerReference(employee, a2aService, r.Scheme); err != nil {
			return err
		}
		if err := safeCreateOrUpdate(ctx, r.Client, a2aService); err != nil {
			return fmt.Errorf("a2a service: %w", err)
		}
	}

	if err := r.ensureHibernation(ctx, employee); err != nil {
		return fmt.Errorf("hibernation: %w", err)
	}
	return nil
}

// safeCreateOrUpdate upserts by fetching, comparing the rendered payload,
// and updating only on drift (avoids hot loops from last-applied churn).
func safeCreateOrUpdate(ctx context.Context, c client.Client, object client.Object) error {
	existing := object.DeepCopyObject().(client.Object)
	err := c.Get(ctx, client.ObjectKeyFromObject(object), existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, object)
	}
	if err != nil {
		return err
	}
	switch typed := object.(type) {
	case *corev1.Secret:
		previous := existing.(*corev1.Secret)
		if reflect.DeepEqual(previous.Data, typed.Data) {
			return nil
		}
		previous.Data = typed.Data
		return c.Update(ctx, previous)
	case *corev1.ConfigMap:
		previous := existing.(*corev1.ConfigMap)
		if reflect.DeepEqual(previous.Data, typed.Data) {
			return nil
		}
		previous.Data = typed.Data
		return c.Update(ctx, previous)
	case *appsv1.Deployment:
		previous := existing.(*appsv1.Deployment)
		// The pod template annotations carry the config checksum: comparing
		// them too is what turns a rendered-config change into a rollout
		// (the runtime loads config at boot; a hot-updated ConfigMap alone
		// would leave the old policy live forever).
		// Template labels are compared too: the component label that
		// NetworkPolicies select on must reach pods of existing employees.
		if reflect.DeepEqual(previous.Spec.Template.Spec, typed.Spec.Template.Spec) &&
			reflect.DeepEqual(previous.Spec.Template.Annotations, typed.Spec.Template.Annotations) &&
			reflect.DeepEqual(previous.Spec.Template.Labels, typed.Spec.Template.Labels) {
			return nil
		}
		previous.Spec.Template.Spec = typed.Spec.Template.Spec
		previous.Spec.Template.Annotations = typed.Spec.Template.Annotations
		previous.Spec.Template.Labels = typed.Spec.Template.Labels
		return c.Update(ctx, previous)
	case *corev1.Service:
		previous := existing.(*corev1.Service)
		if reflect.DeepEqual(previous.Spec.Ports, typed.Spec.Ports) && previous.Spec.Type == typed.Spec.Type {
			return nil
		}
		// ClusterIP is immutable: keep whatever is assigned.
		previous.Spec.Type = typed.Spec.Type
		previous.Spec.Ports = typed.Spec.Ports
		return c.Update(ctx, previous)
	case *KedaScaledObject:
		previous := existing.(*KedaScaledObject)
		if reflect.DeepEqual(previous.Spec, typed.Spec) {
			return nil
		}
		previous.Spec = typed.Spec
		return c.Update(ctx, previous)
	case *KedaTriggerAuthentication:
		previous := existing.(*KedaTriggerAuthentication)
		if reflect.DeepEqual(previous.Spec, typed.Spec) {
			return nil
		}
		previous.Spec = typed.Spec
		return c.Update(ctx, previous)
	}
	return fmt.Errorf("unsupported type %T", object)
}
