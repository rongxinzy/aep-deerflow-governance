package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// catalogJSON builds the admin model catalog payload with probe state.
func catalogJSON(models ...map[string]any) string {
	document := map[string]any{"models": models}
	raw, _ := json.Marshal(document)
	return string(raw)
}

func catalogEntry(id, status string, since *time.Time) map[string]any {
	entry := map[string]any{
		"id": id, "displayName": id, "sourceType": "gateway", "protocol": "anthropic",
		"endpoint": "http://upstream.example:8080", "upstreamModel": "native-" + id,
		"enabled": true, "healthStatus": status,
	}
	if since != nil {
		entry["healthSince"] = since.UTC().Format(time.RFC3339)
	}
	return entry
}

func settingsJSON(chain ...string) string {
	if chain == nil {
		chain = []string{}
	}
	raw, _ := json.Marshal(map[string]any{
		"modelFallbackIds": map[string]any{"override": nil, "effectiveValue": chain, "source": "env"},
	})
	return string(raw)
}

func failoverEmployee(name, primary string) *dev1alpha1.DigitalEmployee {
	return &dev1alpha1.DigitalEmployee{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "digital-employees"},
		Spec: dev1alpha1.DigitalEmployeeSpec{
			Owner: name + "-owner",
			Runtime: dev1alpha1.RuntimeSpec{Models: []dev1alpha1.RuntimeModel{{
				Name: primary, Provider: "anthropic", BaseURL: "http://gw.example/" + primary, ApiKeyEnv: "DEER_BENCH_API_KEY",
			}}},
		},
	}
}

