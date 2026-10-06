package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	cfg      config
	db       *sql.DB
	k8s      client.Client
	aep      *aepAuthenticator
	logger   *slog.Logger
	sessions *sessionStore
	// chatBaseOverride is test-only: replaces the in-cluster runtime URL.
	chatBaseOverride string
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)

func (p *portal) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /api/v1/employees", p.auth(p.applyForEmployee))
	mux.HandleFunc("GET /api/v1/employees", p.auth(p.listEmployees))
	mux.HandleFunc("PATCH /api/v1/employees/{name}", p.auth(p.updateEmployee))
	// Sidebar service-status probes for the admin console (browser never
	// sees the OpenViking root key — the portal holds it server-side).
	mux.HandleFunc("GET /api/v1/departments", p.auth(p.handleListDepartments))
	mux.HandleFunc("GET /api/v1/usage/stats", p.auth(p.handleUsageStats))
	mux.HandleFunc("PATCH /api/v1/departments/{id}", p.auth(p.handleUpdateDepartment))
	mux.HandleFunc("DELETE /api/v1/departments/{id}", p.auth(p.handleDeleteDepartment))
	mux.HandleFunc("GET /api/v1/departments/{id}/members", p.auth(p.handleDepartmentMembers))
	mux.HandleFunc("PUT /api/v1/departments/{id}/members", p.auth(p.handleDepartmentMembers))
	mux.HandleFunc("POST /api/v1/departments", p.auth(p.handleCreateDepartment))
	mux.HandleFunc("GET /api/v1/memory/status", p.auth(p.handleMemoryStatus))
	mux.HandleFunc("POST /api/v1/memory/search", p.auth(p.handleMemorySearch))
	mux.HandleFunc("GET /api/v1/knowledge/status", p.auth(p.handleKnowledgeStatus))
	mux.HandleFunc("/api/v1/knowledge/proxy/{rest...}", p.auth(p.handleKnowledgeProxy))
	mux.HandleFunc("GET /api/v1/knowledge/bases", p.auth(p.handleKnowledgeListBases))
	mux.HandleFunc("GET /api/v1/employees/{name}", p.auth(p.getEmployee))
	mux.HandleFunc("DELETE /api/v1/employees/{name}", p.auth(p.deleteEmployee))
	mux.HandleFunc("GET /api/v1/requests", p.auth(p.listRequests))
	mux.HandleFunc("POST /api/v1/requests/{id}/approve", p.auth(p.adminOnly(p.decideRequest("approved"))))
	mux.HandleFunc("POST /api/v1/requests/{id}/reject", p.auth(p.adminOnly(p.decideRequest("rejected"))))
	// Web chat: browser (AEP session) -> portal -> employee runtime.
	// {rest...} subtree wildcard maps onto the runtime's /api/{rest...}.
	mux.HandleFunc("/api/v1/employees/{name}/chat/{rest...}", p.auth(p.handleChat))
	mux.HandleFunc("/api/v1/employees/{name}/chat", p.auth(p.handleChat))
	// Portal session lifecycle + the /chat entry page (login + employee
	// picker) for the reused DeerFlow frontend.
	mux.HandleFunc("POST /api/v1/session", p.handleSessionLogin)
	mux.HandleFunc("GET /api/v1/session", p.auth(p.handleSessionWhoami))
	mux.HandleFunc("DELETE /api/v1/session", p.handleSessionLogout)
	mux.HandleFunc("POST /api/v1/session/employee", p.auth(p.handleSessionEmployee))
	mux.HandleFunc("GET /chat", p.handleChatEntry)
	return mux
}

