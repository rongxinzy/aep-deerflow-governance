package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

const (
	// runtimeCredsSuffix names the per-employee runtime credentials Secret:
	// de-<name>-runtime-creds holds the instance admin password and the
	// owner account's PAT.
	runtimeCredsSuffix = "-runtime-creds"

	runtimeAdminEmailKey    = "RUNTIME_ADMIN_EMAIL"
	runtimeAdminPasswordKey = "RUNTIME_ADMIN_PASSWORD"
	runtimeOwnerEmailKey    = "RUNTIME_OWNER_EMAIL"
	runtimeOwnerUserIDKey   = "RUNTIME_OWNER_USER_ID"
	runtimeOwnerPATKey      = "RUNTIME_OWNER_PAT"

	// runtimeAccountRetry waits for a runtime that is not answering yet;
	// terminal states requeue slowly so they self-heal (image upgrade,
	// Secret repair) without watch-event dependencies.
	runtimeAccountRetry   = 15 * time.Second
	runtimeAccountRequeue = 5 * time.Minute
)

// runtimeOwnerScopes is the full end-user surface: threads, runs, projects.
var runtimeOwnerScopes = []string{
	"threads:read", "threads:write", "threads:delete",
	"runs:create", "runs:read", "runs:cancel",
	"projects:read", "projects:write", "projects:delete",
}

var (
	// errRuntimeNoAuthAPI: the image predates the auth API entirely.
	errRuntimeNoAuthAPI    = errors.New("runtime does not expose the auth API")
	errRuntimeAlreadyInit  = errors.New("runtime already initialized")
	errRuntimeAuthRejected = errors.New("runtime rejected the admin credentials")
)

// runtimeClient talks to one DeerFlow runtime's auth API. The cookie jar
// carries the admin session (access_token + csrf_token) across calls —
// provisioning is admin-session-only by design, so the operator logs in
// like an interactive admin for the one call that needs it.
type runtimeClient struct {
	base string
	http *http.Client
	jar  *cookiejar.Jar
}

func newRuntimeClient(base string) (*runtimeClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &runtimeClient{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 10 * time.Second, Jar: jar},
		jar:  jar,
	}, nil
}

func (c *runtimeClient) do(ctx context.Context, method, path, contentType string, body io.Reader, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, payload, err
}

// setupStatus reports whether the runtime still needs its first admin.
func (c *runtimeClient) setupStatus(ctx context.Context) (bool, error) {
	code, body, err := c.do(ctx, http.MethodGet, "/api/v1/auth/setup-status", "", nil, nil)
	if err != nil {
		return false, err
	}
	if code == http.StatusNotFound {
		return false, errRuntimeNoAuthAPI
	}
	if code != http.StatusOK {
		return false, fmt.Errorf("setup-status: status %d: %s", code, snippet(body))
	}
	var parsed struct {
		NeedsSetup bool `json:"needs_setup"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false, fmt.Errorf("setup-status: decode: %w", err)
	}
	return parsed.NeedsSetup, nil
}

// initialize creates the first admin on a fresh auth DB.
func (c *runtimeClient) initialize(ctx context.Context, email, password string) error {
	payload, _ := json.Marshal(map[string]string{"email": email, "password": password})
	code, body, err := c.do(ctx, http.MethodPost, "/api/v1/auth/initialize", "application/json", bytes.NewReader(payload), nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusCreated:
		return nil
	case http.StatusConflict:
		return errRuntimeAlreadyInit
	default:
		return fmt.Errorf("initialize: status %d: %s", code, snippet(body))
	}
}

// login exchanges the admin password for a session (cookies in the jar).
func (c *runtimeClient) login(ctx context.Context, email, password string) error {
	form := url.Values{"username": {email}, "password": {password}}
	code, body, err := c.do(ctx, http.MethodPost, "/api/v1/auth/login/local",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return errRuntimeAuthRejected
	default:
		return fmt.Errorf("login: status %d: %s", code, snippet(body))
	}
}

// csrfToken reads the double-submit cookie set by the login response.
func (c *runtimeClient) csrfToken() string {
	parsed, err := url.Parse(c.base)
	if err != nil {
		return ""
	}
	for _, cookie := range c.jar.Cookies(parsed) {
		if cookie.Name == "csrf_token" {
			return cookie.Value
		}
	}
	return ""
}

type runtimeProvisioned struct {
	UserID string
	PAT    string
}

// provision creates a PAT-only account and returns its first PAT. A non-201
// code is returned with the body for the caller to classify (400 = account
// exists, 404 = image predates the endpoint).
func (c *runtimeClient) provision(ctx context.Context, email string, scopes []string) (runtimeProvisioned, int, []byte, error) {
	payload, _ := json.Marshal(map[string]any{
		"email":    email,
		"password": nil,
		"auto_pat": map[string]any{"name": "operator", "scopes": scopes},
	})
	code, body, err := c.do(ctx, http.MethodPost, "/api/v1/auth/provision", "application/json",
		bytes.NewReader(payload), map[string]string{"X-CSRF-Token": c.csrfToken()})
	if err != nil {
		return runtimeProvisioned{}, 0, nil, err
	}
	if code != http.StatusCreated {
		return runtimeProvisioned{}, code, body, nil
	}
	var parsed struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		PAT *struct {
			Token string `json:"token"`
		} `json:"pat"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return runtimeProvisioned{}, code, body, fmt.Errorf("provision: decode: %w", err)
	}
	if parsed.PAT == nil || parsed.PAT.Token == "" {
		return runtimeProvisioned{}, code, body, errors.New("provision: response carries no PAT")
	}
	return runtimeProvisioned{UserID: parsed.User.ID, PAT: parsed.PAT.Token}, code, body, nil
}

