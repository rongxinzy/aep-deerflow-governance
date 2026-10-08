package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Model failover: when a digital employee's default model is unhealthy for
// long enough, the watcher re-points the employee's runtime default to the
// first healthy model in the deployment's configured fallback chain
// (AEP deployment setting `modelFallbackIds`) and restores the original once
// it has been healthy long enough. The hysteresis is derived from the
// health probe's health_since, so the watcher keeps no state of its own; the
// switch itself is recorded on the employee CR (annotation + log) so it is
// visible, auditable and manually revertible.

const failoverAnnotation = "digitalemployee.zhiyuan.io/model-failover"

type failoverState struct {
	Original   string `json:"original"`
	Active     string `json:"active"`
	SwitchedAt string `json:"switchedAt"`
}

// modelHealthView is the probe view of one catalog model.
type modelHealthView struct {
	Present      bool
	Enabled      bool
	HealthStatus string
	HealthSince  *time.Time
}

// runModelFailover watches on the configured interval; a non-positive
// interval disables the watcher.
func (p *portal) runModelFailover(ctx context.Context) {
	interval := p.cfg.failoverInterval
	if interval <= 0 {
		p.logger.Info("model failover watcher disabled")
		return
	}
	p.failoverOnce(ctx, time.Now().UTC())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.failoverOnce(ctx, time.Now().UTC())
		}
	}
}

func (p *portal) failoverOnce(ctx context.Context, now time.Time) {
	token := p.adminToken(ctx)
	if token == "" {
		return
	}
	chain, err := p.failoverChain(ctx, token)
	if err != nil {
		p.logger.Warn("model failover: deployment settings unavailable", "error", err)
		return
	}
	if len(chain) == 0 {
		return
	}
	health, err := p.modelHealth(ctx, token)
	if err != nil {
		p.logger.Warn("model failover: model catalog unavailable", "error", err)
		return
	}
	var employees dev1alpha1.DigitalEmployeeList
	if err := p.k8s.List(ctx, &employees, client.InNamespace(p.cfg.namespace)); err != nil {
		p.logger.Warn("model failover: employee list failed", "error", err)
		return
	}
	for i := range employees.Items {
		p.reconcileEmployeeFailover(ctx, &employees.Items[i], chain, health, now)
	}
}

// failoverChain reads the effective ordered fallback chain from the
// deployment settings.
func (p *portal) failoverChain(ctx context.Context, token string) ([]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.aepBaseURL+"/aep/v1/admin/deployment/settings", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-AEP-Protocol-Version", "1.0")
	response, err := serviceProbeClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deployment settings returned %d", response.StatusCode)
	}
	var document struct {
		ModelFallbackIDs struct {
			EffectiveValue []string `json:"effectiveValue"`
		} `json:"modelFallbackIds"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("deployment settings response was invalid: %w", err)
	}
	return document.ModelFallbackIDs.EffectiveValue, nil
}

// modelHealth reads the catalog with the probe state per model.
func (p *portal) modelHealth(ctx context.Context, token string) (map[string]modelHealthView, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.aepBaseURL+"/aep/v1/admin/models?limit=500", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-AEP-Protocol-Version", "1.0")
	response, err := serviceProbeClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model catalog returned %d", response.StatusCode)
	}
	var page struct {
		Models []struct {
			ID           string  `json:"id"`
			Enabled      bool    `json:"enabled"`
			HealthStatus string  `json:"healthStatus"`
			HealthSince  *string `json:"healthSince"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("model catalog response was invalid: %w", err)
	}
	health := make(map[string]modelHealthView, len(page.Models))
	for _, model := range page.Models {
		view := modelHealthView{Present: true, Enabled: model.Enabled, HealthStatus: model.HealthStatus}
		if model.HealthSince != nil {
			if parsed, err := time.Parse(time.RFC3339, *model.HealthSince); err == nil {
				view.HealthSince = &parsed
			}
		}
		health[model.ID] = view
	}
	return health, nil
}

