package proxy

import "testing"

func TestClaudeStickyKeyStableAcrossBillingHeaderDrift(t *testing.T) {
	build := func(billing string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-x",
			System: []interface{}{
				map[string]interface{}{"type": "text", "text": billing},
				map[string]interface{}{"type": "text", "text": "You are a helpful assistant."},
			},
			Messages: []ClaudeMessage{
				{Role: "user", Content: "hello"},
			},
		}
	}
	a := claudeStickyKey(build("x-anthropic-billing-header: cc_version=1"))
	b := claudeStickyKey(build("x-anthropic-billing-header: cc_version=2"))
	if a != b {
		t.Fatalf("billing-header drift changed the sticky key: %x != %x", a, b)
	}
	if isZeroStickyKey(a) {
		t.Fatalf("expected non-zero key for a real prefix")
	}
}

func TestClaudeStickyKeyDiffersOnSystemAndFirstMessage(t *testing.T) {
	base := &ClaudeRequest{
		System:   "prompt A",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	diffSystem := &ClaudeRequest{
		System:   "prompt B",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	diffMsg := &ClaudeRequest{
		System:   "prompt A",
		Messages: []ClaudeMessage{{Role: "user", Content: "different"}},
	}
	k := claudeStickyKey(base)
	if k == claudeStickyKey(diffSystem) {
		t.Fatal("expected different key when system differs")
	}
	if k == claudeStickyKey(diffMsg) {
		t.Fatal("expected different key when first user message differs")
	}
}

func TestClaudeStickyKeySameConversationSameKey(t *testing.T) {
	turn1 := &ClaudeRequest{
		System:   "prompt A",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	turn2 := &ClaudeRequest{
		System: "prompt A",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
			{Role: "user", Content: "next turn"},
		},
	}
	if claudeStickyKey(turn1) != claudeStickyKey(turn2) {
		t.Fatal("expected same key across turns of one conversation")
	}
}

func TestStickyKeyZeroWhenEmptyPrefix(t *testing.T) {
	if !isZeroStickyKey(claudeStickyKey(&ClaudeRequest{})) {
		t.Fatal("expected zero key for empty Claude request")
	}
	if !isZeroStickyKey(openAIStickyKey(&OpenAIRequest{})) {
		t.Fatal("expected zero key for empty OpenAI request")
	}
}

func TestOpenAIStickyKeyUsesSystemAndFirstUser(t *testing.T) {
	req := &OpenAIRequest{
		Messages: []OpenAIMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "u1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "u2"},
		},
	}
	same := &OpenAIRequest{
		Messages: []OpenAIMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "u1"},
		},
	}
	if openAIStickyKey(req) != openAIStickyKey(same) {
		t.Fatal("expected same key regardless of later turns")
	}
	if isZeroStickyKey(openAIStickyKey(req)) {
		t.Fatal("expected non-zero key")
	}
}
