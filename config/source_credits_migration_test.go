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

// The case that motivated the fix, and that generation 1 missed: a key already
// serving traffic when SourceCreditsUsed appeared has a non-zero source cost covering
// only part of its life, so a "source == 0" test skips it and it keeps reporting its
// whole pre-upgrade charge as margin. These are the real numbers off key Ki3.
func TestSourceCreditsBackfillResetsPartiallyTrackedKey(t *testing.T) {
	seedConfigFile(t, []map[string]interface{}{{
		"id":                "k1",
		"name":              "Ki3",
		"key":               "sk-partial",
		"enabled":           true,
		"creditsUsed":       32.0631,
		"sourceCreditsUsed": 8.9285,
		"requestsCount":     22,
	}}, nil)

	got := GetApiKeyEntry("k1")
	if got.SourceCreditsUsed != 32.0631 {
		t.Fatalf("expected baseline reset to 32.0631, got %v", got.SourceCreditsUsed)
	}
	if margin := got.CreditsUsed - got.SourceCreditsUsed; margin != 0 {
		t.Fatalf("expected zero margin after reset, got %v", margin)
	}
}

// After the reset, real margin accrues and must survive a restart. This is why the
// backfill is gated on a stored generation instead of the data: post-reset state
// (source < credits) looks exactly like the state the reset corrects.
func TestSourceCreditsBackfillPreservesEarnedMargin(t *testing.T) {
	cfgFile := seedConfigFile(t, []map[string]interface{}{{
		"id":          "k1",
		"key":         "sk-old",
		"enabled":     true,
		"creditsUsed": 100.0,
	}}, nil)

	if got := GetApiKeyEntry("k1").SourceCreditsUsed; got != 100 {
		t.Fatalf("first load should reset baseline to 100, got %v", got)
	}

	// Earn real margin: charged 130 against a real cost of 100. The counters are a hot
	// path and only mark the config dirty, so flush before reloading from disk.
	if err := RecordApiKeyUsageWithCost("k1", 0, 30, 0); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if err := FlushDirty(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("reload: %v", err)
	}

	got := GetApiKeyEntry("k1")
	if got.CreditsUsed != 130 || got.SourceCreditsUsed != 100 {
		t.Fatalf("restart wiped earned margin: charged %v source %v", got.CreditsUsed, got.SourceCreditsUsed)
	}
}

// A config that ran generation 1 must still get the corrected reset, otherwise the
// servers that hit the bug are exactly the ones the fix skips.
func TestSourceCreditsBackfillUpgradesFromGenerationOne(t *testing.T) {
	seedConfigFile(t, []map[string]interface{}{{
		"id":                "k1",
		"key":               "sk-gen1",
		"enabled":           true,
		"creditsUsed":       311.6727,
		"sourceCreditsUsed": 5.7779,
	}}, map[string]interface{}{
		"sourceCreditsBackfill": 1,
	})

	if got := GetApiKeyEntry("k1").SourceCreditsUsed; got != 311.6727 {
		t.Fatalf("generation 1 config must be corrected, got %v", got)
	}
}

// A fresh install has nothing to reset but must still record the generation, so a key
// created later and earning real margin is never retroactively flattened.
func TestSourceCreditsBackfillMarksFreshInstall(t *testing.T) {
	seedConfigFile(t, nil, nil)

	cfgLock.RLock()
	gen := cfg.SourceCreditsBackfill
	cfgLock.RUnlock()

	if gen != currentSourceCreditsBackfill {
		t.Fatalf("expected generation %d on a fresh install, got %d", currentSourceCreditsBackfill, gen)
	}
}
