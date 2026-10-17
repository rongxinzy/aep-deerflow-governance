package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// The chat proxy tests use plain testing + assert-style helpers per the
// workspace Go testing discipline (portal is outside the operator's
// Ginkgo rule).

func chatTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := dev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func chatTestPortal(t *testing.T, objs ...runtime.Object) (*portal, *httptest.Server, *[]*http.Request) {
	t.Helper()
	p := &portal{
		cfg: config{
			namespace:  "digital-employees",
			policyMode: "approval",
			adminUsers: map[string]bool{"admin-user": true},
		},
		k8s: fake.NewClientBuilder().WithScheme(chatTestScheme(t)).WithRuntimeObjects(objs...).WithStatusSubresource(&dev1alpha1.DigitalEmployee{}).Build(),
		aep: &aepAuthenticator{},
	}
	var seen []*http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cloned := r.Clone(r.Context())
		cloned.Body = io.NopCloser(bytes.NewReader(body))
		seen = append(seen, cloned)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	p.chatBaseOverride = upstream.URL
	return p, upstream, &seen
}

func readyEmployee(name, owner string) *dev1alpha1.DigitalEmployee {
	return &dev1alpha1.DigitalEmployee{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "digital-employees"},
		Spec:       dev1alpha1.DigitalEmployeeSpec{Owner: owner},
		Status:     dev1alpha1.DigitalEmployeeStatus{Phase: dev1alpha1.PhaseReady},
	}
}

func memkeySecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "de-" + name + "-memkey", Namespace: "digital-employees"},
		Data:       map[string][]byte{"DEER_FLOW_INTERNAL_AUTH_TOKEN": []byte("tok-123")},
	}
}

