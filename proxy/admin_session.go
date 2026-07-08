package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	adminSessionCookieName = "admin_session"
	adminSessionTTL        = 72 * time.Hour
)

// adminSessionStore holds opaque, expiring admin session tokens server-side. The
// browser only ever receives the token in an HttpOnly cookie — the admin password
// is never stored client-side (H5) and the plaintext-password cookie is retired
// (M5). Tokens are 256-bit crypto/rand values, so they are unguessable and a plain
// map lookup (non-constant-time) leaks nothing useful.
type adminSessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
	ttl      time.Duration
}

func newAdminSessionStore(ttl time.Duration) *adminSessionStore {
	s := &adminSessionStore{sessions: make(map[string]time.Time), ttl: ttl}
	go s.janitor()
	return s
}

// mint creates a new session token and stores it with a fresh expiry.
func (s *adminSessionStore) mint() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	s.sessions[token] = time.Now().Add(s.ttl)
	s.mu.Unlock()
	return token, nil
}

// valid reports whether token is a live session and, if so, slides its expiry
// forward so active admins stay logged in while idle sessions eventually expire.
func (s *adminSessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[token]
	if !ok {
		return false
	}
	if now.After(exp) {
		delete(s.sessions, token)
		return false
	}
	s.sessions[token] = now.Add(s.ttl)
	return true
}

// revoke removes a session (logout).
func (s *adminSessionStore) revoke(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// janitor evicts expired sessions hourly so the map can't grow unbounded.
func (s *adminSessionStore) janitor() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.mu.Lock()
		for tok, exp := range s.sessions {
			if now.After(exp) {
				delete(s.sessions, tok)
			}
		}
		s.mu.Unlock()
	}
}

// requestIsHTTPS reports whether the request arrived over TLS, either directly or
// via a trusted TLS-terminating reverse proxy (X-Forwarded-Proto). Used to decide
// whether the session cookie may carry the Secure attribute — setting Secure on a
// plain-HTTP connection would make the browser drop the cookie and break login.
func (h *Handler) requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if h.guard != nil && h.guard.trustProxy &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return true
	}
	return false
}

// setAdminSessionCookie writes the session cookie. When remember is true it is
// persistent (survives browser restart); otherwise it is a session cookie the
// browser clears on close. HttpOnly keeps it out of reach of any XSS; SameSite=Strict
// blunts CSRF; Secure is set only on HTTPS.
func (h *Handler) setAdminSessionCookie(w http.ResponseWriter, r *http.Request, token string, remember bool) {
	c := &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   h.requestIsHTTPS(r),
	}
	if remember {
		c.MaxAge = int(adminSessionTTL / time.Second)
	}
	http.SetCookie(w, c)
}

// clearAdminSessionCookie expires the session cookie (logout).
func (h *Handler) clearAdminSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   h.requestIsHTTPS(r),
		MaxAge:   -1,
	})
}
