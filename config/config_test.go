package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAICompatibleAccountValidationAndNormalization(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := Account{
		ID: "openai-1", Backend: BackendOpenAICompatible,
		ApiKey: "secret", BaseURL: "http://127.0.0.1:8081/v1/",
		Models: []string{" gpt-4.1 ", "gpt-4.1", "gpt-*"}, Enabled: true,
	}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	got := GetAccounts()[0]
	if got.BaseURL != "http://127.0.0.1:8081/v1" {
		t.Fatalf("unexpected normalized baseURL %q", got.BaseURL)
	}
	if len(got.Models) != 2 || got.Models[0] != "gpt-4.1" || got.Models[1] != "gpt-*" {
		t.Fatalf("unexpected normalized models: %#v", got.Models)
	}
	if got.EffectiveAPIFormat() != APIFormatOpenAI {
		t.Fatalf("unexpected default api format %q", got.EffectiveAPIFormat())
	}
}

func TestOpenAICompatibleAccountRejectsUnsafeOrIncompleteConfig(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	cases := []Account{
		{ID: "missing-key", Backend: BackendOpenAICompatible, BaseURL: "https://example.com/v1", Models: []string{"gpt-4.1"}},
		{ID: "unsafe-http", Backend: BackendOpenAICompatible, ApiKey: "secret", BaseURL: "http://example.com/v1", Models: []string{"gpt-4.1"}},
		{ID: "missing-model", Backend: BackendOpenAICompatible, ApiKey: "secret", BaseURL: "https://example.com/v1"},
		{ID: "bad-format", Backend: BackendOpenAICompatible, APIFormat: "other", ApiKey: "secret", BaseURL: "https://example.com/v1", Models: []string{"gpt-4.1"}},
	}
	for _, account := range cases {
		if err := AddAccount(account); err == nil {
			t.Fatalf("expected account %q to be rejected", account.ID)
		}
	}
}

func TestCustomUpstreamPricingNormalization(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := Account{
		ID: "priced", Backend: BackendOpenAICompatible, ApiKey: "secret",
		BaseURL: "https://example.com/v1", Models: []string{"model"},
		Pricing: &UpstreamPricing{InputPerMillion: 10, OutputPerMillion: 50},
	}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add priced account: %v", err)
	}
	got := GetAccounts()[0].Pricing
	if got == nil || got.Markup != 1.4 || got.MinChargeUSD != 0.001 {
		t.Fatalf("unexpected normalized pricing: %#v", got)
	}

	account.ID = "bad-price"
	account.Pricing = &UpstreamPricing{InputPerMillion: -1, Markup: 0.5}
	if err := AddAccount(account); err == nil {
		t.Fatal("expected invalid pricing to be rejected")
	}
}

func TestUpdateSettingsPatchPreservesOmittedAPIKeyFields(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := UpdateSettingsPatch(nil, nil, "new-admin-password"); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "proxy-api-key" {
		t.Fatalf("expected API key to be preserved, got %q", got)
	}
	if !IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to stay enabled")
	}
	if got := GetPassword(); got != "new-admin-password" {
		t.Fatalf("expected password to update, got %q", got)
	}
}

func TestUpdateSettingsPatchCanExplicitlyDisableAPIKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	emptyKey := ""
	requireAPIKey := false
	if err := UpdateSettingsPatch(&emptyKey, &requireAPIKey, ""); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "" {
		t.Fatalf("expected API key to be cleared, got %q", got)
	}
	if IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to be disabled")
	}
	if got := GetPassword(); got != "admin-password" {
		t.Fatalf("expected password to be preserved, got %q", got)
	}
}

func TestRequireProxyRoundTrip(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if GetRequireProxy() {
		t.Fatalf("expected require-proxy to default off")
	}
	if err := UpdateRequireProxy(true); err != nil {
		t.Fatalf("update require-proxy: %v", err)
	}
	if !GetRequireProxy() {
		t.Fatalf("expected require-proxy to be enabled after update")
	}
}

func TestProxyPoolAddDedupeAndRemove(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	const u = "socks5://host:1080"
	if err := AddProxyToPool(u); err != nil {
		t.Fatalf("add proxy: %v", err)
	}
	if err := AddProxyToPool(u); err != nil {
		t.Fatalf("add proxy (dup): %v", err)
	}
	pool := GetProxyPool()
	if len(pool) != 1 {
		t.Fatalf("expected 1 entry after dup add, got %d", len(pool))
	}
	if !pool[0].Healthy {
		t.Fatalf("expected new entry to be Healthy=true")
	}
	if pool[0].LastOKAt == 0 {
		t.Fatalf("expected LastOKAt to be set on new entry")
	}
	if err := RemoveProxyFromPool(u); err != nil {
		t.Fatalf("remove proxy: %v", err)
	}
	if got := len(GetProxyPool()); got != 0 {
		t.Fatalf("expected empty pool after remove, got %d", got)
	}
}

