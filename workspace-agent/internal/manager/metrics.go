// Prometheus /metrics for the node agent. A private registry fed from a live
// supervisor snapshot on every scrape: no global registration (tests can
// build several managers) and no stale values between scrapes.
package manager

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
		"Workspace CR phase (1 = current phase).",
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
	phase string
	fails int
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	c.m.mu.Lock()
	snaps := make(map[string]supSnapshot, len(c.m.supers))
	alive := 0
	for name, s := range c.m.supers {
		if s.driver.Alive() {
			alive++
		}
		snaps[name] = supSnapshot{phase: s.phase, fails: s.bootFails}
	}
	total := len(c.m.supers)
	errs := c.m.resyncErrors.Load()
	c.m.mu.Unlock()

	ch <- prometheus.MustNewConstMetric(descWorkspaces, prometheus.GaugeValue, float64(total))
	ch <- prometheus.MustNewConstMetric(descVMs, prometheus.GaugeValue, float64(alive))
	ch <- prometheus.MustNewConstMetric(descResyncErrors, prometheus.CounterValue, float64(errs))
	for name, s := range snaps {
		ch <- prometheus.MustNewConstMetric(descBootFails, prometheus.GaugeValue, float64(s.fails), name)
		for _, phase := range workspacePhases {
			v := 0.0
			if s.phase == phase {
				v = 1
			}
			ch <- prometheus.MustNewConstMetric(descPhase, prometheus.GaugeValue, v, name, phase)
		}
	}
}
