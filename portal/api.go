package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

type portal struct {
	cfg    config
	db     *sql.DB
	k8s    client.Client
	aep    *aepAuthenticator
	logger *slog.Logger
	// chatBaseOverride is test-only: replaces the in-cluster runtime URL.
	chatBaseOverride string
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)

func (p *portal) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /api/v1/employees", p.auth(p.applyForEmployee))
	mux.HandleFunc("GET /api/v1/employees", p.auth(p.listEmployees))
	mux.HandleFunc("GET /api/v1/employees/{name}", p.auth(p.getEmployee))
	mux.HandleFunc("DELETE /api/v1/employees/{name}", p.auth(p.deleteEmployee))
	mux.HandleFunc("GET /api/v1/requests", p.auth(p.listRequests))
	mux.HandleFunc("POST /api/v1/requests/{id}/approve", p.auth(p.adminOnly(p.decideRequest("approved"))))
	mux.HandleFunc("POST /api/v1/requests/{id}/reject", p.auth(p.adminOnly(p.decideRequest("rejected"))))
	// Web chat: browser (AEP session) -> portal -> employee runtime.
	mux.HandleFunc("/api/v1/employees/{name}/chat/", p.auth(p.handleChat))
	mux.HandleFunc("/api/v1/employees/{name}/chat", p.auth(p.handleChat))
	return mux
}

func (p *portal) auth(next func(http.ResponseWriter, *http.Request, identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
			return
		}
		ident, err := p.aep.verify(r.Context(), token)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}
		next(w, r, ident)
	}
}

func (p *portal) adminOnly(next func(http.ResponseWriter, *http.Request, identity)) func(http.ResponseWriter, *http.Request, identity) {
	return func(w http.ResponseWriter, r *http.Request, ident identity) {
		if !ident.isAdmin(p.cfg) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "administrator role required"})
			return
		}
		next(w, r, ident)
	}
}

type applyRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

func (p *portal) applyForEmployee(w http.ResponseWriter, r *http.Request, ident identity) {
	var body applyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	body.Name = strings.ToLower(strings.TrimSpace(body.Name))
	if !namePattern.MatchString(body.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name must be lowercase letters/digits/hyphens (3-32 chars, letter开头)"})
		return
	}
	if body.DisplayName == "" {
		body.DisplayName = body.Name
	}

	// Duplicate guard: existing CR or pending request with the same name.
	var existing dev1alpha1.DigitalEmployee
	err := p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: body.Name}, &existing)
	if err == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("digital employee %q already exists (phase %s)", body.Name, existing.Status.Phase)})
		return
	} else if !apierrors.IsNotFound(err) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var pending string
	if err := p.db.QueryRowContext(r.Context(), `SELECT id FROM requests WHERE employee_name=$1 AND state='pending'`, body.Name).Scan(&pending); err == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("a pending request (%s) already exists for %q", pending, body.Name)})
		return
	}

	quotaUsed := p.countOwned(r.Context(), ident.UserID) + p.countPending(r.Context(), ident.UserID)
	decisionResult := evaluatePolicy(p.cfg.policyMode, ident.isAdmin(p.cfg), ident.Kind == "human", quotaUsed, p.cfg.quotaPerUser)
	if !decisionResult.Create && !decisionResult.NeedsApproval {
		writeJSON(w, decisionResult.HTTPStatus, map[string]string{"error": decisionResult.Reason})
		return
	}

	spec := p.renderSpec(body.Name, body.DisplayName, ident)
	if decisionResult.Create {
		created, err := p.createEmployee(r.Context(), spec)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		p.record(r.Context(), spec, "deployed", ident, ident, "")
		p.logger.Info("employee created (direct)", "name", body.Name, "owner", ident.UserID, "policy", p.cfg.policyMode)
		writeJSON(w, http.StatusCreated, map[string]any{"employee": summarize(created), "policy": decisionResult.Reason})
		return
	}

	id := newRequestID()
	specJSON, _ := json.Marshal(spec)
	if _, err := p.db.ExecContext(r.Context(),
		`INSERT INTO requests (id, employee_name, owner_id, owner_name, display_name, spec_json, state) VALUES ($1,$2,$3,$4,$5,$6,'pending')`,
		id, body.Name, ident.UserID, ident.DisplayName, body.DisplayName, string(specJSON)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	p.logger.Info("employee request parked", "request", id, "name", body.Name, "owner", ident.UserID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"request": map[string]any{"id": id, "state": "pending", "employeeName": body.Name},
		"policy":  decisionResult.Reason,
	})
}

