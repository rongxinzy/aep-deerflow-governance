// Tests meant for `go test -race`. This file must not import mockey:
// function patching rewrites machine code without synchronization and is
// unsafe next to the race detector and live goroutines, so the guest healthz
// endpoint is a real loopback HTTP server instead of a patched probeHealthz.
package manager

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/disk"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/hostcmd"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/vm"
)

// raceDriver is a concurrency-safe vm.Driver stub (the gomock MockDriver is
// not the thing under test here; concurrent Start/Stop/Alive interleavings
// are).
type raceDriver struct {
	mu     sync.Mutex
	alive  bool
	starts int
	stops  int
}

func (d *raceDriver) Start(vm.Config) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	time.Sleep(2 * time.Millisecond) // widen the window a scrape can interleave
	d.alive = true
	d.starts++
	return nil
}

func (d *raceDriver) Stop(time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	d.alive = false
	d.stops++
	return nil
}

func (d *raceDriver) Alive() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.alive
}

func (d *raceDriver) PID() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.alive {
		return 4242
	}
	return 0
}

// TestScrapeConcurrentWithReconcile hammers /metrics from several goroutines
// while the resync loop boots, runs, healthz-fails and stops a workspace —
// the interleaving that used to race on sup.bootFails/runningSince and on
// driver.Alive vs driver.Stop. Green under `go test -race` is the assertion;
// the value checks below only guard against a broken setup passing vacuously.
func TestScrapeConcurrentWithReconcile(t *testing.T) {
	// The guest IP of 127.0.0.4/30 is 127.0.0.6; serve healthz there with an
	// atomic healthy/hung flag (Linux treats all of 127/8 as loopback).
	var healthy atomic.Bool
	healthy.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	ln, err := net.Listen("tcp", "127.0.0.6:8080")
	if err != nil {
		t.Skipf("loopback alias 127.0.0.6 unavailable: %v", err)
	}
	healthSrv := &http.Server{Handler: mux}
	// Close (not just listener close): DefaultTransport pools keep-alive
	// connections process-wide, and a pooled conn would keep talking to this
	// server across repeated test runs (-count>1), serving a stale flag.
	defer func() { _ = healthSrv.Close() }()
	go func() { _ = healthSrv.Serve(ln) }()

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	objects := baseObjects(manifestV1)
	objects[0].(*v1alpha1.PersonalWorkspace).Status.VMSubnet = "127.0.0.4/30"
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PersonalWorkspace{}).
		WithObjects(objects...).Build()

	workdir := t.TempDir()
	writeFakeTemplate(t, filepath.Join(workdir, "templates", "0.5.0"))
	driver := &raceDriver{}
	runner := &hostcmd.FakeRunner{}
	m := New(Config{
		Namespace: "digital-employees", NodeName: "node-a", HostIP: "10.1.2.3",
		Workdir: workdir, TemplateTag: "0.5.0", PortLo: 8800, PortHi: 8899,
		ModelBaseURL: "http://gw/v1", SerialLogMB: 5,
	}, c, runner, &disk.Images{Run: runner}, func(string) vm.Driver { return driver })
	// The test ends on a Running round; release the host proxy port so
	// repeated runs (-count>1) do not leak listeners.
	defer func() {
		m.mu.Lock()
		for _, s := range m.supers {
			if s.proxy != nil {
				s.proxy.Stop()
				s.proxy = nil
			}
		}
		m.mu.Unlock()
	}()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := m.MetricsHandler()
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			for {
				select {
				case <-stop:
					return
				default:
				}
				h.ServeHTTP(httptest.NewRecorder(), req)
			}
		}()
	}
	// Alternate healthy (boot/Running) and hung (healthz fail → stop) rounds.
	// The backoff clock is rewound between rounds to keep the test fast.
	for round := 0; round < 7; round++ { // odd count: ends on a healthy round
		healthy.Store(round%2 == 0)
		m.mu.Lock()
		if s := m.supers["ws1"]; s != nil {
			s.lastBoot = time.Now().Add(-time.Minute)
		}
		m.mu.Unlock()
		if err := m.Resync(context.Background()); err != nil {
			t.Logf("round %d (healthy=%v): %v", round, round%2 == 0, err)
		}
	}
	close(stop)
	wg.Wait()

	driver.mu.Lock()
	starts, stops := driver.starts, driver.stops
	driver.mu.Unlock()
	assert.GreaterOrEqual(t, starts, 2, "hung rounds force reboots")
	assert.GreaterOrEqual(t, stops, 1, "a hung guest is stopped")

	rec := httptest.NewRecorder()
	m.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `pw_node_agent_workspace_phase{phase="Running",workspace="ws1"} 1`)
}
