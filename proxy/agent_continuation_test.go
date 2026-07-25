package proxy

import (
	"fmt"
	"strings"
	"testing"
)

// TestAgentHistoryNeverShowsIntentWithoutAction is the regression guard for the
// "agent stops after every step" bug.
//
// An earlier version stripped the structured toolUses from every history
// assistant turn except one. What reached the model was a history of turns that
// each announced an intention ("I'll read file A to find the cause") and then
// ended, with the tool output appearing only in the following USER turn. That is
// a few-shot demonstration of "state intent, end turn, wait to be told to
// continue" — and the model reproduced it, so users had to type "continue"
// after every step.
//
// Every assistant turn that the client sent as a tool call must therefore reach
// the upstream still carrying that tool call.
func TestAgentHistoryNeverShowsIntentWithoutAction(t *testing.T) {
	steps := []struct{ say, tool, arg, out string }{
		{"I'll read file A to find the cause.", "read_file", "a.go", "package a"},
		{"Now let me check file B.", "read_file", "b.go", "package b"},
		{"Let me run the tests.", "run_tests", "./...", "FAIL: TestX"},
		{"I'll fix the bug in A.go.", "edit_file", "a.go", "edited"},
	}

	msgs := []ClaudeMessage{{Role: "user", Content: "find the bug and fix it"}}
	for i, s := range steps {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": s.say},
				map[string]interface{}{"type": "tool_use", "id": id, "name": s.tool,
					"input": map[string]interface{}{"path": s.arg}},
			}},
			ClaudeMessage{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": id, "content": s.out},
			}},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "continue"})

	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	var intentOnly int
	for i, h := range payload.ConversationState.History {
		a := h.AssistantResponseMessage
		if a == nil || strings.TrimSpace(a.Content) == "" {
			continue
		}
		// The system-priming acknowledgement is not a model turn.
		if strings.HasPrefix(a.Content, "I will follow these instructions") {
			continue
		}
		if len(a.ToolUses) == 0 {
			intentOnly++
			t.Errorf("history[%d] states an intention with no tool call attached: %q", i, a.Content)
		}
	}
	if intentOnly > 0 {
		t.Fatalf("%d assistant turn(s) demonstrate 'announce, then stop' — the model will imitate this", intentOnly)
	}

	// Each tool result must pair with its call so the cycle is well-formed.
	for i, h := range payload.ConversationState.History {
		a := h.AssistantResponseMessage
		if a == nil || len(a.ToolUses) == 0 {
			continue
		}
		if i+1 >= len(payload.ConversationState.History) {
			t.Fatalf("history[%d] tool call has no following turn to answer it", i)
		}
		next := payload.ConversationState.History[i+1].UserInputMessage
		if next == nil || next.UserInputMessageContext == nil ||
			len(next.UserInputMessageContext.ToolResults) != len(a.ToolUses) {
			t.Fatalf("history[%d] tool call is not answered by structured results on the next turn", i)
		}
		if got, want := next.UserInputMessageContext.ToolResults[0].ToolUseID, a.ToolUses[0].ToolUseID; got != want {
			t.Fatalf("history[%d] result answers %q but the call was %q", i, got, want)
		}
	}
}

// TestToolOutputSentExactlyOnce guards the token cost of the fix. Tool results
// that travel structurally must not ALSO be inlined as text: doing so sends
// every tool output to the upstream twice, doubling the cost of each agent step.
func TestToolOutputSentExactlyOnce(t *testing.T) {
	const marker = "UNIQUE_TOOL_OUTPUT_MARKER"

	count := func(payload *KiroPayload) int {
		n := 0
		countMsg := func(m *KiroUserInputMessage) {
			if m == nil {
				return
			}
			n += strings.Count(m.Content, marker)
			if m.UserInputMessageContext != nil {
				for _, tr := range m.UserInputMessageContext.ToolResults {
					for _, c := range tr.Content {
						n += strings.Count(c.Text, marker)
					}
				}
			}
		}
		countMsg(&payload.ConversationState.CurrentMessage.UserInputMessage)
		for _, h := range payload.ConversationState.History {
			countMsg(h.UserInputMessage)
			if h.AssistantResponseMessage != nil {
				n += strings.Count(h.AssistantResponseMessage.Content, marker)
			}
		}
		return n
	}

	t.Run("claude", func(t *testing.T) {
		payload := ClaudeToKiro(&ClaudeRequest{
			Model: "claude-opus-4.8",
			Messages: []ClaudeMessage{
				{Role: "user", Content: "run it"},
				{Role: "assistant", Content: []interface{}{
					map[string]interface{}{"type": "tool_use", "id": "t1", "name": "exec", "input": map[string]interface{}{}},
				}},
				{Role: "user", Content: []interface{}{
					map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": marker},
				}},
			},
		}, false)
		if got := count(payload); got != 1 {
			t.Fatalf("tool output sent %d times, want exactly 1", got)
		}
	})

	t.Run("openai", func(t *testing.T) {
		payload := OpenAIToKiro(&OpenAIRequest{
			Model: "claude-opus-4.8",
			Messages: []OpenAIMessage{
				{Role: "user", Content: "run it"},
				{Role: "assistant", ToolCalls: []ToolCall{newContinuationToolCall("c1", "exec", "{}")}},
				{Role: "tool", ToolCallID: "c1", Content: marker},
			},
		}, false)
		if got := count(payload); got != 1 {
			t.Fatalf("tool output sent %d times, want exactly 1", got)
		}
	})
}

// TestLargeToolOutputNotSilentlyTruncated covers the 4000-byte hard cap that
// used to chop tool output mid-line. A model handed a truncated file read or
// build log lacks the data it asked for and tends to end its turn to ask for
// direction. Payload size is bounded by truncatePayloadToLimit instead, which
// accounts for the whole request.
func TestLargeToolOutputNotSilentlyTruncated(t *testing.T) {
	big := strings.Repeat("x", 9000) + "TAIL_MARKER"

	// Orphaned result (no matching call) → inlined as text, the path that capped.
	payload := ClaudeToKiro(&ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "orphan", "content": big},
			}},
		},
	}, false)

	content := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if !strings.Contains(content, "TAIL_MARKER") {
		t.Fatalf("tool output was truncated: %d chars kept of %d", len(content), len(big))
	}
}

func newContinuationToolCall(id, name, args string) ToolCall {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}
