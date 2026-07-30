package proxy

import (
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWebAssetsRevalidate guards against the stale-asset trap: app.js and index.html are
// served under fixed names with no version query, so without an explicit Cache-Control a
// browser may keep running an old app.js against new markup — producing UI controls that
// silently do nothing. Assets must ask the browser to revalidate, while still answering
// 304 when unchanged so revalidation stays cheap.
func TestWebAssetsRevalidate(t *testing.T) {
	mustInitConfig(t)

	// serveStaticFile/serveAdminPage read from "web/" relative to the process cwd.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, body := range map[string]string{
		"index.html": "<html><body>panel</body></html>",
		"app.js":     "console.log('v2');",
		"styles.css": "body{}",
	} {
		if err := os.WriteFile(filepath.Join(dir, "web", name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })

	h := &Handler{}
	adminPath := config.GetAdminPath()

	cases := []struct {
		name string
		path string
		call func(http.ResponseWriter, *http.Request)
	}{
		{"admin page", adminPath, h.serveAdminPage},
		{"admin asset", adminPath + "/app.js", h.serveStaticFile},
		{"shared asset", "/assets/app.js", h.serveSharedAsset},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()
			tc.call(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("want 200, got %d", w.Code)
			}
			cc := w.Header().Get("Cache-Control")
			if !strings.Contains(cc, "no-cache") {
				t.Fatalf("Cache-Control must force revalidation, got %q", cc)
			}

			// Revalidation must still short-circuit, otherwise every page load
			// re-downloads ~220 KB of app.js. http.ServeFile does not emit an ETag; it
			// validates on Last-Modified, so replay that.
			lastMod := w.Header().Get("Last-Modified")
			if lastMod == "" {
				t.Fatalf("expected Last-Modified so revalidation can answer 304")
			}
			r2 := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r2.Header.Set("If-Modified-Since", lastMod)
			w2 := httptest.NewRecorder()
			tc.call(w2, r2)
			if w2.Code != http.StatusNotModified {
				t.Fatalf("unchanged asset should answer 304, got %d", w2.Code)
			}
		})
	}
}
