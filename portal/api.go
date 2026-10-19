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
	// provisioningStore keeps department provisioning outcomes so a partial
	// provision stays visible in the listing and can be retried. nil means no
	// durable store: nothing is recorded and retry is refused explicitly.
	provisioningStore provisioningStore
	// chatBaseOverride is test-only: replaces the in-cluster runtime URL.
	chatBaseOverride string
	// workspaceChatOverride is test-only: replaces the workspace gateway URL.
	workspaceChatOverride string
}

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)

// applyNoteAnnotation carries the applicant's free-text note (补充说明)
// from the workbench apply form onto the CR. It rides the spec through the
// pending request (spec_json) untouched, so approval needs no extra
// plumbing. Operators never interpret CR annotations; the note is display
// data for the request detail and the console's approval view.
const applyNoteAnnotation = "digitalemployee.zhiyuan.io/apply-note"

func (p *portal) routes() http.Handler {
	mux := http.NewServeMux()
	p.registerSharedSpaces(mux)
	mux.HandleFunc("GET /api/v1/colleague-companions", p.auth(p.colleagueDirectory))
	mux.HandleFunc("PUT /api/v1/colleague-companions/{workspace}/{companion}", p.auth(p.updateCompanionContact))
	mux.HandleFunc("GET /api/v1/collaborations", p.auth(p.listCollaborations))
	mux.HandleFunc("POST /api/v1/collaborations", p.auth(func(w http.ResponseWriter, r *http.Request, i identity) { p.sendCollaboration(w, r, i, nil) }))
	mux.HandleFunc("GET /api/v1/collaborations/{id}", p.auth(p.getCollaboration))
	mux.HandleFunc("POST /api/v1/collaborations/{id}/reply", p.auth(p.replyCollaboration))
	mux.HandleFunc("POST /api/v1/collaborations/{id}/retry", p.auth(p.retryCollaboration))
	mux.HandleFunc("/api/v1/collaboration-agent/{action}", p.collaborationAgent)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /api/v1/employees", p.auth(p.applyForEmployee))
	// Personal workspaces (个人搭档): self-service, one per user, owner/admin
	// scoped. Chat rides the workspace gateway's per-companion prefix route.
	mux.HandleFunc("POST /api/v1/workspaces", p.auth(p.createWorkspace))
	mux.HandleFunc("POST /api/v1/workspaces/{name}/companions", p.auth(p.addWorkspaceCompanion))
	mux.HandleFunc("GET /api/v1/workspaces/{name}/{companion}/ready", p.auth(p.workspaceCompanionReady))
	mux.HandleFunc("GET /api/v1/workspaces", p.auth(p.listWorkspaces))
	mux.HandleFunc("GET /api/v1/workspaces/{name}", p.auth(p.getWorkspace))
	mux.HandleFunc("PATCH /api/v1/workspaces/{name}", p.auth(p.updateWorkspace))
	mux.HandleFunc("DELETE /api/v1/workspaces/{name}", p.auth(p.deleteWorkspace))
	mux.HandleFunc("/api/v1/workspaces/{name}/{companion}/chat/{rest...}", p.auth(p.handleWorkspaceChat))
	mux.HandleFunc("/api/v1/workspaces/{name}/{companion}/chat", p.auth(p.handleWorkspaceChat))
	// Rooms (群聊), owner-scoped top-level: the workspace is resolved from the
	// session identity (one per user), which also keeps this disjoint from the
	// workspaces/{name}/{companion}/chat wildcard tree.
	mux.HandleFunc("/api/v1/rooms/{rest...}", p.auth(p.handleRooms))
	mux.HandleFunc("/api/v1/rooms", p.auth(p.handleRooms))
	mux.HandleFunc("/api/v1/workplace/{rest...}", p.auth(p.handleWorkplace))
	mux.HandleFunc("/api/v1/workplace", p.auth(p.handleWorkplace))
	// 邮箱底层（一人多箱）：配置/测试/状态/收件/发件直达本人工作区网关。
	mux.HandleFunc("/api/v1/email/{rest...}", p.auth(p.handleEmail))
	// 数字员工自有邮箱：配置写 Secret + CR（admin 或员工 owner），读掩码。
	mux.HandleFunc("GET /api/v1/employees/{name}/email", p.auth(p.handleGetEmployeeEmail))
	mux.HandleFunc("PUT /api/v1/employees/{name}/email", p.auth(p.handlePutEmployeeEmail))
	mux.HandleFunc("/api/v1/email", p.auth(p.handleEmail))
	mux.HandleFunc("GET /api/v1/employees", p.auth(p.listEmployees))
	mux.HandleFunc("GET /api/v1/employees/{name}/channels", p.auth(p.freshAdminOnly(p.handleEmployeeChannels)))
	mux.HandleFunc("POST /api/v1/employees/{name}/channels/wake", p.auth(p.freshAdminOnly(p.handleWakeEmployeeChannels)))
	mux.HandleFunc("PUT /api/v1/employees/{name}/channels/{provider}/activation", p.auth(p.freshAdminOnly(p.handleActivateEmployeeChannel)))
	mux.HandleFunc("POST /api/v1/employees/{name}/channels/{provider}/{action}", p.auth(p.freshAdminOnly(p.handleEmployeeChannelAction)))
	mux.HandleFunc("PATCH /api/v1/employees/{name}", p.auth(p.updateEmployee))
	// Sidebar service-status probes for the admin console (browser never
	// sees the OpenViking root key — the portal holds it server-side).
	// These read platform-wide data (every team, every employee's memory
	// activity, the WeKnora catalog), so they are administrator-only: the
	// workbench gives regular users their own scoped views instead, and an
	// ungated probe would be a platform-wide data leak once every employee
	// holds a session.
	mux.HandleFunc("GET /api/v1/departments", p.auth(p.adminOnly(p.handleListDepartments)))
	mux.HandleFunc("GET /api/v1/usage/stats", p.auth(p.handleUsageStats))
	mux.HandleFunc("PATCH /api/v1/departments/{id}", p.auth(p.handleUpdateDepartment))
	mux.HandleFunc("DELETE /api/v1/departments/{id}", p.auth(p.handleDeleteDepartment))
	mux.HandleFunc("GET /api/v1/departments/{id}/members", p.auth(p.handleDepartmentMembers))
	mux.HandleFunc("PUT /api/v1/departments/{id}/members", p.auth(p.handleDepartmentMembers))
	mux.HandleFunc("POST /api/v1/departments", p.auth(p.handleCreateDepartment))
	// Retry only the pieces of a department's provisioning that never landed;
	// idempotent, so the console can re-post it without a second account or
	// tenant being created.
	mux.HandleFunc("POST /api/v1/departments/{id}/provision", p.auth(p.handleProvisionDepartment))
	mux.HandleFunc("GET /api/v1/memory/status", p.auth(p.adminOnly(p.handleMemoryStatus)))
	// Memory test box: an administrator diagnostic over one employee's own
	// OpenViking key, annotated with the scope it ran in and the runtime
	// recall filters it skips. It is not a requester-scoped recall, so the
	// gate stays adminOnly and no user key is ever borrowed.
	mux.HandleFunc("POST /api/v1/memory/search", p.auth(p.adminOnly(p.handleMemorySearch)))
	mux.HandleFunc("GET /api/v1/knowledge/status", p.auth(p.adminOnly(p.handleKnowledgeStatus)))
	mux.HandleFunc("/api/v1/knowledge/proxy/{rest...}", p.auth(p.handleKnowledgeProxy))
	mux.HandleFunc("GET /api/v1/knowledge/bases", p.auth(p.adminOnly(p.handleKnowledgeListBases)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/readiness", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeReadiness)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/audit", p.auth(p.freshAdminOnly(p.handleKnowledgeAudit)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/bases", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeBases)))
	mux.HandleFunc("POST /api/v1/knowledge/managed/bases", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeCreateBase)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/bases/{baseID}", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeGetBase)))
	mux.HandleFunc("PATCH /api/v1/knowledge/managed/bases/{baseID}", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeUpdateBase)))
	mux.HandleFunc("DELETE /api/v1/knowledge/managed/bases/{baseID}", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeDeleteBase)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/bases/{baseID}/documents", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeDocuments)))
	mux.HandleFunc("POST /api/v1/knowledge/managed/bases/{baseID}/documents/file", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeUpload)))
	mux.HandleFunc("POST /api/v1/knowledge/managed/bases/{baseID}/documents/url", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeImportURL)))
	mux.HandleFunc("POST /api/v1/knowledge/managed/bases/{baseID}/documents/manual", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeImportManual)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/documents/{documentID}", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeGetDocument)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/documents/{documentID}/spans", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeSpans)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/documents/{documentID}/chunks", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeChunks)))
	mux.HandleFunc("PUT /api/v1/knowledge/managed/documents/{documentID}/enable-status", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeSetDocumentEnabled)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/documents/{documentID}/chunks/{chunkID}/images/{index}", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeChunkImage)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/documents/{documentID}/preview", p.auth(p.freshAdminOnly(p.managedDocumentFile("preview"))))
	mux.HandleFunc("GET /api/v1/knowledge/managed/documents/{documentID}/download", p.auth(p.freshAdminOnly(p.managedDocumentFile("download"))))
	mux.HandleFunc("POST /api/v1/knowledge/managed/documents/{documentID}/reparse", p.auth(p.freshAdminOnly(p.managedDocumentAction("reparse"))))
	mux.HandleFunc("POST /api/v1/knowledge/managed/documents/{documentID}/cancel-parse", p.auth(p.freshAdminOnly(p.managedDocumentAction("cancel-parse"))))
	mux.HandleFunc("DELETE /api/v1/knowledge/managed/documents/{documentID}", p.auth(p.freshAdminOnly(p.managedDocumentAction("delete"))))
	mux.HandleFunc("GET /api/v1/knowledge/managed/bases/{baseID}/tags", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeTags)))
	mux.HandleFunc("POST /api/v1/knowledge/managed/bases/{baseID}/tags", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeTags)))
	mux.HandleFunc("PUT /api/v1/knowledge/managed/bases/{baseID}/tags/{tagID}", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeTag)))
	mux.HandleFunc("DELETE /api/v1/knowledge/managed/bases/{baseID}/tags/{tagID}", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeTag)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/bases/{baseID}/folders", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeFolders)))
	mux.HandleFunc("PUT /api/v1/knowledge/managed/bases/{baseID}/folders", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeFolders)))
	mux.HandleFunc("POST /api/v1/knowledge/managed/bases/{baseID}/folders/move", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeFolders)))
	mux.HandleFunc("PUT /api/v1/knowledge/managed/bases/{baseID}/document-tags", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeTagsForDocuments)))
	mux.HandleFunc("PUT /api/v1/knowledge/managed/documents/{documentID}/tags", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeDocumentTags)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/bases/{baseID}/faq/entries", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeFAQ)))
	mux.HandleFunc("POST /api/v1/knowledge/managed/bases/{baseID}/faq/entries", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeFAQEntry)))
	mux.HandleFunc("PUT /api/v1/knowledge/managed/bases/{baseID}/faq/entries/{entryID}", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeFAQEntry)))
	mux.HandleFunc("PUT /api/v1/knowledge/managed/bases/{baseID}/faq/batch/{batchAction}", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeFAQBatch)))
	mux.HandleFunc("DELETE /api/v1/knowledge/managed/bases/{baseID}/faq/batch/{batchAction}", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeFAQBatch)))
	mux.HandleFunc("PUT /api/v1/knowledge/managed/documents/{documentID}/chunks/{chunkID}", p.auth(p.freshAdminOnly(p.handleManagedKnowledgeChunkUpdate)))
	mux.HandleFunc("GET /api/v1/knowledge/managed/bases/{baseID}/grants", p.auth(p.freshAdminOnly(p.handleKnowledgeBaseGrants)))
	mux.HandleFunc("PUT /api/v1/knowledge/managed/bases/{baseID}/grants", p.auth(p.freshAdminOnly(p.handleKnowledgeBaseGrants)))
	// Employee knowledge reads are authenticated by the portal session, then
	// scoped again by each handler to the selected employee and granted bases.
	mux.HandleFunc("POST /api/v1/knowledge/employees/{name}/search", p.auth(p.handleEmployeeKnowledgeSearch))
	mux.HandleFunc("GET /api/v1/knowledge/employees/{name}/documents/{documentID}", p.auth(p.handleEmployeeKnowledgeDocument))
	mux.HandleFunc("GET /api/v1/knowledge/employees/{name}/documents/{documentID}/chunks/{chunkID}/images/{index}", p.auth(p.handleEmployeeKnowledgeChunkImage))
	mux.HandleFunc("GET /api/v1/knowledge/employees/{name}/documents/{documentID}/download", p.auth(p.handleEmployeeKnowledgeDocumentDownload))
	// Internal runtime authorization is authenticated by the employee Secret
	// token in the handler; it deliberately bypasses browser cookie auth.
	mux.HandleFunc("POST /internal/v1/knowledge/employees/{name}/authorize", p.handleKnowledgeRuntimeAuthorize)
	mux.HandleFunc("GET /api/v1/employees/{name}", p.auth(p.getEmployee))
	mux.HandleFunc("DELETE /api/v1/employees/{name}", p.auth(p.deleteEmployee))
	// Workbench (employee-facing) reads: profile + quota, own requests
	// across all states, and one request's detail. The literal /mine
	// segment beats the {id} wildcard (Go 1.22 mux precedence).
	mux.HandleFunc("GET /api/v1/me", p.auth(p.handleMe))
	// Workbench messenger directory (通讯录): company-wide human accounts,
	// projected to id/username/displayName by the portal (AEP's user listing
	// is admin-scoped, so employees never call it directly).
	mux.HandleFunc("GET /api/v1/contacts", p.auth(p.handleContacts))
	// Full department tree (id/name/parentId) for the workbench org view:
	// parent departments with no direct members never appear in me.teams or
	// any contact's teams, so the tree needs this authoritative listing.
	mux.HandleFunc("GET /api/v1/organization", p.auth(p.handleOrganization))
	mux.HandleFunc("GET /api/v1/requests/mine", p.auth(p.handleMyRequests))
	mux.HandleFunc("GET /api/v1/requests/{id}", p.auth(p.handleRequestDetail))
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
			code, message := "AUTH_REQUIRED", "缺少访问令牌或登录会话，请重新登录"
			if status == http.StatusForbidden {
				code, message = "CSRF_INVALID", "CSRF 令牌缺失或不匹配，请刷新页面后重试"
			}
			p.managedWriteError(w, status, code, message, "")
			return
		}
		next(w, r, ident)
	}
}

