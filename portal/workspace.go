// Personal-workspace API: portal is the only CR writer besides the operator.
// One persistent workspace per user hosting N companions (the avatar model —
// companions share the user's files/sessions, keep private SQLite and
// per-companion memory identities). Workspaces are personal: the owner and
// admins see them, nobody else; creation is self-service (no approval flow)
// and bounded by PORTAL_WORKSPACE_QUOTA_PER_USER.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// companionNamePattern mirrors the PersonalWorkspace CRD validation
// (operator/api/v1alpha1/personalworkspace_types.go): DNS label, unique
// inside the workspace.
var companionNamePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,28}[a-z0-9])?$`)

type workspaceCompanionRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"`
	Model       string `json:"model,omitempty"`
}

type workspaceConfiguration struct {
	Desktop     *dev1alpha1.WorkspaceDesktop   `json:"desktop,omitempty"`
	Resources   *dev1alpha1.WorkspaceResources `json:"resources,omitempty"`
	TemplateTag *string                        `json:"templateTag,omitempty"`
}

type createWorkspaceRequest struct {
	workspaceConfiguration
	Runtime    string                      `json:"runtime,omitempty"`
	Name       string                      `json:"name,omitempty"`
	Companions []workspaceCompanionRequest `json:"companions"`
	Image      string                      `json:"image,omitempty"`
}

type updateWorkspaceRequest struct {
	workspaceConfiguration
	Companions *[]workspaceCompanionRequest `json:"companions,omitempty"`
	Image      *string                      `json:"image,omitempty"`
}

// slugUser derives the default workspace name from the caller: deterministic,
// collision-free across users (the short id hash separates same-name users),
// always satisfying namePattern.
func slugUser(ident identity) string {
	base := strings.ToLower(regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(strings.ToLower(ident.Username), "-"))
	base = strings.Trim(base, "-")
	if base == "" {
		base = "user"
	}
	if len(base) > 18 {
		base = base[:18]
	}
	sum := 0
	for _, r := range ident.UserID {
		sum = sum*31 + int(r)
	}
	return fmt.Sprintf("ws-%s-%04x", base, sum&0xffff)
}

// validateWorkspaceCompanions enforces the roster rules shared by create and
// update: 1..10 entries, DNS-label names (matching the CRD), unique, each
// with a model (falling back to the platform default when empty).
func (p *portal) validateWorkspaceCompanions(companions []workspaceCompanionRequest) ([]dev1alpha1.CompanionSpec, string) {
	if len(companions) == 0 {
		return nil, "companions must contain at least one entry"
	}
	if len(companions) > 10 {
		return nil, "companions exceeds the maximum of 10 entries"
	}
	seen := map[string]bool{}
	out := make([]dev1alpha1.CompanionSpec, 0, len(companions))
	for _, c := range companions {
		name := strings.TrimSpace(c.Name)
		if !companionNamePattern.MatchString(name) {
			return nil, "invalid companion name " + c.Name + " (lowercase letters, digits, hyphens; 1-30 chars)"
		}
		if seen[name] {
			return nil, "duplicate companion name " + name
		}
		seen[name] = true
		model := strings.TrimSpace(c.Model)
		if model == "" {
			model = p.cfg.workspaceModel
		}
		out = append(out, dev1alpha1.CompanionSpec{
			Name:        name,
			DisplayName: strings.TrimSpace(c.DisplayName),
			Model:       model,
		})
	}
	return out, ""
}

// renderWorkspaceSpec builds the PersonalWorkspace CR from a validated
// request: the caller is the owner (quota and RBAC subject), memory rides
// the shared OpenViking instance with per-companion identities, credentials
// stay in the platform secret (operator renders the model key from it).
func (p *portal) renderWorkspaceSpec(name string, ident identity, companions []dev1alpha1.CompanionSpec, image string) dev1alpha1.PersonalWorkspace {
	spec := dev1alpha1.PersonalWorkspaceSpec{
		Owner:      ident.UserID,
		Runtime:    "pod",
		Companions: companions,
		Memory: &dev1alpha1.MemorySpec{
			Type:      "openviking",
			ServerURL: p.cfg.memoryServer,
			AccountID: p.cfg.memoryAccount,
		},
		Credentials: dev1alpha1.CredentialsRef{Name: p.cfg.credentialsSecret},
	}
	if image == "" {
		image = p.cfg.workspaceImage // empty still means the operator default
	}
	if image != "" {
		spec.Image = image
	}
	return dev1alpha1.PersonalWorkspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: p.cfg.namespace, Name: name},
		Spec:       spec,
	}
}

