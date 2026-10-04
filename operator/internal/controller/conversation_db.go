package controller

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// Conversation store isolation. A shared conversation database means a
// shared `users` table: every runtime's auth DB (and every thread row) lands
// in one physical database, so the first employee to initialize owns the
// global admin and every other employee's operator-held credentials are
// rejected (401 forever — needs_setup can never become true again). The
// default therefore provisions one database per employee.

const (
	// conversationDBKey is the env/config key carrying the per-employee DSN.
	// It deliberately differs from the platform CONVERSATION_DB_URL so both
	// can coexist in envFrom without precedence games.
	conversationDBKey = "DE_CONVERSATION_DB_URL"
	// conversationAdminURLKey is read from the platform credentials Secret
	// by the db-init init container (CREATE DATABASE needs a maintenance
	// connection, not the per-employee DSN).
	conversationAdminURLKey = "CONVERSATION_ADMIN_DB_URL"
)

// conversationDatabaseName resolves the physical database for an employee.
// ("", false) means the legacy shared platform database; otherwise the
// returned name is a database the operator guarantees exists.
func conversationDatabaseName(employee *dev1alpha1.DigitalEmployee) (string, bool) {
	if employee.Spec.ConversationStore.Backend != "postgres" {
		return "", false
	}
	switch database := employee.Spec.ConversationStore.Database; database {
	case "":
		return "de_" + strings.ReplaceAll(employee.Name, "-", "_"), true
	case "shared":
		return "", false
	default:
		return database, true
	}
}

// conversationDSNFor swaps the database path of the admin DSN for the
// per-employee database. Everything else (credentials, host, TLS) is
// inherited from the platform connection.
func conversationDSNFor(adminURL, database string) (string, error) {
	parsed, err := url.Parse(adminURL)
	if err != nil {
		return "", fmt.Errorf("parse conversation admin URL: %w", err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("conversation admin URL has no host")
	}
	parsed.Path = "/" + database
	return parsed.String(), nil
}

func conversationSecretName(employee *dev1alpha1.DigitalEmployee) string {
	return "de-" + employee.Name + "-convdb"
}

// ensureConversationSecret renders the per-employee DSN Secret. The admin
// URL itself never leaves Secrets: it arrives via the operator env (sourced
// from the platform credentials) and lands in a child Secret the runtime
// consumes through envFrom.
func (r *DigitalEmployeeReconciler) ensureConversationSecret(ctx context.Context, employee *dev1alpha1.DigitalEmployee, database string) error {
	if database == "" || r.ConversationAdminDBURL == "" {
		return nil
	}
	dsn, err := conversationDSNFor(r.ConversationAdminDBURL, database)
	if err != nil {
		return fmt.Errorf("conversation dsn: %w", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: objectMeta(conversationSecretName(employee), employee.Namespace),
		Data:       map[string][]byte{conversationDBKey: []byte(dsn)},
	}
	if err := controllerutil.SetControllerReference(employee, secret, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, secret); err != nil {
		return fmt.Errorf("conversation db secret: %w", err)
	}
	return nil
}

// conversationInitContainer guarantees the database exists before the
// runtime boots (the runtime's alembic bootstrap then builds the schema).
// The database name is CRD-validated ([a-z0-9_]) so embedding it in the SQL
// literal is safe.
func (r *DigitalEmployeeReconciler) conversationInitContainer(employee *dev1alpha1.DigitalEmployee, database string) *corev1.Container {
	if database == "" || r.ConversationAdminDBURL == "" {
		return nil
	}
	check := fmt.Sprintf(`SELECT 1 FROM pg_database WHERE datname='%s'`, database)
	create := fmt.Sprintf(`CREATE DATABASE "%s"`, database)
	return &corev1.Container{
		Name:  "db-init",
		Image: r.ConversationInitImage,
		Command: []string{"sh", "-ec",
			fmt.Sprintf(`psql "$%s" -tAc %q | grep -q 1 || psql "$%s" -c %q`,
				conversationAdminURLKey, check, conversationAdminURLKey, create)},
		Env: []corev1.EnvVar{{
			Name: conversationAdminURLKey,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: employee.Spec.Credentials.Name},
				Key:                  conversationAdminURLKey,
			}},
		}},
	}
}
