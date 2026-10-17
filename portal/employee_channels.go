package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

const channelAdminHeader = "X-Zhiyuan-Channel-Admin"

var channelProviders = []string{"feishu", "wecom", "wechat"}
var channelSessionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
var channelVerifyPattern = regexp.MustCompile(`^[0-9]{1,16}$`)

type employeeChannel struct {
	Provider     string            `json:"provider"`
	Enabled      bool              `json:"enabled"`
	RuntimeReady bool              `json:"runtimeReady"`
	Configured   bool              `json:"configured"`
	WorkerState  string            `json:"workerState"`
	Values       map[string]string `json:"values"`
	HasSecret    bool              `json:"hasSecret"`
}

// CRs own feature gates; credentials stay in the existing runtime PVC store.
func (p *portal) channelEmployee(w http.ResponseWriter, r *http.Request, ident identity) *dev1alpha1.DigitalEmployee {
	w.Header().Set("Cache-Control", "no-store")
	if !ident.isAdmin(p.cfg) || ident.UserID == "" || (p.cfg.aepDeploymentID != "" && ident.DeploymentID != p.cfg.aepDeploymentID) {
		p.managedWriteError(w, 403, "ADMIN_REQUIRED", "需要当前企业管理员权限", "")
		return nil
	}
	name := r.PathValue("name")
	if !namePattern.MatchString(name) {
		p.managedWriteError(w, 400, "INVALID_REQUEST", "数字员工名称无效", "")
		return nil
	}
	if provider := r.PathValue("provider"); provider != "" && !slices.Contains(channelProviders, provider) {
		p.managedWriteError(w, 400, "CHANNEL_PROVIDER_UNSUPPORTED", "不支持的渠道类型", "")
		return nil
	}
	var employee dev1alpha1.DigitalEmployee
	if err := p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: name}, &employee); err != nil {
		p.managedWriteError(w, statusOf(err), "EMPLOYEE_UNAVAILABLE", "无法读取数字员工", "")
		return nil
	}
	return &employee
}

func channelEnabled(spec *dev1alpha1.ChannelsSpec, provider string) bool {
	if spec == nil {
		return false
	}
	switch provider {
	case "feishu":
		return spec.Feishu
	case "wecom":
		return spec.Wecom
	case "wechat":
		return spec.Wechat
	}
	return false
}

// Fixed adapter: caller URLs, paths, query and auth headers are never forwarded.
func (p *portal) runtimeChannelRequest(ctx context.Context, employee, user, method, path string, body any) ([]byte, int, error) {
	var secret corev1.Secret
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: memkeySecretName(employee)}, &secret); err != nil {
		return nil, 503, errors.New("runtime credential unavailable")
	}
	token := strings.TrimSpace(string(secret.Data["DEER_FLOW_INTERNAL_AUTH_TOKEN"]))
	if token == "" {
		return nil, 503, errors.New("runtime credential unavailable")
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, 400, err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, p.runtimeBaseURL(employee)+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 502, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-DeerFlow-Internal-Token", token)
	request.Header.Set(ownerHeaderName, user)
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte("zhiyuan-channel-admin-v1\n" + method + "\n" + path + "\n" + user))
	request.Header.Set(channelAdminHeader, hex.EncodeToString(mac.Sum(nil)))
	upstream := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := upstream.Do(request)
	if err != nil {
		return nil, 502, err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, 502, errors.New("invalid runtime response")
	}
	return data, response.StatusCode, nil
}

