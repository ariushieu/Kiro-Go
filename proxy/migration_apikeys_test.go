package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file tests the full server-migration path over real HTTP, rather than by calling
// handlers directly: an operator exports keys from one instance and imports them into a
// second instance with its own config file on disk. Live customer keys depend on this
// working end to end, so the assertions are about observable behaviour (can a customer
// still authenticate? is their remaining quota the same?) rather than struct fields.

// migrationServer is one Kiro-Go instance backed by its own config.json.
type migrationServer struct {
	t          *testing.T
	srv        *httptest.Server
	handler    *Handler
	configPath string
	password   string
}

// newMigrationServer boots an instance on a fresh config file. Only one may be active
// at a time: package config keeps a single global, so "two servers" means switching the
// global over via config.Init, which activate() does.
//
// The DoS guard is constructed with proxy trust on, because resolveClientIP delegates to
// it — without a guard, X-Forwarded-For is ignored and every request looks like
// 127.0.0.1, which would make per-key IP restrictions untestable. This mirrors the
// documented nginx deployment (DEPLOYMENT.md §5.2).
func newMigrationServer(t *testing.T, name, password string) *migrationServer {
	t.Helper()
	t.Setenv("KIRO_TRUST_PROXY", "true")
	t.Setenv("KIRO_TRUSTED_PROXY_HOPS", "1")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, name+"-config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("init %s: %v", name, err)
	}
	config.SetPassword(password)

	h := &Handler{
		// Production wires these in NewHandler; ServeHTTP dereferences pool on the
		// /v1/* path and resolveClientIP delegates to guard, so both are required.
		pool:          pool.GetPool(),
		guard:         newDosGuard(loadDosGuardConfig()),
		adminGuard:    newAdminAuthGuard(50, time.Minute, time.Minute),
		adminSessions: newAdminSessionStore(time.Hour),
		rpmThrottle:   newRPMThrottle(),
		ipLimiter:     newIPLimiter(),
	}
	ms := &migrationServer{t: t, handler: h, configPath: cfgPath, password: password}
	ms.srv = httptest.NewServer(h)
	t.Cleanup(ms.srv.Close)
	return ms
}

// activate re-points the global config at this instance's file, simulating "this is the
// box you're now talking to".
func (m *migrationServer) activate() {
	m.t.Helper()
	if err := config.Init(m.configPath); err != nil {
		m.t.Fatalf("activate %s: %v", m.configPath, err)
	}
	config.SetPassword(m.password)
}