func TestChatProxyStampsInternalIdentity(t *testing.T) {
	p, _, seen := chatTestPortal(t, readyEmployee("sales-helper", "user-1"), memkeySecret("sales-helper"))
	req := httptest.NewRequest("POST", "/api/v1/employees/sales-helper/chat/threads/t1/runs/wait", nil)
	req.SetPathValue("name", "sales-helper")
	req.SetPathValue("rest", "threads/t1/runs/wait")
	req.Header.Set("Authorization", "Bearer browser-token")
	req.Header.Set(channelAdminHeader, "caller-forged-capability")
	w := httptest.NewRecorder()
	p.handleChat(w, req, identity{UserID: "user-1", DisplayName: "User One"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(*seen) != 1 {
		t.Fatalf("upstream saw %d requests", len(*seen))
	}
	up := (*seen)[0]
	if up.Header.Get(channelAdminHeader) != "" {
		t.Error("chat caller forwarded a channel-management capability")
	}
	if got := up.Header.Get("X-DeerFlow-Internal-Token"); got != "tok-123" {
		t.Errorf("internal token not forwarded: %q", got)
	}
	if got := up.Header.Get("X-DeerFlow-Owner-User-Id"); got != "user-1" {
		t.Errorf("owner header = %q, want user-1", got)
	}
	if got := up.Header.Get("Authorization"); got != "" {
		t.Errorf("browser token leaked upstream: %q", got)
	}
	if got := up.URL.Path; got != "/api/threads/t1/runs/wait" {
		t.Errorf("upstream path = %q", got)
	}
}

func TestChatProxyReplacesCallerKnowledgeContextWithSessionBoundHandle(t *testing.T) {
	employee := readyEmployee("sales-helper", "user-1")
	employee.Spec.Knowledge = &dev1alpha1.KnowledgeSpec{Bases: []dev1alpha1.KnowledgeBaseRef{{ID: "kb-1"}}}
	p, _, seen := chatTestPortal(t, employee, memkeySecret("sales-helper"))
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p.db = db
	p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	mock.ExpectExec(`INSERT INTO knowledge_runtime_contexts \(handle_hash,session_token,employee_name,expires_at\) VALUES`).
		WithArgs(sqlmock.AnyArg(), "portal-session", "sales-helper", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM knowledge_runtime_contexts WHERE expires_at < now\(\)`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO employee_activity`).WithArgs("sales-helper").WillReturnResult(sqlmock.NewResult(0, 1))
	request := httptest.NewRequest("POST", "/api/v1/employees/sales-helper/chat/api/threads/t1/runs/wait", strings.NewReader(`{"input":{"messages":[]},"context":{"zhiyuan_knowledge_context":"forged-body-context","client":"ok"},"config":{"context":{"zhiyuan_knowledge_context":"forged-config-context","zhiyuan_employee_name":"forged-employee","request_id":"keep"},"configurable":{"zhiyuan_knowledge_context":"forged-configurable-context","zhiyuan_employee_name":"forged-employee","thread_id":"t1"}}}`))
	request.SetPathValue("name", "sales-helper")
	request.SetPathValue("rest", "api/threads/t1/runs/wait")
	request.AddCookie(&http.Cookie{Name: portalSessionCookie, Value: "portal-session"})
	request.Header.Set(knowledgeContextHeader, "forged-client-context")
	w := httptest.NewRecorder()
	p.handleChat(w, request, identity{UserID: "user-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, sql = %v", w.Code, w.Body.String(), mock.ExpectationsWereMet())
	}
	if len(*seen) != 1 {
		t.Fatalf("upstream saw %d requests", len(*seen))
	}
	got := (*seen)[0].Header.Get(knowledgeContextHeader)
	if got == "" || got == "forged-client-context" || len(got) != 43 {
		t.Errorf("context header = %q, want a fresh opaque handle", got)
	}
	var payload struct {
		Context map[string]any `json:"context"`
		Config  struct {
			Context      map[string]any `json:"context"`
			Configurable map[string]any `json:"configurable"`
		} `json:"config"`
	}
	if err := json.NewDecoder((*seen)[0].Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if runHandle, _ := payload.Context["zhiyuan_knowledge_context"].(string); runHandle != got {
		t.Errorf("run context handle = %q, want proxy-issued opaque handle", runHandle)
	}
	if employeeName, _ := payload.Context["zhiyuan_employee_name"].(string); employeeName != "sales-helper" {
		t.Errorf("run employee context = %q, want server-selected employee", employeeName)
	}
	if payload.Context["client"] != "ok" {
		t.Errorf("unrelated caller context was not preserved: %#v", payload.Context)
	}
	if payload.Config.Context["zhiyuan_knowledge_context"] != got || payload.Config.Context["zhiyuan_employee_name"] != "sales-helper" {
		t.Errorf("runtime config context was not server-bound: %#v", payload.Config.Context)
	}
	if payload.Config.Context["request_id"] != "keep" {
		t.Errorf("unrelated runtime config context was not preserved: %#v", payload.Config.Context)
	}
	if _, ok := payload.Config.Configurable["zhiyuan_knowledge_context"]; ok {
		t.Errorf("forged configurable handle survived: %#v", payload.Config.Configurable)
	}
	if _, ok := payload.Config.Configurable["zhiyuan_employee_name"]; ok {
		t.Errorf("forged configurable employee survived: %#v", payload.Config.Configurable)
	}
	if payload.Config.Configurable["thread_id"] != "t1" {
		t.Errorf("unrelated configurable field was not preserved: %#v", payload.Config.Configurable)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInjectKnowledgeRunContextRejectsMalformedAndLeavesNonRunAlone(t *testing.T) {
	bad := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not-json"))
	if err := injectKnowledgeRunContext(bad, "threads/t1/runs", "opaque", "employee-1"); err == nil {
		t.Fatal("malformed LangGraph run body was accepted")
	}
	ordinary := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not-json"))
	if err := injectKnowledgeRunContext(ordinary, "models", "opaque", "employee-1"); err != nil {
		t.Fatalf("non-run endpoint was modified: %v", err)
	}
	for _, path := range []string{
		"threads/t1/runs/wait",
		"threads/t1/runs/stream",
		"api/threads/t1/runs/wait",
		"api/threads/t1/runs/stream",
		"runs/wait",
		"runs/stream",
		"api/runs/wait",
		"api/runs/stream",
	} {
		t.Run(path, func(t *testing.T) {
			run := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"context":{"client":"ok"}}`))
			if err := injectKnowledgeRunContext(run, path, "opaque", "employee-1"); err != nil {
				t.Fatalf("LangGraph run path was not recognized: %v", err)
			}
			var payload map[string]any
			if err := json.NewDecoder(run.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			contextValue, _ := payload["context"].(map[string]any)
			if contextValue["zhiyuan_knowledge_context"] != "opaque" || contextValue["zhiyuan_employee_name"] != "employee-1" {
				t.Fatalf("LangGraph run context was not injected: %#v", contextValue)
			}
		})
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "non-object config", body: `{"config":[]}`},
		{name: "non-object config context", body: `{"config":{"context":"bad"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			if err := injectKnowledgeRunContext(req, "threads/t1/runs", "opaque", "employee-1"); err == nil {
				t.Fatal("malformed runtime config was accepted")
			}
		})
	}

	for _, body := range []string{
		`{"config":null}`,
		`{"config":{"context":null,"configurable":null}}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if err := injectKnowledgeRunContext(req, "threads/t1/runs", "opaque", "employee-1"); err != nil {
			t.Fatalf("null runtime config fields should be initialized: %v", err)
		}
		var decoded map[string]any
		if err := json.NewDecoder(req.Body).Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		config, _ := decoded["config"].(map[string]any)
		contextValue, _ := config["context"].(map[string]any)
		if contextValue["zhiyuan_knowledge_context"] != "opaque" || contextValue["zhiyuan_employee_name"] != "employee-1" {
			t.Fatalf("server context missing after null normalization: %#v", contextValue)
		}
	}

	nonRunBody := `{"raw":"unchanged"}`
	nonRun := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(nonRunBody))
	if err := injectKnowledgeRunContext(nonRun, "models", "opaque", "employee-1"); err != nil {
		t.Fatalf("non-run endpoint was modified: %v", err)
	}
	gotBody, err := io.ReadAll(nonRun.Body)
	if err != nil || string(gotBody) != nonRunBody {
		t.Fatalf("non-run body changed: got %q, err=%v", gotBody, err)
	}
	for _, path := range []string{"models", "threads/t1/read", "runs/get", "threads/t1/runs/get"} {
		raw := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(nonRunBody))
		if err := injectKnowledgeRunContext(raw, path, "opaque", "employee-1"); err != nil {
			t.Fatalf("non-run path %q was rejected: %v", path, err)
		}
		body, err := io.ReadAll(raw.Body)
		if err != nil || string(body) != nonRunBody {
			t.Fatalf("non-run path %q body changed: %q, err=%v", path, body, err)
		}
	}
	getRun := httptest.NewRequest(http.MethodGet, "/", strings.NewReader(nonRunBody))
	if err := injectKnowledgeRunContext(getRun, "runs/wait", "opaque", "employee-1"); err != nil {
		t.Fatalf("GET run path was rejected: %v", err)
	}
	getBody, err := io.ReadAll(getRun.Body)
	if err != nil || string(getBody) != nonRunBody {
		t.Fatalf("GET run body changed: %q, err=%v", getBody, err)
	}

	oversized := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", maxKnowledgeRunBodyBytes+1)))
	if err := injectKnowledgeRunContext(oversized, "threads/t1/runs", "opaque", "employee-1"); err == nil {
		t.Fatal("oversized knowledge run body was accepted")
	}
}

// The reused DeerFlow frontend builds REST URLs as ${prefix}/api/... — the
// director strips the redundant api/ segment so the runtime sees its native
// path; SDK paths (threads/...) pass through untouched.
func TestChatProxyStripsRedundantApiPrefix(t *testing.T) {
	p, _, seen := chatTestPortal(t, readyEmployee("sales-helper", "user-1"), memkeySecret("sales-helper"))

	req := httptest.NewRequest("GET", "/api/v1/employees/sales-helper/chat/api/models", nil)
	req.SetPathValue("name", "sales-helper")
	req.SetPathValue("rest", "api/models")
	req.Header.Set("Cookie", portalSessionCookie+"=st; "+portalCsrfCookie+"=sc")
	req.Header.Set("X-CSRF-Token", "sc")
	w := httptest.NewRecorder()
	p.handleChat(w, req, identity{UserID: "user-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	up := (*seen)[0]
	if got := up.URL.Path; got != "/api/models" {
		t.Errorf("prefixed path = %q, want /api/models", got)
	}
	if got := up.Header.Get("Cookie"); got != "" {
		t.Errorf("portal session cookie leaked upstream: %q", got)
	}
	if got := up.Header.Get("X-CSRF-Token"); got != "" {
		t.Errorf("portal csrf header leaked upstream: %q", got)
	}

	req = httptest.NewRequest("POST", "/api/v1/employees/sales-helper/chat/threads/t1/runs/stream", nil)
	req.SetPathValue("name", "sales-helper")
	req.SetPathValue("rest", "threads/t1/runs/stream")
	w = httptest.NewRecorder()
	p.handleChat(w, req, identity{UserID: "user-1"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := (*seen)[1].URL.Path; got != "/api/threads/t1/runs/stream" {
		t.Errorf("sdk path = %q, want /api/threads/t1/runs/stream", got)
	}
}

func TestChatProxyRejectsNonOwner(t *testing.T) {
	p, _, _ := chatTestPortal(t, readyEmployee("sales-helper", "user-1"), memkeySecret("sales-helper"))
	req := httptest.NewRequest("POST", "/api/v1/employees/sales-helper/chat/threads", nil)
	req.SetPathValue("name", "sales-helper")
	req.SetPathValue("rest", "threads")
	w := httptest.NewRecorder()
	p.handleChat(w, req, identity{UserID: "stranger"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestChatProxyAdminMayChat(t *testing.T) {
	p, _, seen := chatTestPortal(t, readyEmployee("sales-helper", "user-1"), memkeySecret("sales-helper"))
	req := httptest.NewRequest("POST", "/api/v1/employees/sales-helper/chat/runs/wait", nil)
	req.SetPathValue("name", "sales-helper")
	req.SetPathValue("rest", "runs/wait")
	w := httptest.NewRecorder()
	p.handleChat(w, req, identity{UserID: "admin-user"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if len(*seen) != 1 {
		t.Fatalf("upstream saw %d requests", len(*seen))
	}
}

func TestChatProxyRequiresReady(t *testing.T) {
	pending := readyEmployee("young-emp", "user-1")
	pending.Status.Phase = dev1alpha1.PhasePending
	p, _, _ := chatTestPortal(t, pending, memkeySecret("young-emp"))
	req := httptest.NewRequest("POST", "/api/v1/employees/young-emp/chat/runs/wait", nil)
	req.SetPathValue("name", "young-emp")
	req.SetPathValue("rest", "runs/wait")
	w := httptest.NewRecorder()
	p.handleChat(w, req, identity{UserID: "user-1"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

func TestChatProxyMissingSecret(t *testing.T) {
	p, _, _ := chatTestPortal(t, readyEmployee("sales-helper", "user-1"))
	req := httptest.NewRequest("POST", "/api/v1/employees/sales-helper/chat/runs/wait", nil)
	req.SetPathValue("name", "sales-helper")
	req.SetPathValue("rest", "runs/wait")
	w := httptest.NewRecorder()
	p.handleChat(w, req, identity{UserID: "user-1"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func sleepingEmployee(name, owner string) *dev1alpha1.DigitalEmployee {
	item := readyEmployee(name, owner)
	item.Status.Phase = dev1alpha1.PhaseSleeping
	enabled := true
	item.Spec.Hibernation = &dev1alpha1.HibernationSpec{Enabled: &enabled}
	return item
}

func readyDeployment(name string) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "de-" + name, Namespace: "digital-employees"},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
}

func scaledToZeroDeployment(name string) *appsv1.Deployment {
	zero := int32(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "de-" + name, Namespace: "digital-employees"},
		Spec:       appsv1.DeploymentSpec{Replicas: &zero},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 0},
	}
}

// A Sleeping employee is still authorizable: the memkey persists and the
// portal wakes the runtime before proxying. With the deployment already
// ready (race: someone else woke it first) the request flows straight
// through.
func TestChatProxyWakesSleepingEmployee(t *testing.T) {
	p, _, seen := chatTestPortal(t,
		sleepingEmployee("sales-helper", "user-a"),
		memkeySecret("sales-helper"),
		readyDeployment("sales-helper"),
	)
	req := httptest.NewRequest("POST", "/api/v1/employees/sales-helper/chat/threads", nil)
	req.SetPathValue("name", "sales-helper")
	req.SetPathValue("rest", "threads")
	req.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	p.handleChat(w, req, identity{UserID: "user-a", DisplayName: "User A"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if len(*seen) != 1 {
		t.Fatalf("expected the proxy call to go through, saw %d", len(*seen))
	}
}

// Cold start slower than the wake budget: the caller gets an explicit 503
// explaining the employee is waking, not a bare bad gateway.
func TestChatReportsWakeTimeout(t *testing.T) {
	p, _, _ := chatTestPortal(t,
		sleepingEmployee("sales-helper", "user-a"),
		memkeySecret("sales-helper"),
		scaledToZeroDeployment("sales-helper"),
	)
	p.cfg.wakeTimeout = 1500 * time.Millisecond // one poll tick then bail
	req := httptest.NewRequest("POST", "/api/v1/employees/sales-helper/chat/threads", nil)
	req.SetPathValue("name", "sales-helper")
	req.SetPathValue("rest", "threads")
	req.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	start := time.Now()
	p.handleChat(w, req, identity{UserID: "user-a", DisplayName: "User A"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "waking up") {
		t.Fatalf("body should explain the wake, got %s", w.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("wake wait took %s, timeout not honored", elapsed)
	}
}