// auth gates a handler behind either an AEP Bearer token or a portal session
// cookie (see authenticate for the CSRF rule).
func (p *portal) auth(next func(http.ResponseWriter, *http.Request, identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ident, status, ok := p.authenticate(r)
		if !ok {
			message := "missing bearer token or session"
			if status == http.StatusForbidden {
				message = "csrf token missing or mismatched"
			}
			writeJSON(w, status, map[string]string{"error": message})
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

type skillInput struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type visibilityInput struct {
	Mode  string   `json:"mode"`  // all | restricted
	Teams []string `json:"teams"` // AEP team ids
	Users []string `json:"users"` // AEP user ids
}

type applyRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	// Description (用途说明) is a required release condition (≤500 runes).
	Description string `json:"description"`
	// Team is the owning department; required for new employees.
	Team string `json:"team"`
	// Models are AEP model ids, first = default; empty = PORTAL_MODEL.
	Models []string `json:"models"`
	// Owner optionally designates another AEP user as the responsible
	// owner; empty means the caller. Setting another user requires the
	// administrator role (enforced in the handler).
	Owner string `json:"owner"`
	// KnowledgeBases whitelists WeKnora knowledge bases for this
	// employee's retrieval. nil keeps the legacy unrestricted credential;
	// an explicitly empty list denies every knowledge search.
	KnowledgeBases []string `json:"knowledgeBases"`
	// Skills pin enabled AEP skills (with published versions) to register
	// on the employee.
	Skills []skillInput `json:"skills"`
	// Visibility is the explicit open scope (who may see and converse).
	// nil keeps the legacy owner/team rules; the console always sends it.
	Visibility *visibilityInput `json:"visibility"`
}

// updateRequest is the PATCH body: present fields are replaced (after the
// same validation as apply), absent fields keep the employee's current
// configuration. Slices distinguish absent (nil) from an explicitly empty
// list — empty knowledgeBases hardens to deny-all, empty skills unregisters
// every skill, while an empty models list is a release-condition violation.
// There is deliberately no way back to the legacy unrestricted knowledge
// policy: once explicit, an employee stays explicit.
type updateRequest struct {
	DisplayName    *string          `json:"displayName"`
	Description    *string          `json:"description"`
	Team           *string          `json:"team"`
	Owner          *string          `json:"owner"`
	Models         []string         `json:"models"`
	KnowledgeBases []string         `json:"knowledgeBases"`
	Skills         []skillInput     `json:"skills"`
	Visibility     *visibilityInput `json:"visibility"`
}

func (p *portal) applyForEmployee(w http.ResponseWriter, r *http.Request, ident identity) {
	var body applyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	body.Name = normalizeEmployeeName(body.Name)
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

	// Owner override is an administrator-only action: a regular applicant
	// always owns what they apply for.
	if body.Owner != "" && body.Owner != ident.UserID && !ident.isAdmin(p.cfg) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "只有管理员可以指定其他负责人"})
		return
	}
	// Opening an employee to everyone must stay an explicit, reviewed
	// choice: administrators create directly, approval mode parks the
	// request for an administrator to review, self-service is refused.
	if body.Visibility != nil && body.Visibility.Mode == "all" && !ident.isAdmin(p.cfg) && p.cfg.policyMode != "approval" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "全员开放需要管理员创建或经审批创建"})
		return
	}

	quotaUsed := p.countOwned(r.Context(), ident.UserID) + p.countPending(r.Context(), ident.UserID)
	decisionResult := evaluatePolicy(p.cfg.policyMode, ident.isAdmin(p.cfg), ident.Kind == "human", quotaUsed, p.cfg.quotaPerUser)
	if !decisionResult.Create && !decisionResult.NeedsApproval {
		writeJSON(w, decisionResult.HTTPStatus, map[string]string{"error": decisionResult.Reason})
		return
	}

	runtimeModels, err := p.resolveRuntimeModels(r.Context(), ident, body.Models)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resolved, violations, err := p.validateApply(r.Context(), ident, body)
	if err != nil {
		// Fail closed: never create an employee whose references were not
		// checked because an upstream was down.
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "发布条件校验依赖的服务暂不可用：" + err.Error()})
		return
	}
	if len(body.Models) == 0 && p.cfg.model == "" {
		violations = append(violations, violation{Field: "models", Message: "使用模型为必填发布条件"})
	}
	if len(violations) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":      fmt.Sprintf("发布条件校验未通过（%d 项）", len(violations)),
			"violations": violations,
		})
		return
	}
	spec := p.renderSpec(body, resolved, runtimeModels)
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