func (p *portal) reconcileEmployeeFailover(ctx context.Context, employee *dev1alpha1.DigitalEmployee, chain []string, health map[string]modelHealthView, now time.Time) {
	models := employee.Spec.Runtime.Models
	if len(models) == 0 || models[0].Name == "" {
		return
	}
	state, hasState := parseFailoverState(employee)
	if hasState {
		if models[0].Name != state.Active {
			// The default model changed after our switch (operator edit);
			// the failover record is moot.
			clearFailoverState(employee)
			p.updateForFailover(ctx, employee, "model failover: record cleared (model changed externally)", "employee", employee.Name)
			return
		}
		original := health[state.Original]
		if original.Present && original.Enabled && original.HealthStatus == "healthy" && healthyFor(original, p.cfg.failoverMinHealthy, now) {
			route, err := p.resolveRuntimeModels(ctx, identity{}, []string{state.Original})
			if err != nil || len(route) == 0 {
				p.logger.Warn("model failover: cannot resolve the original model for restore", "employee", employee.Name, "model", state.Original, "error", err)
				return
			}
			employee.Spec.Runtime.Models = prependModel(route[0], models)
			clearFailoverState(employee)
			p.updateForFailover(ctx, employee, "model failover: restored", "employee", employee.Name, "model", state.Original)
		}
		return
	}

	primary := models[0].Name
	view := health[primary]
	eligible, reason := failoverEligible(view, p.cfg.failoverMinUnhealthy, now)
	if !eligible {
		return
	}
	fallback := ""
	for _, id := range chain {
		if id == primary {
			continue
		}
		candidate := health[id]
		if candidate.Present && candidate.Enabled && candidate.HealthStatus == "healthy" {
			fallback = id
			break
		}
	}
	if fallback == "" {
		p.logger.Warn("model failover: no healthy fallback available", "employee", employee.Name, "model", primary, "reason", reason)
		return
	}
	route, err := p.resolveRuntimeModels(ctx, identity{}, []string{fallback})
	if err != nil || len(route) == 0 {
		p.logger.Warn("model failover: cannot resolve the fallback model", "employee", employee.Name, "model", fallback, "error", err)
		return
	}
	employee.Spec.Runtime.Models = prependModel(route[0], models)
	setFailoverState(employee, failoverState{Original: primary, Active: fallback, SwitchedAt: now.Format(time.RFC3339)})
	p.updateForFailover(ctx, employee, "model failover: switched", "employee", employee.Name, "from", primary, "to", fallback, "reason", reason)
}

// failoverEligible decides whether the current default model warrants a
// switch. "unknown" means the probe has not classified the model yet. Hard
// failures — the credential is rejected (401), access is denied (403, often
// throttling), or the upstream model is gone — mean the model cannot serve
// right now, so they switch on detection and let the probe's next cycles
// decide recovery. Transport-level failures (unreachable/error) may be
// transient and must persist for the configured minimum first.
func failoverEligible(view modelHealthView, minUnhealthy time.Duration, now time.Time) (bool, string) {
	if !view.Present {
		return true, "not-in-catalog"
	}
	if !view.Enabled {
		return true, "disabled"
	}
	switch view.HealthStatus {
	case "", "unknown", "healthy":
		return false, ""
	case "credential_invalid", "denied", "model_missing":
		return true, view.HealthStatus
	}
	if view.HealthSince == nil {
		return false, ""
	}
	if now.Sub(*view.HealthSince) < minUnhealthy {
		return false, ""
	}
	return true, view.HealthStatus
}

func healthyFor(view modelHealthView, minHealthy time.Duration, now time.Time) bool {
	if view.HealthStatus != "healthy" {
		return false
	}
	if view.HealthSince == nil {
		return false
	}
	return now.Sub(*view.HealthSince) >= minHealthy
}

// prependModel puts entry first and drops any later duplicate by name,
// preserving the order of the remaining entries.
func prependModel(entry dev1alpha1.RuntimeModel, models []dev1alpha1.RuntimeModel) []dev1alpha1.RuntimeModel {
	result := make([]dev1alpha1.RuntimeModel, 0, len(models)+1)
	result = append(result, entry)
	for _, model := range models {
		if model.Name == entry.Name {
			continue
		}
		result = append(result, model)
	}
	return result
}

func parseFailoverState(employee *dev1alpha1.DigitalEmployee) (failoverState, bool) {
	raw := employee.GetAnnotations()[failoverAnnotation]
	if raw == "" {
		return failoverState{}, false
	}
	var state failoverState
	if err := json.Unmarshal([]byte(raw), &state); err != nil || state.Original == "" || state.Active == "" {
		return failoverState{}, false
	}
	return state, true
}

func setFailoverState(employee *dev1alpha1.DigitalEmployee, state failoverState) {
	annotations := employee.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	encoded, _ := json.Marshal(state)
	annotations[failoverAnnotation] = string(encoded)
	employee.SetAnnotations(annotations)
}

func clearFailoverState(employee *dev1alpha1.DigitalEmployee) {
	annotations := employee.GetAnnotations()
	if annotations == nil {
		return
	}
	delete(annotations, failoverAnnotation)
	employee.SetAnnotations(annotations)
}

func (p *portal) updateForFailover(ctx context.Context, employee *dev1alpha1.DigitalEmployee, message string, args ...any) {
	if err := p.k8s.Update(ctx, employee); err != nil {
		p.logger.Warn("model failover: employee update failed", "employee", employee.Name, "error", err)
		return
	}
	p.logger.Info(message, args...)
}
