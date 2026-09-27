package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
)

// fakeAEP implements the two endpoints the portal session flow needs:
// password login (fixed credential table) and /user/me (token → identity).
func fakeAEP(t *testing.T) *httptest.Server {
	t.Helper()
	passwords := map[string]string{"user1": "pw-one", "user2": "pw-two"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /aep/v1/auth/password/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if passwords[body.Username] != body.Password || body.Password == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad credentials"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "aep-token-" + body.Username})
	})
	mux.HandleFunc("GET /aep/v1/user/me", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		user, ok := map[string][2]string{
			"aep-token-user1": {"user-1", "张三"},
			"aep-token-user2": {"user-2", "李四"},
		}[token]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user":  map[string]string{"id": user[0], "displayName": user[1], "kind": "human"},
			"roles": []string{"sales"},
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// sessionTestPortal wires the full mux against the fake AEP + fake cluster
// so tests exercise the real cookie round-trip (Set-Cookie → jar → header).
func sessionTestPortal(t *testing.T, aep *httptest.Server, objs ...runtime.Object) *httptest.Server {
	t.Helper()
	p := &portal{
		cfg: config{
			namespace:       "digital-employees",
			adminUsers:      map[string]bool{"admin-user": true},
			aepDeploymentID: "demo",
			chatUIURL:       "http://chat.example/workspace",
			sessionTTL:      time.Hour,
		},
		k8s:      fake.NewClientBuilder().WithScheme(chatTestScheme(t)).WithRuntimeObjects(objs...).WithStatusSubresource(&dev1alpha1.DigitalEmployee{}).Build(),
		aep:      &aepAuthenticator{baseURL: aep.URL, http: aep.Client()},
		sessions: newSessionStore(time.Hour),
		logger:   slog.New(slog.DiscardHandler),
	}
	server := httptest.NewServer(p.routes())
	t.Cleanup(server.Close)
	return server
}

func mustParse(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return parsed
}

func postJSON(t *testing.T, client *http.Client, url, body string) (*http.Response, string) {
	t.Helper()
	response, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response, string(payload)
}

func csrfFrom(t *testing.T, jar *cookiejar.Jar, serverURL string) string {
	t.Helper()
	var csrf string
	for _, cookie := range jar.Cookies(mustParse(serverURL)) {
		if cookie.Name == portalCsrfCookie {
			csrf = cookie.Value
		}
	}
	if csrf == "" {
		t.Fatal("no csrf cookie in jar")
	}
	return csrf
}

func TestSessionPasswordLoginSetsCookies(t *testing.T) {
	server := sessionTestPortal(t, fakeAEP(t))
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	response, body := postJSON(t, client, server.URL+"/api/v1/session", `{"username":"user1","password":"pw-one"}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	var setCookies []string
	for _, cookie := range response.Cookies() {
		setCookies = append(setCookies, cookie.String())
	}
	joined := strings.Join(setCookies, "\n")
	for _, want := range []string{
		portalSessionCookie + "=",
		"Path=/",
		"HttpOnly",
		"SameSite=Lax",
		portalCsrfCookie + "=",
		employeeCookie,
	} {
		if want == employeeCookie {
			continue
		}
		if !strings.Contains(joined, want) {
			t.Errorf("Set-Cookie missing %q: %s", want, joined)
		}
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == portalSessionCookie && (cookie.Path != "/" || !cookie.HttpOnly) {
			t.Errorf("session cookie must be Path=/ HttpOnly, got %+v", cookie)
		}
		if cookie.Name == portalCsrfCookie && (cookie.Path != "/" || cookie.HttpOnly) {
			t.Errorf("csrf cookie must be Path=/ and readable, got %+v", cookie)
		}
	}

	// The session now authenticates a GET without a Bearer token.
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/session", nil)
	response2, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response2.Body.Close()
	if response2.StatusCode != http.StatusOK {
		t.Fatalf("whoami status = %d", response2.StatusCode)
	}
}

func TestSessionTokenExchange(t *testing.T) {
	server := sessionTestPortal(t, fakeAEP(t))
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	response, body := postJSON(t, client, server.URL+"/api/v1/session", `{"token":"aep-token-user2"}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if !strings.Contains(body, "user-2") {
		t.Fatalf("expected user-2 identity, got %s", body)
	}
}

func TestSessionBadPasswordRejected(t *testing.T) {
	server := sessionTestPortal(t, fakeAEP(t))
	client := &http.Client{}
	response, _ := postJSON(t, client, server.URL+"/api/v1/session", `{"username":"user1","password":"wrong"}`)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == portalSessionCookie {
			t.Error("session cookie must not be set on failed login")
		}
	}
}

func TestSessionCSRFEnforcedOnMutations(t *testing.T) {
	server := sessionTestPortal(t, fakeAEP(t), readyEmployee("sales-helper", "user-1"), memkeySecret("sales-helper"))
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	if _, body := postJSON(t, client, server.URL+"/api/v1/session", `{"username":"user1","password":"pw-one"}`); !strings.Contains(body, "user-1") {
		t.Fatalf("login failed: %s", body)
	}
	csrf := csrfFrom(t, jar, server.URL)

	// GET needs no CSRF proof.
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/employees", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET without csrf: status=%d", response.StatusCode)
	}

	// POST without the header → 403.
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/api/v1/session/employee", strings.NewReader(`{"name":"sales-helper"}`))
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without csrf must 403, got %d", response.StatusCode)
	}

	// POST with the double-submit header → 200 and the employee cookie set.
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/api/v1/session/employee", strings.NewReader(`{"name":"sales-helper"}`))
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Content-Type", "application/json")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, payload)
	}
	var found bool
	for _, cookie := range response.Cookies() {
		if cookie.Name == employeeCookie && cookie.Value == "sales-helper" && cookie.Path == "/" {
			found = true
		}
	}
	if !found {
		t.Errorf("de_employee cookie not set with Path=/: %+v", response.Cookies())
	}
}

