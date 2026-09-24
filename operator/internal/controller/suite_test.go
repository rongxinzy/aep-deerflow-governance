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
	"github.com/go-logr/logr"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
}

func newOVStub() *ovStub { return &ovStub{users: map[string]string{}} }

func (s *ovStub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/admin/accounts/{account}/users", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var body struct{ UserID string `json:"user_id"` }
		_ = json.NewDecoder(r.Body).Decode(&body)
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

// fixture wires a reconciler against a fake cluster plus the OpenViking stub.
type fixture struct {
	client  client.Client
	stub    *ovStub
	server  *httptest.Server
	updates int
}

func newFixture(ctx context.Context, objects ...client.Object) *fixture {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&v1alpha1.DigitalEmployee{}).Build()
	stub := newOVStub()
	server := httptest.NewServer(stub.handler())
	return &fixture{client: c, stub: stub, server: server}
}

func (f *fixture) reconciler() *DigitalEmployeeReconciler {
	return &DigitalEmployeeReconciler{
		Client: f.client,
		Scheme: f.client.Scheme(),
		OVRootFn: func(context.Context, client.Client, string, string) (string, error) {
			return "root-key", nil
		},
	}
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
			Knowledge:  &v1alpha1.KnowledgeSpec{Type: "weknora", MCPURL: "http://weknora-mcp:8830/mcp"},
			Governance: v1alpha1.GovernanceSpec{AEPBaseURL: "http://aep:8080", DeploymentID: "demo", DefaultAEPUser: "u1", IdentitySource: "gov-feishu", GovernedTools: []string{"t1"}, TeamNames: map[string]string{"gov-rd": "研发部"}},
			Credentials: v1alpha1.CredentialsRef{Name: "creds"},
			Owner:       "owner-1", OwnerName: "Owner One",
			A2A:        &v1alpha1.A2ASpec{Image: "reg/a2a:0.1", MaxDepth: 3},
			Replicas:   1,
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
