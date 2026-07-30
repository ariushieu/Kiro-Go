package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

func seedKeyWithExpiry(t *testing.T, name string, expiresAt int64) config.ApiKeyEntry {
	t.Helper()
	created, err := config.AddApiKeys([]config.ApiKeyEntry{{
		Name:      name,
		Key:       config.GenerateApiKeyValue(),
		Enabled:   true,
		ExpiresAt: expiresAt,
	}})
	if err != nil {
		t.Fatalf("AddApiKeys(%s): %v", name, err)
	}
	if len(created) != 1 {
		t.Fatalf("expected 1 key, got %d", len(created))
	}
	return created[0]
}

func postExtend(t *testing.T, h *Handler, body string) (int, map[string]interface{}) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost,
		config.GetAdminPath()+"/api/api-keys/extend", strings.NewReader(body))
	r.Header.Set("X-Admin-Password", "s3cret")
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()

	h.handleAdminAPI(w, r)

	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode (%d): %v; body=%s", w.Code, err, w.Body.String())
	}
	return w.Code, out
}

func expiryOf(t *testing.T, id string) int64 {
	t.Helper()
	e := config.GetApiKeyEntry(id)
	if e == nil {
		t.Fatalf("key %s vanished", id)
	}
	return e.ExpiresAt
}

// TestExtendAddsToRemainingTime: a still-valid key gets the delta stacked on top
// of its existing expiry, not measured from now.
func TestExtendAddsToRemainingTime(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	future := time.Now().Unix() + 10*86400
	key := seedKeyWithExpiry(t, "valid", future)

	code, out := postExtend(t, h, fmt.Sprintf(`{"ids":[%q],"seconds":86400}`, key.ID))
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n, _ := out["extended"].(float64); int(n) != 1 {
		t.Fatalf("extended = %v, want 1", out["extended"])
	}
	if got := expiryOf(t, key.ID); got != future+86400 {
		t.Errorf("expiry = %d, want %d (old expiry + 1 day)", got, future+86400)
	}
}

// TestExtendRevivesExpiredKeyFromNow: adding a day to a key that lapsed a week ago
// must make it valid, not leave it a week in the past plus a day.
func TestExtendRevivesExpiredKeyFromNow(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	past := time.Now().Unix() - 7*86400
	key := seedKeyWithExpiry(t, "lapsed", past)

	before := time.Now().Unix()
	if _, out := postExtend(t, h, fmt.Sprintf(`{"ids":[%q],"seconds":86400}`, key.ID)); out["error"] != nil {
		t.Fatalf("error: %v", out["error"])
	}
	got := expiryOf(t, key.ID)

	if got <= time.Now().Unix() {
		t.Fatalf("key is still expired (expiry %d, now %d)", got, time.Now().Unix())
	}
	if got < before+86400 || got > time.Now().Unix()+86400+5 {
		t.Errorf("expiry = %d, want ~now+1day (%d)", got, before+86400)
	}
}

// TestExtendSkipsNeverExpiresKeys is the important safety property: adding time to
// an unlimited key would give it an expiry and cut off a paying customer.
func TestExtendSkipsNeverExpiresKeys(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	unlimited := seedKeyWithExpiry(t, "unlimited", 0)
	future := time.Now().Unix() + 86400
	limited := seedKeyWithExpiry(t, "limited", future)

	code, out := postExtend(t, h,
		fmt.Sprintf(`{"ids":[%q,%q],"seconds":86400}`, unlimited.ID, limited.ID))
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n, _ := out["extended"].(float64); int(n) != 1 {
		t.Errorf("extended = %v, want 1", out["extended"])
	}
	if n, _ := out["skippedNeverExpires"].(float64); int(n) != 1 {
		t.Errorf("skippedNeverExpires = %v, want 1", out["skippedNeverExpires"])
	}
	if got := expiryOf(t, unlimited.ID); got != 0 {
		t.Errorf("never-expires key was given expiry %d; it must stay 0", got)
	}
	if got := expiryOf(t, limited.ID); got != future+86400 {
		t.Errorf("limited key expiry = %d, want %d", got, future+86400)
	}
}

// TestExtendAllTargetsEveryKey covers the "+1 day for all" button.
func TestExtendAllTargetsEveryKey(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	base := time.Now().Unix() + 86400
	a := seedKeyWithExpiry(t, "a", base)
	b := seedKeyWithExpiry(t, "b", base+3600)
	unlimited := seedKeyWithExpiry(t, "c", 0)

	code, out := postExtend(t, h, `{"all":true,"seconds":1800}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n, _ := out["extended"].(float64); int(n) != 2 {
		t.Errorf("extended = %v, want 2 (the unlimited key is skipped)", out["extended"])
	}
	if got := expiryOf(t, a.ID); got != base+1800 {
		t.Errorf("a expiry = %d, want %d", got, base+1800)
	}
	if got := expiryOf(t, b.ID); got != base+3600+1800 {
		t.Errorf("b expiry = %d, want %d", got, base+3600+1800)
	}
	if got := expiryOf(t, unlimited.ID); got != 0 {
		t.Errorf("unlimited expiry = %d, want 0", got)
	}
}

// TestExtendEmptySelectionIsRejected: an empty ids list must not be treated as
// "every key". Without this an accidental empty selection silently rewrites the
// whole key list.
func TestExtendEmptySelectionIsRejected(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	future := time.Now().Unix() + 86400
	key := seedKeyWithExpiry(t, "untouched", future)

	code, _ := postExtend(t, h, `{"ids":[],"seconds":86400}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if got := expiryOf(t, key.ID); got != future {
		t.Errorf("expiry changed to %d; an empty selection must touch nothing", got)
	}
}

// TestExtendRejectsZeroAndOutOfRange guards the custom-amount box against a units
// mixup pushing expiry somewhere unrepresentable.
func TestExtendRejectsZeroAndOutOfRange(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)
	key := seedKeyWithExpiry(t, "k", time.Now().Unix()+86400)

	for _, body := range []string{
		fmt.Sprintf(`{"ids":[%q],"seconds":0}`, key.ID),
		fmt.Sprintf(`{"ids":[%q],"seconds":999999999999}`, key.ID),
		fmt.Sprintf(`{"ids":[%q],"seconds":-999999999999}`, key.ID),
	} {
		code, out := postExtend(t, h, body)
		if code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400 (%v)", body, code, out["error"])
		}
	}
}