func (p *portal) renderSpec(body applyRequest, resolved applyContext, models []dev1alpha1.RuntimeModel) dev1alpha1.DigitalEmployee {
	runtime := dev1alpha1.RuntimeSpec{Type: "deerflow", Image: p.cfg.runtimeImage, Model: p.cfg.model}
	if len(models) > 0 {
		// Explicit model selection from the AEP catalog: the first entry is
		// the runtime default, later entries are name-selectable.
		runtime = dev1alpha1.RuntimeSpec{Type: "deerflow", Image: p.cfg.runtimeImage, Models: models}
	}
	return dev1alpha1.DigitalEmployee{
		ObjectMeta: metav1.ObjectMeta{Name: body.Name, Namespace: p.cfg.namespace},
		Spec: dev1alpha1.DigitalEmployeeSpec{
			DisplayName: body.DisplayName,
			Description: body.Description,
			Team:        body.Team,
			Runtime:     runtime,
			// Conversation threads ride the central postgres instance
			// (one database per employee — the operator renders the DSN
			// child Secret). The operator-level sqlite default is for
			// standalone use; portal employees must be inside the backup
			// and central-ops perimeter.
			ConversationStore: dev1alpha1.ConversationStoreSpec{Backend: "postgres"},
			Memory: &dev1alpha1.MemorySpec{
				Type: "openviking", ServerURL: p.cfg.memoryServer, AccountID: p.cfg.memoryAccount,
				SingleTenantInstance: true,
			},
			// resolved.KBs is nil for legacy requests (unrestricted) and
			// non-nil otherwise (empty = deny every knowledge search).
			Knowledge: &dev1alpha1.KnowledgeSpec{Type: "weknora", MCPURL: p.cfg.knowledgeMCPURL, Bases: resolved.KBs},
			Governance: dev1alpha1.GovernanceSpec{
				// The employee acts with its owner's data scope by default.
				AEPBaseURL:     p.cfg.aepBaseURL,
				DeploymentID:   "demo",
				DefaultAEPUser: resolved.OwnerID,
			},
			Credentials: dev1alpha1.CredentialsRef{Name: p.cfg.credentialsSecret},
			A2A:         &dev1alpha1.A2ASpec{Image: p.cfg.a2aImage, MaxDepth: 3},
			Owner:       resolved.OwnerID,
			OwnerName:   resolved.OwnerName,
			Skills:      resolved.Skills,
			Visibility:  resolved.Visibility,
		},
	}
}

// catalogModel mirrors the fields the portal derives runtime routes from.
type catalogModel struct {
	ID            string `json:"id"`
	SourceType    string `json:"sourceType"`
	Protocol      string `json:"protocol"`
	Endpoint      string `json:"endpoint"`
	UpstreamModel string `json:"upstreamModel"`
	Enabled       bool   `json:"enabled"`
}

