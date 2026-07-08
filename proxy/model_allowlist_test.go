package proxy

import (
	"path/filepath"
	"testing"

	"kiro-go/config"
)

// TestApplyModelOverrideAllowlist covers the per-key Models allowlist semantics:
// a client model in the list passes through unchanged; one not in the list is
// remapped to the first entry; an empty list leaves the client model alone.
func TestApplyModelOverrideAllowlist(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	_ = config.SetForceModel("") // ensure the global override is off

	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Key:    "sk-test-allowlist",
		Models: []string{"claude-opus-4.8", "claude-sonnet-4.5"},
	})
	if err != nil {
		t.Fatalf("AddApiKey: %v", err)
	}

	empty, err := config.AddApiKey(config.ApiKeyEntry{Key: "sk-test-noallow"})
	if err != nil {
		t.Fatalf("AddApiKey (empty): %v", err)
	}

	tests := []struct {
		name     string
		resolved string
		apiKeyID string
		want     string
	}{
		{"allowed model passes through", "claude-sonnet-4.5", entry.ID, "claude-sonnet-4.5"},
		{"first allowed model passes through", "claude-opus-4.8", entry.ID, "claude-opus-4.8"},
		{"disallowed model remaps to first", "claude-haiku-4.5", entry.ID, "claude-opus-4.8"},
		{"empty allowlist keeps client model", "claude-haiku-4.5", empty.ID, "claude-haiku-4.5"},
		{"no api key keeps client model", "claude-haiku-4.5", "", "claude-haiku-4.5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyModelOverride(tc.resolved, tc.apiKeyID, "-thinking"); got != tc.want {
				t.Fatalf("applyModelOverride(%q, %q) = %q, want %q", tc.resolved, tc.apiKeyID, got, tc.want)
			}
		})
	}

	// Global ForceModel wins over any per-key allowlist.
	if err := config.SetForceModel("claude-opus-4.7"); err != nil {
		t.Fatalf("SetForceModel: %v", err)
	}
	if got := applyModelOverride("claude-sonnet-4.5", entry.ID, "-thinking"); got != "claude-opus-4.7" {
		t.Fatalf("ForceModel should win: got %q, want claude-opus-4.7", got)
	}
}
