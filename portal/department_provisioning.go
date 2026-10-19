package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Department provisioning spans three systems (AEP, OpenViking, WeKnora) and a
// partial outcome used to vanish with the request: the console saw 200 with
// `phase=partial`, the record was never stored, and there was no way to retry —
// which is how a department can sit without its memory account for months.
// This file is that record plus the store behind it.

// departmentProvision is one department's cross-system provisioning outcome.
type departmentProvision struct {
	ID            string
	Name          string
	Phase         string // ready | partial | failed
	Partials      []string
	OvAccountID   string
	WeKnoraTenant string
	Message       string
	UpdatedAt     time.Time
}

const departmentProvisionSchema = `
CREATE TABLE IF NOT EXISTS department_provisioning (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL,
	phase          TEXT NOT NULL DEFAULT 'partial' CHECK (phase IN ('ready','partial','failed')),
	partials       TEXT NOT NULL DEFAULT '[]',
	ov_account_id  TEXT NOT NULL DEFAULT '',
	weknora_tenant TEXT NOT NULL DEFAULT '',
	message        TEXT NOT NULL DEFAULT '',
	updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// provisioningStore persists provisioning outcomes. The portal uses the
// Postgres-backed implementation; tests substitute an in-memory one, and a
// portal without a usable database leaves it nil (see storeDepartmentProvision).
type provisioningStore interface {
	Save(ctx context.Context, record departmentProvision) error
	Load(ctx context.Context) (map[string]departmentProvision, error)
}

type sqlProvisioningStore struct {
	db *sql.DB
}

func (s *sqlProvisioningStore) Save(ctx context.Context, record departmentProvision) error {
	partials, err := json.Marshal(record.Partials)
	if err != nil {
		return err
	}
	if record.Phase == "" {
		record.Phase = "partial"
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO department_provisioning
			(id, name, phase, partials, ov_account_id, weknora_tenant, message, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (id) DO UPDATE SET
			name           = EXCLUDED.name,
			phase          = EXCLUDED.phase,
			partials       = EXCLUDED.partials,
			ov_account_id  = EXCLUDED.ov_account_id,
			weknora_tenant = EXCLUDED.weknora_tenant,
			message        = EXCLUDED.message,
			updated_at     = now()`,
		record.ID, record.Name, record.Phase, string(partials),
		record.OvAccountID, record.WeKnoraTenant, record.Message)
	return err
}

