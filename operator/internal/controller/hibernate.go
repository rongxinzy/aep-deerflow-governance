package controller

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// Hibernation: scale-to-zero for idle employees. KEDA polls the portal
// database (wake + activity tables written by the portal) and drives the
// employee Deployment between 0 and 1 replicas; the operator renders the
// KEDA objects from the CR's hibernation stanza. The Deployment's replica
// count is owned by KEDA from then on — safeCreateOrUpdate deliberately
// never writes spec.replicas on update.

// wakeDBCoordinates is the parsed PORTAL_DB_URL the KEDA trigger points at.
type wakeDBCoordinates struct {
	Host     string
	Port     string
	DBName   string
	User     string
	Password string
}

func parseWakeDBURL(raw string) (wakeDBCoordinates, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return wakeDBCoordinates{}, fmt.Errorf("wake db url: %w", err)
	}
	host := parsed.Hostname()
	dbName := strings.TrimPrefix(parsed.Path, "/")
	if host == "" || dbName == "" || parsed.User.Username() == "" {
		return wakeDBCoordinates{}, fmt.Errorf("wake db url incomplete (need host, db, user)")
	}
	coords := wakeDBCoordinates{
		Host:   host,
		Port:   parsed.Port(),
		DBName: dbName,
		User:   parsed.User.Username(),
	}
	if coords.Port == "" {
		coords.Port = "5432"
	}
	coords.Password, _ = parsed.User.Password()
	return coords, nil
}

// hibernationEnabled reports whether the employee opted in.
func hibernationEnabled(employee *v1alpha1.DigitalEmployee) bool {
	return employee.Spec.Hibernation != nil && employee.Spec.Hibernation.Enabled != nil && *employee.Spec.Hibernation.Enabled
}

// hibernationPolicyName is the operator-watched ConfigMap holding cluster
// defaults, the LRU keep-warm pool size, and per-team idle overrides.
const hibernationPolicyName = "de-hibernation-policy"

// HibernationPolicy is the parsed de-hibernation-policy ConfigMap.
type HibernationPolicy struct {
	IdleSeconds int32            // default idle window
	KeepWarm    int              // LRU pool: the K most-recently-used stay resident
	TeamIdle    map[string]int32 // per-AEP-team idle overrides
}

func loadHibernationPolicy(ctx context.Context, c client.Client, namespace string) HibernationPolicy {
	policy := HibernationPolicy{TeamIdle: map[string]int32{}}
	var configMap corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: hibernationPolicyName}, &configMap); err != nil {
		return policy // absent: pure operator-default behaviour
	}
	atoi := func(raw string) (int32, bool) {
		value, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || value <= 0 {
			return 0, false
		}
		return int32(value), true
	}
	if seconds, ok := atoi(configMap.Data["defaultIdleSeconds"]); ok {
		policy.IdleSeconds = seconds
	}
	if keep, ok := atoi(configMap.Data["keepWarm"]); ok {
		policy.KeepWarm = int(keep)
	}
	for key, raw := range configMap.Data {
		if !strings.HasPrefix(key, "team.") || !strings.HasSuffix(key, ".idleSeconds") {
			continue
		}
		if seconds, ok := atoi(raw); ok {
			policy.TeamIdle[strings.TrimSuffix(strings.TrimPrefix(key, "team."), ".idleSeconds")] = seconds
		}
	}
	return policy
}

// effectiveIdleSeconds merges the precedence chain
// CR.idleTimeout > policy.team.<id>.idleSeconds > policy.defaultIdleSeconds
// > operator default. The channel-gateway URL relaxes the WeCom refusal:
// with a gateway holding the platform websocket, those employees may sleep.
func effectiveIdleSeconds(employee *v1alpha1.DigitalEmployee, policy HibernationPolicy, defaultIdleSeconds int32) int32 {
	if employee.Spec.Hibernation != nil {
		if seconds, ok := durationToSeconds(employee.Spec.Hibernation.IdleTimeout); ok {
			return seconds
		}
	}
	if seconds, ok := policy.TeamIdle[employee.Spec.Team]; ok {
		return seconds
	}
	if policy.IdleSeconds > 0 {
		return policy.IdleSeconds
	}
	return defaultIdleSeconds
}