// patValid probes a PAT: /me refuses non-session auth by design, so 403
// means the token authenticated (and the route denied it); 401 means dead.
func (c *runtimeClient) patValid(ctx context.Context, pat string) bool {
	code, _, err := c.do(ctx, http.MethodGet, "/api/v1/auth/me", "", nil,
		map[string]string{"Authorization": "Bearer " + pat})
	return err == nil && code == http.StatusForbidden
}

// runtimeURL resolves the auth API base for one employee.
func (r *DigitalEmployeeReconciler) runtimeURL(employee *v1alpha1.DigitalEmployee) string {
	if r.RuntimeURLFn != nil {
		return r.RuntimeURLFn(employee.Namespace, employee.Name)
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:8001", serviceName(employee), employee.Namespace)
}

// ensureRuntimeAccount bootstraps the runtime's auth state: the per-instance
// admin (operator-held) and the employee owner account with a PAT, persisted
// in de-<name>-runtime-creds. The runtime home volume is an EmptyDir, so a
// pod restart wipes the auth DB — the flow rebuilds from the Secret: the
// admin password is minted once and re-initializes the fresh DB, and the
// owner PAT is re-minted because the reset killed the old one. Outcomes
// land in status.runtimeAccount; only the caller persists status.
func (r *DigitalEmployeeReconciler) ensureRuntimeAccount(ctx context.Context, employee *v1alpha1.DigitalEmployee) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	secret := &corev1.Secret{ObjectMeta: objectMeta(runtimeCredsSecretName(employee), employee.Namespace)}
	if err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	adminEmail := string(secret.Data[runtimeAdminEmailKey])
	adminPassword := string(secret.Data[runtimeAdminPasswordKey])
	ownerEmail := string(secret.Data[runtimeOwnerEmailKey])
	ownerPAT := string(secret.Data[runtimeOwnerPATKey])
	if ownerEmail == "" {
		ownerEmail = runtimeOwnerEmail(employee)
	}

	rt, err := newRuntimeClient(r.runtimeURL(employee))
	if err != nil {
		return ctrl.Result{}, err
	}

	needsSetup, err := rt.setupStatus(ctx)
	switch {
	case errors.Is(err, errRuntimeNoAuthAPI):
		employee.Status.RuntimeAccount = v1alpha1.RuntimeAccountUnsupported
		employee.Status.RuntimeAccountMessage = "runtime image does not expose the auth API; upgrade the runtime image to get runtime accounts"
		return ctrl.Result{RequeueAfter: runtimeAccountRequeue}, nil
	case err != nil:
		// Not answering yet (still starting, transient network). Not an
		// error: the workload phase stays Ready, this sub-state retries.
		employee.Status.RuntimeAccount = v1alpha1.RuntimeAccountPending
		employee.Status.RuntimeAccountMessage = "runtime auth API unreachable: " + err.Error()
		return ctrl.Result{RequeueAfter: runtimeAccountRetry}, nil
	}

	if needsSetup {
		// Fresh (or wiped) auth DB. Stored credentials are authoritative:
		// reuse them so pod restarts converge instead of locking us out.
		if adminEmail == "" || adminPassword == "" {
			adminEmail = runtimeAdminEmail(employee)
			adminPassword = randomToken()
		}
		if err := rt.initialize(ctx, adminEmail, adminPassword); err != nil && !errors.Is(err, errRuntimeAlreadyInit) {
			employee.Status.RuntimeAccount = v1alpha1.RuntimeAccountFailed
			employee.Status.RuntimeAccountMessage = fmt.Sprintf("initialize runtime admin: %v", err)
			return ctrl.Result{RequeueAfter: runtimeAccountRequeue}, nil
		}
	} else if adminEmail == "" || adminPassword == "" {
		// Initialized runtime, no stored credentials: the Secret was lost
		// after the DB picked a different admin. Manual repair required.
		employee.Status.RuntimeAccount = v1alpha1.RuntimeAccountFailed
		employee.Status.RuntimeAccountMessage = "runtime already initialized but " + runtimeCredsSecretName(employee) + " holds no admin credentials; delete the runtime pod to rebuild the auth DB"
		return ctrl.Result{RequeueAfter: runtimeAccountRequeue}, nil
	}

	if err := rt.login(ctx, adminEmail, adminPassword); err != nil {
		if errors.Is(err, errRuntimeAuthRejected) {
			employee.Status.RuntimeAccount = v1alpha1.RuntimeAccountFailed
			employee.Status.RuntimeAccountMessage = "runtime rejected the stored admin credentials (password rotated?); restore " + runtimeAdminPasswordKey + " in " + runtimeCredsSecretName(employee) + " or delete the runtime pod to rebuild the auth DB"
			return ctrl.Result{RequeueAfter: runtimeAccountRequeue}, nil
		}
		return ctrl.Result{}, errWrap("runtime admin login: ", err)
	}

	// Zero-write convergence: a PAT that still authenticates is left alone.
	if ownerPAT != "" && !needsSetup && rt.patValid(ctx, ownerPAT) {
		employee.Status.RuntimeAccount = v1alpha1.RuntimeAccountReady
		employee.Status.RuntimeAccountMessage = ""
		return ctrl.Result{}, nil
	}

	provisioned, code, body, err := rt.provision(ctx, ownerEmail, runtimeOwnerScopes)
	if err != nil {
		return ctrl.Result{}, errWrap("provision runtime owner: ", err)
	}
	switch code {
	case http.StatusCreated:
	case http.StatusBadRequest:
		// The account survived but its PAT does not validate (revoked, or
		// mixed-generation DB). /provision cannot re-mint for an existing
		// account — surface for manual rotation or a DB rebuild.
		employee.Status.RuntimeAccount = v1alpha1.RuntimeAccountFailed
		employee.Status.RuntimeAccountMessage = fmt.Sprintf("owner account %s exists but its PAT no longer validates; rotate the PAT or delete the runtime pod to rebuild the auth DB (%s)", ownerEmail, snippet(body))
		return ctrl.Result{RequeueAfter: runtimeAccountRequeue}, nil
	case http.StatusNotFound:
		employee.Status.RuntimeAccount = v1alpha1.RuntimeAccountUnsupported
		employee.Status.RuntimeAccountMessage = "runtime image predates the provision API; upgrade the runtime image to get runtime accounts"
		return ctrl.Result{RequeueAfter: runtimeAccountRequeue}, nil
	default:
		return ctrl.Result{}, fmt.Errorf("provision runtime owner: status %d: %s", code, snippet(body))
	}

	data := map[string][]byte{
		runtimeAdminEmailKey:    []byte(adminEmail),
		runtimeAdminPasswordKey: []byte(adminPassword),
		runtimeOwnerEmailKey:    []byte(ownerEmail),
		runtimeOwnerUserIDKey:   []byte(provisioned.UserID),
		runtimeOwnerPATKey:      []byte(provisioned.PAT),
	}
	// Same write discipline as ensureMemorySecret: canonical Data, create
	// then update-on-conflict.
	desired := &corev1.Secret{ObjectMeta: objectMeta(runtimeCredsSecretName(employee), employee.Namespace), Data: data}
	if err := controllerutil.SetControllerReference(employee, desired, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	switch err := r.Create(ctx, desired); {
	case err == nil:
	case apierrors.IsAlreadyExists(err):
		secret.Data = data
		if err := r.Update(ctx, secret); err != nil {
			return ctrl.Result{}, errWrap("persist runtime credentials: ", err)
		}
	default:
		return ctrl.Result{}, errWrap("persist runtime credentials: ", err)
	}

	employee.Status.RuntimeAccount = v1alpha1.RuntimeAccountReady
	employee.Status.RuntimeAccountMessage = ""
	logger.Info("runtime account provisioned", "employee", employee.Name, "owner", ownerEmail)
	return ctrl.Result{}, nil
}

func runtimeCredsSecretName(employee *v1alpha1.DigitalEmployee) string {
	return "de-" + employee.Name + runtimeCredsSuffix
}

// runtimeAdminEmail derives the operator-held admin login. `.internal` is a
// resolvable-free suffix DeerFlow's email validation accepts (`.local` is
// special-use and rejected).
func runtimeAdminEmail(employee *v1alpha1.DigitalEmployee) string {
	return "admin@" + employee.Name + ".runtime.internal"
}

func runtimeOwnerEmail(employee *v1alpha1.DigitalEmployee) string {
	return "de-" + employee.Name + "@runtime.internal"
}

func snippet(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) > 200 {
		trimmed = trimmed[:200] + "…"
	}
	return trimmed
}
