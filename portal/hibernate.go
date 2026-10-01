package main

import (
	"context"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// Hibernation support (portal side). Idle employees run at zero replicas
// (KEDA watches this database); the portal is the wake path for browser
// chat: mark activity, drop a wake row, and wait out the cold start.

// markActivity records that someone just talked to this employee. KEDA's
// activity signal reads this table, so an ongoing conversation keeps the
// runtime resident without any explicit lease. Best-effort: a failed upsert
// only costs a slightly stale idle signal.
func (p *portal) markActivity(employee string) {
	if p.db == nil {
		return
	}
	if _, err := p.db.Exec(
		`INSERT INTO employee_activity (employee, last_active) VALUES ($1, now())
		 ON CONFLICT (employee) DO UPDATE SET last_active = now()`, employee); err != nil {
		p.logger.Warn("activity upsert failed", "employee", employee, "error", err)
	}
}

// wakeRuntime brings a scaled-to-zero employee back. Every wake signal
// (chat, channel message, console preview) lands as a row in channel_wake;
// KEDA polls it and raises the Deployment. The portal then waits for
// readiness so the caller never sees a dead upstream mid-handshake.
func (p *portal) wakeRuntime(ctx context.Context, employee string) (int, string) {
	var deployment appsv1.Deployment
	err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: "de-" + employee}, &deployment)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// No deployment to watch (not KEDA-managed, or already gone):
			// fall through and let the proxy surface the real upstream error.
			return 0, ""
		}
		return http.StatusInternalServerError, err.Error()
	}
	if deployment.Status.ReadyReplicas > 0 {
		return 0, ""
	}
	if p.db != nil {
		if _, err := p.db.Exec(`INSERT INTO channel_wake (employee) VALUES ($1)`, employee); err != nil {
			p.logger.Warn("wake row insert failed", "employee", employee, "error", err)
		}
	}
	deadline := time.Now().Add(p.cfg.wakeTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return http.StatusGatewayTimeout, "request cancelled while waking employee"
		case <-time.After(time.Second):
		}
		if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: "de-" + employee}, &deployment); err != nil {
			return http.StatusInternalServerError, err.Error()
		}
		if deployment.Status.ReadyReplicas > 0 {
			p.logger.Info("employee woke", "employee", employee)
			return 0, ""
		}
	}
	return http.StatusServiceUnavailable, "employee is waking up but did not become ready in time (try again shortly)"
}

// hibernating reports whether the employee opts into scale-to-zero (the
// portal only waits-and-wakes when KEDA can actually answer).
func hibernating(item *dev1alpha1.DigitalEmployee) bool {
	return item.Spec.Hibernation != nil && item.Spec.Hibernation.Enabled != nil && *item.Spec.Hibernation.Enabled
}