func (s *sqlProvisioningStore) Load(ctx context.Context) (map[string]departmentProvision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, phase, partials, ov_account_id, weknora_tenant, message, updated_at
		FROM department_provisioning`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	records := map[string]departmentProvision{}
	for rows.Next() {
		var record departmentProvision
		var partials []byte
		if err := rows.Scan(&record.ID, &record.Name, &record.Phase, &partials,
			&record.OvAccountID, &record.WeKnoraTenant, &record.Message, &record.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(partials, &record.Partials)
		records[record.ID] = record
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// memoryProvisioningStore keeps the outcome in process: the test double, and
// also what a db-less portal would get if it had no other choice.
type memoryProvisioningStore struct {
	mu      sync.Mutex
	records map[string]departmentProvision
}

func newMemoryProvisioningStore() *memoryProvisioningStore {
	return &memoryProvisioningStore{records: map[string]departmentProvision{}}
}

func (s *memoryProvisioningStore) Save(_ context.Context, record departmentProvision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.Phase == "" {
		record.Phase = "partial"
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}
	s.records[record.ID] = record
	return nil
}

func (s *memoryProvisioningStore) Load(_ context.Context) (map[string]departmentProvision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]departmentProvision, len(s.records))
	for id, record := range s.records {
		out[id] = record
	}
	return out, nil
}

// storeDepartmentProvision records the outcome so the console can show a
// partial provision and retry it. Failure to record is reported to the caller's
// log, never to the provision result itself: the systems that were contacted
// did answer, and turning a working provision into an error because the audit
// row failed would be the wrong trade.
func (p *portal) storeDepartmentProvision(ctx context.Context, record departmentProvision) error {
	if p.provisioningStore == nil {
		return nil
	}
	return p.provisioningStore.Save(ctx, record)
}

// loadDepartmentProvisioning returns every recorded outcome, or no records
// (never an error) when there is no store: the listing stays usable.
func (p *portal) loadDepartmentProvisioning(ctx context.Context) map[string]departmentProvision {
	if p.provisioningStore == nil {
		return nil
	}
	records, err := p.provisioningStore.Load(ctx)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("department provisioning records unreadable", "error", err.Error())
		}
		return nil
	}
	return records
}

// provisioningRecordFromStatus builds the row worth persisting for one
// provisioning attempt.
func provisioningRecordFromStatus(status departmentStatus, partials []string) departmentProvision {
	if partials == nil {
		partials = []string{}
	}
	return departmentProvision{
		ID:            status.ID,
		Name:          status.Name,
		Phase:         status.Phase,
		Partials:      partials,
		OvAccountID:   status.OvAccountID,
		WeKnoraTenant: status.WeKnoraTenant,
		Message:       status.Message,
		UpdatedAt:     time.Now().UTC(),
	}
}

// retryMessage is the same wording handleCreateDepartment reports, so a retried
// department and a freshly created one read identically in the console.
func retryMessage(partials []string) string {
	if len(partials) == 0 {
		return ""
	}
	return fmt.Sprintf("partially provisioned: %v", partials)
}

// handleProvisionDepartment retries the parts of a department's provisioning
// that never succeeded — and only those, which makes it idempotent: an account
// or tenant that already exists is not touched, and running it twice on a ready
// department makes no upstream call at all.
//
// A department provisioned before this record existed has no row, so its name
// comes from the AEP team listing; without that it could never be repaired,
// which is how a half-provisioned department stayed half-provisioned for
// months. Admin only, like the create call.
func (p *portal) handleProvisionDepartment(w http.ResponseWriter, r *http.Request, ident identity) {
	if !ident.isAdmin(p.cfg) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin required"})
		return
	}
	if p.provisioningStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no provisioning store configured; retry unavailable"})
		return
	}
	id := r.PathValue("id")
	adminToken := p.adminToken(r.Context())
	if adminToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "admin token unavailable"})
		return
	}
	records, err := p.provisioningStore.Load(r.Context())
	if err != nil {
		// Guessing without the stored outcome could re-provision what already
		// succeeded, so the retry refuses instead.
		if p.logger != nil {
			p.logger.Warn("department provisioning records unreadable", "error", err.Error())
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "provisioning records unavailable; retry later"})
		return
	}
	record, known := records[id]
	if !known {
		record = departmentProvision{ID: id}
	}
	// Already fully provisioned: report the stored outcome and make no upstream
	// call at all. This is what makes the endpoint safe to post twice, and it
	// also covers an upstream that answered without reporting the id it
	// created — a ready department must not be re-provisioned because of that.
	if known && record.Phase == "ready" {
		writeJSON(w, http.StatusOK, departmentStatus{
			ID:            record.ID,
			Name:          record.Name,
			AepTeamID:     record.ID,
			OvAccountID:   record.OvAccountID,
			WeKnoraTenant: record.WeKnoraTenant,
			Phase:         record.Phase,
			Message:       record.Message,
		})
		return
	}

	// The WeKnora tenant is named after the department: without a stored row the
	// name has to come from AEP rather than be invented.
	if record.Name == "" {
		teams, listErr := p.fetchTeams(r.Context(), adminToken)
		if listErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "aep returned an error listing teams"})
			return
		}
		for _, team := range teams {
			if team.ID == id {
				record.Name = team.Name
				break
			}
		}
		if record.Name == "" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("department %q not found", id)})
			return
		}
	}

	partials := []string{}
	if record.OvAccountID == "" {
		ovAccount, ovErr := p.createOpenVikingAccount(r, id)
		if ovErr != nil {
			partials = append(partials, fmt.Sprintf("openviking: %v", ovErr))
		} else {
			record.OvAccountID = ovAccount
		}
	}
	if p.cfg.knowledgeAPIURL != "" && record.WeKnoraTenant == "" {
		tenant, tenantErr := p.createWeKnoraTenant(r, departmentRequest{Name: record.Name})
		if tenantErr != nil {
			partials = append(partials, fmt.Sprintf("weknora: %v", tenantErr))
		} else {
			record.WeKnoraTenant = tenant
		}
	}

	phase := "ready"
	if len(partials) > 0 {
		phase = "partial"
	}
	record.Phase = phase
	record.Partials = partials
	record.Message = retryMessage(partials)
	record.UpdatedAt = time.Now().UTC()
	if saveErr := p.provisioningStore.Save(r.Context(), record); saveErr != nil && p.logger != nil {
		p.logger.Warn("department provisioning outcome not recorded", "id", id, "error", saveErr.Error())
	}

	writeJSON(w, http.StatusOK, departmentStatus{
		ID:            id,
		Name:          record.Name,
		AepTeamID:     id,
		OvAccountID:   record.OvAccountID,
		WeKnoraTenant: record.WeKnoraTenant,
		Phase:         phase,
		Message:       record.Message,
	})
}
