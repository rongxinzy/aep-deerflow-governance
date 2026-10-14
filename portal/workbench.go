package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// The workbench read surface: the employee-facing companion to the admin
// console. Unlike the platform-wide admin probes, these handlers serve
// data scoped to the caller (own profile and quota, own requests). The
// approval leg is read straight from the requests table; the deployment
// leg is derived from the employee CR's phase so 审批状态 and 部署状态
// stay separately visible, as the wireframes require.

// workbenchTeam is one of the caller's teams in /me: the parentId link is
// what lets the workbench org tree place the node inside a department
// hierarchy instead of always at the root.
type workbenchTeam struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	ParentID *string `json:"parentId,omitempty"`
}

// handleMe reports the caller's workbench profile: identity, team names,
// and quota. /me is the workbench first paint, so every enrichable lookup
// degrades instead of failing the whole response.
func (p *portal) handleMe(w http.ResponseWriter, r *http.Request, ident identity) {
	resolver := newReferenceResolver(p, r.Context())
	teams := make([]workbenchTeam, 0, len(ident.Teams))
	for _, id := range ident.Teams {
		team := workbenchTeam{ID: id}
		if resolved, found, err := resolver.lookupTeam(id); err != nil {
			// A name lookup failure must not blank the workbench: the id
			// still renders and the next request retries.
			p.logger.Warn("workbench team lookup failed", "team", id, "error", err)
		} else if found {
			team.Name = resolved.Name
			team.ParentID = resolved.ParentID
		}
		teams = append(teams, team)
	}
	// Same quota formula as the apply gate (applyForEmployee): owned CRs
	// plus parked requests, so "配额已满" from the server and the number
	// shown by the workbench can never drift apart.
	owned := p.countOwned(r.Context(), ident.UserID)
	pending := p.countPending(r.Context(), ident.UserID)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":         map[string]string{"id": ident.UserID, "displayName": identUsername(ident), "kind": ident.Kind},
		"teams":        teams,
		"quota":        map[string]int{"limit": p.cfg.quotaPerUser, "used": owned + pending, "owned": owned, "pending": pending},
		"policyMode":   p.cfg.policyMode,
		"defaultModel": p.cfg.model,
	})
}