func (p *portal) adminOnly(next func(http.ResponseWriter, *http.Request, identity)) func(http.ResponseWriter, *http.Request, identity) {
	return func(w http.ResponseWriter, r *http.Request, ident identity) {
		if !ident.isAdmin(p.cfg) {
			p.managedWriteError(w, http.StatusForbidden, "ADMIN_REQUIRED", "需要管理员权限", "")
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
	// Note is the applicant's optional free-text context (补充说明, ≤500
	// runes). It is stored as a CR annotation via applyNoteAnnotation and
	// never interpreted by the operator.
	Note string `json:"note"`
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
	if len(body.Models) == 0 {
		if p.cfg.model == "" {
			violations = append(violations, violation{Field: "models", Message: "使用模型为必填发布条件"})
		} else if _, err := p.resolveRuntimeModels(r.Context(), ident, []string{p.cfg.model}); err != nil {
			// The workbench apply form has no model picker — employees
			// created from it ride the platform default. Verify the
			// default is still an enabled gateway model now; otherwise a
			// stale default slips through approval and only surfaces at
			// the first conversation.
			p.logger.Warn("platform default model unavailable", "model", p.cfg.model, "error", err)
			violations = append(violations, violation{Field: "models", Message: fmt.Sprintf("平台默认模型 %s 当前不可用，请联系管理员", p.cfg.model)})
		}
	}
	if len(violations) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":      fmt.Sprintf("发布条件校验未通过（%d 项）", len(violations)),
			"violations": violations,
		})
		return
	}
	deploymentName, err := p.deploymentName(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "企业实例身份暂不可用：" + err.Error()})
		return
	}
	resolved.DeploymentName = deploymentName
	spec := p.renderSpec(body, resolved, runtimeModels)
	knowledgeLock, err := p.lockKnowledgeBindings(r.Context(), ident.DeploymentID, employeeKnowledgeIDs(spec))
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "知识库正在删除或无法安全绑定"})
		return
	}
	if knowledgeLock != nil {
		defer func() { _ = knowledgeLock.Rollback() }()
	}
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
	meta := metav1.ObjectMeta{Name: body.Name, Namespace: p.cfg.namespace}
	if note := strings.TrimSpace(body.Note); note != "" {
		meta.Annotations = map[string]string{applyNoteAnnotation: note}
	}
	return dev1alpha1.DigitalEmployee{
		ObjectMeta: meta,
		Spec: dev1alpha1.DigitalEmployeeSpec{
			DisplayName: body.DisplayName,
			Description: body.Description,
			Team:        body.Team,
			TeamName:    resolved.TeamName,
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
				DeploymentID:   p.cfg.aepDeploymentID,
				DeploymentName: resolved.DeploymentName,
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
	// ReasoningCompatibility is the AEP reasoning contract (thinkingFormat
	// deepseek/zai + reasoning-content history requirement); a declared
	// format maps onto the runtime's declarative reasoning block.
	ReasoningCompatibility *catalogReasoning `json:"reasoningCompatibility,omitempty"`
}

// runtimeReasoning projects the AEP reasoning contract onto the runtime's
// declarative block: a declared thinkingFormat (deepseek or zai) means
// thinking can be toggled (optional); an upstream that requires
// reasoning_content on assistant messages maps to history preservation.
// Effort vocabulary is not expressed by the AEP contract and stays unset.
func runtimeReasoning(compatibility *catalogReasoning) *dev1alpha1.RuntimeReasoning {
	if compatibility == nil || compatibility.ThinkingFormat == "" {
		return nil
	}
	return &dev1alpha1.RuntimeReasoning{
		Thinking: "optional",
		History:  map[bool]string{true: "preserve", false: ""}[compatibility.RequiresReasoningContentOnAssistantMessages],
	}
}

type catalogReasoning struct {
	ThinkingFormat                              string `json:"thinkingFormat"`
	SupportsReasoningEffort                     bool   `json:"supportsReasoningEffort"`
	RequiresReasoningContentOnAssistantMessages bool   `json:"requiresReasoningContentOnAssistantMessages"`
}

// resolveRuntimeModels maps requested AEP model ids onto runtime routes by
// consulting the model catalog with the requester's own token (the console
// admin already holds models.read). Both protocols ride the gateway:
// anthropic models under their per-model path prefix, openai-compatible
// models under the shared /v1 path (the authorizer stamps X-AEP-Model-ID
// from the request body and the data plane routes on that header).
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
			// Gateway-routed, never the upstream endpoint: the data plane
			// routes openai models by the x-aep-model-id header under the
			// shared /v1 path (ai-proxy rehomes it onto the provider base
			// and injects the credential). Handing runtimes the raw
			// endpoint made employees dial the provider directly with the
			// platform key — unauthorizer, unaccounted, and 401 for
			// key-authenticating providers.
			route.Provider = "openai"
			route.BaseURL = p.cfg.gatewayBaseURL + "/v1"
		default:
			return nil, fmt.Errorf("model %q uses unsupported protocol %q", id, model.Protocol)
		}
		if route.BaseURL == "" {
			return nil, fmt.Errorf("model %q has no endpoint", id)
		}
		if reasoning := runtimeReasoning(model.ReasoningCompatibility); reasoning != nil {
			route.Reasoning = reasoning
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
		summary := summarize(item)
		// "Why can I use this one" for the workbench roster; computed from
		// data already in hand, no extra upstream call.
		summary["accessReason"] = ident.accessReason(item.Spec.Team, item.Spec.Owner, item.Spec.Visibility)
		out = append(out, summary)
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
	summary := summarize(&item)
	summary["accessReason"] = ident.accessReason(item.Spec.Team, item.Spec.Owner, item.Spec.Visibility)
	writeJSON(w, http.StatusOK, map[string]any{"employee": summary})
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
	// Only directory-dependent edits refresh the enterprise snapshot. A
	// displayName-only update must work while AEP is temporarily unavailable.
	if body.Team != nil || body.Owner != nil {
		deploymentName, err := p.deploymentName(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "企业实例身份暂不可用：" + err.Error()})
			return
		}
		item.Spec.Governance.DeploymentID = p.cfg.aepDeploymentID
		item.Spec.Governance.DeploymentName = deploymentName
	}
	if body.DisplayName != nil && *body.DisplayName != "" {
		item.Spec.DisplayName = *body.DisplayName
	}
	if body.Description != nil {
		item.Spec.Description = *body.Description
	}
	if body.Team != nil {
		item.Spec.Team = *body.Team
		item.Spec.TeamName = resolved.TeamName
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
	if body.KnowledgeBases != nil {
		knowledgeLock, err := p.lockKnowledgeBindings(r.Context(), ident.DeploymentID, employeeKnowledgeIDs(item))
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "知识库正在删除或无法安全绑定"})
			return
		}
		if knowledgeLock != nil {
			defer func() { _ = knowledgeLock.Rollback() }()
		}
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
			if err := p.refreshApprovalIdentity(r.Context(), &spec); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": "审批时企业身份校验失败：" + err.Error()})
				return
			}
			spec.ResourceVersion = ""
			knowledgeLock, err := p.lockKnowledgeBindings(r.Context(), ident.DeploymentID, employeeKnowledgeIDs(spec))
			if err != nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "知识库正在删除或无法安全绑定"})
				return
			}
			if knowledgeLock != nil {
				defer func() { _ = knowledgeLock.Rollback() }()
			}
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
		channels["feishu"] = item.Spec.Channels.Feishu
		channels["wechat"] = item.Spec.Channels.Wechat
		channels["feishuName"] = item.Spec.Channels.FeishuName
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
	// Surface an active model failover so every employee list (workbench and
	// console) can warn that the default model is temporarily substituted.
	var modelFailover any
	if state, ok, _ := parseFailoverState(item); ok {
		modelFailover = map[string]any{"original": state.Original, "active": state.Active, "switchedAt": state.SwitchedAt}
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
		"visibility":     visibility,    // nil = legacy owner/team rules
		"modelFailover":  modelFailover, // nil = the default model is serving
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