func (p *portal) renderSpec(name, displayName string, owner identity) dev1alpha1.DigitalEmployee {
	return dev1alpha1.DigitalEmployee{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.cfg.namespace},
		Spec: dev1alpha1.DigitalEmployeeSpec{
			DisplayName: displayName,
			Runtime:     dev1alpha1.RuntimeSpec{Type: "deerflow", Image: p.cfg.runtimeImage, Model: p.cfg.model},
			Memory: &dev1alpha1.MemorySpec{
				Type: "openviking", ServerURL: p.cfg.memoryServer, AccountID: p.cfg.memoryAccount,
				SingleTenantInstance: true,
			},
			Knowledge:   &dev1alpha1.KnowledgeSpec{Type: "weknora", MCPURL: p.cfg.knowledgeMCPURL},
			Governance:  dev1alpha1.GovernanceSpec{
				// The employee acts with its owner's data scope by default.
				AEPBaseURL:     p.cfg.aepBaseURL,
				DeploymentID:   "demo",
				DefaultAEPUser: owner.UserID,
			},
			Credentials: dev1alpha1.CredentialsRef{Name: p.cfg.credentialsSecret},
			A2A:         &dev1alpha1.A2ASpec{Image: p.cfg.a2aImage, MaxDepth: 3},
			Owner:       owner.UserID,
			OwnerName:   identUsername(owner),
		},
	}
}

func identUsername(ident identity) string {
	if ident.DisplayName != "" {
		return ident.DisplayName
	}
	return ident.UserID
}

func (p *portal) createEmployee(ctx context.Context, spec dev1alpha1.DigitalEmployee) (*dev1alpha1.DigitalEmployee, error) {
	if err := p.k8s.Create(ctx, &spec); err != nil {
		return nil, err
	}
	created := spec
	return &created, nil
}

func (p *portal) countOwned(ctx context.Context, ownerID string) int {
	var list dev1alpha1.DigitalEmployeeList
	if err := p.k8s.List(ctx, &list, client.InNamespace(p.cfg.namespace)); err != nil {
		return 0
	}
	count := 0
	for i := range list.Items {
		if list.Items[i].Spec.Owner == ownerID {
			count++
		}
	}
	return count
}

