package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const contactAnnotation = "digitalemployee.zhiyuan.io/companion-contacts"
const collaborationTokenHeader = "X-Collaboration-Token"

var errCollaborationOwnerUnavailable = errors.New("同事账号不可用")

type companionContact struct {
	Mode        string `json:"mode"`
	Description string `json:"description"`
}
type colleagueCompanion struct {
	ID          string `json:"id"`
	Workspace   string `json:"workspace"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	OwnerID     string `json:"ownerId"`
	OwnerName   string `json:"ownerName"`
	Description string `json:"description"`
	ContactMode string `json:"contactMode"`
	Phase       string `json:"phase"`
}

func contactSettings(ws *dev.PersonalWorkspace) map[string]companionContact {
	out := map[string]companionContact{}
	_ = json.Unmarshal([]byte(ws.Annotations[contactAnnotation]), &out)
	if out == nil {
		out = map[string]companionContact{}
	}
	return out
}
func initializeContacts(ws *dev.PersonalWorkspace, previous []dev.CompanionSpec) {
	settings := contactSettings(ws)
	old := map[string]bool{}
	for _, c := range previous {
		old[c.Name] = true
	}
	for _, c := range ws.Spec.Companions {
		if !old[c.Name] {
			if _, ok := settings[c.Name]; !ok {
				settings[c.Name] = companionContact{Mode: "organization"}
			}
		}
	}
	if ws.Annotations == nil {
		ws.Annotations = map[string]string{}
	}
	raw, _ := json.Marshal(settings)
	ws.Annotations[contactAnnotation] = string(raw)
}
func (p *portal) collaborationTarget(ctx context.Context, id string, caller identity, requireContact bool) (*colleagueCompanion, *dev.PersonalWorkspace, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 2 {
		return nil, nil, errors.New("搭档不存在")
	}
	var ws dev.PersonalWorkspace
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: parts[0]}, &ws); err != nil {
		return nil, nil, errors.New("搭档不存在")
	}
	for _, c := range ws.Spec.Companions {
		if c.Name != parts[1] {
			continue
		}
		setting := contactSettings(&ws)[c.Name]
		if setting.Mode == "" {
			setting.Mode = "private"
		}
		if requireContact && caller.UserID != ws.Spec.Owner && setting.Mode != "organization" {
			return nil, nil, errors.New("这位搭档暂未开放联系")
		}
		owner, found, err := newReferenceResolver(p, ctx).lookupUser(ws.Spec.Owner)
		if err != nil {
			return nil, nil, errors.New("同事身份暂时无法核对")
		}
		if !found || owner.Status != "active" || owner.Kind != "human" {
			return nil, nil, errCollaborationOwnerUnavailable
		}
		display := c.DisplayName
		if display == "" {
			display = c.Name
		}
		ownerName := owner.DisplayName
		if ownerName == "" {
			ownerName = owner.Username
		}
		return &colleagueCompanion{ID: id, Workspace: ws.Name, Name: c.Name, DisplayName: display, OwnerID: ws.Spec.Owner, OwnerName: ownerName, Description: setting.Description, ContactMode: setting.Mode, Phase: string(ws.Status.Phase)}, &ws, nil
	}
	return nil, nil, errors.New("搭档不存在")
}
func (p *portal) colleagueDirectory(w http.ResponseWriter, r *http.Request, ident identity) {
	var workspaces dev.PersonalWorkspaceList
	if err := p.k8s.List(r.Context(), &workspaces, client.InNamespace(p.cfg.namespace)); err != nil {
		writeJSON(w, 503, map[string]string{"error": "搭档目录暂不可用"})
		return
	}
	out := []colleagueCompanion{}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	for i := range workspaces.Items {
		ws := &workspaces.Items[i]
		for _, c := range ws.Spec.Companions {
			if ws.Spec.Owner != ident.UserID && contactSettings(ws)[c.Name].Mode != "organization" {
				continue
			}
			target, _, err := p.collaborationTarget(r.Context(), ws.Name+"/"+c.Name, ident, true)
			if errors.Is(err, errCollaborationOwnerUnavailable) {
				continue
			}
			if err != nil {
				writeJSON(w, 503, map[string]string{"error": "搭档目录身份暂时无法核对"})
				return
			}
			if q == "" || strings.Contains(strings.ToLower(target.OwnerName+" "+target.DisplayName+" "+target.Description), q) {
				out = append(out, *target)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"companions": out})
}
func (p *portal) updateCompanionContact(w http.ResponseWriter, r *http.Request, ident identity) {
	var ws dev.PersonalWorkspace
	if err := p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: r.PathValue("workspace")}, &ws); err != nil || ws.Spec.Owner != ident.UserID {
		writeJSON(w, 404, map[string]string{"error": "搭档不存在"})
		return
	}
	name := r.PathValue("companion")
	found := false
	for _, c := range ws.Spec.Companions {
		if c.Name == name {
			found = true
		}
	}
	var input companionContact
	if !found {
		writeJSON(w, 404, map[string]string{"error": "搭档不存在"})
		return
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil || (input.Mode != "organization" && input.Mode != "private") || len(input.Description) > 600 {
		writeJSON(w, 400, map[string]string{"error": "请选择组织内可联系或仅自己"})
		return
	}
	settings := contactSettings(&ws)
	settings[name] = input
	raw, _ := json.Marshal(settings)
	if ws.Annotations == nil {
		ws.Annotations = map[string]string{}
	}
	ws.Annotations[contactAnnotation] = string(raw)
	if err := p.k8s.Update(r.Context(), &ws); err != nil {
		writeJSON(w, statusOf(err), map[string]string{"error": "保存失败，请刷新后重试"})
		return
	}
	writeJSON(w, 200, input)
}

// The runtime currently inherits the primary companion credential across its shared
// workspace. Use it only for this workspace-level broker, never as a user session.
func (p *portal) collaborationWorkspaceToken(ctx context.Context, ws *dev.PersonalWorkspace) (string, error) {
	if len(ws.Spec.Companions) == 0 {
		return "", errors.New("搭档未配置")
	}
	c := ws.Spec.Companions[0]
	name := workspaceMemkeySecretName(ws.Name, c.Name)
	if c.Memory != nil && c.Memory.UserKeySecret != "" {
		name = c.Memory.UserKeySecret
	}
	var secret corev1.Secret
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: name}, &secret); err != nil {
		return "", err
	}
	value := string(secret.Data["WORKSPACE_INTERNAL_TOKEN"])
	if value == "" {
		return "", errors.New("工作区凭据未就绪")
	}
	return value, nil
}
func (p *portal) collaborationAgent(w http.ResponseWriter, r *http.Request) {
	var ws dev.PersonalWorkspace
	name := r.Header.Get("X-Collaboration-Workspace")
	companion := r.Header.Get("X-Collaboration-Companion")
	if p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: name}, &ws) != nil {
		writeJSON(w, 403, map[string]string{"error": "协作身份无效"})
		return
	}
	token, err := p.collaborationWorkspaceToken(r.Context(), &ws)
	if err != nil || subtle.ConstantTimeCompare([]byte(token), []byte(r.Header.Get(collaborationTokenHeader))) != 1 {
		writeJSON(w, 403, map[string]string{"error": "协作身份无效"})
		return
	}
	found := false
	for _, c := range ws.Spec.Companions {
		if c.Name == companion {
			found = true
		}
	}
	if !found {
		writeJSON(w, 403, map[string]string{"error": "搭档不在工作区中"})
		return
	}
	owner, ok, err := newReferenceResolver(p, r.Context()).lookupUser(ws.Spec.Owner)
	if err != nil || !ok || owner.Status != "active" || owner.Kind != "human" {
		writeJSON(w, 403, map[string]string{"error": "账号不可用"})
		return
	}
	ident := identity{UserID: ws.Spec.Owner, Username: owner.Username, DisplayName: owner.DisplayName, Kind: "human"}
	switch r.PathValue("action") {
	case "directory":
		if r.Method == "GET" {
			p.colleagueDirectory(w, r, ident)
			return
		}
	case "send":
		if r.Method == "POST" {
			p.sendCollaboration(w, r, ident, &collaborationSource{Workspace: name, Companion: companion})
			return
		}
	case "read":
		if r.Method == "GET" {
			if r.URL.Query().Get("id") == "" {
				query := r.URL.Query()
				if query.Get("conversationId") == "" {
					writeJSON(w, 400, map[string]string{"error": "需要原会话编号"})
					return
				}
				query.Set("sourceWorkspace", name)
				query.Set("sourceCompanion", companion)
				query.Set("sourceConversationId", query.Get("conversationId"))
				r.URL.RawQuery = query.Encode()
				p.listCollaborations(w, r, ident)
				return
			}
			r.SetPathValue("id", r.URL.Query().Get("id"))
			p.getCollaboration(w, r, ident)
			return
		}
	}
	writeJSON(w, 404, map[string]string{"error": "not found"})
}