func (p *portal) handleEmployeeChannels(w http.ResponseWriter, r *http.Request, ident identity) {
	employee := p.channelEmployee(w, r, ident)
	if employee == nil {
		return
	}
	items := make([]employeeChannel, 0, len(channelProviders))
	hibernating := employee.Spec.Hibernation != nil && employee.Spec.Hibernation.Enabled != nil && *employee.Spec.Hibernation.Enabled
	for _, provider := range channelProviders {
		items = append(items, employeeChannel{Provider: provider, Enabled: channelEnabled(employee.Spec.Channels, provider), WorkerState: "unknown", Values: map[string]string{}})
	}
	// A config read must not wake the employee or invent a platform status.
	if employee.Status.Phase != dev1alpha1.PhaseReady {
		writeJSON(w, 200, map[string]any{"employee": employee.Name, "phase": employee.Status.Phase, "providers": items, "runtimeAvailable": false, "hibernationEnabled": hibernating})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	data, status, err := p.runtimeChannelRequest(ctx, employee.Name, ident.UserID, "GET", "/api/channels/providers", nil)
	var result struct {
		Enabled   *bool `json:"enabled"`
		Providers []struct {
			Provider   string            `json:"provider"`
			Enabled    bool              `json:"enabled"`
			Configured bool              `json:"configured"`
			Values     map[string]string `json:"credential_values"`
		} `json:"providers"`
	}
	available := err == nil && status == 200 && json.Unmarshal(data, &result) == nil && result.Enabled != nil && result.Providers != nil
	if available {
		for i := range items {
			for _, provider := range result.Providers {
				if items[i].Provider != provider.Provider {
					continue
				}
				items[i].RuntimeReady = *result.Enabled && provider.Enabled
				items[i].Configured = provider.Configured
				switch provider.Provider {
				case "feishu":
					items[i].Values["app_id"] = provider.Values["app_id"]
					items[i].HasSecret = provider.Values["app_secret"] != ""
				case "wecom":
					items[i].Values["bot_id"] = provider.Values["bot_id"]
					items[i].HasSecret = provider.Values["bot_secret"] != ""
				case "wechat":
					items[i].HasSecret = provider.Values["bot_token"] != ""
				}
			}
		}
		state, stateStatus, stateErr := p.runtimeChannelRequest(ctx, employee.Name, ident.UserID, "GET", "/api/channels/", nil)
		var workers struct {
			Service  *bool `json:"service_running"`
			Channels map[string]struct {
				Running   *bool `json:"running"`
				Connected *bool `json:"connected"`
			} `json:"channels"`
		}
		if stateErr == nil && stateStatus == 200 && json.Unmarshal(state, &workers) == nil && workers.Service != nil {
			for i := range items {
				if running := workers.Channels[items[i].Provider].Running; running != nil {
					items[i].WorkerState = "stopped"
					if *workers.Service && *running {
						items[i].WorkerState = "running"
						if connected := workers.Channels[items[i].Provider].Connected; connected != nil && *connected {
							items[i].WorkerState = "connected"
						}
					}
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"employee": employee.Name, "phase": employee.Status.Phase, "providers": items, "runtimeAvailable": available, "hibernationEnabled": hibernating, "checkedAt": time.Now().UTC()})
}

// Wake uses the existing activity/KEDA path without changing deployment settings.
func (p *portal) handleWakeEmployeeChannels(w http.ResponseWriter, r *http.Request, ident identity) {
	employee := p.channelEmployee(w, r, ident)
	if employee == nil {
		return
	}
	if employee.Status.Phase != dev1alpha1.PhaseSleeping && employee.Status.Phase != dev1alpha1.PhaseReady {
		p.managedWriteError(w, 409, "CHANNEL_RUNTIME_NOT_READY", "数字员工尚未完成部署，请稍后重试", "")
		return
	}
	p.markActivity(employee.Name)
	if code, _ := p.wakeRuntime(r.Context(), employee.Name); code != 0 {
		p.managedWriteError(w, code, "CHANNEL_WAKE_PENDING", "数字员工仍在唤醒，请稍后刷新状态", "")
		return
	}
	writeJSON(w, 202, map[string]bool{"accepted": true})
}

// Provider activation is a separate credential-free step before runtime setup.
func (p *portal) handleActivateEmployeeChannel(w http.ResponseWriter, r *http.Request, ident identity) {
	employee := p.channelEmployee(w, r, ident)
	if employee == nil {
		return
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if decodeStrictJSON(w, r, &input, 1024) != nil || input.Enabled == nil {
		p.managedWriteError(w, 400, "INVALID_REQUEST", "需要 enabled 布尔值", "")
		return
	}
	provider := r.PathValue("provider")
	if *input.Enabled && employee.Spec.Hibernation != nil && employee.Spec.Hibernation.Enabled != nil && *employee.Spec.Hibernation.Enabled {
		p.managedWriteError(w, 409, "CHANNEL_HIBERNATION_CONFLICT", "内置渠道需要数字员工常驻，请先关闭此员工的休眠配置再启用接入", "")
		return
	}
	// Stop/remove the independent runtime-store entry before clearing the CR.
	if !*input.Enabled {
		// An incompatible historic spec may already be in Error while its
		// runtime remains reachable. Allow removal so the UI can recover it.
		if employee.Status.Phase != dev1alpha1.PhaseReady && employee.Status.Phase != dev1alpha1.PhaseError {
			p.managedWriteError(w, 409, "CHANNEL_RUNTIME_NOT_READY", "请先唤醒数字员工并等待就绪，再停用渠道", "")
			return
		}
		_, status, err := p.runtimeChannelRequest(r.Context(), employee.Name, ident.UserID, "DELETE", "/api/channels/"+provider+"/runtime-config", nil)
		if err != nil || status != 200 {
			p.channelRuntimeError(w, status)
			return
		}
	}
	base := employee.DeepCopy()
	if employee.Spec.Channels == nil {
		employee.Spec.Channels = &dev1alpha1.ChannelsSpec{}
	}
	switch provider {
	case "feishu":
		employee.Spec.Channels.Feishu = *input.Enabled
	case "wecom":
		employee.Spec.Channels.Wecom = *input.Enabled
	case "wechat":
		employee.Spec.Channels.Wechat = *input.Enabled
	}
	if err := p.k8s.Patch(r.Context(), employee, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		p.managedWriteError(w, statusOf(err), "CHANNEL_CONFIG_UPDATE_FAILED", "渠道配置更新失败，请刷新后重试；停用时连接可能已停止", "")
		return
	}
	p.channelAudit(ident, employee.Name, provider, "activation", 202)
	writeJSON(w, 202, map[string]any{"enabled": *input.Enabled, "pending": true})
}

func (p *portal) handleEmployeeChannelAction(w http.ResponseWriter, r *http.Request, ident identity) {
	employee := p.channelEmployee(w, r, ident)
	if employee == nil {
		return
	}
	provider, action := r.PathValue("provider"), r.PathValue("action")
	if !channelEnabled(employee.Spec.Channels, provider) {
		p.managedWriteError(w, 409, "CHANNEL_NOT_ENABLED", "请先启用此数字员工的渠道接入", "")
		return
	}
	if employee.Status.Phase != dev1alpha1.PhaseReady {
		p.managedWriteError(w, 409, "CHANNEL_RUNTIME_NOT_READY", "数字员工尚未就绪，请唤醒或等待配置生效", "")
		return
	}
	path := "/api/channels/" + provider + "/"
	var payload any
	switch action {
	case "runtime-config":
		var input struct {
			Values map[string]string `json:"values"`
		}
		if decodeStrictJSON(w, r, &input, 16<<10) != nil {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "渠道参数无效", "")
			return
		}
		allowed := map[string][]string{"feishu": {"app_id", "app_secret"}, "wecom": {"bot_id", "bot_secret"}, "wechat": {"bot_token"}}[provider]
		for key, value := range input.Values {
			if !slices.Contains(allowed, key) || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
				p.managedWriteError(w, 400, "INVALID_REQUEST", "渠道字段或长度无效", "")
				return
			}
		}
		payload = input
	case "restart":
		payload = nil
	case "qr-login":
		if provider != "wechat" {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "只有微信支持扫码接入", "")
			return
		}
	case "qr-poll", "qr-cancel":
		if provider != "wechat" {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "只有微信支持扫码接入", "")
			return
		}
		var input struct {
			ID         string `json:"id"`
			VerifyCode string `json:"verifyCode,omitempty"`
		}
		if decodeStrictJSON(w, r, &input, 1024) != nil || !channelSessionPattern.MatchString(input.ID) || (input.VerifyCode != "" && !channelVerifyPattern.MatchString(input.VerifyCode)) {
			p.managedWriteError(w, 400, "INVALID_REQUEST", "扫码会话或验证码无效", "")
			return
		}
		path += "qr-login/" + input.ID
		if action == "qr-poll" {
			path += "/poll"
			values := map[string]string{}
			if input.VerifyCode != "" {
				values["verify_code"] = input.VerifyCode
			}
			payload = values
		}
	default:
		p.managedWriteError(w, 404, "INVALID_REQUEST", "未知渠道操作", "")
		return
	}
	if action != "qr-poll" && action != "qr-cancel" {
		path += action
	}
	method := "POST"
	if action == "qr-cancel" {
		method = "DELETE"
	}
	data, status, err := p.runtimeChannelRequest(r.Context(), employee.Name, ident.UserID, method, path, payload)
	p.channelAudit(ident, employee.Name, provider, action, status)
	if err != nil || (status != 200 && status != 204) {
		p.channelRuntimeError(w, status)
		return
	}
	if strings.HasPrefix(action, "qr-") && action != "qr-cancel" {
		var qr struct {
			ID      string `json:"id"`
			Status  string `json:"status"`
			Content string `json:"qrcode_content"`
			Expires int    `json:"expires_in"`
		}
		if json.Unmarshal(data, &qr) != nil || !channelSessionPattern.MatchString(qr.ID) || len(qr.Content) > 8192 || !slices.Contains([]string{"pending", "scanned", "verification_required", "confirmed", "expired", "failed"}, qr.Status) {
			p.channelRuntimeError(w, 502)
			return
		}
		// The nested provider and credentials are intentionally not forwarded.
		writeJSON(w, 200, qr)
		return
	}
	if action == "restart" {
		var result struct {
			Success bool `json:"success"`
		}
		if json.Unmarshal(data, &result) != nil || !result.Success {
			p.channelRuntimeError(w, 502)
			return
		}
	}
	writeJSON(w, 200, map[string]bool{"accepted": true})
}

func (p *portal) channelRuntimeError(w http.ResponseWriter, status int) {
	code, message := "CHANNEL_RUNTIME_UNAVAILABLE", "渠道运行时不可用，请检查服务状态后重试"
	if status == 400 || status == 409 {
		code, message = "CHANNEL_CONFIGURATION_REJECTED", "接入未完成，请检查凭据、平台权限及运行时配置是否生效"
	}
	if status == 403 {
		code, message = "CHANNEL_RUNTIME_UPGRADE_REQUIRED", "运行时未接受渠道管理授权，请更新配套运行时版本"
	}
	if status == 404 {
		code, message = "CHANNEL_RUNTIME_UPGRADE_REQUIRED", "运行时不支持此操作或扫码会话已失效，请更新版本或重新接入"
	}
	if status < 400 || status > 599 {
		status = 502
	}
	// Raw SDK errors can contain credentials or private URLs.
	p.managedWriteError(w, status, code, message, "")
}

func (p *portal) channelAudit(ident identity, employee, provider, action string, status int) {
	if p.logger != nil && action != "qr-poll" {
		p.logger.Info("channel management", "actorUserId", ident.UserID, "deploymentId", ident.DeploymentID, "employee", employee, "provider", provider, "action", action, "status", status)
	}
}