func (p *portal) countPending(ctx context.Context, ownerID string) int {
	var count int
	_ = p.db.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE owner_id=$1 AND state='pending'`, ownerID).Scan(&count)
	return count
}

func (p *portal) listEmployees(w http.ResponseWriter, r *http.Request, ident identity) {
	var list dev1alpha1.DigitalEmployeeList
	if err := p.k8s.List(r.Context(), &list, client.InNamespace(p.cfg.namespace)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	admin := ident.isAdmin(p.cfg)
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		if !admin && item.Spec.Owner != ident.UserID {
			continue
		}
		out = append(out, summarize(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"employees": out})
}

func (p *portal) getEmployee(w http.ResponseWriter, r *http.Request, ident identity) {
	var item dev1alpha1.DigitalEmployee
	if err := p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: r.PathValue("name")}, &item); err != nil {
		writeJSON(w, statusOf(err), map[string]string{"error": err.Error()})
		return
	}
	if !ident.isAdmin(p.cfg) && item.Spec.Owner != ident.UserID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not the owner"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"employee": summarize(&item)})
}

func (p *portal) deleteEmployee(w http.ResponseWriter, r *http.Request, ident identity) {
	name := r.PathValue("name")
	var item dev1alpha1.DigitalEmployee
	if err := p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: name}, &item); err != nil {
		writeJSON(w, statusOf(err), map[string]string{"error": err.Error()})
		return
	}
	if !ident.isAdmin(p.cfg) && item.Spec.Owner != ident.UserID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only the owner or an administrator may delete this digital employee"})
		return
	}
	if err := p.k8s.Delete(r.Context(), &item); err != nil {
		writeJSON(w, statusOf(err), map[string]string{"error": err.Error()})
		return
	}
	p.logger.Info("employee deleted", "name", name, "by", ident.UserID)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
}

func (p *portal) listRequests(w http.ResponseWriter, r *http.Request, ident identity) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "pending"
	}
	admin := ident.isAdmin(p.cfg)
	rows, err := p.db.QueryContext(r.Context(),
		`SELECT id, employee_name, owner_id, owner_name, display_name, state, decision_reason, created_at FROM requests WHERE state=$1 ORDER BY created_at DESC LIMIT 100`, state)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, ownerID, ownerName, displayName, requestState, reason string
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &ownerID, &ownerName, &displayName, &requestState, &reason, &createdAt); err != nil {
			continue
		}
		if !admin && ownerID != ident.UserID {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "employeeName": name, "owner": ownerName, "displayName": displayName,
			"state": requestState, "reason": reason, "createdAt": createdAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

func (p *portal) decideRequest(finalState string) func(http.ResponseWriter, *http.Request, identity) {
	return func(w http.ResponseWriter, r *http.Request, ident identity) {
		id := r.PathValue("id")
		var name, specJSON string
		err := p.db.QueryRowContext(r.Context(), `SELECT employee_name, spec_json FROM requests WHERE id=$1 AND state='pending'`, id).Scan(&name, &specJSON)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no pending request " + id})
			return
		} else if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		var reason string
		if finalState == "rejected" {
			var body struct {
				Reason string `json:"reason"`
			}
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
			reason = body.Reason
		}
		if finalState == "approved" {
			var spec dev1alpha1.DigitalEmployee
			if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "stored spec corrupt: " + err.Error()})
				return
			}
			spec.ResourceVersion = ""
			if _, err := p.createEmployee(r.Context(), spec); err != nil && !apierrors.IsAlreadyExists(err) {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		}
		if _, err := p.db.ExecContext(r.Context(),
			`UPDATE requests SET state=$1, decision_reason=$2, decided_by=$3, decided_at=now() WHERE id=$4`,
			finalState, reason, ident.UserID, id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		p.logger.Info("request decided", "request", id, "state", finalState, "by", ident.UserID)
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "state": finalState})
	}
}

func (p *portal) record(ctx context.Context, spec dev1alpha1.DigitalEmployee, state string, owner, decider identity, reason string) {
	specJSON, _ := json.Marshal(spec)
	_, _ = p.db.ExecContext(ctx,
		`INSERT INTO requests (id, employee_name, owner_id, owner_name, display_name, spec_json, state, decision_reason, decided_by, decided_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now()) ON CONFLICT (id) DO NOTHING`,
		newRequestID(), spec.Name, owner.UserID, identUsername(owner), spec.Spec.DisplayName, string(specJSON), state, reason, decider.UserID)
}

func summarize(item *dev1alpha1.DigitalEmployee) map[string]any {
	return map[string]any{
		"name": item.Name, "displayName": item.Spec.DisplayName, "phase": item.Status.Phase,
		"runtime": item.Spec.Runtime.Type, "model": item.Spec.Runtime.Model,
		"owner": item.Spec.OwnerName, "ownerId": item.Spec.Owner,
		"memoryUser": item.Status.MemoryUser, "createdAt": item.CreationTimestamp,
	}
}

func newRequestID() string {
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

func statusOf(err error) int {
	if apierrors.IsNotFound(err) {
		return http.StatusNotFound
	}
	if apierrors.IsAlreadyExists(err) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