func newFailoverPortal(t *testing.T, catalog, settings string, employees ...*dev1alpha1.DigitalEmployee) *portal {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, schemeAddK8s(scheme))
	require.NoError(t, dev1alpha1.AddToScheme(scheme))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "de-platform-creds", Namespace: "digital-employees"},
		Data:       map[string][]byte{"AEP_ADMIN_USER": []byte("admin"), "AEP_ADMIN_PASSWORD": []byte("pw")},
	}
	objects := []runtime.Object{secret}
	for _, employee := range employees {
		objects = append(objects, employee)
	}
	aep := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/aep/v1/auth/password/login":
			_, _ = w.Write([]byte(`{"accessToken":"failover-token"}`))
		case "/aep/v1/admin/deployment/settings":
			_, _ = w.Write([]byte(settings))
		case "/aep/v1/admin/models":
			_, _ = w.Write([]byte(catalog))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(aep.Close)
	adminTokenCache.token = ""
	adminTokenCache.expires = time.Time{}
	return &portal{
		cfg: config{
			namespace: "digital-employees", aepBaseURL: aep.URL, aepDeploymentID: "demo",
			credentialsSecret: "de-platform-creds", gatewayBaseURL: "http://gw.example", modelKeyEnv: "DEER_BENCH_API_KEY",
			failoverInterval: time.Minute, failoverMinUnhealthy: 5 * time.Minute, failoverMinHealthy: 20 * time.Minute,
		},
		k8s:    fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build(),
		aep:    &aepAuthenticator{},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func loadEmployee(t *testing.T, p *portal, name string) *dev1alpha1.DigitalEmployee {
	t.Helper()
	var item dev1alpha1.DigitalEmployee
	require.NoError(t, p.k8s.Get(context.Background(), client.ObjectKey{Namespace: "digital-employees", Name: name}, &item))
	return &item
}

func TestFailoverSwitchesImmediatelyOnCredentialFailure(t *testing.T) {
	// 401 (credential_invalid) discovered one minute ago — well inside the
	// transient grace — still switches: a rejected credential cannot serve.
	recent := time.Now().Add(-time.Minute)
	catalog := catalogJSON(
		catalogEntry("bench-qwen38-fast", "credential_invalid", &recent),
		catalogEntry("bench-qwen", "healthy", &recent),
	)
	p := newFailoverPortal(t, catalog, settingsJSON("bench-qwen", "bench-glm"), failoverEmployee("de-a", "bench-qwen38-fast"))

	p.failoverOnce(context.Background(), time.Now().UTC())

	employee := loadEmployee(t, p, "de-a")
	require.Len(t, employee.Spec.Runtime.Models, 2, "the original stays configured behind the fallback")
	assert.Equal(t, "bench-qwen", employee.Spec.Runtime.Models[0].Name)
	assert.Equal(t, "http://gw.example/bench-qwen", employee.Spec.Runtime.Models[0].BaseURL)
	assert.Equal(t, "bench-qwen38-fast", employee.Spec.Runtime.Models[1].Name)
	state, ok, corrupt := parseFailoverState(employee)
	require.True(t, ok, "the switch must be recorded on the employee")
	require.False(t, corrupt)
	assert.Equal(t, "bench-qwen38-fast", state.Original)
	assert.Equal(t, "bench-qwen", state.Active)
}

func TestFailoverSwitchesImmediatelyOnDenied(t *testing.T) {
	// 403 (denied — often throttling) is equally unusable right now.
	recent := time.Now().Add(-time.Minute)
	catalog := catalogJSON(
		catalogEntry("bench-qwen38-fast", "denied", &recent),
		catalogEntry("bench-qwen", "healthy", &recent),
	)
	p := newFailoverPortal(t, catalog, settingsJSON("bench-qwen"), failoverEmployee("de-a", "bench-qwen38-fast"))

	p.failoverOnce(context.Background(), time.Now().UTC())

	employee := loadEmployee(t, p, "de-a")
	assert.Equal(t, "bench-qwen", employee.Spec.Runtime.Models[0].Name)
}

func TestFailoverWaitsOutTransientFailures(t *testing.T) {
	recent := time.Now().Add(-time.Minute)
	catalog := catalogJSON(
		catalogEntry("bench-qwen38-fast", "unreachable", &recent),
		catalogEntry("bench-qwen", "healthy", &recent),
	)
	p := newFailoverPortal(t, catalog, settingsJSON("bench-qwen"), failoverEmployee("de-a", "bench-qwen38-fast"))

	p.failoverOnce(context.Background(), time.Now().UTC())
	employee := loadEmployee(t, p, "de-a")
	assert.Equal(t, "bench-qwen38-fast", employee.Spec.Runtime.Models[0].Name, "inside the grace window nothing moves")

	// Once the failure has persisted past the minimum the switch happens.
	past := time.Now().Add(-6 * time.Minute)
	catalog = catalogJSON(
		catalogEntry("bench-qwen38-fast", "unreachable", &past),
		catalogEntry("bench-qwen", "healthy", &past),
	)
	p2 := newFailoverPortal(t, catalog, settingsJSON("bench-qwen"), failoverEmployee("de-a", "bench-qwen38-fast"))
	p2.failoverOnce(context.Background(), time.Now().UTC())
	employee = loadEmployee(t, p2, "de-a")
	assert.Equal(t, "bench-qwen", employee.Spec.Runtime.Models[0].Name)
}

func TestFailoverSkipsUnknownHealth(t *testing.T) {
	catalog := catalogJSON(
		catalogEntry("bench-qwen38-fast", "unknown", nil),
		catalogEntry("bench-qwen", "healthy", nil),
	)
	p := newFailoverPortal(t, catalog, settingsJSON("bench-qwen"), failoverEmployee("de-a", "bench-qwen38-fast"))
	p.failoverOnce(context.Background(), time.Now().UTC())
	assert.Equal(t, "bench-qwen38-fast", loadEmployee(t, p, "de-a").Spec.Runtime.Models[0].Name)
}

func TestFailoverRestoresWhenOriginalRecovers(t *testing.T) {
	healthyLong := time.Now().Add(-30 * time.Minute)
	catalog := catalogJSON(
		catalogEntry("bench-qwen38-fast", "healthy", &healthyLong),
		catalogEntry("bench-qwen", "healthy", &healthyLong),
	)
	employee := failoverEmployee("de-a", "bench-qwen")
	employee.Spec.Runtime.Models = append(employee.Spec.Runtime.Models, dev1alpha1.RuntimeModel{
		Name: "bench-qwen38-fast", Provider: "anthropic", BaseURL: "http://gw.example/bench-qwen38-fast", ApiKeyEnv: "DEER_BENCH_API_KEY",
	})
	state, _ := json.Marshal(failoverState{Original: "bench-qwen38-fast", Active: "bench-qwen", SwitchedAt: time.Now().Add(-time.Hour).Format(time.RFC3339)})
	employee.SetAnnotations(map[string]string{failoverAnnotation: string(state)})
	p := newFailoverPortal(t, catalog, settingsJSON("bench-qwen"), employee)

	p.failoverOnce(context.Background(), time.Now().UTC())

	restored := loadEmployee(t, p, "de-a")
	assert.Equal(t, "bench-qwen38-fast", restored.Spec.Runtime.Models[0].Name)
	assert.Len(t, restored.Spec.Runtime.Models, 2, "the fallback stays available as a secondary")
	_, ok, _ := parseFailoverState(restored)
	assert.False(t, ok, "a completed restore clears the failover record")
}

func TestFailoverClearsRecordWhenModelChangedExternally(t *testing.T) {
	healthyLong := time.Now().Add(-30 * time.Minute)
	catalog := catalogJSON(catalogEntry("bench-qwen38-fast", "healthy", &healthyLong))
	employee := failoverEmployee("de-a", "bench-glm") // operator already repointed it
	state, _ := json.Marshal(failoverState{Original: "bench-qwen38-fast", Active: "bench-qwen", SwitchedAt: "x"})
	employee.SetAnnotations(map[string]string{failoverAnnotation: string(state)})
	p := newFailoverPortal(t, catalog, settingsJSON("bench-qwen"), employee)

	p.failoverOnce(context.Background(), time.Now().UTC())

	updated := loadEmployee(t, p, "de-a")
	assert.Equal(t, "bench-glm", updated.Spec.Runtime.Models[0].Name, "external changes win")
	_, ok, _ := parseFailoverState(updated)
	assert.False(t, ok)
}

func TestFailoverNoopWithoutChain(t *testing.T) {
	recent := time.Now().Add(-time.Minute)
	catalog := catalogJSON(catalogEntry("bench-qwen38-fast", "credential_invalid", &recent))
	p := newFailoverPortal(t, catalog, settingsJSON(), failoverEmployee("de-a", "bench-qwen38-fast"))
	p.failoverOnce(context.Background(), time.Now().UTC())
	assert.Equal(t, "bench-qwen38-fast", loadEmployee(t, p, "de-a").Spec.Runtime.Models[0].Name)
}

func TestFailoverSkipsIncompleteCatalogPage(t *testing.T) {
	recent := time.Now().Add(-time.Minute)
	next := "cursor-2"
	raw, _ := json.Marshal(map[string]any{
		"models":     []map[string]any{catalogEntry("bench-qwen", "healthy", &recent)},
		"nextCursor": next,
	})
	p := newFailoverPortal(t, string(raw), settingsJSON("bench-qwen"), failoverEmployee("de-a", "bench-qwen38-fast"))

	p.failoverOnce(context.Background(), time.Now().UTC())

	employee := loadEmployee(t, p, "de-a")
	assert.Equal(t, "bench-qwen38-fast", employee.Spec.Runtime.Models[0].Name,
		"a partial catalog must never trigger a false not-in-catalog failover")
}

func TestFailoverRestoresWhenChainCleared(t *testing.T) {
	// Clearing the deployment chain must not pin an already-switched
	// employee to the fallback forever.
	healthyLong := time.Now().Add(-30 * time.Minute)
	catalog := catalogJSON(
		catalogEntry("bench-qwen38-fast", "healthy", &healthyLong),
		catalogEntry("bench-qwen", "healthy", &healthyLong),
	)
	employee := failoverEmployee("de-a", "bench-qwen")
	employee.Spec.Runtime.Models = append(employee.Spec.Runtime.Models, dev1alpha1.RuntimeModel{
		Name: "bench-qwen38-fast", Provider: "anthropic", BaseURL: "http://gw.example/bench-qwen38-fast", ApiKeyEnv: "DEER_BENCH_API_KEY",
	})
	state, _ := json.Marshal(failoverState{Original: "bench-qwen38-fast", Active: "bench-qwen", SwitchedAt: time.Now().Add(-time.Hour).Format(time.RFC3339)})
	employee.SetAnnotations(map[string]string{failoverAnnotation: string(state)})
	p := newFailoverPortal(t, catalog, settingsJSON(), employee) // empty chain

	p.failoverOnce(context.Background(), time.Now().UTC())

	restored := loadEmployee(t, p, "de-a")
	assert.Equal(t, "bench-qwen38-fast", restored.Spec.Runtime.Models[0].Name)
	_, ok, _ := parseFailoverState(restored)
	assert.False(t, ok, "the failover record clears even without a configured chain")
}

func TestFailoverAdvancesWhenActiveFallbackFails(t *testing.T) {
	recent := time.Now().Add(-time.Minute)
	catalog := catalogJSON(
		catalogEntry("bench-qwen38-fast", "unreachable", &recent), // original still down
		catalogEntry("bench-qwen", "credential_invalid", &recent), // active now dead too
		catalogEntry("bench-glm", "healthy", &recent),             // next entry is fine
	)
	employee := failoverEmployee("de-a", "bench-qwen")
	employee.Spec.Runtime.Models = append(employee.Spec.Runtime.Models, dev1alpha1.RuntimeModel{
		Name: "bench-qwen38-fast", Provider: "anthropic", BaseURL: "http://gw.example/bench-qwen38-fast", ApiKeyEnv: "DEER_BENCH_API_KEY",
	})
	state, _ := json.Marshal(failoverState{Original: "bench-qwen38-fast", Active: "bench-qwen", SwitchedAt: time.Now().Add(-time.Hour).Format(time.RFC3339)})
	employee.SetAnnotations(map[string]string{failoverAnnotation: string(state)})
	p := newFailoverPortal(t, catalog, settingsJSON("bench-qwen", "bench-glm"), employee)

	p.failoverOnce(context.Background(), time.Now().UTC())

	advanced := loadEmployee(t, p, "de-a")
	assert.Equal(t, "bench-glm", advanced.Spec.Runtime.Models[0].Name, "a dead fallback must advance along the chain")
	updated, ok, _ := parseFailoverState(advanced)
	require.True(t, ok)
	assert.Equal(t, "bench-qwen38-fast", updated.Original, "the original model stays recorded across advances")
	assert.Equal(t, "bench-glm", updated.Active)
}

func TestFailoverCorruptRecordIsLeftUntouched(t *testing.T) {
	recent := time.Now().Add(-time.Minute)
	catalog := catalogJSON(
		catalogEntry("bench-qwen", "credential_invalid", &recent),
		catalogEntry("bench-glm", "healthy", &recent),
	)
	employee := failoverEmployee("de-a", "bench-qwen")
	employee.SetAnnotations(map[string]string{failoverAnnotation: "{corrupt"})
	p := newFailoverPortal(t, catalog, settingsJSON("bench-glm"), employee)

	p.failoverOnce(context.Background(), time.Now().UTC())

	untouched := loadEmployee(t, p, "de-a")
	assert.Equal(t, "bench-qwen", untouched.Spec.Runtime.Models[0].Name,
		"an unreadable record must never be overwritten — the original model id would be lost")
	assert.Equal(t, "{corrupt", untouched.GetAnnotations()[failoverAnnotation])
}

func TestFailoverIntervalZeroDisablesWatcher(t *testing.T) {
	t.Setenv("PORTAL_FAILOVER_INTERVAL_SECONDS", "0")
	cfg := loadConfig()
	if cfg.failoverInterval > 0 {
		t.Fatalf("interval 0 must disable the watcher, got %s", cfg.failoverInterval)
	}
}

func TestFailoverReportsNoHealthyFallback(t *testing.T) {
	recent := time.Now().Add(-time.Minute)
	catalog := catalogJSON(
		catalogEntry("bench-qwen38-fast", "credential_invalid", &recent),
		catalogEntry("bench-qwen", "unreachable", &recent),
	)
	p := newFailoverPortal(t, catalog, settingsJSON("bench-qwen"), failoverEmployee("de-a", "bench-qwen38-fast"))
	p.failoverOnce(context.Background(), time.Now().UTC())
	employee := loadEmployee(t, p, "de-a")
	assert.Equal(t, "bench-qwen38-fast", employee.Spec.Runtime.Models[0].Name)
	assert.NotContains(t, fmt.Sprint(employee.GetAnnotations()), failoverAnnotation)
}
