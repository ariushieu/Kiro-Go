package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

// TestCustomerErrorsCarrySupportContact: every failure the customer cannot act on
// must tell them where to ask. This is the whole point of the feature.
func TestCustomerErrorsCarrySupportContact(t *testing.T) {
	mustInitConfig(t)

	for _, tc := range []struct {
		name string
		got  string
	}{
		{"pool exhausted", noAccountsClientMessage()},
		{"upstream throttled", rateLimitedClientMessage()},
		{"key blocked", limitNoticeClientMessage()},
	} {
		if !strings.Contains(tc.got, "t.me/ariushieu") {
			t.Errorf("%s: message lacks the support contact: %q", tc.name, tc.got)
		}
	}
}

// TestSupportContactIsConfigurable: the contact can be changed without a rebuild.
func TestSupportContactIsConfigurable(t *testing.T) {
	mustInitConfig(t)

	if err := config.SetSupportContact("t.me/someoneelse"); err != nil {
		t.Fatalf("SetSupportContact: %v", err)
	}
	msg := noAccountsClientMessage()
	if !strings.Contains(msg, "t.me/someoneelse") {
		t.Errorf("message = %q, want the configured contact", msg)
	}
	if strings.Contains(msg, "ariushieu") {
		t.Errorf("message = %q, still carries the default", msg)
	}
}

// TestSupportContactBlankRestoresDefault: an empty value means "not configured",
// since Go cannot distinguish a cleared string from an unset one.
func TestSupportContactBlankRestoresDefault(t *testing.T) {
	mustInitConfig(t)

	if err := config.SetSupportContact("t.me/temp"); err != nil {
		t.Fatalf("SetSupportContact: %v", err)
	}
	if err := config.SetSupportContact("  "); err != nil {
		t.Fatalf("SetSupportContact(blank): %v", err)
	}
	if got := config.GetSupportContact(); got != "t.me/ariushieu" {
		t.Errorf("GetSupportContact() = %q, want the default", got)
	}
}

// TestSupportContactDashSuppresses gives the admin a way to turn the line off,
// which blank cannot express.
func TestSupportContactDashSuppresses(t *testing.T) {
	mustInitConfig(t)

	if err := config.SetSupportContact("-"); err != nil {
		t.Fatalf("SetSupportContact: %v", err)
	}
	if got := config.GetSupportContact(); got != "" {
		t.Errorf("GetSupportContact() = %q, want empty", got)
	}
	msg := noAccountsClientMessage()
	if msg != noAccountsClientText {
		t.Errorf("message = %q, want the bare text with no contact line", msg)
	}
	if strings.Contains(msg, "Chi tiết") {
		t.Errorf("suppressed contact still rendered a label: %q", msg)
	}
}

// TestSupportContactNotDuplicated: a custom limit notice that already names the
// contact must not get it appended twice.
func TestSupportContactNotDuplicated(t *testing.T) {
	mustInitConfig(t)

	if err := config.SetLimitNoticeMessage("Key hết hạn, liên hệ t.me/ariushieu để gia hạn."); err != nil {
		t.Fatalf("SetLimitNoticeMessage: %v", err)
	}
	msg := limitNoticeClientMessage()
	if n := strings.Count(msg, "t.me/ariushieu"); n != 1 {
		t.Errorf("contact appears %d times in %q, want 1", n, msg)
	}
}

// TestUpstreamErrorsStayMasked is the property this must not regress: adding a
// contact line must not start leaking why the request actually failed.
func TestUpstreamErrorsStayMasked(t *testing.T) {
	mustInitConfig(t)

	for _, secret := range []string{
		"HTTP 402 from Kiro IDE: OVERAGE limit exceeded for account seller@example.com",
		"Your User ID temporarily is suspended",
		"proxyconnect tcp: dial tcp 10.0.0.5:1080: connect: connection refused",
		"Authentication failed - token invalid or expired",
	} {
		_, msg := clientFacingUpstreamError(errors.New(secret))
		if strings.Contains(msg, "OVERAGE") || strings.Contains(msg, "suspended") ||
			strings.Contains(msg, "10.0.0.5") || strings.Contains(msg, "token") ||
			strings.Contains(msg, "seller@example.com") {
			t.Errorf("upstream detail leaked into %q (from %q)", msg, secret)
		}
		if !strings.Contains(msg, "t.me/ariushieu") {
			t.Errorf("message %q should still carry the contact", msg)
		}
	}
}