func TestMarkProxyUnhealthyHealthyTransition(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	const u = "http://host:3128"
	if err := AddProxyToPool(u); err != nil {
		t.Fatalf("add proxy: %v", err)
	}

	// First unhealthy: healthy -> unhealthy transition.
	changed, err := MarkProxyUnhealthy(u)
	if err != nil {
		t.Fatalf("mark unhealthy: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true on first unhealthy")
	}
	// Second unhealthy: already unhealthy, no transition but FailCount still increments.
	changed, err = MarkProxyUnhealthy(u)
	if err != nil {
		t.Fatalf("mark unhealthy (2): %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false on second unhealthy")
	}

	pool := GetProxyPool()
	if len(pool) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(pool))
	}
	if pool[0].Healthy {
		t.Fatalf("expected Healthy=false after unhealthy calls")
	}
	if pool[0].FailCount != 2 {
		t.Fatalf("expected FailCount=2, got %d", pool[0].FailCount)
	}
	if pool[0].LastFailAt == 0 {
		t.Fatalf("expected LastFailAt to be set")
	}

	// First healthy: unhealthy -> healthy transition.
	changed, err = MarkProxyHealthy(u)
	if err != nil {
		t.Fatalf("mark healthy: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true on first healthy")
	}
	// Second healthy: already healthy, no transition.
	changed, err = MarkProxyHealthy(u)
	if err != nil {
		t.Fatalf("mark healthy (2): %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false on second healthy")
	}

	pool = GetProxyPool()
	if !pool[0].Healthy {
		t.Fatalf("expected Healthy=true after healthy calls")
	}
	if pool[0].FailCount != 0 {
		t.Fatalf("expected FailCount reset to 0, got %d", pool[0].FailCount)
	}
	if pool[0].LastOKAt == 0 {
		t.Fatalf("expected LastOKAt to be set")
	}
}

func TestProxyPoolRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	urls := []string{"socks5://a:1080", "http://b:3128"}
	for _, u := range urls {
		if err := AddProxyToPool(u); err != nil {
			t.Fatalf("add proxy %q: %v", u, err)
		}
	}

	// Reload a fresh Config from the same path.
	if err := Init(path); err != nil {
		t.Fatalf("reload config: %v", err)
	}
	pool := GetProxyPool()
	if len(pool) != len(urls) {
		t.Fatalf("expected %d entries after reload, got %d", len(urls), len(pool))
	}
	got := map[string]bool{}
	for _, p := range pool {
		got[p.URL] = true
	}
	for _, u := range urls {
		if !got[u] {
			t.Fatalf("expected %q to survive reload", u)
		}
	}
}

// TestAccountAllowOverageMigration verifies that a config.json from before the
// upstream-Overages-switch refactor (which carried `allowOverage: true` per
// account) is migrated into OverageStatus="ENABLED" on first load, and that
// the legacy field is cleared so future saves don't re-emit it.
func TestAccountAllowOverageMigration(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	seed := map[string]interface{}{
		"password":      "p",
		"port":          8080,
		"host":          "0.0.0.0",
		"requireApiKey": false,
		"accounts": []map[string]interface{}{
			{"id": "acc-allow", "enabled": true, "allowOverage": true},
			{"id": "acc-deny", "enabled": true, "allowOverage": false},
			{"id": "acc-already-set", "enabled": true, "allowOverage": true, "overageStatus": "DISABLED"},
		},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	accounts := GetAccounts()
	byID := map[string]Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}

	if got := byID["acc-allow"].OverageStatus; got != "ENABLED" {
		t.Fatalf("expected acc-allow to migrate to OverageStatus=ENABLED, got %q", got)
	}
	if byID["acc-allow"].LegacyAllowOverage {
		t.Fatalf("expected legacy allowOverage to be cleared after migration")
	}
	if got := byID["acc-deny"].OverageStatus; got != "" {
		t.Fatalf("expected acc-deny to keep empty OverageStatus, got %q", got)
	}
	// Pre-set OverageStatus must win over the legacy field.
	if got := byID["acc-already-set"].OverageStatus; got != "DISABLED" {
		t.Fatalf("expected acc-already-set OverageStatus to be preserved, got %q", got)
	}
	if byID["acc-already-set"].LegacyAllowOverage {
		t.Fatalf("expected legacy field to still be cleared on acc-already-set")
	}

	// Re-read the file and confirm legacy field is gone (so it doesn't drift
	// back in on later saves).
	on_disk, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var reloaded struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(on_disk, &reloaded); err != nil {
		t.Fatalf("decode reload: %v", err)
	}
	for _, a := range reloaded.Accounts {
		if _, ok := a["allowOverage"]; ok {
			t.Fatalf("expected allowOverage to be omitted from persisted file, got %+v", a)
		}
	}
}

func TestAdminPathOverrideNormalization(t *testing.T) {
	t.Cleanup(func() { SetAdminPath("") })

	if got := GetAdminPath(); got != DefaultAdminPath {
		t.Fatalf("default: want %q, got %q", DefaultAdminPath, got)
	}
	for in, want := range map[string]string{
		"panel":           "/panel",
		"/panel":          "/panel",
		"/panel/":         "/panel",
		"  /ops/panel/  ": "/ops/panel",
		"my-secret-9f2a":  "/my-secret-9f2a",
	} {
		SetAdminPath(in)
		if got := GetAdminPath(); got != want {
			t.Errorf("SetAdminPath(%q): want %q, got %q", in, want, got)
		}
	}
	// Clearing the override restores the default.
	SetAdminPath("")
	if got := GetAdminPath(); got != DefaultAdminPath {
		t.Fatalf("cleared: want %q, got %q", DefaultAdminPath, got)
	}
}
