// Prometheus /metrics for the node agent. A private registry fed from a live
// snapshot on every scrape: no global registration (tests can build several
// managers) and no stale values between scrapes. Sources: supervisor set
// (counts, bootFails) + the manager's CR-List-derived phase map (phases, so
// failures before supervisor creation are visible).
package manager

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/vm"
)

// workspacePhases enumerates the CR status.phase values (label domain of the
// phase gauge); keep in sync with operator api/v1alpha1.
var workspacePhases = []string{"Pending", "Running", "Failed", "Terminating"}

var (
	descWorkspaces = prometheus.NewDesc(
		"pw_node_agent_workspaces",
		"Workspaces supervised on this node.", nil, nil)
	descVMs = prometheus.NewDesc(
		"pw_node_agent_vms",
		"Supervised workspaces whose VM process is alive.", nil, nil)
	descPhase = prometheus.NewDesc(
		"pw_node_agent_workspace_phase",
		"Workspace CR phase from the resync loop's CR List (1 = current phase); includes workspaces that failed before supervision started.",
		[]string{"workspace", "phase"}, nil)
	descBootFails = prometheus.NewDesc(
		"pw_node_agent_workspace_boot_fails",
		"Consecutive boot/health failures counted for the workspace.",
		[]string{"workspace"}, nil)
	descResyncErrors = prometheus.NewDesc(
		"pw_node_agent_resync_errors_total",
		"Resync rounds that returned an error.", nil, nil)
)

// MetricsHandler serves the agent's metrics on /metrics.
func (m *Manager) MetricsHandler() http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(&collector{m: m})
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

type collector struct{ m *Manager }

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descWorkspaces
	ch <- descVMs
	ch <- descPhase
	ch <- descBootFails
	ch <- descResyncErrors
}

type supSnapshot struct {
	fails int
	alive bool
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	// Scalars (and the phase map) under the lock; driver.Alive() outside it.
	// Alive/Stop synchronize inside the driver, but a Stop still takes seconds
	// (grace window) — holding m.mu across it would stall every reconcile
	// field update behind a scrape.
	c.m.mu.Lock()
	snaps := make(map[string]supSnapshot, len(c.m.supers))
	drivers := make(map[string]vm.Driver, len(c.m.supers))
	for name, s := range c.m.supers {
		snaps[name] = supSnapshot{fails: s.bootFails}
		drivers[name] = s.driver
	}
	phases := make(map[string]string, len(c.m.phases))
	for name, phase := range c.m.phases {
		phases[name] = phase
	}
	total := len(c.m.supers)
	c.m.mu.Unlock()

	alive := 0
	for name, d := range drivers {
		if d.Alive() {
			alive++
			s := snaps[name]
			s.alive = true
			snaps[name] = s
		}
	}
	errs := c.m.resyncErrors.Load()

	ch <- prometheus.MustNewConstMetric(descWorkspaces, prometheus.GaugeValue, float64(total))
	ch <- prometheus.MustNewConstMetric(descVMs, prometheus.GaugeValue, float64(alive))
	ch <- prometheus.MustNewConstMetric(descResyncErrors, prometheus.CounterValue, float64(errs))
	for name, s := range snaps {
		ch <- prometheus.MustNewConstMetric(descBootFails, prometheus.GaugeValue, float64(s.fails), name)
	}
	// Phases come from the CR List (Manager.phases), not the supervisor set:
	// workspaces whose reconcile fails before supervisor creation still emit
	// a series, so WorkspacePhaseStuck sees early provisioning failures.
	for name, current := range phases {
		for _, phase := range workspacePhases {
			v := 0.0
			if current == phase {
				v = 1
			}
			ch <- prometheus.MustNewConstMetric(descPhase, prometheus.GaugeValue, v, name, phase)
		}
	}
}
