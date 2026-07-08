package proxy

import (
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newAdminTestHandler builds a Handler with just the admin-auth collaborators wired,
// and sets a known admin password.
func newAdminTestHandler(t *testing.T, password string) *Handler {
	t.Helper()
	mustInitConfig(t)
	config.SetPassword(password)
	return &Handler{
		adminGuard:    newAdminAuthGuard(10, time.Minute, time.Minute),
		adminSessions: newAdminSessionStore(time.Hour),
	}
}

func sessionCookie(res *http.Response) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == adminSessionCookieName {
			return c
		}
	}
	return nil
}

func TestAdminLoginIssuesHttpOnlySessionCookie(t *testing.T) {
	h := newAdminTestHandler(t, "s3cret")

	r := httptest.NewRequest(http.MethodPost, "/admin/api/login",
		strings.NewReader(`{"password":"s3cret","remember":true}`))
	w := httptest.NewRecorder()
	h.handleAdminLogin(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("login: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	c := sessionCookie(w.Result())
	if c == nil {
		t.Fatal("login did not set a session cookie")
	}
	if !c.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie must be SameSite=Strict")
	}
	if c.Value == "" {
		t.Fatal("session cookie must carry a token")
	}
	// remember:true → persistent cookie.
	if c.MaxAge <= 0 {
		t.Fatalf("remember=true should set a positive Max-Age, got %d", c.MaxAge)
	}
}

func TestAdminLoginWrongPasswordRejected(t *testing.T) {
	h := newAdminTestHandler(t, "s3cret")
	r := httptest.NewRequest(http.MethodPost, "/admin/api/login",
		strings.NewReader(`{"password":"wrong"}`))
	w := httptest.NewRecorder()
	h.handleAdminLogin(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: want 401, got %d", w.Code)
	}
	if sessionCookie(w.Result()) != nil {
		t.Fatal("no session cookie should be issued for a wrong password")
	}
}

func TestAdminAPIAcceptsSessionCookie(t *testing.T) {
	h := newAdminTestHandler(t, "s3cret")

	// Log in to obtain a session cookie.
	lr := httptest.NewRequest(http.MethodPost, "/admin/api/login",
		strings.NewReader(`{"password":"s3cret"}`))
	lw := httptest.NewRecorder()
	h.handleAdminLogin(lw, lr)
	c := sessionCookie(lw.Result())
	if c == nil {
		t.Fatal("expected session cookie from login")
	}

	// The gate must authorize a request carrying that cookie.
	r := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
	r.AddCookie(c)
	if ok, _ := h.adminAuthorized(r); !ok {
		t.Fatal("valid session cookie must authorize")
	}
}

func TestAdminAPIAcceptsPasswordHeader(t *testing.T) {
	h := newAdminTestHandler(t, "s3cret")
	r := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
	r.Header.Set("X-Admin-Password", "s3cret")
	if ok, _ := h.adminAuthorized(r); !ok {
		t.Fatal("correct X-Admin-Password must authorize")
	}
}

// TestAdminAPIRejectsLegacyPasswordCookie confirms M5: the old plaintext-password
// cookie is no longer accepted as a credential.
func TestAdminAPIRejectsLegacyPasswordCookie(t *testing.T) {
	h := newAdminTestHandler(t, "s3cret")
	r := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
	r.AddCookie(&http.Cookie{Name: "admin_password", Value: "s3cret"})
	if ok, _ := h.adminAuthorized(r); ok {
		t.Fatal("legacy plaintext-password cookie must NOT authorize")
	}
}

func TestAdminLogoutRevokesSession(t *testing.T) {
	h := newAdminTestHandler(t, "s3cret")

	lr := httptest.NewRequest(http.MethodPost, "/admin/api/login",
		strings.NewReader(`{"password":"s3cret"}`))
	lw := httptest.NewRecorder()
	h.handleAdminLogin(lw, lr)
	c := sessionCookie(lw.Result())
	if c == nil {
		t.Fatal("expected session cookie from login")
	}

	// Logout with the cookie.
	or := httptest.NewRequest(http.MethodPost, "/admin/api/logout", nil)
	or.AddCookie(c)
	ow := httptest.NewRecorder()
	h.handleAdminLogout(ow, or)
	if ow.Code != http.StatusOK {
		t.Fatalf("logout: want 200, got %d", ow.Code)
	}

	// The revoked token must no longer authorize.
	r := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
	r.AddCookie(c)
	w := httptest.NewRecorder()
	h.handleAdminAPI(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session must be rejected, got %d", w.Code)
	}
}

func TestAdminBruteForceLockout(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("s3cret")
	// Small policy: 3 failures → lockout.
	h := &Handler{
		adminGuard:    newAdminAuthGuard(3, time.Minute, time.Minute),
		adminSessions: newAdminSessionStore(time.Hour),
	}
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
		r.Header.Set("X-Admin-Password", "wrong")
		r.RemoteAddr = "203.0.113.5:1234"
		w := httptest.NewRecorder()
		h.handleAdminAPI(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i, w.Code)
		}
	}
	// Next attempt from the same IP is locked out (429), even with the RIGHT password.
	r := httptest.NewRequest(http.MethodGet, "/admin/api/status", nil)
	r.Header.Set("X-Admin-Password", "s3cret")
	r.RemoteAddr = "203.0.113.5:1234"
	w := httptest.NewRecorder()
	h.handleAdminAPI(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("after lockout: want 429, got %d", w.Code)
	}
}
