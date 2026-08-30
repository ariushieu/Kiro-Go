package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// seedConfigFile writes a config file with the given API key entries and loads it.
func seedConfigFile(t *testing.T, keys []map[string]interface{}, extra map[string]interface{}) string {
	t.Helper()
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	seed := map[string]interface{}{
		"password": "p",
		"port":     8080,
		"host":     "0.0.0.0",
		"accounts": []interface{}{},
		"apiKeys":  keys,
	}
	for k, v := range extra {
		seed[k] = v
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
	return cfgFile
}

// A key that predates SourceCreditsUsed accumulated its CreditsUsed back when charge
// always equalled cost. Without the backfill its whole historical charge reads as
// margin — the symptom being a real cost far smaller than the reported margin.
func TestSourceCreditsBackfillSeedsPreExistingKeys(t *testing.T) {
	seedConfigFile(t, []map[string]interface{}{{
		"id":            "k1",
		"name":          "Ki2",
		"key":           "sk-old",
		"enabled":       true,
		"creditsUsed":   311.6727,
		"tokensUsed":    33782659,
		"requestsCount": 369,
	}}, nil)

	got := GetApiKeyEntry("k1")
	if got == nil {
		t.Fatalf("key missing after load")
	}
	if got.SourceCreditsUsed != 311.6727 {
		t.Fatalf("expected backfill to 311.6727, got %v", got.SourceCreditsUsed)
	}
	if margin := got.CreditsUsed - got.SourceCreditsUsed; margin != 0 {
		t.Fatalf("historical usage must report zero margin, got %v", margin)
	}
}

// Once seeded, the flag stops the backfill from running again. This is the case the
// flag exists for: a key reset to zero and then charged for a request whose upstream
// cost was zero would match a naive "source==0 && credits!=0" test and be re-seeded,
// silently reporting margin the operator never actually earned.
func TestSourceCreditsBackfillRunsOnlyOnce(t *testing.T) {
	cfgFile := seedConfigFile(t, []map[string]interface{}{{
		"id":          "k1",
		"key":         "sk-old",
		"enabled":     true,
		"creditsUsed": 100.0,
	}}, nil)

	if got := GetApiKeyEntry("k1").SourceCreditsUsed; got != 100 {
		t.Fatalf("first load should seed 100, got %v", got)
	}

	// Simulate the narrow post-reset case, then reload from the same file.
	cfgLock.Lock()
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == "k1" {
			cfg.ApiKeys[i].SourceCreditsUsed = 0
		}
	}
	err := saveLocked()
	cfgLock.Unlock()
	if err != nil {
		t.Fatalf("clear source credits: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if got := GetApiKeyEntry("k1").SourceCreditsUsed; got != 0 {
		t.Fatalf("backfill must not re-run, expected 0 got %v", got)
	}
}

// A fresh install has nothing to backfill but must still record that the migration
// ran, so a key created later and legitimately holding a zero source cost is never
// retroactively seeded.
func TestSourceCreditsBackfillMarksFreshInstall(t *testing.T) {
	seedConfigFile(t, nil, nil)

	cfgLock.RLock()
	seeded := cfg.SourceCreditsSeeded
	cfgLock.RUnlock()

	if !seeded {
		t.Fatalf("expected SourceCreditsSeeded to be set on a fresh install")
	}
}
