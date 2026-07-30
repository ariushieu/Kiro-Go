package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type importResponse struct {
	Success  bool         `json:"success"`
	Total    int          `json:"total"`
	Imported int          `json:"imported"`
	Skipped  int          `json:"skipped"`
	Invalid  int          `json:"invalid"`
	ApiKeys  []apiKeyView `json:"apiKeys"`
	Error    string       `json:"error"`
}

func postImport(t *testing.T, body string) (int, importResponse) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/api-keys/import", strings.NewReader(body))
	w := httptest.NewRecorder()

	h := &Handler{}
	h.apiRestoreApiKeys(w, r)

	var out importResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal %q: %v", w.Body.String(), err)
	}
	return w.Code, out
}

// exportPayload runs a cleartext export of everything currently configured and returns
// the raw JSON, i.e. exactly the file an operator downloads from the panel.
func exportPayload(t *testing.T) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/admin/api/api-keys/export",
		strings.NewReader(`{"includeSecrets":true}`))
	w := httptest.NewRecorder()

	h := &Handler{}
	h.apiExportApiKeys(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("export failed: %d %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestImportApiKeysRoundTrip is the migration scenario end to end: export with secrets
// on server A, import into a fresh config (server B), and confirm the keys actually
// authenticate there with their quota state intact.
func TestImportApiKeysRoundTrip(t *testing.T) {
	mustInitConfig(t)

	seeded, err := config.AddApiKey(config.ApiKeyEntry{
		Name:        "customer-a",
		Key:         "sk-customer-a",
		Enabled:     true,
		TokenLimit:  10000,
		CreditLimit: 25,
		RPMLimit:    60,
		IPLimit:     2,
		IPAllowlist: []string{"198.51.100.7"},
		TPMLimit:    4000,
		Models:      []string{"claude-sonnet-4-5"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := config.RecordApiKeyUsage(seeded.ID, 2500, 4.5); err != nil {
		t.Fatalf("usage: %v", err)
	}
	payload := exportPayload(t)

	// Fresh server: new empty config, then import the file.
	mustInitConfig(t)
	if len(config.ListApiKeys()) != 0 {
		t.Fatalf("expected an empty config before import")
	}

	code, out := postImport(t, payload)
	if code != http.StatusOK {
		t.Fatalf("import failed: %d %+v", code, out)
	}
	if out.Imported != 1 || out.Skipped != 0 || out.Invalid != 0 {
		t.Fatalf("want 1 imported/0 skipped/0 invalid, got %+v", out)
	}

	restored := config.FindApiKeyByValue("sk-customer-a")
	if restored == nil {
		t.Fatalf("imported key does not resolve by its value")
	}
	if restored.ID == seeded.ID {
		t.Fatalf("import must assign a fresh ID, reused %s", seeded.ID)
	}
	if restored.Name != "customer-a" || !restored.Enabled {
		t.Fatalf("name/enabled not restored: %+v", restored)
	}
	if restored.TokenLimit != 10000 || restored.CreditLimit != 25 {
		t.Fatalf("limits not restored: %+v", restored)
	}
	if restored.RPMLimit != 60 || restored.IPLimit != 2 || restored.TPMLimit != 4000 {
		t.Fatalf("rate/IP limits not restored: %+v", restored)
	}
	if len(restored.IPAllowlist) != 1 || restored.IPAllowlist[0] != "198.51.100.7" {
		t.Fatalf("ipAllowlist not restored: %+v", restored.IPAllowlist)
	}
	if len(restored.Models) != 1 || restored.Models[0] != "claude-sonnet-4-5" {
		t.Fatalf("models not restored: %+v", restored.Models)
	}
	// Counters carried over so the customer's quota resumes rather than resetting.
	if restored.TokensUsed != 2500 || restored.CreditsUsed != 4.5 || restored.RequestsCount != 1 {
		t.Fatalf("current-period counters not restored: %+v", restored)
	}
	if restored.LifetimeTokens != 2500 || restored.LifetimeCredits != 4.5 || restored.LifetimeRequests != 1 {
		t.Fatalf("lifetime counters not restored: %+v", restored)
	}

	// The restored key must work for real traffic, not just look right in config.
	// The request has to come from the restored ipAllowlist entry — which doubles as
	// proof the allowlist survived the round-trip and is being enforced.
	requireAuth(t)
	h := &Handler{}
	authReq := newAuthTestRequest(t, "X-Api-Key", "sk-customer-a")
	authReq.RemoteAddr = "198.51.100.7:44321"
	entry, authErr := h.authenticate(authReq)
	if authErr != nil {
		t.Fatalf("restored key failed to authenticate: %v", authErr)
	}
	if entry == nil || entry.Name != "customer-a" {
		t.Fatalf("authenticate returned unexpected entry: %+v", entry)
	}

	// And an IP outside the restored allowlist must still be turned away.
	blockedReq := newAuthTestRequest(t, "X-Api-Key", "sk-customer-a")
	blockedReq.RemoteAddr = "203.0.113.99:1234"
	if _, err := h.authenticate(blockedReq); err == nil {
		t.Fatalf("restored ipAllowlist should reject an unlisted IP")
	}
}

// TestImportApiKeysIdempotent: importing the same file twice must not duplicate keys.
func TestImportApiKeysIdempotent(t *testing.T) {
	mustInitConfig(t)
	if _, err := config.AddApiKey(config.ApiKeyEntry{Name: "a", Key: "sk-dup-a", Enabled: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	payload := exportPayload(t)

	mustInitConfig(t)
	if code, out := postImport(t, payload); code != http.StatusOK || out.Imported != 1 {
		t.Fatalf("first import: %d %+v", code, out)
	}
	code, out := postImport(t, payload)
	if code != http.StatusOK {
		t.Fatalf("second import failed: %d %+v", code, out)
	}
	if out.Imported != 0 || out.Skipped != 1 {
		t.Fatalf("second import should skip everything, got %+v", out)
	}
	if n := len(config.ListApiKeys()); n != 1 {
		t.Fatalf("expected 1 key after re-import, got %d", n)
	}
}

// TestImportApiKeysRejectsMaskedExport is the likely operator mistake: importing the
// default masked report. It has keyMasked but no key, so it must fail loudly rather
// than report a successful no-op.
func TestImportApiKeysRejectsMaskedExport(t *testing.T) {
	mustInitConfig(t)
	if _, err := config.AddApiKey(config.ApiKeyEntry{Name: "m", Key: "sk-masked-source", Enabled: true}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/admin/api/api-keys/export", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	(&Handler{}).apiExportApiKeys(w, r)
	maskedPayload := w.Body.String()

	mustInitConfig(t)
	code, out := postImport(t, maskedPayload)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for masked payload, got %d %+v", code, out)
	}
	if !strings.Contains(out.Error, "masked") {
		t.Fatalf("error should explain the payload is masked, got %q", out.Error)
	}
	if n := len(config.ListApiKeys()); n != 0 {
		t.Fatalf("masked import must create nothing, got %d keys", n)
	}
}

// TestImportApiKeysBareArray covers `jq '.apiKeys' data/config.json` output, which has
// no envelope. Also checks the enabled default and that an empty key is counted invalid
// instead of failing the batch.
func TestImportApiKeysBareArray(t *testing.T) {
	mustInitConfig(t)

	code, out := postImport(t, `[
		{"key":"sk-bare-1","name":"one"},
		{"key":"","name":"empty"},
		{"key":"sk-bare-2","name":"two","enabled":false}
	]`)
	if code != http.StatusOK {
		t.Fatalf("import failed: %d %+v", code, out)
	}
	if out.Imported != 2 || out.Invalid != 1 {
		t.Fatalf("want 2 imported/1 invalid, got %+v", out)
	}

	one := config.FindApiKeyByValue("sk-bare-1")
	if one == nil || !one.Enabled {
		t.Fatalf("a key omitting \"enabled\" should default to enabled: %+v", one)
	}
	two := config.FindApiKeyByValue("sk-bare-2")
	if two == nil || two.Enabled {
		t.Fatalf("explicit enabled=false must be honoured: %+v", two)
	}
}

// TestImportApiKeysDropsUnknownBoundAccounts: bound account IDs come from the source
// server, where accounts had different UUIDs. Dead references must not persist.
func TestImportApiKeysDropsUnknownBoundAccounts(t *testing.T) {
	mustInitConfig(t)

	code, out := postImport(t, `{"apiKeys":[
		{"key":"sk-bound","name":"bound","boundAccountIds":["acct-from-old-server"]}
	]}`)
	if code != http.StatusOK || out.Imported != 1 {
		t.Fatalf("import failed: %d %+v", code, out)
	}
	got := config.FindApiKeyByValue("sk-bound")
	if got == nil {
		t.Fatalf("key not imported")
	}
	if len(got.BoundAccountIDs) != 0 {
		t.Fatalf("unknown bound account IDs should be dropped, got %+v", got.BoundAccountIDs)
	}
}

// TestImportApiKeysInvalidBodies covers the 400 paths that aren't the masked case.
func TestImportApiKeysInvalidBodies(t *testing.T) {
	mustInitConfig(t)

	for name, body := range map[string]string{
		"malformed":    `{"apiKeys":[`,
		"emptyList":    `{"apiKeys":[]}`,
		"emptyArray":   `[]`,
		"wrongShape":   `{"keys":"sk-a"}`,
		"allEmptyKeys": `[{"name":"x"},{"name":"y"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			code, out := postImport(t, body)
			if code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d %+v", code, out)
			}
			if out.Error == "" {
				t.Fatalf("expected an error message")
			}
		})
	}
	if n := len(config.ListApiKeys()); n != 0 {
		t.Fatalf("no key should have been created, got %d", n)
	}
}

// TestImportApiKeysRouting guards the switch-case ordering in handleAdminAPI: the
// literal /api-keys/import case must precede the generic /api-keys/<id> prefix cases,
// or "import" gets treated as a key ID and the POST 404s.
func TestImportApiKeysRouting(t *testing.T) {
	mustInitConfig(t)
	config.SetPassword("s3cret")
	h := &Handler{
		adminGuard:    newAdminAuthGuard(10, time.Minute, time.Minute),
		adminSessions: newAdminSessionStore(time.Hour),
	}

	r := httptest.NewRequest(http.MethodPost, config.GetAdminPath()+"/api/api-keys/import",
		strings.NewReader(`[{"key":"sk-routed","name":"routed"}]`))
	r.Header.Set("X-Admin-Password", "s3cret")
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()

	h.handleAdminAPI(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("import route not reached: %d %s", w.Code, w.Body.String())
	}
	if config.FindApiKeyByValue("sk-routed") == nil {
		t.Fatalf("key was not imported through the admin route")
	}
}

// TestImportApiKeysIgnoresMigratedFlag: Migrated is internal bookkeeping for the
// legacy single-ApiKey migration. A request body must not be able to set it.
func TestImportApiKeysIgnoresMigratedFlag(t *testing.T) {
	mustInitConfig(t)

	if code, out := postImport(t, `[{"key":"sk-mig","migrated":true}]`); code != http.StatusOK || out.Imported != 1 {
		t.Fatalf("import failed: %d %+v", code, out)
	}
	got := config.FindApiKeyByValue("sk-mig")
	if got == nil {
		t.Fatalf("key not imported")
	}
	if got.Migrated {
		t.Fatalf("migrated must not be settable from the request body")
	}
}
