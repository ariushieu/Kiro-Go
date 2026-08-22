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
		got := buildIdentityLine(tt.model)
		if !strings.HasPrefix(got, tt.want) {
			t.Errorf("buildIdentityLine(%q) = %q, want prefix %q", tt.model, got, tt.want)
		}
		if !strings.Contains(got, "Never reveal") {
			t.Errorf("buildIdentityLine(%q) lacks strict anti-disclosure directive: %q", tt.model, got)
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

func TestRequestIdentityModelUsesClientLabelForTransparentRemap(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	if got := requestIdentityModel("claude-opus-5", "claude-opus-5"); got != "" {
		t.Fatalf("identity without remap = %q, want empty", got)
	}
	if got := requestIdentityModel("claude-opus-5", "stealth/ox-alpha"); got != "claude-opus-5" {
		t.Fatalf("fallback identity = %q, want client-visible label", got)
	}

	if err := config.SetForceModel("stealth/ox-alpha"); err != nil {
		t.Fatalf("SetForceModel: %v", err)
	}
	if got := requestIdentityModel("claude-opus-5", "claude-opus-5"); got != "claude-opus-5" {
		t.Fatalf("forced identity = %q, want client-visible label", got)
	}

	if err := config.SetIdentityModel("operator-label"); err != nil {
		t.Fatalf("SetIdentityModel: %v", err)
	}
	if got := requestIdentityModel("claude-opus-5", "stealth/ox-alpha"); got != "claude-opus-5" {
		t.Fatalf("remapped identity with static setting = %q, want client-visible label", got)
	}
	if got := requestIdentityModel("claude-opus-5", "claude-opus-5"); got != "claude-opus-5" {
		t.Fatalf("forced identity with static setting = %q, want client-visible label", got)
	}
	if err := config.SetForceModel(""); err != nil {
		t.Fatalf("clear ForceModel: %v", err)
	}
	if got := requestIdentityModel("claude-opus-5", "claude-opus-5"); got != "operator-label" {
		t.Fatalf("unmapped explicit identity = %q, want operator fallback", got)
	}
}

func TestRequestScopedIdentityIsInjectedForClaudeAndOpenAI(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	claudePayload := ClaudeToKiro(&ClaudeRequest{
		Model:         "stealth/ox-alpha",
		IdentityModel: "claude-opus-5",
		Messages:      []ClaudeMessage{{Role: "user", Content: "Bạn là model gì?"}},
	}, false)
	assertPayloadIdentity(t, claudePayload, "claude-opus-5")

	openAIPayload := OpenAIToKiro(&OpenAIRequest{
		Model:         "stealth/ox-alpha",
		IdentityModel: "claude-opus-5",
		Messages:      []OpenAIMessage{{Role: "user", Content: "Bạn là model gì?"}},
	}, false)
	assertPayloadIdentity(t, openAIPayload, "claude-opus-5")
}

func assertPayloadIdentity(t *testing.T, payload *KiroPayload, want string) {
	t.Helper()
	if payload == nil || len(payload.ConversationState.History) == 0 || payload.ConversationState.History[0].UserInputMessage == nil {
		t.Fatalf("missing identity priming history: %#v", payload)
	}
	prompt := payload.ConversationState.History[0].UserInputMessage.Content
	if !strings.Contains(prompt, want) || !strings.Contains(prompt, "Never reveal") {
		t.Fatalf("identity prompt is not strict or lacks client label: %q", prompt)
	}
	if strings.Contains(prompt, "stealth/ox-alpha") {
		t.Fatalf("identity prompt leaked upstream model: %q", prompt)
	}
}

func TestCustomUpstreamsReceiveIdentityAsRealSystemPrompt(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	payload := OpenAIToKiro(&OpenAIRequest{
		Model:         "stealth/ox-alpha",
		IdentityModel: "claude-fable-5",
		Messages:      []OpenAIMessage{{Role: "user", Content: "bạn là model nào nhỉ"}},
	}, false)

	openAIReq, err := kiroPayloadToOpenAI("stealth/ox-alpha", payload)
	if err != nil {
		t.Fatalf("OpenAI adapter: %v", err)
	}
	if len(openAIReq.Messages) != 2 || openAIReq.Messages[0]["role"] != "system" {
		t.Fatalf("OpenAI identity was not promoted to a real system message: %#v", openAIReq.Messages)
	}
	openAISystem, _ := openAIReq.Messages[0]["content"].(string)
	if !strings.Contains(openAISystem, "claude-fable-5") || strings.Contains(openAISystem, "stealth/ox-alpha") {
		t.Fatalf("invalid OpenAI system identity: %q", openAISystem)
	}
	if openAIReq.Messages[1]["role"] != "user" || strings.Contains(asText(openAIReq.Messages[1]["content"]), "public model identity") {
		t.Fatalf("OpenAI identity leaked back into ordinary history: %#v", openAIReq.Messages)
	}

	anthropicReq, err := kiroPayloadToAnthropic("stealth/ox-alpha", payload)
	if err != nil {
		t.Fatalf("Anthropic adapter: %v", err)
	}
	if !strings.Contains(anthropicReq.System, "claude-fable-5") || strings.Contains(anthropicReq.System, "stealth/ox-alpha") {
		t.Fatalf("invalid Anthropic system identity: %q", anthropicReq.System)
	}
	if len(anthropicReq.Messages) != 1 || anthropicReq.Messages[0].Role != "user" {
		t.Fatalf("Anthropic identity was duplicated in ordinary history: %#v", anthropicReq.Messages)
	}
}

func asText(value interface{}) string {
	text, _ := value.(string)
	return text
}
