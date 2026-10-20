/* Copyright 2026.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
*/

// Suite wiring: the reconcile specs run against a controller-runtime fake
// client with the OpenViking admin API stubbed by httptest — no envtest
// assets or cluster access required (testing discipline, see AGENTS.md).
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-logr/logr"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// ovStub is an in-memory OpenViking admin API: it records the lifecycle
// calls the operator makes (register / regenerate / delete).
type ovStub struct {
	mu         sync.Mutex
	users      map[string]string // user id → key
	registers  int
	regens     int
	deletes    int
	forceFound bool // behave as if the user pre-exists (returns no key)
	// missingAccounts are accounts OpenViking answers "Account not found" for,
	// the way it does for a department whose provisioning step never ran.
	missingAccounts map[string]bool
	// accountCreates records every admin account creation, in order.
	accountCreates []string
	// failAccountCreate makes account creation fail (the retry then reports
	// both the original 404 and the creation error).
	failAccountCreate bool
}

func newOVStub() *ovStub {
	return &ovStub{users: map[string]string{}, missingAccounts: map[string]bool{}}
}

func (s *ovStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/admin/accounts", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var body struct {
			AccountID string `json:"account_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.accountCreates = append(s.accountCreates, body.AccountID)
		if s.failAccountCreate {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, "account creation failed")
			return
		}
		delete(s.missingAccounts, body.AccountID)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /api/v1/admin/accounts/{account}/users", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var body struct {
			UserID string `json:"user_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if s.missingAccounts[r.PathValue("account")] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, "Account not found: %s", r.PathValue("account"))
			return
		}
		s.registers++
		if _, ok := s.users[body.UserID]; ok || s.forceFound {
			writeOVError(w, "ALREADY_EXISTS", "user exists")
			return
		}
		key := "ovkey-" + body.UserID
		s.users[body.UserID] = key
		writeOVJSON(w, map[string]any{"user_key": key})
	})
	mux.HandleFunc("POST /api/v1/admin/accounts/{account}/users/{user}/key", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.regens++
		key := "ovkey-regen-" + r.PathValue("user")
		s.users[r.PathValue("user")] = key
		writeOVJSON(w, map[string]any{"user_key": key})
	})
	mux.HandleFunc("DELETE /api/v1/admin/accounts/{account}/users/{user}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.deletes++
		delete(s.users, r.PathValue("user"))
		w.WriteHeader(200)
	})
	return mux
}

func writeOVJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"result": body})
}