// TestExtendNegativeClampsAtNow: shortening is allowed, but it must not silently
// land in the past — the floor is "expires now".
func TestExtendNegativeClampsAtNow(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	key := seedKeyWithExpiry(t, "shorten", time.Now().Unix()+3600)

	before := time.Now().Unix()
	if _, out := postExtend(t, h, fmt.Sprintf(`{"ids":[%q],"seconds":-86400}`, key.ID)); out["error"] != nil {
		t.Fatalf("error: %v", out["error"])
	}
	got := expiryOf(t, key.ID)
	if got < before {
		t.Errorf("expiry = %d is before the call started (%d); should clamp at now", got, before)
	}
	if got > time.Now().Unix()+2 {
		t.Errorf("expiry = %d, want ~now", got)
	}
}

// TestExtendReportsNotFound so a stale UI selection is visible rather than silent.
func TestExtendReportsNotFound(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	key := seedKeyWithExpiry(t, "real", time.Now().Unix()+86400)

	_, out := postExtend(t, h,
		fmt.Sprintf(`{"ids":[%q,"ghost-id"],"seconds":3600}`, key.ID))
	if n, _ := out["extended"].(float64); int(n) != 1 {
		t.Errorf("extended = %v, want 1", out["extended"])
	}
	if n, _ := out["notFound"].(float64); int(n) != 1 {
		t.Errorf("notFound = %v, want 1", out["notFound"])
	}
}

// TestExtendRequiresAdminPassword: this rewrites billing-relevant state.
func TestExtendRequiresAdminPassword(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)
	key := seedKeyWithExpiry(t, "k", time.Now().Unix()+86400)

	r := httptest.NewRequest(http.MethodPost,
		config.GetAdminPath()+"/api/api-keys/extend",
		strings.NewReader(fmt.Sprintf(`{"ids":[%q],"seconds":86400}`, key.ID)))
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()

	h.handleAdminAPI(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// TestExtendRouting guards the switch ordering: the literal /api-keys/extend case
// must precede the generic /api-keys/<id> cases, or "extend" is read as an ID.
func TestExtendRouting(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)
	key := seedKeyWithExpiry(t, "routed", time.Now().Unix()+86400)

	code, out := postExtend(t, h, fmt.Sprintf(`{"ids":[%q],"seconds":60}`, key.ID))
	if code == http.StatusNotFound {
		t.Fatalf("route not reached: %v", out)
	}
	if code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, out["error"])
	}
}

// TestExtendPersistsAcrossReload: the new expiry must survive a config reload, not
// just live in memory.
func TestExtendPersistsAcrossReload(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	future := time.Now().Unix() + 86400
	key := seedKeyWithExpiry(t, "persisted", future)
	if _, out := postExtend(t, h, fmt.Sprintf(`{"ids":[%q],"seconds":7200}`, key.ID)); out["error"] != nil {
		t.Fatalf("error: %v", out["error"])
	}

	// Re-read config.json from disk to prove the change was persisted, not just
	// mutated in memory.
	if err := config.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := expiryOf(t, key.ID); got != future+7200 {
		t.Errorf("after reload expiry = %d, want %d", got, future+7200)
	}
}

// TestExtendedKeyActuallyAuthenticates closes the loop: a lapsed key must be
// usable again after being extended, since that is the whole point.
func TestExtendedKeyActuallyAuthenticates(t *testing.T) {
	mustInitConfig(t)
	h := newUpstreamTestHandler(t)

	created, err := config.AddApiKeys([]config.ApiKeyEntry{{
		Name:      "lapsed",
		Key:       "sk-extend-me",
		Enabled:   true,
		ExpiresAt: time.Now().Unix() - 3600,
	}})
	if err != nil {
		t.Fatalf("AddApiKeys: %v", err)
	}
	key := created[0]

	if entry := config.FindApiKeyByValue("sk-extend-me"); entry == nil || !config.ApiKeyExpired(*entry) {
		t.Fatal("precondition: the key should start out expired")
	}

	if _, out := postExtend(t, h, fmt.Sprintf(`{"ids":[%q],"seconds":86400}`, key.ID)); out["error"] != nil {
		t.Fatalf("extend failed: %v", out["error"])
	}

	entry := config.FindApiKeyByValue("sk-extend-me")
	if entry == nil {
		t.Fatal("key lookup failed after extend")
	}
	if config.ApiKeyExpired(*entry) {
		t.Error("key is still reported expired after being extended")
	}
}