func (p *portal) countWorkspacesOwned(ctx context.Context, ownerID string) int {
	var list dev1alpha1.PersonalWorkspaceList
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

func (p *portal) createWorkspace(w http.ResponseWriter, r *http.Request, ident identity) {
	var body createWorkspaceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	companions, errMsg := p.validateWorkspaceCompanions(body.Companions)
	if errMsg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = slugUser(ident)
	}
	if !namePattern.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid workspace name (lowercase letters, digits, hyphens; 3-32 chars)"})
		return
	}
	if quota := p.cfg.workspaceQuotaPerUser; quota > 0 && p.countWorkspacesOwned(r.Context(), ident.UserID) >= quota {
		writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf("workspace quota reached (%d per user)", quota)})
		return
	}
	spec := p.renderWorkspaceSpec(name, ident, companions, body.Image)
	if body.Runtime != "" {
		spec.Spec.Runtime = body.Runtime
	}
	if err := applyWorkspaceConfiguration(&spec.Spec, body.workspaceConfiguration, false); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := p.k8s.Create(r.Context(), &spec); err != nil {
		writeJSON(w, statusOf(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"workspace": summarizeWorkspace(&spec)})
}

func (p *portal) listWorkspaces(w http.ResponseWriter, r *http.Request, ident identity) {
	var list dev1alpha1.PersonalWorkspaceList
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
		out = append(out, summarizeWorkspace(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": out})
}

func (p *portal) getWorkspace(w http.ResponseWriter, r *http.Request, ident identity) {
	item, code, errMsg := p.getAuthorizedWorkspace(r, ident)
	if code != 0 {
		writeJSON(w, code, map[string]string{"error": errMsg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": summarizeWorkspace(item)})
}

// getAuthorizedWorkspace resolves the CR and enforces the personal-scope
// rule: the owner or an admin, nobody else.
func (p *portal) getAuthorizedWorkspace(r *http.Request, ident identity) (*dev1alpha1.PersonalWorkspace, int, string) {
	var item dev1alpha1.PersonalWorkspace
	if err := p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: r.PathValue("name")}, &item); err != nil {
		return nil, statusOf(err), err.Error()
	}
	if item.Spec.Owner != ident.UserID && !ident.isAdmin(p.cfg) {
		return nil, http.StatusForbidden, "not authorized for this workspace"
	}
	return &item, 0, ""
}

func (p *portal) updateWorkspace(w http.ResponseWriter, r *http.Request, ident identity) {
	item, code, errMsg := p.getAuthorizedWorkspace(r, ident)
	if code != 0 {
		writeJSON(w, code, map[string]string{"error": errMsg})
		return
	}
	var body updateWorkspaceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if body.Companions != nil {
		companions, errMsg := p.validateWorkspaceCompanions(*body.Companions)
		if errMsg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": errMsg})
			return
		}
		item.Spec.Companions = companions
	}
	if body.Image != nil {
		item.Spec.Image = strings.TrimSpace(*body.Image)
	}
	if err := applyWorkspaceConfiguration(&item.Spec, body.workspaceConfiguration, true); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := p.k8s.Update(r.Context(), item); err != nil {
		writeJSON(w, statusOf(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspace": summarizeWorkspace(item)})
}

func (p *portal) deleteWorkspace(w http.ResponseWriter, r *http.Request, ident identity) {
	item, code, errMsg := p.getAuthorizedWorkspace(r, ident)
	if code != 0 {
		writeJSON(w, code, map[string]string{"error": errMsg})
		return
	}
	if err := p.k8s.Delete(r.Context(), item); err != nil {
		writeJSON(w, statusOf(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": item.Name})
}

func summarizeWorkspace(item *dev1alpha1.PersonalWorkspace) map[string]any {
	runtime := item.Spec.Runtime
	if runtime == "" {
		runtime = "pod" // CRD default, not a claim that a VM is provisioned.
	}
	companions := make([]map[string]any, 0, len(item.Spec.Companions))
	for i := range item.Spec.Companions {
		c := &item.Spec.Companions[i]
		entry := map[string]any{"name": c.Name, "model": c.Model}
		if c.DisplayName != "" {
			entry["displayName"] = c.DisplayName
		}
		companions = append(companions, entry)
	}
	out := map[string]any{
		"name":        item.Name,
		"owner":       item.Spec.Owner,
		"phase":       string(item.Status.Phase),
		"companions":  companions,
		"runtime":     runtime,
		"resources":   item.Spec.Resources,
		"desktop":     item.Spec.Desktop,
		"templateTag": item.Spec.TemplateTag,
		"applied":     item.Status.VM,
	}
	if item.Status.GatewayEndpoint != "" {
		out["gatewayEndpoint"] = item.Status.GatewayEndpoint
	}
	if item.Status.Message != "" {
		out["message"] = item.Status.Message
	}
	return out
}