// durationToSeconds parses the simple "30m"/"2h"/"90s" pattern the CRD
// allows. controller-runtime duration parsing would accept more shapes than
// the CRD validates, so keep the accepted set explicit.
func durationToSeconds(raw string) (int32, bool) {
	if raw == "" {
		return 0, false
	}
	unit := raw[len(raw)-1]
	multiplier := map[byte]int64{'s': 1, 'm': 60, 'h': 3600}[unit]
	if multiplier == 0 {
		return 0, false
	}
	value, err := strconv.ParseInt(raw[:len(raw)-1], 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	seconds := value * multiplier
	if seconds > 24*3600 {
		seconds = 24 * 3600 // cap: a day idle is as good as always-on
	}
	return int32(seconds), true
}

// ensureHibernation renders (or removes) the KEDA objects. Employees with an
// outbound IM channel (WeCom) are refused: their pod must stay resident or
// messages are lost until the channel-gateway lands.
func (r *DigitalEmployeeReconciler) ensureHibernation(ctx context.Context, employee *v1alpha1.DigitalEmployee) error {
	scaledObject := &KedaScaledObject{ObjectMeta: objectMeta(scaledObjectName(employee), employee.Namespace)}
	if !hibernationEnabled(employee) || r.hibernationBlockedByChannel(employee) {
		// Opted out or refused: remove any previously rendered objects so
		// disabling hibernation actually stops KEDA from owning replicas.
		if err := r.Delete(ctx, scaledObject); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("scaled object cleanup: %w", err)
		}
		if hibernationEnabled(employee) && r.hibernationBlockedByChannel(employee) {
			if err := r.setStatus(ctx, employee, v1alpha1.PhaseError,
				"hibernation is not supported for employees with the WeCom channel (outbound websocket would lose messages); remove channels.wecom or disable hibernation"); err != nil {
				return err
			}
			// Loud failure: silently ignoring the request would look like
			// hibernation while the pod actually stays resident.
			return fmt.Errorf("hibernation unsupported for %s: WeCom channel requires a resident pod", employee.Name)
		}
		return nil
	}

	if r.WakeDBFn == nil {
		return r.setStatus(ctx, employee, v1alpha1.PhaseError,
			"hibernation requested but this operator has no wake database configured (set HIBERNATION_DB_URL_SECRET)")
	}
	rawURL, err := r.WakeDBFn(ctx, employee.Namespace)
	if err != nil {
		return fmt.Errorf("wake db: %w", err)
	}
	coords, err := parseWakeDBURL(rawURL)
	if err != nil {
		return err
	}

	// Shared credentials secret + trigger auth (one per cluster; the first
	// reconciling employee creates them, later ones are no-ops).
	if err := r.ensureWakeDBSecret(ctx, employee, coords); err != nil {
		return err
	}
	auth := renderTriggerAuthentication(employee)
	if err := controllerutil.SetControllerReference(employee, auth, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, auth); err != nil {
		return fmt.Errorf("trigger authentication: %w", err)
	}

	policy := loadHibernationPolicy(ctx, r.Client, employee.Namespace)
	rendered := renderScaledObject(employee, coords, effectiveIdleSeconds(employee, policy, r.DefaultIdleSeconds), policy.KeepWarm)
	if err := controllerutil.SetControllerReference(employee, rendered, r.Scheme); err != nil {
		return err
	}
	if err := safeCreateOrUpdate(ctx, r.Client, rendered); err != nil {
		return fmt.Errorf("scaled object: %w", err)
	}
	return nil
}

// hibernationBlockedByChannel: WeCom holds an outbound websocket inside the
// runtime pod; scaling it to zero severs the channel — unless a
// channel-gateway holds the platform leg for the cluster.
func (r *DigitalEmployeeReconciler) hibernationBlockedByChannel(employee *v1alpha1.DigitalEmployee) bool {
	if r.ChannelGatewayURL != "" {
		return false
	}
	return employee.Spec.Channels != nil && employee.Spec.Channels.Wecom
}

func scaledObjectName(employee *v1alpha1.DigitalEmployee) string {
	return deploymentName(employee) + "-wake"
}