// resolveRuntimeModels maps requested AEP model ids onto runtime routes by
// consulting the model catalog with the requester's own token (the console
// admin already holds models.read). anthropic models ride the per-model
// gateway prefix; openai-compatible models use their registered endpoint.
func (p *portal) resolveRuntimeModels(ctx context.Context, ident identity, modelIDs []string) ([]dev1alpha1.RuntimeModel, error) {
	if len(modelIDs) == 0 {
		return nil, nil
	}
	// The catalog is platform-wide reference data; regular applicants do
	// not hold models.read, so their token gets 403 — retry once with the
	// portal admin token instead of failing their apply. Model USE stays
	// gated by AEP model assignments; this only resolves route metadata.
	fetch := func(authorization string) ([]byte, int, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.aepBaseURL+"/aep/v1/admin/models?limit=500", nil)
		if err != nil {
			return nil, 0, err
		}
		request.Header.Set("Authorization", authorization)
		request.Header.Set("X-AEP-Protocol-Version", "1.0")
		response, err := serviceProbeClient.Do(request)
		if err != nil {
			return nil, 0, fmt.Errorf("model catalog unavailable: %w", err)
		}
		defer func() { _ = response.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return raw, response.StatusCode, nil
	}
	raw, status, err := fetch("Bearer " + ident.Token)
	if err != nil {
		return nil, err
	}
	if status == http.StatusForbidden || status == http.StatusUnauthorized {
		if admin := p.adminToken(ctx); admin != "" {
			if raw, status, err = fetch("Bearer " + admin); err != nil {
				return nil, err
			}
		}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("model catalog returned %d: %s", status, strings.TrimSpace(string(raw)))
	}
	var page struct {
		Models []catalogModel `json:"models"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("model catalog response was invalid: %w", err)
	}
	byID := make(map[string]catalogModel, len(page.Models))
	for _, model := range page.Models {
		byID[model.ID] = model
	}
	routes := make([]dev1alpha1.RuntimeModel, 0, len(modelIDs))
	seen := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		model, ok := byID[id]
		if !ok || !model.Enabled || model.SourceType != "gateway" {
			return nil, fmt.Errorf("model %q is not an enabled gateway model", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		route := dev1alpha1.RuntimeModel{Name: model.ID, NativeModel: model.UpstreamModel, ApiKeyEnv: p.cfg.modelKeyEnv}
		switch model.Protocol {
		case "anthropic":
			route.Provider = "anthropic"
			route.BaseURL = p.cfg.gatewayBaseURL + "/" + model.ID
		case "openai-compatible":
			route.Provider = "openai"
			route.BaseURL = model.Endpoint
		default:
			return nil, fmt.Errorf("model %q uses unsupported protocol %q", id, model.Protocol)
		}
		if route.BaseURL == "" {
			return nil, fmt.Errorf("model %q has no endpoint", id)
		}
		routes = append(routes, route)
	}
	return routes, nil
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
	// Optional team filter: ?team=rd-dept returns only that department's
	// employees (admin can filter to any team; non-admin is already scoped).
	teamFilter := r.URL.Query().Get("team")
	admin := ident.isAdmin(p.cfg)
	out := make([]map[string]any, 0, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		if teamFilter != "" && item.Spec.Team != teamFilter {
			continue
		}
		if !admin && !ident.canAccessEmployee(p.cfg, item.Spec.Team, item.Spec.Owner, item.Spec.Visibility) {
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
	// Consistent with list/chat authorization: visibility grants use, and
	// the detail view is what a listed employee links to (owner-only here
	// would hide details from team members who can already converse).
	if !ident.canAccessEmployee(p.cfg, item.Spec.Team, item.Spec.Owner, item.Spec.Visibility) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not authorized for this digital employee"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"employee": summarize(&item)})
}

// updateEmployee applies partial configuration updates: model switches,
// knowledge whitelists, skills, audience, owner transfer, and the basic
// fields. Only fields present in the body are validated and replaced — an
// untouched field never consults an upstream, so re-pointing an employee
// off a decommissioned model is not blocked by unrelated stale references.
// Owner transfer and opening to everyone stay administrator-only (updates
// have no approval parking); the operator reconciles spec changes (runtime
// config + memory account re-derivation) on the next pass.
func (p *portal) updateEmployee(w http.ResponseWriter, r *http.Request, ident identity) {
	name := r.PathValue("name")
	var body updateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	var item dev1alpha1.DigitalEmployee
	if err := p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: name}, &item); err != nil {
		writeJSON(w, statusOf(err), map[string]string{"error": err.Error()})
		return
	}
	// Mutation stays owner/admin-only: visibility grants use of the
	// employee, not the right to reconfigure it.
	if !ident.isAdmin(p.cfg) && item.Spec.Owner != ident.UserID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not authorized to modify this employee"})
		return
	}
	// Transferring ownership to someone else is admin-only, mirroring the
	// apply-time override rule; the current owner may keep or re-assert
	// themselves.
	if body.Owner != nil && *body.Owner != item.Spec.Owner && *body.Owner != ident.UserID && !ident.isAdmin(p.cfg) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "只有管理员可以变更负责人"})
		return
	}
	// Opening an existing employee to everyone stays a reviewed
	// administrator action (unlike creation there is no approval queue
	// for updates).
	if body.Visibility != nil && body.Visibility.Mode == "all" && !ident.isAdmin(p.cfg) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "全员开放需要管理员操作"})
		return
	}
	// Model lists resolve first with apply's fail-fast semantics.
	var runtimeModels []dev1alpha1.RuntimeModel
	if body.Models != nil {
		if len(body.Models) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":      "发布条件校验未通过（1 项）",
				"violations": []violation{{Field: "models", Message: "使用模型为必填发布条件"}},
			})
			return
		}
		var err error
		if runtimeModels, err = p.resolveRuntimeModels(r.Context(), ident, body.Models); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	resolved, violations, err := p.validateUpdate(r.Context(), ident, body)
	if err != nil {
		// Fail closed: never write an update whose references were not
		// checked because an upstream was down.
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "发布条件校验依赖的服务暂不可用：" + err.Error()})
		return
	}
	if len(violations) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":      fmt.Sprintf("发布条件校验未通过（%d 项）", len(violations)),
			"violations": violations,
		})
		return
	}
	if body.DisplayName != nil && *body.DisplayName != "" {
		item.Spec.DisplayName = *body.DisplayName
	}
	if body.Description != nil {
		item.Spec.Description = *body.Description
	}
	if body.Team != nil {
		item.Spec.Team = *body.Team
	}
	if body.Owner != nil && resolved.OwnerID != "" {
		item.Spec.Owner = resolved.OwnerID
		item.Spec.OwnerName = resolved.OwnerName
		// The employee acts with its owner's data scope — governance
		// identity follows the transfer.
		item.Spec.Governance.DefaultAEPUser = resolved.OwnerID
	}
	if body.Models != nil {
		// The explicit list replaces the singular default too, or
		// summarize would keep reporting the retired model as default.
		item.Spec.Runtime.Models = runtimeModels
		item.Spec.Runtime.Model = ""
	}
	if body.KnowledgeBases != nil {
		if item.Spec.Knowledge == nil {
			item.Spec.Knowledge = &dev1alpha1.KnowledgeSpec{Type: "weknora", MCPURL: p.cfg.knowledgeMCPURL}
		}
		item.Spec.Knowledge.Bases = resolved.KBs
	}
	if body.Skills != nil {
		item.Spec.Skills = resolved.Skills
	}
	if body.Visibility != nil {
		item.Spec.Visibility = resolved.Visibility
	}
	if err := p.k8s.Update(r.Context(), &item); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	p.logger.Info("employee updated", "name", name, "fields", updatedFields(body), "by", ident.UserID)
	writeJSON(w, http.StatusOK, map[string]any{"employee": summarize(&item)})
}

// updatedFields names the fields an update carried, for the audit log.
func updatedFields(body updateRequest) []string {
	fields := make([]string, 0, 8)
	if body.DisplayName != nil {
		fields = append(fields, "displayName")
	}
	if body.Description != nil {
		fields = append(fields, "description")
	}
	if body.Team != nil {
		fields = append(fields, "team")
	}
	if body.Owner != nil {
		fields = append(fields, "owner")
	}
	if body.Models != nil {
		fields = append(fields, "models")
	}
	if body.KnowledgeBases != nil {
		fields = append(fields, "knowledgeBases")
	}
	if body.Skills != nil {
		fields = append(fields, "skills")
	}
	if body.Visibility != nil {
		fields = append(fields, "visibility")
	}
	return fields
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
	defer func() { _ = rows.Close() }()
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
	channels := map[string]any{"wecom": false}
	if item.Spec.Channels != nil {
		channels["wecom"] = item.Spec.Channels.Wecom
		if item.Spec.Channels.WecomName != "" {
			channels["wecomName"] = item.Spec.Channels.WecomName
		}
	}
	// With an explicit model list the first entry is the runtime default.
	model := item.Spec.Runtime.Model
	if model == "" && len(item.Spec.Runtime.Models) > 0 {
		model = item.Spec.Runtime.Models[0].Name
	}
	// The full ordered list feeds the console's edit form (it needs every
	// selectable model, not just the default).
	models := make([]string, 0, len(item.Spec.Runtime.Models))
	for _, entry := range item.Spec.Runtime.Models {
		models = append(models, entry.Name)
	}
	var knowledgeBases any
	if item.Spec.Knowledge != nil && item.Spec.Knowledge.Bases != nil {
		bases := make([]map[string]string, 0, len(item.Spec.Knowledge.Bases))
		for _, base := range item.Spec.Knowledge.Bases {
			bases = append(bases, map[string]string{"id": base.ID, "name": base.Name})
		}
		knowledgeBases = bases
	}
	skills := make([]map[string]string, 0, len(item.Spec.Skills))
	for _, skill := range item.Spec.Skills {
		skills = append(skills, map[string]string{"id": skill.ID, "name": skill.Name, "version": skill.Version})
	}
	var visibility any
	if item.Spec.Visibility != nil {
		teams := make([]map[string]string, 0, len(item.Spec.Visibility.Teams))
		for _, team := range item.Spec.Visibility.Teams {
			teams = append(teams, map[string]string{"id": team.ID, "name": team.Name})
		}
		users := make([]map[string]string, 0, len(item.Spec.Visibility.Users))
		for _, user := range item.Spec.Visibility.Users {
			users = append(users, map[string]string{"id": user.ID, "name": user.Name})
		}
		visibility = map[string]any{"mode": item.Spec.Visibility.Mode, "teams": teams, "users": users}
	}
	return map[string]any{
		"name": item.Name, "displayName": item.Spec.DisplayName, "phase": item.Status.Phase,
		"runtime": item.Spec.Runtime.Type, "model": model, "models": models,
		"owner": item.Spec.OwnerName, "ownerId": item.Spec.Owner,
		"team":        item.Spec.Team,
		"description": item.Spec.Description,
		"memoryUser":  item.Status.MemoryUser, "createdAt": item.CreationTimestamp,
		"channels":       channels,
		"knowledgeBases": knowledgeBases, // nil = legacy unrestricted (not configured)
		"skills":         skills,
		"visibility":     visibility, // nil = legacy owner/team rules
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

// handleUsageStats returns aggregated usage metrics for the admin console
// dashboard: conversation counts, message volume, model calls, active users,
// broken down by department. Queries the centralized PG conversation store.
func (p *portal) handleUsageStats(w http.ResponseWriter, r *http.Request, ident identity) {
	if !ident.isAdmin(p.cfg) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin required"})
		return
	}
	ctx := r.Context()

	// Total conversations and messages (all time)
	var totalThreads, totalRuns int
	_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads_meta`).Scan(&totalThreads)
	_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&totalRuns)

	// Today's activity
	var todayThreads, todayRuns int
	_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads_meta WHERE created_at >= CURRENT_DATE`).Scan(&todayThreads)
	_ = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE created_at >= CURRENT_DATE`).Scan(&todayRuns)

	// Active employees
	var activeEmployees, readyEmployees int
	var list dev1alpha1.DigitalEmployeeList
	if err := p.k8s.List(ctx, &list, client.InNamespace(p.cfg.namespace)); err == nil {
		activeEmployees = len(list.Items)
		for i := range list.Items {
			if list.Items[i].Status.Phase == dev1alpha1.PhaseReady {
				readyEmployees++
			}
		}
	}

	// Per-department breakdown from employee specs
	deptStats := make(map[string]map[string]int)
	for i := range list.Items {
		team := list.Items[i].Spec.Team
		if team == "" {
			team = "(unassigned)"
		}
		if deptStats[team] == nil {
			deptStats[team] = map[string]int{"employees": 0, "ready": 0}
		}
		deptStats[team]["employees"]++
		if list.Items[i].Status.Phase == dev1alpha1.PhaseReady {
			deptStats[team]["ready"]++
		}
	}

	// Threads per department (from PG, last 7 days)
	threadsByDept := make(map[string]int)
	rows, err := p.db.QueryContext(ctx, `
		SELECT COALESCE(tenant_id, '(default)') as dept, COUNT(*) as cnt
		FROM threads_meta
		WHERE created_at >= CURRENT_DATE - INTERVAL '7 days'
		GROUP BY COALESCE(tenant_id, '(default)')
	`)
	if err == nil {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var dept string
			var cnt int
			if rows.Scan(&dept, &cnt) == nil {
				threadsByDept[dept] = cnt
			}
		}
	}

	// Runs per model (last 7 days)
	modelCalls := make(map[string]int)
	rows2, err2 := p.db.QueryContext(ctx, `
		SELECT COALESCE(model_name, '(unknown)') as model, COUNT(*) as cnt
		FROM runs
		WHERE created_at >= CURRENT_DATE - INTERVAL '7 days'
		GROUP BY COALESCE(model_name, '(unknown)')
	`)
	if err2 == nil {
		defer func() { _ = rows2.Close() }()
		for rows2.Next() {
			var model string
			var cnt int
			if rows2.Scan(&model, &cnt) == nil {
				modelCalls[model] = cnt
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"totals": map[string]int{
			"conversations":      totalThreads,
			"runs":               totalRuns,
			"todayConversations": todayThreads,
			"todayRuns":          todayRuns,
			"employees":          activeEmployees,
			"readyEmployees":     readyEmployees,
		},
		"byDepartment":          deptStats,
		"threadsByDepartment7d": threadsByDept,
		"modelCalls7d":          modelCalls,
	})
}