// handleMyRequests lists the caller's own requests across every state
// (pending/approved/rejected/deployed). Administrators see their own rows
// too — the platform-wide approval queue stays on GET /api/v1/requests.
func (p *portal) handleMyRequests(w http.ResponseWriter, r *http.Request, ident identity) {
	state := r.URL.Query().Get("state")
	switch state {
	case "", "all":
		state = ""
	case "pending", "approved", "rejected", "deployed":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown state filter " + state})
		return
	}
	query := `SELECT id, employee_name, owner_id, owner_name, display_name, state, decision_reason, created_at
		FROM requests WHERE owner_id=$1`
	args := []any{ident.UserID}
	if state != "" {
		query += ` AND state=$2`
		args = append(args, state)
	}
	query += ` ORDER BY created_at DESC LIMIT 100`
	rows, err := p.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer func() { _ = rows.Close() }()

	// One List resolves the deploy leg for every row (the list shows
	// 部署状态 per row; per-row Gets would be N+1).
	phaseByName := map[string]dev1alpha1.DigitalEmployeePhase{}
	var employees dev1alpha1.DigitalEmployeeList
	if err := p.k8s.List(r.Context(), &employees, client.InNamespace(p.cfg.namespace)); err == nil {
		for i := range employees.Items {
			phaseByName[employees.Items[i].Name] = employees.Items[i].Status.Phase
		}
	}

	out := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, ownerID, ownerName, displayName, requestState, reason string
		var createdAt time.Time
		if err := rows.Scan(&id, &name, &ownerID, &ownerName, &displayName, &requestState, &reason, &createdAt); err != nil {
			continue
		}
		deploy := map[string]any{"exists": false, "phase": ""}
		if requestState == "approved" || requestState == "deployed" {
			phase, ok := phaseByName[name]
			deploy["exists"] = ok
			deploy["phase"] = string(phase)
		}
		out = append(out, map[string]any{
			"id": id, "employeeName": name, "owner": ownerName, "displayName": displayName,
			"state": requestState, "reason": reason, "createdAt": createdAt, "deploy": deploy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

// handleRequestDetail returns one request with everything the detail page
// shows: the decoded spec snapshot (description/team/note/model) and the
// separate deployment view. Non-owners get 404 — request ids are
// enumerable (req-<timestamp>), so "exists but not yours" must not be
// distinguishable from "missing".
func (p *portal) handleRequestDetail(w http.ResponseWriter, r *http.Request, ident identity) {
	id := r.PathValue("id")
	var (
		employeeName, ownerID, ownerName, displayName, specJSON, state, reason, decidedBy string
		createdAt                                                                         time.Time
		decidedAt                                                                         sql.NullTime
	)
	err := p.db.QueryRowContext(r.Context(),
		`SELECT employee_name, owner_id, owner_name, display_name, spec_json, state, decision_reason, decided_by, created_at, decided_at
		 FROM requests WHERE id=$1`,
		id).Scan(&employeeName, &ownerID, &ownerName, &displayName, &specJSON, &state, &reason, &decidedBy, &createdAt, &decidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "request " + id + " not found"})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ident.isAdmin(p.cfg) && ownerID != ident.UserID {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "request " + id + " not found"})
		return
	}

	var spec dev1alpha1.DigitalEmployee
	// Best-effort: pre-spec rows carry '{}'.
	_ = json.Unmarshal([]byte(specJSON), &spec)

	// Name resolution is display sugar: failures leave ids in place.
	resolver := newReferenceResolver(p, r.Context())
	teamName := ""
	if spec.Spec.Team != "" {
		if team, found, err := resolver.lookupTeam(spec.Spec.Team); err == nil && found {
			teamName = team.Name
		}
	}
	decidedByName := ""
	if decidedBy != "" {
		if user, found, err := resolver.lookupUser(decidedBy); err == nil && found {
			decidedByName = user.DisplayName
			if decidedByName == "" {
				decidedByName = user.Username
			}
		}
	}
	// Mirror summarize's default rule: an explicit list's first entry is
	// the runtime default.
	model := spec.Spec.Runtime.Model
	if model == "" && len(spec.Spec.Runtime.Models) > 0 {
		model = spec.Spec.Runtime.Models[0].Name
	}

	detail := map[string]any{
		"id": id, "employeeName": employeeName, "displayName": displayName,
		"ownerId": ownerID, "owner": ownerName,
		"state": state, "reason": reason,
		"createdAt":   createdAt,
		"description": spec.Spec.Description,
		"teamId":      spec.Spec.Team, "teamName": teamName,
		"note":      spec.Annotations[applyNoteAnnotation],
		"model":     model,
		"decidedBy": decidedBy, "decidedByName": decidedByName,
		"deploy": p.requestDeployView(r.Context(), employeeName, state),
	}
	if decidedAt.Valid {
		detail["decidedAt"] = decidedAt.Time
	} else {
		detail["decidedAt"] = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": detail})
}

// requestDeployView is the deployment leg of one request. Pending and
// rejected requests never probe the cluster; an approved request whose
// employee is gone reports exists=false (the deployment record is gone).
func (p *portal) requestDeployView(ctx context.Context, employeeName, state string) map[string]any {
	view := map[string]any{"exists": false, "phase": "", "message": ""}
	if state != "approved" && state != "deployed" {
		return view
	}
	var item dev1alpha1.DigitalEmployee
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: employeeName}, &item); err != nil {
		return view
	}
	view["exists"] = true
	view["phase"] = string(item.Status.Phase)
	view["message"] = item.Status.Message
	view["createdAt"] = item.CreationTimestamp.Time
	return view
}
