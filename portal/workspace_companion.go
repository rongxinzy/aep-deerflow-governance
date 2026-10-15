package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	dev "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"k8s.io/client-go/util/retry"
)

// Creation is an owner-confirmed, additive operation. The stable companion name
// is the command identity; retries cannot replace an existing companion.
func (p *portal) addWorkspaceCompanion(w http.ResponseWriter, r *http.Request, ident identity) {
	var body workspaceCompanionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid json body"})
		return
	}
	body.DisplayName = strings.TrimSpace(body.DisplayName)
	if body.DisplayName == "" || len([]rune(body.DisplayName)) > 60 {
		writeJSON(w, 400, map[string]string{"error": "搭档名称需要 1 至 60 个字符"})
		return
	}
	companions, validation := p.validateWorkspaceCompanions([]workspaceCompanionRequest{body})
	if validation != "" {
		writeJSON(w, 400, map[string]string{"error": validation})
		return
	}
	desired := companions[0]
	var saved *dev.PersonalWorkspace
	code, message := 0, ""
	created := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		item, status, msg := p.getAuthorizedWorkspace(r, ident)
		if status != 0 {
			code, message = status, msg
			return nil
		}
		if item.Spec.Owner != ident.UserID {
			code, message = 403, "只有工作区主人可以确认创建搭档"
			return nil
		}
		for _, existing := range item.Spec.Companions {
			if existing.Name != desired.Name {
				continue
			}
			if existing.DisplayName != desired.DisplayName || existing.Model != desired.Model || existing.Persona != desired.Persona {
				code, message = 409, "这位搭档已存在，配置不同；请刷新后查看"
				return nil
			}
			saved = item
			return nil
		}
		if len(item.Spec.Companions) >= 10 {
			code, message = 409, "一台云电脑最多容纳 10 位搭档"
			return nil
		}
		previous := item.Spec.Companions
		item.Spec.Companions = append(item.Spec.Companions, desired)
		initializeContacts(item, previous)
		if err := p.k8s.Update(r.Context(), item); err != nil {
			return err
		}
		saved, created = item, true
		return nil
	})
	if err != nil {
		writeJSON(w, statusOf(err), map[string]string{"error": err.Error()})
		return
	}
	if code != 0 {
		writeJSON(w, code, map[string]string{"error": message})
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"workspace": summarizeWorkspace(saved), "companion": desired, "created": created})
}

// A stored CR is not proof that its new roster reached the runtime.
func (p *portal) workspaceCompanionReady(w http.ResponseWriter, r *http.Request, ident identity) {
	item, code, message := p.getAuthorizedWorkspace(r, ident)
	if code != 0 {
		writeJSON(w, code, map[string]string{"error": message})
		return
	}
	name := r.PathValue("companion")
	_, code, message = p.authorizeWorkspaceChat(r.Context(), item.Name, name, ident)
	if code != 0 {
		if code == 409 || code == 503 {
			writeJSON(w, 200, map[string]bool{"ready": false})
			return
		}
		writeJSON(w, code, map[string]string{"error": message})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.workspaceChatBaseURL(item)+"/healthz", nil)
	if err != nil {
		writeJSON(w, 200, map[string]bool{"ready": false})
		return
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		writeJSON(w, 200, map[string]bool{"ready": false})
		return
	}
	defer func() { _ = response.Body.Close() }()
	var health struct {
		Companions []struct {
			Name string `json:"name"`
		} `json:"companions"`
	}
	ready := false
	if response.StatusCode == 200 && json.NewDecoder(http.MaxBytesReader(w, response.Body, 65536)).Decode(&health) == nil {
		for _, entry := range health.Companions {
			if entry.Name == name {
				ready = true
				break
			}
		}
	}
	writeJSON(w, 200, map[string]bool{"ready": ready})
}
