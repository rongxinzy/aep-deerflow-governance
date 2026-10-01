package main

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"log/slog"
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

// sessionStore keeps portal sessions in memory with the portal database as
// the durability layer: a rolling deploy or crash reloads live sessions on
// boot instead of logging everyone out (portal.yaml still pins one replica —
// the memory map stays the fast path and the single writer).
type sessionStore struct {
	mu    sync.Mutex
	ttl   time.Duration
	next  func() string // token source, overridable in tests
	items map[string]session
	db    *sql.DB // nil in unit tests: memory-only behaviour
}

type session struct {
	identity identity
	csrf     string
	expires  time.Time
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, next: randomSessionToken, items: map[string]session{}}
}

func newSessionStoreWithDB(ttl time.Duration, db *sql.DB) *sessionStore {
	store := newSessionStore(ttl)
	store.db = db
	return store
}

// sessionIdentityJSON round-trips an identity through the database. A plain
// json.Marshal of identity would drop Token (json:"-"), and losing it would
// break the AEP-authenticated flows after a restart.
type sessionIdentityJSON struct {
	Token       string   `json:"token"`
	UserID      string   `json:"id"`
	Username    string   `json:"username"`
	DisplayName string   `json:"displayName"`
	Kind        string   `json:"kind"`
	Roles       []string `json:"roles"`
	Teams       []string `json:"teams"`
}

func marshalIdentity(ident identity) ([]byte, error) {
	return json.Marshal(sessionIdentityJSON{
		Token:       ident.Token,
		UserID:      ident.UserID,
		Username:    ident.Username,
		DisplayName: ident.DisplayName,
		Kind:        ident.Kind,
		Roles:       ident.Roles,
	})
}

func unmarshalIdentity(raw []byte) (identity, error) {
	var shadow sessionIdentityJSON
	if err := json.Unmarshal(raw, &shadow); err != nil {
		return identity{}, err
	}
	return identity(shadow), nil
}

func randomSessionToken() string {
	raw := make([]byte, 32) // 256-bit
	if _, err := rand.Read(raw); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// issue mints a session token plus its CSRF proof and persists it.
func (s *sessionStore) issue(ident identity) (token, csrf string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	token, csrf = s.next(), s.next()
	expires := time.Now().Add(s.ttl)
	s.items[token] = session{identity: ident, csrf: csrf, expires: expires}
	s.persistLocked(token, ident, csrf, expires)
	return token, csrf
}

// persistLocked writes a session row; durability is best-effort because the
// memory map already serves the live process — a failed write only costs the
// restart-survival of this one session.
func (s *sessionStore) persistLocked(token string, ident identity, csrf string, expires time.Time) {
	if s.db == nil {
		return
	}
	raw, err := marshalIdentity(ident)
	if err != nil {
		slog.Error("session persist: marshal failed", "error", err)
		return
	}
	if _, err := s.db.Exec(`INSERT INTO sessions (token, identity_json, csrf, expires_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (token) DO UPDATE SET identity_json = EXCLUDED.identity_json, csrf = EXCLUDED.csrf, expires_at = EXCLUDED.expires_at`,
		token, raw, csrf, expires.UTC()); err != nil {
		slog.Error("session persist failed", "error", err)
	}
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < now()`); err != nil {
		slog.Error("session gc failed", "error", err)
	}
}

// get returns the live session (sliding renewal) or not-found. A memory miss
// with a database configured reloads the row first — the cookie outlives
// process restarts.
func (s *sessionStore) get(token string) (identity, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[token]
	if !ok {
		item, ok = s.loadLocked(token)
		if !ok {
			return identity{}, "", false
		}
		s.items[token] = item
	}
	if time.Now().After(item.expires) {
		delete(s.items, token)
		s.forgetLocked(token)
		return identity{}, "", false
	}
	item.expires = time.Now().Add(s.ttl)
	s.items[token] = item
	if s.db != nil {
		if _, err := s.db.Exec(`UPDATE sessions SET expires_at = $2 WHERE token = $1`, token, item.expires.UTC()); err != nil {
			slog.Error("session slide failed", "error", err)
		}
	}
	return item.identity, item.csrf, true
}

// loadLocked reads a session row that survived a restart.
func (s *sessionStore) loadLocked(token string) (session, bool) {
	if s.db == nil {
		return session{}, false
	}
	var raw []byte
	var csrf string
	var expires time.Time
	err := s.db.QueryRow(`SELECT identity_json, csrf, expires_at FROM sessions WHERE token = $1 AND expires_at > now()`, token).
		Scan(&raw, &csrf, &expires)
	if err != nil {
		return session{}, false
	}
	ident, err := unmarshalIdentity(raw)
	if err != nil {
		slog.Error("session load: unmarshal failed", "error", err)
		return session{}, false
	}
	return session{identity: ident, csrf: csrf, expires: expires}, true
}

func (s *sessionStore) drop(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, token)
	s.forgetLocked(token)
}

func (s *sessionStore) forgetLocked(token string) {
	if s.db == nil {
		return
	}
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE token = $1`, token); err != nil {
		slog.Error("session delete failed", "error", err)
	}
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

	// Department scoping: /user/me doesn't carry teamIds, so enrich the
	// identity with team memberships from the admin API once at login.
	// The result is cached in the portal session for its lifetime.
	p.enrichTeams(r.Context(), &ident)

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
