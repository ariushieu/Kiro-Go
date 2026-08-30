package config

import (
	"encoding/json"
	"fmt"
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

// The baseline reset is a rule over key state, not a repair of particular keys, so
// this covers the state space rather than any one account: every combination of
// charge and recorded source cost a key can be in when the migration runs, all in one
// config so the pass is exercised over a whole pool at once rather than a single
// entry. Whatever the pool holds — two keys or two hundred — falls into one of these
// rows.
func TestSourceCreditsBaselineResetCoversEveryKeyState(t *testing.T) {
	cases := []struct {
		name       string
		credits    float64
		source     float64
		wantSource float64
		why        string
	}{{
		name: "never used", credits: 0, source: 0, wantSource: 0,
		why: "nothing charged, nothing to reconcile",
	}, {
		name: "entirely before the field existed", credits: 311.6727, source: 0, wantSource: 311.6727,
		why: "whole charge was untracked, so all of it is cost, not margin",
	}, {
		name: "partly before, partly after", credits: 32.0631, source: 8.9285, wantSource: 32.0631,
		why: "the tracked part cannot be separated from the untracked backlog",
	}, {
		name: "tracked from the first request", credits: 40, source: 40, wantSource: 40,
		why: "already consistent; the reset must be a no-op",
	}, {
		name: "charge rounds to the source cost", credits: 0.001, source: 0.001, wantSource: 0.001,
		why: "equality holds at small magnitudes too, no drift-driven reset",
	}, {
		name: "source somehow exceeds charge", credits: 5, source: 9, wantSource: 9,
		why: "never revise a source cost downward; a charge below cost is a real loss to keep visible",
	}}

	keys := make([]map[string]interface{}, len(cases))
	for i, tc := range cases {
		keys[i] = map[string]interface{}{
			"id":                fmt.Sprintf("k%d", i),
			"name":              tc.name,
			"key":               fmt.Sprintf("sk-%d", i),
			"enabled":           true,
			"creditsUsed":       tc.credits,
			"sourceCreditsUsed": tc.source,
		}
	}
	seedConfigFile(t, keys, nil)

	for i, tc := range cases {
		id := fmt.Sprintf("k%d", i)
		got := GetApiKeyEntry(id)
		if got == nil {
			t.Fatalf("%s: key missing after load", tc.name)
		}
		if got.SourceCreditsUsed != tc.wantSource {
			t.Errorf("%s: source cost = %v, want %v (%s)", tc.name, got.SourceCreditsUsed, tc.wantSource, tc.why)
		}
		if got.CreditsUsed != tc.credits {
			t.Errorf("%s: the reset must never touch the customer charge, got %v want %v",
				tc.name, got.CreditsUsed, tc.credits)
		}
	}
}

// Keys created after the migration cannot land in the broken state at all: their
// source cost is recorded from their first request, so charge and cost cover the same
// span and the margin is real from the start. This is what makes the reset a one-time
// repair rather than something that has to keep catching up with new keys.
func TestSourceCreditsNewKeysNeedNoReset(t *testing.T) {
	cfgFile := seedConfigFile(t, nil, nil)

	created, err := AddApiKey(ApiKeyEntry{Name: "fresh", Key: "sk-fresh", Enabled: true})
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
	// Charged 3.0 for a request that really cost 2.0 — a rate of 1.5.
	if err := RecordApiKeyUsageWithCost(created.ID, 100, 3.0, 2.0); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if err := FlushDirty(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("reload: %v", err)
	}

	got := GetApiKeyEntry(created.ID)
	if got.CreditsUsed != 3.0 || got.SourceCreditsUsed != 2.0 {
		t.Fatalf("a key created after the migration must keep its real margin: charged %v source %v",
			got.CreditsUsed, got.SourceCreditsUsed)
	}
}

// After the reset, earned margin must survive every later restart. This is why the
// migration is gated on a stored generation instead of on the data: post-reset state
// (source < credits) is indistinguishable from the state the reset corrects, so an
// inferred guard would flatten real margin on every boot.
func TestSourceCreditsBaselineResetPreservesEarnedMargin(t *testing.T) {
	cfgFile := seedConfigFile(t, []map[string]interface{}{{
		"id":          "k1",
		"key":         "sk-old",
		"enabled":     true,
		"creditsUsed": 100.0,
	}}, nil)

	if got := GetApiKeyEntry("k1").SourceCreditsUsed; got != 100 {
		t.Fatalf("first load should reset the baseline to 100, got %v", got)
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

// A config that applied generation 1 must still receive the corrected reset. That
// generation only seeded keys whose source cost was exactly zero, so it skipped every
// key that had already served a request under the new build — the affected ones.
func TestSourceCreditsBaselineResetUpgradesFromGenerationOne(t *testing.T) {
	seedConfigFile(t, []map[string]interface{}{{
		"id":                "k1",
		"key":               "sk-gen1",
		"enabled":           true,
		"creditsUsed":       311.6727,
		"sourceCreditsUsed": 5.7779,
	}, {
		"id":                "k2",
		"key":               "sk-gen1-b",
		"enabled":           true,
		"creditsUsed":       32.0631,
		"sourceCreditsUsed": 8.9285,
	}}, map[string]interface{}{
		"sourceCreditsBackfill": 1,
	})

	for id, want := range map[string]float64{"k1": 311.6727, "k2": 32.0631} {
		if got := GetApiKeyEntry(id).SourceCreditsUsed; got != want {
			t.Errorf("%s: generation 1 config must be corrected, got %v want %v", id, got, want)
		}
	}
}

// A fresh install has nothing to reset but must still record the generation, so a key
// created later and earning real margin is never retroactively flattened.
func TestSourceCreditsBaselineResetMarksFreshInstall(t *testing.T) {
	seedConfigFile(t, nil, nil)

	cfgLock.RLock()
	gen := cfg.SourceCreditsBackfill
	cfgLock.RUnlock()

	if gen != currentSourceCreditsBackfill {
		t.Fatalf("expected generation %d on a fresh install, got %d", currentSourceCreditsBackfill, gen)
	}
}