func writeOVError(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// runtimeStub is an in-memory DeerFlow auth API: setup-status / initialize /
// login / provision / me, with the same session + CSRF double-submit
// discipline the real gateway enforces (provision is admin-session-only).
type runtimeStub struct {
	failMode        bool // answer 5xx on the auth API to exercise failure paths
	glitchProvision bool // user-create answers 201 with a PAT-less body
	mu              sync.Mutex
	needsSetup      bool
	adminEmail      string
	adminPassword   string
	accounts        map[string]string // owner email → live PAT
	userIDs         map[string]string // owner email → runtime user id
	sessions        map[string]string // access_token cookie → session email
	nextID          int
	initializations int
	logins          int
	provisions      int
	failProvision   int  // non-zero: provision answers this status instead
	hideAuthAPI     bool // simulate an image that predates the auth API
	rateLimitLogin  bool // login answers 429 (IP throttle) regardless of credentials
}

func newRuntimeStub() *runtimeStub {
	return &runtimeStub{needsSetup: true, accounts: map[string]string{}, userIDs: map[string]string{}, sessions: map[string]string{}}
}

func (s *runtimeStub) startSession(w http.ResponseWriter, email string) {
	s.nextID++
	token, csrf := fmt.Sprintf("sess-%d", s.nextID), fmt.Sprintf("csrf-%d", s.nextID)
	s.sessions[token] = email
	http.SetCookie(w, &http.Cookie{Name: "access_token", Value: token, Path: "/"})
	http.SetCookie(w, &http.Cookie{Name: "csrf_token", Value: csrf, Path: "/"})
}

func (s *runtimeStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/auth/setup-status", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.failMode {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if s.hideAuthAPI {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeRuntimeJSON(w, http.StatusOK, map[string]any{"needs_setup": s.needsSetup})
	})
	mux.HandleFunc("POST /api/v1/auth/initialize", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.hideAuthAPI {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.initializations++
		if !s.needsSetup {
			writeRuntimeJSON(w, http.StatusConflict, map[string]any{"detail": "already initialized"})
			return
		}
		s.adminEmail, s.adminPassword = body.Email, body.Password
		s.needsSetup = false
		s.startSession(w, body.Email)
		writeRuntimeJSON(w, http.StatusCreated, map[string]any{"id": "admin-1", "email": body.Email, "system_role": "admin"})
	})
	mux.HandleFunc("POST /api/v1/auth/login/local", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = r.ParseForm()
		s.logins++
		if s.rateLimitLogin {
			writeRuntimeJSON(w, http.StatusTooManyRequests, map[string]any{"detail": "Too many login attempts. Try again later."})
			return
		}
		if s.needsSetup || r.Form.Get("username") != s.adminEmail || r.Form.Get("password") != s.adminPassword {
			writeRuntimeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "incorrect email or password"})
			return
		}
		s.startSession(w, s.adminEmail)
		writeRuntimeJSON(w, http.StatusOK, map[string]any{"expires_in": 86400})
	})
	mux.HandleFunc("POST /api/v1/auth/provision", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var access, csrf string
		for _, cookie := range r.Cookies() {
			switch cookie.Name {
			case "access_token":
				access = cookie.Value
			case "csrf_token":
				csrf = cookie.Value
			}
		}
		email, ok := s.sessions[access]
		if !ok || email != s.adminEmail || csrf == "" || r.Header.Get("X-CSRF-Token") != csrf {
			writeRuntimeJSON(w, http.StatusForbidden, map[string]any{"detail": "admin session required"})
			return
		}
		var body struct {
			Email   string `json:"email"`
			AutoPat *struct {
				Name   string   `json:"name"`
				Scopes []string `json:"scopes"`
			} `json:"auto_pat"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.provisions++
		if s.failProvision != 0 {
			w.WriteHeader(s.failProvision)
			return
		}
		if s.glitchProvision {
			writeRuntimeJSON(w, http.StatusCreated, map[string]any{"user": map[string]any{"id": "u-glitch"}})
			return
		}
		if _, exists := s.accounts[body.Email]; exists {
			writeRuntimeJSON(w, http.StatusBadRequest, map[string]any{"detail": map[string]string{"code": "EMAIL_ALREADY_EXISTS"}})
			return
		}
		if body.AutoPat == nil || len(body.AutoPat.Scopes) == 0 {
			writeRuntimeJSON(w, http.StatusBadRequest, map[string]any{"detail": "auto_pat.scopes must be a non-empty list"})
			return
		}
		s.nextID++
		pat := fmt.Sprintf("dfpat-%s-%d", body.Email, s.nextID)
		userID := fmt.Sprintf("u-%d", s.nextID)
		s.accounts[body.Email] = pat
		s.userIDs[body.Email] = userID
		writeRuntimeJSON(w, http.StatusCreated, map[string]any{
			"user": map[string]any{"id": userID, "email": body.Email, "system_role": "user"},
			"pat":  map[string]any{"id": fmt.Sprintf("pat-%d", s.nextID), "token": pat, "scopes": body.AutoPat.Scopes},
		})
	})
	mux.HandleFunc("GET /api/v1/auth/me", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		for _, pat := range s.accounts {
			if pat == bearer && bearer != "" {
				// Authenticated, but the route refuses PATs by design.
				writeRuntimeJSON(w, http.StatusForbidden, map[string]any{"detail": "session required"})
				return
			}
		}
		writeRuntimeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "not authenticated"})
	})
	return mux
}

// resetDB simulates a runtime pod restart: the home volume is an EmptyDir,
// so the whole auth database (admin, accounts, PATs) is gone.
func (s *runtimeStub) resetDB() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.needsSetup = true
	s.adminEmail, s.adminPassword = "", ""
	s.accounts = map[string]string{}
	s.userIDs = map[string]string{}
	s.sessions = map[string]string{}
}

// rotateAdminPassword simulates an out-of-band admin password change.
func (s *runtimeStub) rotateAdminPassword(password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adminPassword = password
}

func writeRuntimeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// fixture wires a reconciler against a fake cluster plus the OpenViking and
// runtime auth stubs.
type fixture struct {
	client       client.Client
	stub         *ovStub
	server       *httptest.Server
	runtime      *runtimeStub
	runtimeSrv   *httptest.Server
	runtimeURL   string // overrides the runtime auth base (dead URL specs)
	wakeDBURL    string // non-empty enables the hibernation wake-db source
	gatewayURL   string // non-empty enables channel-gateway mode
	modelTools   *ModelToolsConfig
	convAdminURL string // non-empty enables per-employee conversation databases
}

func newFixture(ctx context.Context, objects ...client.Object) *fixture {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	AddKedaTypesToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&v1alpha1.DigitalEmployee{}).
		WithStatusSubresource(&v1alpha1.PersonalWorkspace{}).Build()
	stub := newOVStub()
	server := httptest.NewServer(stub.handler())
	rt := newRuntimeStub()
	rtServer := httptest.NewServer(rt.handler())
	return &fixture{client: c, stub: stub, server: server, runtime: rt, runtimeSrv: rtServer, runtimeURL: rtServer.URL}
}

func (f *fixture) reconciler() *DigitalEmployeeReconciler {
	r := &DigitalEmployeeReconciler{
		Client: f.client,
		Scheme: f.client.Scheme(),
		OVRootFn: func(context.Context, client.Client, string, string) (string, error) {
			return "root-key", nil
		},
		RuntimeURLFn: func(string, string) string { return f.runtimeURL },
	}
	if f.wakeDBURL != "" {
		r.WakeDBFn = func(context.Context, string) (string, error) { return f.wakeDBURL, nil }
	}
	r.ChannelGatewayURL = f.gatewayURL
	if f.modelTools != nil {
		r.ModelTools = *f.modelTools
	}
	r.ConversationAdminDBURL = f.convAdminURL
	r.ConversationInitImage = "reg/postgres:17-alpine"
	r.DefaultIdleSeconds = 1800
	return r
}

// employeeFixture returns a complete CR pointing at the stub server.
func (f *fixture) employeeFixture(name string) *v1alpha1.DigitalEmployee {
	return &v1alpha1.DigitalEmployee{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: v1alpha1.DigitalEmployeeSpec{
			DisplayName: "测试员工 " + name,
			Runtime:     v1alpha1.RuntimeSpec{Type: "deerflow", Image: "reg/deerflow:v7", Model: "bench-glm"},
			Memory: &v1alpha1.MemorySpec{
				Type: "openviking", ServerURL: f.server.URL, AccountID: "zhiyuan",
				SingleTenantInstance: true,
			},
			Knowledge: &v1alpha1.KnowledgeSpec{
				Type: "weknora", MCPURL: "http://weknora-mcp:8830/mcp",
				Bases: []v1alpha1.KnowledgeBaseRef{{ID: "kb-1", Name: "销售知识库"}, {ID: "kb-2", Name: "产品知识库"}},
			},
			Governance:  v1alpha1.GovernanceSpec{AEPBaseURL: "http://aep:8080", DeploymentID: "demo", DefaultAEPUser: "u1", IdentitySource: "gov-feishu", GovernedTools: []string{"t1"}, TeamNames: map[string]string{"gov-rd": "研发部"}},
			Credentials: v1alpha1.CredentialsRef{Name: "creds"},
			Description: "整理销售资料并辅助生成周报。",
			Skills:      []v1alpha1.SkillRef{{ID: "report-writer", Name: "报告撰写", Version: "1.2.0"}},
			Visibility: &v1alpha1.VisibilitySpec{
				Mode:  "restricted",
				Teams: []v1alpha1.SubjectRef{{ID: "sales", Name: "销售团队"}},
				Users: []v1alpha1.SubjectRef{{ID: "u-9", Name: "李四"}},
			},
			Owner: "owner-1", OwnerName: "Owner One",
			A2A:      &v1alpha1.A2ASpec{Image: "reg/a2a:0.1", MaxDepth: 3},
			Replicas: 1,
		},
	}
}

const testNamespace = "default"

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DigitalEmployee controller suite")
}

var _ = BeforeSuite(func() {
	// Silence controller-runtime logger noise inside specs.
	ctrl.SetLogger(logr.Discard())
})
