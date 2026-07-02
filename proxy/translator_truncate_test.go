package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"kiro-go/config"
)

// TestClaudeToKiroTruncatesOversizedHistory builds a conversation whose history
// far exceeds the upstream input limit and verifies the converted payload is
// trimmed below the configured maxPayloadBytes, that a truncation placeholder is
// inserted, and that the current message is preserved.
func TestClaudeToKiroTruncatesOversizedHistory(t *testing.T) {
	// ~2KB chunk repeated across many turns to blow past the byte limit.
	big := strings.Repeat("lorem ipsum dolor sit amet ", 80) // ~2.1KB

	msgs := []ClaudeMessage{
		{Role: "user", Content: "start the long task"},
	}
	// ~1200 turns × 2 msgs × ~2.1KB ≈ 5MB, comfortably above any configured
	// maxPayloadBytes preset (max 4MB) so truncation is exercised regardless of
	// the exact cap value.
	for i := 0; i < 1200; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "step result: " + big},
			ClaudeMessage{Role: "user", Content: "next: " + big},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "FINAL: summarize everything above"})

	req := &ClaudeRequest{
		Model:    "claude-opus-4.8",
		System:   "You are a helpful assistant.",
		Messages: msgs,
	}

	payload := ClaudeToKiro(req, false)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	limit := config.GetMaxPayloadBytes()
	if len(raw) > limit {
		t.Fatalf("payload size %d exceeds limit %d after truncation", len(raw), limit)
	}

	// The current message must be preserved.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "FINAL: summarize everything above") {
		t.Fatalf("current message lost after truncation, got %q", cur.Content[:min(80, len(cur.Content))])
	}

	// A truncation placeholder must be present in history.
	foundPlaceholder := false
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			foundPlaceholder = true
			break
		}
	}
	if !foundPlaceholder {
		t.Fatalf("expected a truncation placeholder in history")
	}

	// System priming should still be at the front.
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected priming retained, history too short")
	}
	primingUser := payload.ConversationState.History[0].UserInputMessage
	if primingUser == nil || !strings.Contains(primingUser.Content, "helpful assistant") {
		t.Fatalf("expected system priming retained at front")
	}
}

// TestClaudeToKiroSmallPayloadNotTruncated ensures normal-sized conversations
// are left untouched (no placeholder inserted).
func TestClaudeToKiroSmallPayloadNotTruncated(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-opus-4.8",
		System: "You are helpful.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: "how are you?"},
		},
	}
	payload := ClaudeToKiro(req, false)
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			t.Fatalf("small payload should not be truncated")
		}
	}
}

// TestClaudeToKiroTrimsOnTokenWindowUnderByteCap verifies the token-window
// ceiling fires independently of the byte cap: a 200K-window model fed a history
// that is well under the 2MB byte cap but far over its token window must still be
// trimmed so input leaves output headroom.
func TestClaudeToKiroTrimsOnTokenWindowUnderByteCap(t *testing.T) {
	// ~4.5 chars/token → ~1.2M chars ≈ 1.2MB (< 2MB byte cap) but ≈ 270K tokens
	// (> 200K window), so only the token ceiling should trigger truncation.
	chunk := strings.Repeat("alpha beta gamma delta ", 260) // ~6KB per turn

	msgs := []ClaudeMessage{{Role: "user", Content: "begin"}}
	for i := 0; i < 100; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: chunk},
			ClaudeMessage{Role: "user", Content: chunk},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "FINAL question"})

	req := &ClaudeRequest{
		Model:     "claude-sonnet-4.5", // 200K window
		System:    "You are helpful.",
		Messages:  msgs,
		MaxTokens: 8000,
	}

	payload := ClaudeToKiro(req, false)

	raw, _ := json.Marshal(payload)
	if len(raw) >= config.GetMaxPayloadBytes() {
		t.Fatalf("precondition: payload %d should be under byte cap; test no longer isolates token trim", len(raw))
	}

	budget := maxInputTokensForModel(payload, "claude-sonnet-4.5")
	if got := payloadInputTokenSize(payload); got > budget {
		t.Fatalf("input tokens %d exceed budget %d after truncation", got, budget)
	}

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "FINAL question") {
		t.Fatalf("current message lost after truncation")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
