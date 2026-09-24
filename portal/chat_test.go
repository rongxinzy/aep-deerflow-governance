package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
			namespace: "digital-employees",
			policyMode: "approval",
			adminUsers: map[string]bool{"admin-user": true},
		},
		k8s: fake.NewClientBuilder().WithScheme(chatTestScheme(t)).WithRuntimeObjects(objs...).WithStatusSubresource(&dev1alpha1.DigitalEmployee{}).Build(),
		aep: &aepAuthenticator{},
	}
	var seen []*http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Clone(r.Context()))
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
	w := httptest.NewRecorder()
	p.handleChat(w, req, identity{UserID: "user-1", DisplayName: "User One"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(*seen) != 1 {
		t.Fatalf("upstream saw %d requests", len(*seen))
	}
	up := (*seen)[0]
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