// adminPost issues an admin API call over HTTP with the password header.
func (m *migrationServer) adminPost(path, body string) (int, string) {
	m.t.Helper()
	req, err := http.NewRequest(http.MethodPost, m.srv.URL+config.GetAdminPath()+"/api"+path,
		strings.NewReader(body))
	if err != nil {
		m.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Admin-Password", m.password)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		m.t.Fatalf("POST %s: %v", path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

// callAPI sends a real /v1/messages request with a customer key and returns the status
// plus body. It goes through the same ServeHTTP → authenticate path production traffic
// uses.
//
// Note on expected results: an unknown key gets a 401, but a key that is valid yet
// *blocked* (over limit, expired, wrong IP) deliberately gets HTTP 200 carrying the
// limit-notice text as an assistant reply — see sendClaudeNotice — so client tooling
// does not hard-fail. Callers therefore inspect the body, not just the status.
func (m *migrationServer) callAPI(apiKey, clientIP string) (int, string) {
	m.t.Helper()
	req, err := http.NewRequest(http.MethodPost, m.srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		m.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	if clientIP != "" {
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		m.t.Fatalf("POST /v1/messages: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

// limitNoticeText is the built-in blocked-key reply, used to tell "key accepted" apart
// from "key valid but blocked" given both return HTTP 200.
const limitNoticeText = "reached its limit or has expired"

// seedCustomer creates a key the way the admin panel does, over HTTP.
func (m *migrationServer) seedCustomer(t *testing.T, payload string) string {
	t.Helper()
	code, body := m.adminPost("/api-keys", payload)
	if code != http.StatusOK {
		t.Fatalf("create key: %d %s", code, body)
	}
	var out struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode create: %v (%s)", err, body)
	}
	if out.Key == "" {
		t.Fatalf("expected a cleartext key on creation: %s", body)
	}
	return out.Key
}

// TestMigrationKeysOnlyEndToEnd is the scenario the user actually runs: carry only the
// customer API keys to a new VPS (nginx/.env copied separately), leaving accounts to be
// re-added there. Asserts each customer's key still works, their quota resumes, and
// per-key restrictions are still enforced afterwards.
func TestMigrationKeysOnlyEndToEnd(t *testing.T) {
	oldVPS := newMigrationServer(t, "old", "old-pass")

	// Three customers with different shapes: plain, quota-limited with usage already
	// spent, and IP-restricted.
	plainKey := oldVPS.seedCustomer(t, `{"name":"plain-customer","enabled":true}`)
	quotaKey := oldVPS.seedCustomer(t,
		`{"name":"quota-customer","enabled":true,"tokenLimit":10000,"creditLimit":50,"rpmLimit":60}`)
	ipKey := oldVPS.seedCustomer(t,
		`{"name":"ip-customer","enabled":true,"ipAllowlist":["198.51.100.7"],"ipLimit":2}`)

	// Simulate traffic so the quota customer has history worth preserving.
	quotaEntry := config.FindApiKeyByValue(quotaKey)
	if quotaEntry == nil {
		t.Fatalf("seeded quota key not found")
	}
	for i := 0; i < 3; i++ {
		if err := config.RecordApiKeyUsage(quotaEntry.ID, 1000, 5); err != nil {
			t.Fatalf("record usage: %v", err)
		}
	}
	// Flush like the real shutdown path, so the export reflects persisted state.
	if err := config.FlushDirty(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// Turn the auth gate on — this is the state a live deployment is in.
	on := true
	if err := config.UpdateSettingsPatch(nil, &on, ""); err != nil {
		t.Fatalf("enable auth: %v", err)
	}

	// Operator downloads a cleartext export from the panel.
	code, exported := oldVPS.adminPost("/api-keys/export", `{"includeSecrets":true}`)
	if code != http.StatusOK {
		t.Fatalf("export: %d %s", code, exported)
	}
	for _, k := range []string{plainKey, quotaKey, ipKey} {
		if !strings.Contains(exported, k) {
			t.Fatalf("export is missing a customer key")
		}
	}

	// ---- New VPS: fresh config, no keys, no accounts ----
	newVPS := newMigrationServer(t, "new", "new-pass")
	if n := len(config.ListApiKeys()); n != 0 {
		t.Fatalf("new server should start with no keys, got %d", n)
	}

	code, importBody := newVPS.adminPost("/api-keys/import", exported)
	if code != http.StatusOK {
		t.Fatalf("import: %d %s", code, importBody)
	}
	var imp struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
		Invalid  int `json:"invalid"`
	}
	if err := json.Unmarshal([]byte(importBody), &imp); err != nil {
		t.Fatalf("decode import: %v", err)
	}
	if imp.Imported != 3 || imp.Skipped != 0 || imp.Invalid != 0 {
		t.Fatalf("want 3 imported, got %+v", imp)
	}

	// The gate does NOT come across in the export: it lives on Config, not on keys.
	// Verify that explicitly, since silently-open /v1/* is the dangerous failure.
	if config.IsApiKeyRequired() {
		t.Fatalf("requireApiKey must not be carried by the key export")
	}
	if err := config.UpdateSettingsPatch(nil, &on, ""); err != nil {
		t.Fatalf("enable auth on new server: %v", err)
	}

	// Every customer key must still resolve, with limits intact.
	for _, tc := range []struct {
		name        string
		key         string
		wantName    string
		tokenLimit  int64
		creditLimit float64
		rpm         int
		allowlist   int
	}{
		{"plain", plainKey, "plain-customer", 0, 0, 0, 0},
		{"quota", quotaKey, "quota-customer", 10000, 50, 60, 0},
		{"ip", ipKey, "ip-customer", 0, 0, 0, 1},
	} {
		got := config.FindApiKeyByValue(tc.key)
		if got == nil {
			t.Fatalf("%s: key did not survive migration", tc.name)
		}
		if got.Name != tc.wantName || !got.Enabled {
			t.Fatalf("%s: name/enabled wrong: %+v", tc.name, got)
		}
		if got.TokenLimit != tc.tokenLimit || got.CreditLimit != tc.creditLimit {
			t.Fatalf("%s: limits wrong: %+v", tc.name, got)
		}
		if got.RPMLimit != tc.rpm {
			t.Fatalf("%s: rpmLimit wrong: %+v", tc.name, got)
		}
		if len(got.IPAllowlist) != tc.allowlist {
			t.Fatalf("%s: ipAllowlist wrong: %+v", tc.name, got.IPAllowlist)
		}
	}

	// Quota must resume, not reset — otherwise a customer over their limit gets a free
	// refill just because the operator moved servers.
	q := config.FindApiKeyByValue(quotaKey)
	if q.TokensUsed != 3000 || q.CreditsUsed != 15 || q.RequestsCount != 3 {
		t.Fatalf("current-period usage not carried over: %+v", q)
	}
	if q.LifetimeTokens != 3000 || q.LifetimeCredits != 15 || q.LifetimeRequests != 3 {
		t.Fatalf("lifetime usage not carried over: %+v", q)
	}

	// Real request path. A migrated key must get past auth (it then fails downstream on
	// "no accounts", which is expected here); an unknown key must be rejected outright.
	if code, body := newVPS.callAPI(plainKey, ""); code == http.StatusUnauthorized {
		t.Fatalf("migrated key rejected as unauthorized: %s", body)
	} else if strings.Contains(body, limitNoticeText) {
		t.Fatalf("migrated key was treated as blocked: %s", body)
	}
	if code, _ := newVPS.callAPI("sk-never-existed", ""); code != http.StatusUnauthorized {
		t.Fatalf("unknown key should be 401, got %d", code)
	}

	// The IP allowlist must still bite on the new server. A blocked key returns 200 plus
	// the limit notice rather than an error status, so assert on the body.
	if _, body := newVPS.callAPI(ipKey, "203.0.113.99"); !strings.Contains(body, limitNoticeText) {
		t.Fatalf("ip-restricted key should be blocked for an unlisted IP, got: %s", body)
	}
	// ...and still be served for the address it was restored with.
	if _, body := newVPS.callAPI(ipKey, "198.51.100.7"); strings.Contains(body, limitNoticeText) {
		t.Fatalf("ip-restricted key must be served for its allowlisted IP, got: %s", body)
	}
}

// TestMigrationImportedKeysPersistToDisk: the imported keys must be in config.json, not
// only in memory — otherwise the first container restart on the new VPS loses every
// customer.
func TestMigrationImportedKeysPersistToDisk(t *testing.T) {
	oldVPS := newMigrationServer(t, "old", "pw")
	key := oldVPS.seedCustomer(t, `{"name":"persisted","enabled":true,"tokenLimit":777}`)

	code, exported := oldVPS.adminPost("/api-keys/export", `{"includeSecrets":true}`)
	if code != http.StatusOK {
		t.Fatalf("export: %d %s", code, exported)
	}

	newVPS := newMigrationServer(t, "new", "pw")
	if code, body := newVPS.adminPost("/api-keys/import", exported); code != http.StatusOK {
		t.Fatalf("import: %d %s", code, body)
	}

	// Read the file straight off disk — no in-memory help.
	raw, err := os.ReadFile(newVPS.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(raw), key) {
		t.Fatalf("imported key was not persisted to %s", newVPS.configPath)
	}

	// Reload from that file, as a restart would, and confirm the key is usable.
	if err := config.Init(newVPS.configPath); err != nil {
		t.Fatalf("reload: %v", err)
	}
	reloaded := config.FindApiKeyByValue(key)
	if reloaded == nil {
		t.Fatalf("key missing after reload from disk")
	}
	if reloaded.TokenLimit != 777 {
		t.Fatalf("limit lost across reload: %+v", reloaded)
	}
}

// TestMigrationPartialImportOntoExistingKeys covers the messier real case: the new box
// already has some keys (operator created a few by hand), and the import must add the
// missing ones without touching or duplicating what is already there.
func TestMigrationPartialImportOntoExistingKeys(t *testing.T) {
	oldVPS := newMigrationServer(t, "old", "pw")
	keyA := oldVPS.seedCustomer(t, `{"name":"cust-a","enabled":true}`)
	keyB := oldVPS.seedCustomer(t, `{"name":"cust-b","enabled":true}`)
	code, exported := oldVPS.adminPost("/api-keys/export", `{"includeSecrets":true}`)
	if code != http.StatusOK {
		t.Fatalf("export: %d %s", code, exported)
	}

	// New box: cust-a was already recreated manually, with a DIFFERENT limit.
	newVPS := newMigrationServer(t, "new", "pw")
	if code, body := newVPS.adminPost("/api-keys",
		fmt.Sprintf(`{"name":"cust-a-manual","key":%q,"enabled":true,"tokenLimit":123}`, keyA)); code != http.StatusOK {
		t.Fatalf("manual create: %d %s", code, body)
	}

	code, body := newVPS.adminPost("/api-keys/import", exported)
	if code != http.StatusOK {
		t.Fatalf("import: %d %s", code, body)
	}
	var imp struct {
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(body), &imp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if imp.Imported != 1 || imp.Skipped != 1 {
		t.Fatalf("want 1 imported / 1 skipped, got %+v", imp)
	}

	// Exactly two keys, no duplicate of A.
	all := config.ListApiKeys()
	if len(all) != 2 {
		t.Fatalf("want 2 keys, got %d", len(all))
	}
	seen := map[string]int{}
	for _, e := range all {
		seen[e.Key]++
	}
	if seen[keyA] != 1 || seen[keyB] != 1 {
		t.Fatalf("key multiplicity wrong: %+v", seen)
	}

	// The pre-existing entry must be left exactly as it was: skip means skip, not
	// silently overwrite the operator's local edit.
	a := config.FindApiKeyByValue(keyA)
	if a.Name != "cust-a-manual" || a.TokenLimit != 123 {
		t.Fatalf("existing key was modified by the import: %+v", a)
	}
}

// TestMigrationDisabledAndExpiredKeysKeepState: a suspended or expired customer must not
// come back enabled on the new server.
func TestMigrationDisabledAndExpiredKeysKeepState(t *testing.T) {
	oldVPS := newMigrationServer(t, "old", "pw")

	disabledKey := oldVPS.seedCustomer(t, `{"name":"suspended","enabled":false}`)
	expiry := time.Now().Unix() - 3600
	expiredKey := oldVPS.seedCustomer(t,
		fmt.Sprintf(`{"name":"expired","enabled":true,"expiresAt":%d}`, expiry))

	code, exported := oldVPS.adminPost("/api-keys/export", `{"includeSecrets":true}`)
	if code != http.StatusOK {
		t.Fatalf("export: %d %s", code, exported)
	}

	newVPS := newMigrationServer(t, "new", "pw")
	if code, body := newVPS.adminPost("/api-keys/import", exported); code != http.StatusOK {
		t.Fatalf("import: %d %s", code, body)
	}

	d := config.FindApiKeyByValue(disabledKey)
	if d == nil || d.Enabled {
		t.Fatalf("suspended key must stay disabled after migration: %+v", d)
	}
	e := config.FindApiKeyByValue(expiredKey)
	if e == nil {
		t.Fatalf("expired key missing")
	}
	if e.ExpiresAt != expiry {
		t.Fatalf("expiry not preserved: got %d want %d", e.ExpiresAt, expiry)
	}
	if !config.ApiKeyExpired(*e) {
		t.Fatalf("expired key should still read as expired")
	}
}

// TestMigrationExhaustedKeyStaysExhausted: a customer who burned their quota must not
// get a free refill from the move. This is the money-losing failure mode of resetting
// counters on import, so it is asserted through the real request path.
func TestMigrationExhaustedKeyStaysExhausted(t *testing.T) {
	oldVPS := newMigrationServer(t, "old", "pw")
	key := oldVPS.seedCustomer(t, `{"name":"burned","enabled":true,"tokenLimit":1000}`)

	entry := config.FindApiKeyByValue(key)
	if entry == nil {
		t.Fatalf("seed missing")
	}
	if err := config.RecordApiKeyUsage(entry.ID, 1000, 0); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	on := true
	if err := config.UpdateSettingsPatch(nil, &on, ""); err != nil {
		t.Fatalf("enable auth: %v", err)
	}
	// Confirm it is genuinely blocked before the move, so the post-move assertion means
	// something.
	if _, body := oldVPS.callAPI(key, "198.51.100.1"); !strings.Contains(body, limitNoticeText) {
		t.Fatalf("key should already be exhausted on the old server, got: %s", body)
	}

	code, exported := oldVPS.adminPost("/api-keys/export", `{"includeSecrets":true}`)
	if code != http.StatusOK {
		t.Fatalf("export: %d %s", code, exported)
	}

	newVPS := newMigrationServer(t, "new", "pw")
	if code, body := newVPS.adminPost("/api-keys/import", exported); code != http.StatusOK {
		t.Fatalf("import: %d %s", code, body)
	}
	if err := config.UpdateSettingsPatch(nil, &on, ""); err != nil {
		t.Fatalf("enable auth: %v", err)
	}

	restored := config.FindApiKeyByValue(key)
	if restored == nil {
		t.Fatalf("key missing after migration")
	}
	overTok, _ := config.ApiKeyOverLimit(*restored)
	if !overTok {
		t.Fatalf("exhausted key came back under limit: used=%d limit=%d",
			restored.TokensUsed, restored.TokenLimit)
	}
	if _, body := newVPS.callAPI(key, "198.51.100.1"); !strings.Contains(body, limitNoticeText) {
		t.Fatalf("exhausted key must stay blocked after migration, got: %s", body)
	}
}

// TestMigrationManyKeys exercises a realistic customer base in one payload, checking
// nothing is dropped, reordered into a collision, or given a duplicate ID.
func TestMigrationManyKeys(t *testing.T) {
	const total = 250

	oldVPS := newMigrationServer(t, "old", "pw")
	// Use the bulk endpoint, as an operator with many customers would.
	code, body := oldVPS.adminPost("/api-keys/bulk",
		fmt.Sprintf(`{"count":100,"namePrefix":"cust","tokenLimit":5000}`))
	if code != http.StatusOK {
		t.Fatalf("bulk create: %d %s", code, body)
	}
	// Top up past the bulk cap of 100 per call.
	for i := 0; i < 150; i++ {
		oldVPS.seedCustomer(t, fmt.Sprintf(`{"name":"extra-%d","enabled":true}`, i))
	}
	if n := len(config.ListApiKeys()); n != total {
		t.Fatalf("setup: want %d keys, got %d", total, n)
	}

	before := make(map[string]string, total) // key value -> name
	for _, e := range config.ListApiKeys() {
		before[e.Key] = e.Name
	}

	code, exported := oldVPS.adminPost("/api-keys/export", `{"includeSecrets":true}`)
	if code != http.StatusOK {
		t.Fatalf("export: %d %s", code, exported)
	}

	newVPS := newMigrationServer(t, "new", "pw")
	code, importBody := newVPS.adminPost("/api-keys/import", exported)
	if code != http.StatusOK {
		t.Fatalf("import: %d %s", code, importBody)
	}
	var imp struct {
		Total    int `json:"total"`
		Imported int `json:"imported"`
		Skipped  int `json:"skipped"`
		Invalid  int `json:"invalid"`
	}
	if err := json.Unmarshal([]byte(importBody), &imp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if imp.Total != total || imp.Imported != total || imp.Skipped != 0 || imp.Invalid != 0 {
		t.Fatalf("want all %d imported, got %+v", total, imp)
	}

	after := config.ListApiKeys()
	if len(after) != total {
		t.Fatalf("want %d keys after import, got %d", total, len(after))
	}
	ids := make(map[string]bool, total)
	for _, e := range after {
		wantName, ok := before[e.Key]
		if !ok {
			t.Fatalf("unexpected key value after import: %s", config.MaskApiKey(e.Key))
		}
		if e.Name != wantName {
			t.Fatalf("name mismatch for %s: want %q got %q", config.MaskApiKey(e.Key), wantName, e.Name)
		}
		if ids[e.ID] {
			t.Fatalf("duplicate ID assigned: %s", e.ID)
		}
		ids[e.ID] = true
		delete(before, e.Key)
	}
	if len(before) != 0 {
		t.Fatalf("%d key(s) did not survive the migration", len(before))
	}
}

// TestMigrationMaskedExportIsNotSilentlyAccepted: the operator downloads the default
// (masked) export by mistake and imports it. This must fail loudly and change nothing,
// so nobody concludes the migration succeeded and decommissions the old VPS.
func TestMigrationMaskedExportIsNotSilentlyAccepted(t *testing.T) {
	oldVPS := newMigrationServer(t, "old", "pw")
	oldVPS.seedCustomer(t, `{"name":"cust","enabled":true}`)

	code, masked := oldVPS.adminPost("/api-keys/export", `{}`)
	if code != http.StatusOK {
		t.Fatalf("export: %d %s", code, masked)
	}
	if strings.Contains(masked, `"key"`) {
		t.Fatalf("default export must not contain cleartext keys: %s", masked)
	}

	newVPS := newMigrationServer(t, "new", "pw")
	code, body := newVPS.adminPost("/api-keys/import", masked)
	if code != http.StatusBadRequest {
		t.Fatalf("masked import must be rejected, got %d %s", code, body)
	}
	if !strings.Contains(body, "masked") {
		t.Fatalf("error should name the cause: %s", body)
	}
	if n := len(config.ListApiKeys()); n != 0 {
		t.Fatalf("nothing should have been created, got %d keys", n)
	}
}

// TestMigrationViaConfigJsonArray covers the documented alternative to the panel export:
// `jq '.apiKeys' data/config.json` from the old box, pasted into Import.
func TestMigrationViaConfigJsonArray(t *testing.T) {
	oldVPS := newMigrationServer(t, "old", "pw")
	key := oldVPS.seedCustomer(t, `{"name":"jq-customer","enabled":true,"tokenLimit":4242}`)

	// Pull the apiKeys array out of the on-disk config, as jq would.
	raw, err := os.ReadFile(oldVPS.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var onDisk struct {
		ApiKeys []map[string]interface{} `json:"apiKeys"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	bare, err := json.Marshal(onDisk.ApiKeys)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	newVPS := newMigrationServer(t, "new", "pw")
	code, body := newVPS.adminPost("/api-keys/import", string(bare))
	if code != http.StatusOK {
		t.Fatalf("bare-array import: %d %s", code, body)
	}
	got := config.FindApiKeyByValue(key)
	if got == nil {
		t.Fatalf("key not imported from a raw config.json array")
	}
	if got.Name != "jq-customer" || got.TokenLimit != 4242 {
		t.Fatalf("fields lost importing from config.json: %+v", got)
	}
}