// TestSettingsRoundTripsSupportContact: the settings API must return the stored
// value, not the rendered one, or saving the form would append the contact again
// and again.
func TestSettingsRoundTripsSupportContact(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	post := func(body string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, config.GetAdminPath()+"/api/settings",
			strings.NewReader(body))
		r.Header.Set("X-Admin-Password", "s3cret")
		r.RemoteAddr = "203.0.113.9:1234"
		w := httptest.NewRecorder()
		h.handleAdminAPI(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("POST settings = %d: %s", w.Code, w.Body.String())
		}
	}
	get := func() map[string]interface{} {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, config.GetAdminPath()+"/api/settings", nil)
		r.Header.Set("X-Admin-Password", "s3cret")
		r.RemoteAddr = "203.0.113.9:1234"
		w := httptest.NewRecorder()
		h.handleAdminAPI(w, r)
		var out map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode settings: %v; body=%s", err, w.Body.String())
		}
		return out
	}

	post(`{"supportContact":"t.me/roundtrip"}`)
	if got, _ := get()["supportContact"].(string); got != "t.me/roundtrip" {
		t.Fatalf("supportContact = %q, want t.me/roundtrip", got)
	}

	// The limit notice must come back as the admin typed it, with no contact glued on.
	post(`{"limitNoticeMessage":"Key của bạn đã hết hạn."}`)
	got, _ := get()["limitNoticeMessage"].(string)
	if got != "Key của bạn đã hết hạn." {
		t.Errorf("limitNoticeMessage = %q; the settings API must return the raw stored value", got)
	}
	if strings.Contains(got, "t.me/roundtrip") {
		t.Error("the rendered contact leaked into the settings response; re-saving would compound it")
	}
	// ...while the customer-facing render does include it.
	if rendered := limitNoticeClientMessage(); !strings.Contains(rendered, "t.me/roundtrip") {
		t.Errorf("rendered notice = %q, want the contact appended", rendered)
	}
}

// TestBlockedKeysGetNoticeNotAccess covers a gap found while testing this change:
// with the expiry check removed from authenticate(), the entire suite still passed,
// even though an expired key would have been served upstream traffic. Each blocked
// state must resolve to the notice path, never to a valid credential.
func TestBlockedKeysGetNoticeNotAccess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry config.ApiKeyEntry
	}{
		{"expired", config.ApiKeyEntry{
			ID: "k1", Name: "expired", Key: "sk-blocked-expired", Enabled: true,
			ExpiresAt: time.Now().Unix() - 3600,
		}},
		{"disabled", config.ApiKeyEntry{
			ID: "k2", Name: "disabled", Key: "sk-blocked-disabled", Enabled: false,
		}},
		{"over token limit", config.ApiKeyEntry{
			ID: "k3", Name: "overtoken", Key: "sk-blocked-tokens", Enabled: true,
			TokenLimit: 100, TokensUsed: 500,
		}},
		{"over credit limit", config.ApiKeyEntry{
			ID: "k4", Name: "overcredit", Key: "sk-blocked-credits", Enabled: true,
			CreditLimit: 1, CreditsUsed: 5,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mustInitConfig(t)
			if _, err := config.AddApiKeys([]config.ApiKeyEntry{tc.entry}); err != nil {
				t.Fatalf("AddApiKeys: %v", err)
			}
			requireAuth(t)

			h := newUpstreamTestHandler(t)
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			r.Header.Set("Authorization", "Bearer "+tc.entry.Key)
			r.RemoteAddr = "203.0.113.50:9999"

			entry, err := h.authenticate(r)
			if err == nil {
				t.Fatalf("a %s key authenticated successfully; it must be blocked", tc.name)
			}
			if entry != nil {
				t.Errorf("a %s key returned a usable entry (%s)", tc.name, entry.Name)
			}
			// Blocked-but-valid keys get the in-chat notice rather than a hard 401,
			// so clients do not crash on an auth error.
			ae, ok := err.(*authError)
			if !ok || !ae.notice {
				t.Errorf("%s: err = %#v, want a notice-flagged authError", tc.name, err)
			}
		})
	}
}

// TestSupportContactSurvivesReload guards persistence.
func TestSupportContactSurvivesReload(t *testing.T) {
	mustInitConfig(t)

	if err := config.SetSupportContact("t.me/persisted"); err != nil {
		t.Fatalf("SetSupportContact: %v", err)
	}
	if err := config.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := config.GetSupportContact(); got != "t.me/persisted" {
		t.Errorf("after reload = %q, want t.me/persisted", got)
	}
}