// ensureWakeDBSecret upserts the shared secret with discrete connection
// fields (KEDA's TriggerAuthentication reads keys, not URLs). It carries no
// owner reference: its lifecycle is the cluster's, not one employee's.
func (r *DigitalEmployeeReconciler) ensureWakeDBSecret(ctx context.Context, employee *v1alpha1.DigitalEmployee, coords wakeDBCoordinates) error {
	// Data (not StringData): the fake client in tests persists the object
	// verbatim, so the StringData→Data conversion the API server does never
	// happens there.
	secretData := map[string][]byte{
		"DB_HOST":     []byte(coords.Host),
		"DB_PORT":     []byte(coords.Port),
		"DB_NAME":     []byte(coords.DBName),
		"DB_USER":     []byte(coords.User),
		"DB_PASSWORD": []byte(coords.Password),
	}
	existing := &corev1.Secret{ObjectMeta: objectMeta(wakeDBSecret, employee.Namespace)}
	if err := r.Get(ctx, client.ObjectKeyFromObject(existing), existing); err != nil {
		if apierrors.IsNotFound(err) {
			return r.Create(ctx, &corev1.Secret{
				ObjectMeta: objectMeta(wakeDBSecret, employee.Namespace),
				Data:       secretData,
			})
		}
		return err
	}
	if string(existing.Data["DB_HOST"]) != coords.Host || string(existing.Data["DB_NAME"]) != coords.DBName {
		existing.Data = secretData
		return r.Update(ctx, existing)
	}
	return nil
}

func renderTriggerAuthentication(employee *v1alpha1.DigitalEmployee) *KedaTriggerAuthentication {
	return &KedaTriggerAuthentication{
		TypeMeta:   metav1.TypeMeta{APIVersion: kedaAPIVersion, Kind: "TriggerAuthentication"},
		ObjectMeta: objectMeta(wakeAuthName, employee.Namespace),
		Spec: KedaTriggerAuthenticationSpec{
			SecretTargetRef: []KedaSecretTargetRef{
				{Parameter: "password", Name: wakeDBSecret, Key: "DB_PASSWORD"},
			},
		},
	}
}

func renderScaledObject(employee *v1alpha1.DigitalEmployee, coords wakeDBCoordinates, idleSeconds int32, keepWarm int) *KedaScaledObject {
	polling := int32(5)
	cooldown := idleSeconds
	minReplicas := int32(0)
	maxReplicas := int32(1)
	// Signal freshness window: the portal upserts activity on every chat
	// request, so an ongoing conversation keeps the metric warm; KEDA's
	// cooldownPeriod carries the configured idle window. The keep-warm term
	// pins the K most-recently-used employees resident (LRU warm pool).
	query := fmt.Sprintf(
		"SELECT (SELECT count(*) FROM channel_wake WHERE employee='%s' AND created_at > now() - interval '2 minutes') + (SELECT count(*) FROM employee_activity WHERE employee='%s' AND last_active > now() - interval '2 minutes')",
		employee.Name, employee.Name)
	if keepWarm > 0 {
		query += fmt.Sprintf(
			" + (SELECT count(*) FROM (SELECT employee FROM employee_activity ORDER BY last_active DESC LIMIT %d) hot WHERE hot.employee='%s')",
			keepWarm, employee.Name)
	}
	return &KedaScaledObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: kedaAPIVersion, Kind: "ScaledObject"},
		ObjectMeta: metav1.ObjectMeta{Name: scaledObjectName(employee), Namespace: employee.Namespace, Labels: labels(employee)},
		Spec: KedaScaledObjectSpec{
			ScaleTargetRef:  KedaScaleTargetRef{Name: deploymentName(employee)},
			PollingInterval: &polling,
			CooldownPeriod:  &cooldown,
			MinReplicaCount: &minReplicas,
			MaxReplicaCount: &maxReplicas,
			Triggers: []KedaScaleTrigger{{
				Type: "postgresql",
				Metadata: map[string]string{
					"host":     coords.Host,
					"port":     coords.Port,
					"dbName":   coords.DBName,
					"userName": coords.User,
					// The shared conversation PG serves plain TCP on the
					// intranet; KEDA's postgresql scaler requires an
					// explicit sslmode.
					"sslmode":          "disable",
					"query":            query,
					"targetQueryValue": "1",
				},
				AuthenticationRef: &KedaScaledObjectAuthRef{Name: wakeAuthName},
			}},
		},
	}
}
