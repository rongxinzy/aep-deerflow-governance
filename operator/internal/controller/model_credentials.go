package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// validateModelAPIKeyEnvs rejects a CR whose declared model credentials cannot
// be satisfied by the platform secret the runtime loads through envFrom.
//
// The runtime renders every route as `"api_key": "$<apiKeyEnv>"` and resolves
// it from the process environment at startup, refusing to boot when the
// variable is absent:
//
//	ValueError: Environment variable BIGMODEL_API_KEY not found for config value $BIGMODEL_API_KEY
//
// Without this check that mismatch is only visible as a CrashLoopBackOff after
// the CR was already accepted, so it is reported on the CR instead — the
// deployment-time rejection the runtime cannot express.
func (r *DigitalEmployeeReconciler) validateModelAPIKeyEnvs(ctx context.Context, employee *v1alpha1.DigitalEmployee) error {
	envs := modelAPIKeyEnvs(employee.Spec.Runtime)
	if len(envs) == 0 {
		return nil
	}
	secretName := employee.Spec.Credentials.Name
	if secretName == "" {
		return fmt.Errorf(
			"runtime declares apiKeyEnv %s but spec.credentials.name is empty; the runtime reads that variable from the credentials secret's envFrom and will not start without it",
			strings.Join(envs, ", "),
		)
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: employee.Namespace, Name: secretName}, &secret); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf(
				"credentials secret %s/%s does not exist; the runtime resolves apiKeyEnv %s from it and will not start without it",
				employee.Namespace, secretName, strings.Join(envs, ", "),
			)
		}
		return errWrap("read credentials secret: ", err)
	}
	missing := make([]string, 0, len(envs))
	for _, name := range envs {
		if len(secret.Data[name]) == 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"credentials secret %s/%s has no %s; the runtime resolves $%s from its environment and refuses to start, so add the key to the secret or point apiKeyEnv at an existing one",
		employee.Namespace, secretName, strings.Join(missing, ", "), missing[0],
	)
}

// modelAPIKeyEnvs lists the credential variables the runtime will resolve for
// its model routes: every declared route env plus the legacy singular field.
// An empty declaration is not listed — the runtime then falls back to its
// platform default, which the platform secret is expected to carry.
func modelAPIKeyEnvs(spec v1alpha1.RuntimeSpec) []string {
	envs := make([]string, 0, len(spec.Models)+1)
	seen := make(map[string]bool, len(spec.Models)+1)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		envs = append(envs, name)
	}
	for _, model := range spec.Models {
		add(model.ApiKeyEnv)
	}
	add(spec.ApiKeyEnv)
	return envs
}
