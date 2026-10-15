// Employee mailbox configuration: the employee's own mailbox(es), stored as
// one Secret (EMAIL_MAILBOXES JSON with credentials) that the operator
// envFrom-injects into the runtime, where the governance middleware
// registers the email tools iff the env is present. The portal is the single
// writer: callers never touch the Secret or the CR email stanza directly.
//
// Authorization is stricter than chat visibility: configuring a mailbox is
// management, so only the platform admin or the employee's owner may write.
// Reads mask credentials (hasPassword), mirroring the companion-side surface.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

const (
	emailMailboxesKey    = "EMAIL_MAILBOXES"
	emailSecretSuffix    = "-email"
	maxEmployeeMailboxes = 10
)

var (
	imapPortAllowlist = map[int]bool{143: true, 993: true}
	smtpPortAllowlist = map[int]bool{25: true, 465: true, 587: true}
	mailboxAddressRe  = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// mailboxInput is one mailbox as the API accepts it (password write-only).
type mailboxInput struct {
	Label   string `json:"label"`
	Address string `json:"address"`
	Imap    struct {
		Host string `json:"host"`
		Port int    `json:"port"`
		TLS  *bool  `json:"tls"`
	} `json:"imap"`
	Smtp struct {
		Host string `json:"host"`
		Port int    `json:"port"`
		TLS  *bool  `json:"tls"`
	} `json:"smtp"`
	Username string `json:"username"`
	Password string `json:"password"` // write-only; GET returns hasPassword
}

// mailboxStored is the stored JSON shape (middleware parses this).
type mailboxStored struct {
	Label   string `json:"label"`
	Address string `json:"address"`
	Imap    struct {
		Host string `json:"host"`
		Port int    `json:"port"`
		TLS  bool   `json:"tls"`
	} `json:"imap"`
	Smtp struct {
		Host string `json:"host"`
		Port int    `json:"port"`
		TLS  bool   `json:"tls"`
	} `json:"smtp"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func emailSecretName(employee string) string { return "de-" + employee + emailSecretSuffix }

// validateMailboxes mirrors the runtime-side rules (port allowlists, address
// shape, required fields) so a bad config is rejected before it ever reaches
// a Secret — the middleware would also refuse it, but there it can only log.
func validateMailboxes(inputs []mailboxInput) error {
	if len(inputs) == 0 {
		return fmt.Errorf("至少配置一个邮箱")
	}
	if len(inputs) > maxEmployeeMailboxes {
		return fmt.Errorf("一个员工至多 %d 个邮箱", maxEmployeeMailboxes)
	}
	seen := map[string]bool{}
	for i, box := range inputs {
		at := fmt.Sprintf("mailbox[%d]", i)
		if box.Label == "" || seen[box.Label] {
			return fmt.Errorf("%s.label 必填且不重复", at)
		}
		seen[box.Label] = true
		if !mailboxAddressRe.MatchString(box.Address) {
			return fmt.Errorf("%s.address 必须是合法邮箱", at)
		}
		if box.Imap.Host == "" {
			return fmt.Errorf("%s.imap.host 必填", at)
		}
		if !imapPortAllowlist[box.Imap.Port] {
			return fmt.Errorf("%s.imap.port 只允许 143/993（防运行时被当扫描器）", at)
		}
		if box.Smtp.Host == "" {
			return fmt.Errorf("%s.smtp.host 必填", at)
		}
		if !smtpPortAllowlist[box.Smtp.Port] {
			return fmt.Errorf("%s.smtp.port 只允许 25/465/587", at)
		}
		if box.Username == "" {
			return fmt.Errorf("%s.username 必填", at)
		}
		if box.Password == "" {
			return fmt.Errorf("%s.password 必填", at)
		}
	}
	return nil
}

// handleGetEmployeeEmail returns the employee's mailbox configuration with
// credentials masked. Admin or owner only.
func (p *portal) handleGetEmployeeEmail(w http.ResponseWriter, r *http.Request, ident identity) {
	name := r.PathValue("name")
	employee, code, errMsg := p.authorizeEmailManage(r.Context(), name, ident)
	if code != 0 {
		writeJSON(w, code, map[string]string{"error": errMsg})
		return
	}
	out := []map[string]any{}
	if employee.Spec.Email != nil {
		secret := &corev1.Secret{}
		if err := p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: employee.Spec.Email.SecretName}, secret); err == nil {
			var stored []mailboxStored
			if json.Unmarshal(secret.Data[emailMailboxesKey], &stored) == nil {
				for _, box := range stored {
					out = append(out, map[string]any{
						"label": box.Label, "address": box.Address,
						"imap":     map[string]any{"host": box.Imap.Host, "port": box.Imap.Port, "tls": box.Imap.TLS},
						"smtp":     map[string]any{"host": box.Smtp.Host, "port": box.Smtp.Port, "tls": box.Smtp.TLS},
						"username": box.Username, "hasPassword": box.Password != "",
					})
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"employee": name, "mailboxes": out})
}

// handlePutEmployeeEmail validates, writes the Secret, and points the CR's
// email stanza at it. The operator reconciles envFrom into the runtime; the
// middleware registers the tools on the next rollout.
func (p *portal) handlePutEmployeeEmail(w http.ResponseWriter, r *http.Request, ident identity) {
	name := r.PathValue("name")
	employee, code, errMsg := p.authorizeEmailManage(r.Context(), name, ident)
	if code != 0 {
		writeJSON(w, code, map[string]string{"error": errMsg})
		return
	}
	var body struct {
		Mailboxes []mailboxInput `json:"mailboxes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if err := validateMailboxes(body.Mailboxes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	stored := make([]mailboxStored, 0, len(body.Mailboxes))
	for _, box := range body.Mailboxes {
		var entry mailboxStored
		entry.Label, entry.Address, entry.Username, entry.Password = box.Label, box.Address, box.Username, box.Password
		entry.Imap.Host, entry.Imap.Port, entry.Imap.TLS = box.Imap.Host, box.Imap.Port, box.Imap.TLS == nil || *box.Imap.TLS
		entry.Smtp.Host, entry.Smtp.Port, entry.Smtp.TLS = box.Smtp.Host, box.Smtp.Port, box.Smtp.TLS == nil || *box.Smtp.TLS
		stored = append(stored, entry)
	}
	payload, err := json.Marshal(stored)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	secretName := emailSecretName(name)
	secret := &corev1.Secret{}
	apiErr := p.k8s.Get(r.Context(), client.ObjectKey{Namespace: p.cfg.namespace, Name: secretName}, secret)
	if apierrors.IsNotFound(apiErr) {
		secret = &corev1.Secret{Data: map[string][]byte{}}
		secret.Name, secret.Namespace = secretName, p.cfg.namespace
		secret.Data[emailMailboxesKey] = payload
		if err := p.k8s.Create(r.Context(), secret); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	} else if apiErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": apiErr.Error()})
		return
	} else {
		secret.Data[emailMailboxesKey] = payload
		if err := p.k8s.Update(r.Context(), secret); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	if employee.Spec.Email == nil || employee.Spec.Email.SecretName != secretName {
		employee.Spec.Email = &dev1alpha1.EmailSpec{SecretName: secretName}
		if err := p.k8s.Update(r.Context(), employee); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"employee": name, "secretName": secretName, "mailboxes": len(stored)})
}

// authorizeEmailManage resolves the employee and enforces the management
// boundary: platform admin or the employee's owner — mere team visibility
// (chat access) does not confer mailbox configuration.
func (p *portal) authorizeEmailManage(ctx context.Context, name string, ident identity) (*dev1alpha1.DigitalEmployee, int, string) {
	var employee dev1alpha1.DigitalEmployee
	if err := p.k8s.Get(ctx, client.ObjectKey{Namespace: p.cfg.namespace, Name: name}, &employee); err != nil {
		return nil, statusOf(err), err.Error()
	}
	if !ident.isAdmin(p.cfg) && employee.Spec.Owner != ident.UserID {
		return nil, http.StatusForbidden, "mailbox configuration requires the platform admin or the employee owner"
	}
	return &employee, 0, ""
}
