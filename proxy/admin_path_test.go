package proxy

import (
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRoutingTestHandler builds a Handler with the admin collaborators wired and a
// fake web/ directory as CWD so serveAdminPage / static file serving work.
func newRoutingTestHandler(t *testing.T) *Handler {
	t.Helper()
	mustInitConfig(t)
	config.SetPassword("s3cret")

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0755); err != nil {
		t.Fatalf("mkdir web: %v", err)
	}
	for name, body := range map[string]string{
		"index.html":  "<html>admin-panel</html>",
		"portal.html": "<html>portal</html>",
		"styles.css":  "body{}",
	} {
		if err := os.WriteFile(filepath.Join(dir, "web", name), []byte(body), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	return &Handler{
		adminGuard:    newAdminAuthGuard(10, time.Minute, time.Minute),
		adminSessions: newAdminSessionStore(time.Hour),
	}
}

func doGet(h *Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// With ADMIN_PATH customized, the panel moves to the new prefix and the old
// /admin prefix behaves exactly like any unknown route (plain 404, wildcard
// CORS headers included) so scanners cannot locate the panel.
func TestCustomAdminPathRouting(t *testing.T) {
	h := newRoutingTestHandler(t)
	config.SetAdminPath("/panel-9f2a")
	t.Cleanup(func() { config.SetAdminPath("") })

	// Bare prefix redirects to the slash form so relative asset/api URLs resolve.
	if w := doGet(h, "/panel-9f2a"); w.Code != http.StatusMovedPermanently ||
		w.Header().Get("Location") != "/panel-9f2a/" {
		t.Fatalf("bare prefix: want 301 → /panel-9f2a/, got %d → %q", w.Code, w.Header().Get("Location"))
	}

	// Slash form serves the panel, without wildcard CORS.
	w := doGet(h, "/panel-9f2a/")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "admin-panel") {
		t.Fatalf("panel page: want 200 with panel HTML, got %d (%s)", w.Code, w.Body.String())
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("admin prefix must not carry a wildcard CORS header")
	}

	// Static files resolve under the custom prefix (relative refs in index.html).
	if w := doGet(h, "/panel-9f2a/styles.css"); w.Code != http.StatusOK {
		t.Fatalf("static under custom prefix: want 200, got %d", w.Code)
	}

	// Admin API routes under the custom prefix: login succeeds end-to-end.
	lw := httptest.NewRecorder()
	h.ServeHTTP(lw, httptest.NewRequest(http.MethodPost, "/panel-9f2a/api/login",
		strings.NewReader(`{"password":"s3cret"}`)))
	if lw.Code != http.StatusOK {
		t.Fatalf("login via custom prefix: want 200, got %d (%s)", lw.Code, lw.Body.String())
	}

	// The retired default prefix is indistinguishable from unknown routes.
	for _, p := range []string{"/admin", "/admin/", "/admin/api/accounts", "/admin/styles.css"} {
		w := doGet(h, p)
		ref := doGet(h, "/no-such-route")
		if w.Code != http.StatusNotFound || w.Code != ref.Code || w.Body.String() != ref.Body.String() {
			t.Fatalf("%s: want the same plain 404 as unknown routes, got %d (%s)", p, w.Code, w.Body.String())
		}
	}
}

// Without an override the panel stays at /admin (backwards compatible).
func TestDefaultAdminPathStillServed(t *testing.T) {
	h := newRoutingTestHandler(t)
	config.SetAdminPath("")

	if w := doGet(h, "/admin"); w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/admin/" {
		t.Fatalf("/admin: want 301 → /admin/, got %d → %q", w.Code, w.Header().Get("Location"))
	}
	if w := doGet(h, "/admin/"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "admin-panel") {
		t.Fatalf("/admin/: want 200 with panel HTML, got %d", w.Code)
	}
}

// /assets/ serves the shared static files for the public pages, but never HTML —
// pages are only reachable through their dedicated routes.
func TestSharedAssetsRoute(t *testing.T) {
	h := newRoutingTestHandler(t)

	w := doGet(h, "/assets/styles.css")
	if w.Code != http.StatusOK {
		t.Fatalf("/assets/styles.css: want 200, got %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Fatal("/assets/ is public and should carry the wildcard CORS header")
	}
	for _, p := range []string{"/assets/index.html", "/assets/portal.html", "/assets/"} {
		if w := doGet(h, p); w.Code != http.StatusNotFound {
			t.Fatalf("%s: want 404, got %d", p, w.Code)
		}
	}
}
