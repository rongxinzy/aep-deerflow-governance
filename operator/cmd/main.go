// The digital-employee operator: watches DigitalEmployee CRs and reconciles
// the child workloads. The operator always holds full rights over its CRs
// and child resources; whether a given user may create a digital employee
// is a business-layer policy, never Kubernetes RBAC.
package main

import (
	"context"
	"flag"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/rongxinzy/aep-deerflow-governance/operator/internal/controller"
)

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var namespace string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint bind address")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"enable leader election for this operator (multiple replicas coordinate)")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe bind address")
	flag.StringVar(&namespace, "namespace", "digital-employees", "namespace to watch")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(logger)

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	// Locally-modelled KEDA CRDs (see internal/controller/keda_types.go):
	// registering them lets the manager's client create the ScaledObject /
	// TriggerAuthentication renders without importing keda client-go.
	controller.AddKedaTypesToScheme(scheme)

	// Watch only the configured namespace: employee workloads are
	// namespaced and the operator's blast radius should match.
	config := ctrl.GetConfigOrDie()
	options := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "de-operator.zhiyuan.io",
	}
	if namespace != "" {
		options.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}}
	}
	mgr, err := ctrl.NewManager(config, options)
	if err != nil {
		logger.Error(err, "manager setup failed")
		os.Exit(1)
	}

	wakeDBSecret := envOrDefault("HIBERNATION_DB_URL_SECRET", "de-portal-db")
	reconciler := &controller.DigitalEmployeeReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		OVRootFn: func(ctx context.Context, c client.Client, namespace, secretName string) (string, error) {
			return readSecretKey(ctx, c, namespace, secretName, "OPENVIKING_ROOT_API_KEY")
		},
		WakeDBFn: func(ctx context.Context, namespace string) (string, error) {
			return readSecretKey(ctx, mgr.GetClient(), namespace, wakeDBSecret, "PORTAL_DB_URL")
		},
		DefaultIdleSeconds:     int32(envIntOrDefault("HIBERNATION_IDLE_TIMEOUT_SECONDS", 1800)),
		ChannelGatewayURL:      envOrDefault("CHANNEL_GATEWAY_URL", ""),
		ConversationAdminDBURL: envOrDefault("CONVERSATION_ADMIN_DB_URL", ""),
		ConversationInitImage:  envOrDefault("CONVERSATION_INIT_IMAGE", "postgres:17.6-alpine"),
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "controller setup failed")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "health check setup failed")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "ready check setup failed")
		os.Exit(1)
	}
	logger.Info("starting digital-employee operator", "namespace", namespace)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "operator stopped")
		os.Exit(1)
	}
}

func readSecretKey(ctx context.Context, mgrClient client.Client, namespace, secretName, key string) (string, error) {
	if strings.TrimSpace(secretName) == "" {
		return "", errNoCredentialsSecret
	}
	secret := &corev1.Secret{}
	if err := mgrClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secretName}, secret); err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(secret.Data[key]))
	if value == "" {
		return "", errMissingKey
	}
	return value, nil
}

type stringError string

func (e stringError) Error() string { return string(e) }

const (
	errNoCredentialsSecret = stringError("spec.credentials.name must reference the platform credentials secret")
	errMissingKey          = stringError("credentials secret is missing OPENVIKING_ROOT_API_KEY")
)

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	value := 0
	for _, c := range envOrDefault(key, "") {
		if c < '0' || c > '9' {
			return fallback
		}
		value = value*10 + int(c-'0')
	}
	if value == 0 {
		return fallback
	}
	return value
}
