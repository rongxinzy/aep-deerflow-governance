// workspace-agent supervises PersonalWorkspace microVMs on one KVM node:
// resync loop (10s) reconciles assigned workspaces into firecracker
// processes, taps, whitelist chains, disks and TCP proxies.
//
// Deployment shape (chart): privileged DaemonSet, host root mounted at
// /host (commands run via chroot), workdir on hostPath, template payload
// materialized by the initContainer. NODE_NAME / HOST_IP come from the
// Downward API.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	v1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/disk"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/hostcmd"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/manager"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/template"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/vm"
)

func main() {
	var cfg manager.Config
	flag.StringVar(&cfg.Namespace, "namespace", envOr("POD_NAMESPACE", "digital-employees"), "namespace of workspaces")
	flag.StringVar(&cfg.NodeName, "node-name", os.Getenv("NODE_NAME"), "this node's name (Downward API)")
	flag.StringVar(&cfg.HostIP, "host-ip", os.Getenv("HOST_IP"), "this node's host IP (Downward API)")
	flag.StringVar(&cfg.Workdir, "workdir", "/var/lib/zhiyuan/workspace-agent", "hostPath workdir root")
	flag.StringVar(&cfg.TemplateTag, "template-tag", envOr("TEMPLATE_TAG", "0.5.0"), "pw-rootfs template tag")
	portRange := flag.String("port-range", envOr("PORT_RANGE", "8800-8899"), "host port range for proxies")
	flag.StringVar(&cfg.ModelBaseURL, "model-base-url", envOr("MODEL_BASE_URL", "http://higress-gateway.higress-system.svc.cluster.local/v1"), "model gateway route")
	flag.StringVar(&cfg.MemoryBaseURL, "memory-base-url", envOr("MEMORY_BASE_URL", "http://openviking.memory-system.svc.cluster.local:1933"), "OpenViking service the companions dial directly")
	hostRoot := flag.String("host-root", envOr("HOST_ROOT", "/host"), "mounted host filesystem root")
	metricsAddr := flag.String("metrics-addr", envOr("METRICS_ADDR", ":9091"), "Prometheus /metrics listen address")
	serialMB := flag.Int("serial-log-mb", 5, "serial log rotation threshold (MiB)")
	interval := flag.Duration("resync", 10*time.Second, "resync interval")
	flag.Parse()

	logf.SetLogger(zap.New(zap.UseDevMode(false)))
	logger := logf.Log.WithName("workspace-agent")

	if cfg.NodeName == "" || cfg.HostIP == "" {
		logger.Error(nil, "NODE_NAME and HOST_IP are required (Downward API)")
		os.Exit(1)
	}
	lo, hi, err := parsePortRange(*portRange)
	if err != nil {
		logger.Error(err, "bad port range", "range", *portRange)
		os.Exit(1)
	}
	cfg.PortLo, cfg.PortHi = lo, hi
	cfg.SerialLogMB = *serialMB

	// Template must verify before anything boots.
	if _, err := template.Resolve(cfg.Workdir+"/templates", cfg.TemplateTag); err != nil {
		logger.Error(err, "template verification failed", "tag", cfg.TemplateTag)
		os.Exit(1)
	}
	// Workspaces can pin older templates. Retention is explicit until GC can
	// account for all workspace references; startup must not delete pinned roots.

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Deliberately a direct client (no informer cache): the resync loop is
	// 10s, object counts are single digits, and mutation-free reads keep
	// the status single-writer story simple.
	restConfig := ctrlconfig.GetConfigOrDie()
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		logger.Error(err, "client build failed")
		os.Exit(1)
	}

	m := manager.New(cfg, c,
		hostcmd.NewChrootRunner(*hostRoot),
		&disk.Images{
			Run:         hostcmd.NewChrootRunner(*hostRoot),
			StagingRoot: filepath.Join(cfg.Workdir, "cfg-staging"),
		},
		func(runDir string) vm.Driver { return vm.NewFirecracker(runDir) },
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("/metrics", m.MetricsHandler())
	metricsSrv := &http.Server{Addr: *metricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	// Fail fast on an unbindable metrics port: logging and continuing would
	// silently blind every alert built on pw_node_agent_* (port conflicts are
	// plausible — the DaemonSet is hostNetwork and hostPort-publishes 9091).
	metricsLn, err := net.Listen("tcp", *metricsAddr)
	if err != nil {
		logger.Error(err, "metrics listen failed", "addr", *metricsAddr)
		os.Exit(1)
	}
	go func() {
		if err := metricsSrv.Serve(metricsLn); err != nil && err != http.ErrServerClosed {
			logger.Error(err, "metrics server", "addr", *metricsAddr)
		}
	}()
	defer func() { _ = metricsSrv.Close() }()

	logger.Info("workspace agent starting",
		"node", cfg.NodeName, "hostIP", cfg.HostIP, "namespace", cfg.Namespace,
		"workdir", cfg.Workdir, "template", cfg.TemplateTag, "ports", portRange,
		"metricsAddr", *metricsAddr)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	run := func() {
		if err := m.Resync(ctx); err != nil {
			logger.Error(err, "resync")
		}
	}
	run()
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down; VMs keep running until the container dies with them")
			return
		case <-ticker.C:
			run()
		}
	}
}

func parsePortRange(s string) (int32, int32, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("port range %q must be lo-hi", s)
	}
	lo, err := strconv.ParseInt(parts[0], 10, 32)
	if err != nil {
		return 0, 0, err
	}
	hi, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil {
		return 0, 0, err
	}
	if lo <= 0 || lo > hi {
		return 0, 0, fmt.Errorf("port range %q invalid", s)
	}
	return int32(lo), int32(hi), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
