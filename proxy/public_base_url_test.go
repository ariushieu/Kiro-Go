package proxy

import (
	"kiro-go/config"
	"testing"
)

// TestResolvePublicBaseURL exercises the redirect-base resolution used to build OAuth
// redirect_uri values. Only the explicit config override is honored — auto-detect from the
// admin request Host is intentionally absent (the loopback server listens on a different
// port than the admin UI, so the request host names the wrong endpoint).
func TestResolvePublicBaseURL(t *testing.T) {
	mustInitConfig(t)

	t.Run("config override wins", func(t *testing.T) {
		if err := config.UpdatePublicBaseURL("https://azr.hian.software/"); err != nil {
			t.Fatalf("set base url: %v", err)
		}
		defer config.UpdatePublicBaseURL("")

		if got := resolvePublicBaseURL(); got != "https://azr.hian.software" {
			t.Fatalf("override: got %q, want trimmed config value", got)
		}
	})

	t.Run("unset yields empty base (falls back to localhost loopback)", func(t *testing.T) {
		if err := config.UpdatePublicBaseURL(""); err != nil {
			t.Fatalf("clear base url: %v", err)
		}
		if got := resolvePublicBaseURL(); got != "" {
			t.Fatalf("unset: got %q, want empty", got)
		}
	})
}
