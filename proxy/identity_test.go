package proxy

import (
	"path/filepath"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestBuildIdentityLine(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"claude-opus-4.8", "You are Claude Opus 4.8. Model ID: claude-opus-4-8."},
		{"claude-sonnet-4.5", "You are Claude Sonnet 4.5. Model ID: claude-sonnet-4-5."},
		{"claude-opus-4-7", "You are Claude Opus 4 7. Model ID: claude-opus-4-7."},
	}
	for _, tt := range tests {
		if got := buildIdentityLine(tt.model); got != tt.want {
			t.Errorf("buildIdentityLine(%q) = %q, want %q", tt.model, got, tt.want)
		}
	}
}

func TestPrependIdentityInjectsWhenSet(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	// Off by default: prompt unchanged.
	if got := applyPromptFilters("hello"); got != "hello" {
		t.Fatalf("identity off: expected unchanged prompt, got %q", got)
	}

	if err := config.SetIdentityModel("claude-opus-4.8"); err != nil {
		t.Fatalf("SetIdentityModel: %v", err)
	}

	// Non-empty prompt: identity line prepended.
	got := applyPromptFilters("original system prompt")
	if !strings.HasPrefix(got, "You are Claude Opus 4.8.") {
		t.Fatalf("expected identity prefix, got %q", got)
	}
	if !strings.Contains(got, "original system prompt") {
		t.Fatalf("expected original prompt retained, got %q", got)
	}

	// Empty client prompt: identity line still injected.
	gotEmpty := applyPromptFilters("")
	if !strings.HasPrefix(gotEmpty, "You are Claude Opus 4.8.") {
		t.Fatalf("expected identity injected on empty prompt, got %q", gotEmpty)
	}

	// Clearing restores passthrough.
	if err := config.SetIdentityModel(""); err != nil {
		t.Fatalf("clear IdentityModel: %v", err)
	}
	if got := applyPromptFilters(""); got != "" {
		t.Fatalf("identity cleared: expected empty, got %q", got)
	}
}