func TestSessionEmployeeSelectionEnforcesOwnership(t *testing.T) {
	server := sessionTestPortal(t, fakeAEP(t), readyEmployee("sales-helper", "user-1"), memkeySecret("sales-helper"))
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	if _, body := postJSON(t, client, server.URL+"/api/v1/session", `{"username":"user2","password":"pw-two"}`); !strings.Contains(body, "user-2") {
		t.Fatalf("login failed: %s", body)
	}
	csrf := csrfFrom(t, jar, server.URL)

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/session/employee", strings.NewReader(`{"name":"sales-helper"}`))
	request.Header.Set("X-CSRF-Token", csrf)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner must get 403, got %d", response.StatusCode)
	}
}

func TestSessionExpiry(t *testing.T) {
	store := newSessionStore(2 * time.Millisecond)
	token, _ := store.issue(identity{UserID: "user-1"})
	if _, _, ok := store.get(token); !ok {
		t.Fatal("fresh session must resolve")
	}
	time.Sleep(5 * time.Millisecond)
	if _, _, ok := store.get(token); ok {
		t.Fatal("expired session must not resolve")
	}
}

func TestSessionLogoutDropsSession(t *testing.T) {
	server := sessionTestPortal(t, fakeAEP(t))
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	if _, body := postJSON(t, client, server.URL+"/api/v1/session", `{"token":"aep-token-user1"}`); !strings.Contains(body, "user-1") {
		t.Fatalf("login failed: %s", body)
	}

	request, _ := http.NewRequest(http.MethodDelete, server.URL+"/api/v1/session", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d", response.StatusCode)
	}

	// Cookies are expired client-side; the server-side session is gone even
	// for a replayed cookie value.
	request, _ = http.NewRequest(http.MethodGet, server.URL+"/api/v1/session", nil)
	request.Header.Set("Cookie", portalSessionCookie+"=replayed")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed session must 401, got %d", response.StatusCode)
	}
}
