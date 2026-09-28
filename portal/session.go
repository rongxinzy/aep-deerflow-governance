package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Portal sessions bridge the AEP identity model onto the cookie-based
// DeerFlow frontend: the browser logs in once with AEP credentials, then the
// session cookie (plus a readable CSRF cookie) authenticates every proxied
// call. Cookie names intentionally match what the reused frontend already
// reads/writes: csrf_token for double-submit, de_employee for the selected
// digital employee.
const (
	portalSessionCookie = "de_portal_session"
	portalCsrfCookie    = "csrf_token"
	employeeCookie      = "de_employee"
)

// sessionStore keeps portal sessions in memory. The portal runs as a single
// replica (deploy/portal.yaml pins replicas: 1), so process state is the
// honest trade: a restart logs everyone out, and scaling past one replica
// means moving the store to the portal database first.
type sessionStore struct {
	mu    sync.Mutex
	ttl   time.Duration
	next  func() string // token source, overridable in tests
	items map[string]session
}

type session struct {
	identity identity
	csrf     string
	expires  time.Time
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, next: randomSessionToken, items: map[string]session{}}
}

func randomSessionToken() string {
	raw := make([]byte, 32) // 256-bit
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// issue mints a session token plus its CSRF proof.
func (s *sessionStore) issue(ident identity) (token, csrf string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	token, csrf = s.next(), s.next()
	s.items[token] = session{identity: ident, csrf: csrf, expires: time.Now().Add(s.ttl)}
	return token, csrf
}

// get returns the live session (sliding renewal) or not-found.
func (s *sessionStore) get(token string) (identity, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[token]
	if !ok {
		return identity{}, "", false
	}
	if time.Now().After(item.expires) {
		delete(s.items, token)
		return identity{}, "", false
	}
	item.expires = time.Now().Add(s.ttl)
	s.items[token] = item
	return item.identity, item.csrf, true
}

func (s *sessionStore) drop(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, token)
}

func (s *sessionStore) evictLocked() {
	now := time.Now()
	for token, item := range s.items {
		if now.After(item.expires) {
			delete(s.items, token)
		}
	}
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func normalizeEmployeeName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// authenticate resolves the caller from a Bearer token or the portal session
// cookie. Cookie-authenticated state-changing requests additionally require
// the double-submit X-CSRF-Token header — the DeerFlow frontend's fetch
// wrapper and LangGraph client inject it automatically. The returned status
// distinguishes 401 (no/invalid credential) from 403 (CSRF mismatch).
func (p *portal) authenticate(r *http.Request) (identity, int, bool) {
	if token := bearer(r); token != "" {
		ident, err := p.aep.verify(r.Context(), token)
		if err != nil {
			return identity{}, http.StatusUnauthorized, false
		}
		return ident, http.StatusOK, true
	}
	if p.sessions == nil {
		return identity{}, http.StatusUnauthorized, false
	}
	cookie, err := r.Cookie(portalSessionCookie)
	if err != nil || cookie.Value == "" {
		return identity{}, http.StatusUnauthorized, false
	}
	ident, csrf, ok := p.sessions.get(cookie.Value)
	if !ok {
		return identity{}, http.StatusUnauthorized, false
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		if header := r.Header.Get("X-CSRF-Token"); header == "" || !constantTimeEqual(header, csrf) {
			return identity{}, http.StatusForbidden, false
		}
	}
	return ident, http.StatusOK, true
}

// handleSessionLogin exchanges AEP credentials (or an existing AEP access
// token) for a portal session. Unauthenticated by design — it is the
// credential itself.
func (p *portal) handleSessionLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	var ident identity
	switch {
	case body.Token != "":
		verified, err := p.aep.verify(r.Context(), body.Token)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "aep token rejected"})
			return
		}
		ident = verified
	case body.Username != "" && body.Password != "":
		token, err := p.aep.passwordLogin(r.Context(), p.cfg.aepDeploymentID, body.Username, body.Password)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "incorrect username or password"})
			return
		}
		verified, err := p.aep.verify(r.Context(), token)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "aep verify failed after login"})
			return
		}
		ident = verified
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username/password or token required"})
		return
	}

	token, csrf := p.sessions.issue(ident)
	// Path=/ explicitly: the default path (/api/v1) would hide the cookies
	// from the workspace document, SSR cookies(), and the cross-port chat UI
	// origin — silently breaking the whole flow.
	http.SetCookie(w, &http.Cookie{Name: portalSessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: portalCsrfCookie, Value: csrf, Path: "/", SameSite: http.SameSiteLaxMode})
	p.logger.Info("portal session opened", "user", ident.UserID)
	writeJSON(w, http.StatusOK, map[string]any{"user": map[string]string{"id": ident.UserID, "displayName": ident.DisplayName}})
}

// handleSessionWhoami reports the session identity for the /chat entry page.
func (p *portal) handleSessionWhoami(w http.ResponseWriter, r *http.Request, ident identity) {
	writeJSON(w, http.StatusOK, map[string]any{"user": map[string]string{"id": ident.UserID, "displayName": ident.DisplayName}})
}

// handleSessionLogout drops the session cookie directly (no CSRF proof: a
// forged logout can only end the victim's own chat session — nuisance, not
// privilege).
func (p *portal) handleSessionLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(portalSessionCookie); err == nil && cookie.Value != "" && p.sessions != nil {
		p.sessions.drop(cookie.Value)
	}
	for _, name := range []string{portalSessionCookie, portalCsrfCookie, employeeCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1})
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "logged out"})
}

// handleSessionEmployee selects the digital employee to chat with. The
// ownership check reuses authorizeChat (owner/admin + Ready + credentials
// present) so the cookie can never name an employee the caller cannot use.
func (p *portal) handleSessionEmployee(w http.ResponseWriter, r *http.Request, ident identity) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	body.Name = normalizeEmployeeName(body.Name)
	if !namePattern.MatchString(body.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid employee name"})
		return
	}
	if _, code, errMsg := p.authorizeChat(r.Context(), body.Name, ident); code != 0 {
		writeJSON(w, code, map[string]string{"error": errMsg})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: employeeCookie, Value: body.Name, Path: "/", SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]any{"employee": body.Name, "chatURL": p.chatUIURL(r)})
}
