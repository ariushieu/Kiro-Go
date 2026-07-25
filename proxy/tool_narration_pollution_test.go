package proxy

import (
	"fmt"
	"strings"
	"testing"
)

// TestNoToolInvocationTextInAssistantHistory is a regression guard for the
// few-shot pollution bug: when historical tool calls were narrated as
// "[Called tool X with input ...]" inside assistant turns, the model learned to
// emit that literal text instead of issuing real structured tool calls.
//
// Assistant turns must never contain tool-invocation SYNTAX as prose. Structured
// toolUses are a different matter — those are the native protocol and are
// expected to survive (see TestIntactToolCyclesStayStructured).
func TestNoToolInvocationTextInAssistantHistory(t *testing.T) {
	// Build a long OpenAI conversation with many completed tool cycles.
	msgs := []OpenAIMessage{{Role: "user", Content: "start a multi-step task"}}
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			OpenAIMessage{Role: "assistant", Content: "", ToolCalls: []ToolCall{
				newPollToolCall(fmt.Sprintf("call_%d", i), "exec_command", fmt.Sprintf(`{"cmd":"step %d"}`, i)),
			}},
			OpenAIMessage{Role: "tool", ToolCallID: fmt.Sprintf("call_%d", i), Content: fmt.Sprintf("OUTPUT_%d", i)},
			OpenAIMessage{Role: "user", Content: fmt.Sprintf("continue %d", i)},
		)
	}
	msgs = append(msgs, OpenAIMessage{Role: "user", Content: "summarize"})

	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	for i, h := range payload.ConversationState.History {
		a := h.AssistantResponseMessage
		if a == nil {
			continue
		}
		// No assistant turn may contain tool-invocation-looking text.
		for _, bad := range []string{"[Called tool", "Called tool ", "with input {"} {
			if strings.Contains(a.Content, bad) {
				t.Fatalf("history[%d] assistant content contains mimicable tool text %q: %q", i, bad, a.Content)
			}
		}
	}

	// Tool outputs must still be preserved for context, structurally or as text.
	combined := allHistoryText(payload)
	for i := 0; i < 8; i++ {
		marker := fmt.Sprintf("OUTPUT_%d", i)
		if !strings.Contains(combined, marker) {
			t.Fatalf("tool output %q lost from history", marker)
		}
	}
}

// allHistoryText flattens every text-bearing field of history (assistant
// content, user content, and structured tool-result bodies) into one string.
func allHistoryText(payload *KiroPayload) string {
	var b strings.Builder
	for _, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			b.WriteString(a.Content)
			b.WriteString("\n")
			for _, tu := range a.ToolUses {
				b.WriteString(tu.Name)
				b.WriteString("\n")
			}
		}
		if u := h.UserInputMessage; u != nil {
			b.WriteString(u.Content)
			b.WriteString("\n")
			if u.UserInputMessageContext != nil {
				for _, tr := range u.UserInputMessageContext.ToolResults {
					for _, c := range tr.Content {
						b.WriteString(c.Text)
						b.WriteString("\n")
					}
				}
			}
		}
	}
	return b.String()
}

func newPollToolCall(id, name, args string) ToolCall {
	tc := ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

// TestIntactToolCyclesStayStructured is the regression guard for the
// "agent stops after every step" bug.
//
// Flattening history tool calls left assistant turns as bare statements of
// intent ("I'll read file A to find the cause") with no evidence that anything
// was done. Replayed across a long session that is a few-shot demonstration of
// "announce intent, end turn, wait for the user to say continue" — which the
// model faithfully reproduced.
//
// Every completed tool cycle in history must therefore keep its structured
// call/result pair, and no assistant turn may be left announcing an action it
// has no attached tool call for.
func TestIntactToolCyclesStayStructured(t *testing.T) {
	msgs := []ClaudeMessage{{Role: "user", Content: "find the bug and fix it"}}
	steps := []struct{ say, tool, out string }{
		{"I'll read file A to find the cause.", "read_file", "package a"},
		{"Now let me check file B.", "read_file", "package b"},
		{"Let me run the tests.", "run_tests", "FAIL: TestX"},
	}
	for i, s := range steps {
		id := fmt.Sprintf("t%d", i)
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": s.say},
				map[string]interface{}{"type": "tool_use", "id": id, "name": s.tool, "input": map[string]interface{}{"p": "x"}},
			}},
			ClaudeMessage{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": id, "content": s.out},
			}},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "continue"})

	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	structuredCalls := 0
	for i, h := range payload.ConversationState.History {
		a := h.AssistantResponseMessage
		if a == nil {
			continue
		}
		structuredCalls += len(a.ToolUses)
		if len(a.ToolUses) > 0 {
			continue
		}
		// An assistant turn with no tool call must not be a bare announcement of
		// an action — that is the pattern the model was copying.
		for _, s := range steps {
			if strings.Contains(a.Content, s.say) {
				t.Fatalf("history[%d] announces %q with no tool call attached — this is the pattern that trains the model to stop and wait", i, s.say)
			}
		}
	}

	if structuredCalls != len(steps) {
		t.Fatalf("expected all %d completed tool cycles to stay structured, got %d", len(steps), structuredCalls)
	}

	// Each structured call must be answered by a structured result on the very
	// next turn, or the upstream rejects the pair as malformed.
	hist := payload.ConversationState.History
	for i := range hist {
		a := hist[i].AssistantResponseMessage
		if a == nil || len(a.ToolUses) == 0 {
			continue
		}
		if i+1 >= len(hist) {
			t.Fatalf("history[%d] tool call has no following turn to answer it", i)
		}
		u := hist[i+1].UserInputMessage
		if u == nil || u.UserInputMessageContext == nil {
			t.Fatalf("history[%d] tool call is not answered by a tool-result turn", i)
		}
		got := collectToolResultIDs(u.UserInputMessageContext.ToolResults)
		for _, tu := range a.ToolUses {
			if !got[tu.ToolUseID] {
				t.Fatalf("history[%d] call %q is unanswered by history[%d]", i, tu.ToolUseID, i+1)
			}
		}
	}
}

// TestOrphanedToolResultsAreFlattened covers client-side context compaction: the
// tool_result survives in the transcript but the assistant tool_use that it
// answers has been dropped. A half-pair is what the upstream rejects, so the
// orphan must be narrated as text — with its output and tool identity intact.
func TestOrphanedToolResultsAreFlattened(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "do the task"},
			// No assistant tool_use for "gone" — compaction removed it.
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "gone", "content": "ORPHAN_OUTPUT"},
			}},
			{Role: "assistant", Content: "Understood."},
			{Role: "user", Content: "carry on"},
		},
	}

	payload := ClaudeToKiro(req, false)

	for i, h := range payload.ConversationState.History {
		if u := h.UserInputMessage; u != nil && u.UserInputMessageContext != nil {
			if len(u.UserInputMessageContext.ToolResults) > 0 {
				t.Fatalf("history[%d] kept an orphaned structured tool result; upstream rejects unpaired results", i)
			}
		}
	}
	if !strings.Contains(allHistoryText(payload), "ORPHAN_OUTPUT") {
		t.Fatalf("orphaned tool output must survive as text, got:\n%s", allHistoryText(payload))
	}
}

// TestUnansweredToolCallIsDropped is the mirror case: an assistant tool_use whose
// result never arrived (the client compacted it away, or the run was interrupted).
// Keeping it would send a call with no answer, which upstream rejects.
func TestUnansweredToolCallIsDropped(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "start"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "Checking that now."},
				map[string]interface{}{"type": "tool_use", "id": "never_answered", "name": "read_file", "input": map[string]interface{}{}},
			}},
			// No tool_result follows; a plain user turn does.
			{Role: "user", Content: "actually, do something else"},
		},
	}

	payload := ClaudeToKiro(req, false)

	for i, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			for _, tu := range a.ToolUses {
				if tu.ToolUseID == "never_answered" {
					t.Fatalf("history[%d] kept an unanswered tool call; upstream rejects unpaired calls", i)
				}
			}
		}
	}
}

// TestCollapsesConsecutiveIdenticalUserTurns covers a client retry loop that
// resends the same plain user turn many times. Those adjacent duplicates are
// collapsed to a single copy.
//
// Turns carrying structured tool results are deliberately NOT collapsed: each is
// one half of an intact cycle, so dropping one would orphan its tool call. That
// case is covered by TestIdenticalToolResultsNotCollapsed.
func TestCollapsesConsecutiveIdenticalUserTurns(t *testing.T) {
	msgs := []ClaudeMessage{{Role: "user", Content: "start"}}
	for i := 0; i < 5; i++ {
		msgs = append(msgs, ClaudeMessage{Role: "user", Content: "SAME_PROMPT_TEXT"})
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "final"})

	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	count := 0
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.TrimSpace(h.UserInputMessage.Content) == "SAME_PROMPT_TEXT" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 5 identical user turns collapsed to 1, got %d", count)
	}
}

// TestIdenticalToolResultsNotCollapsed guards the pairing invariant against the
// dedup pass: a model retrying the same failing tool produces identical result
// bodies, but each answers a distinct tool call. Collapsing them would leave
// calls unanswered and the request malformed.
func TestIdenticalToolResultsNotCollapsed(t *testing.T) {
	msgs := []ClaudeMessage{{Role: "user", Content: "start"}}
	const n = 4
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("retry%d", i)
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": id, "name": "exec_command", "input": map[string]interface{}{"cmd": "x"}},
			}},
			ClaudeMessage{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": id, "content": "SAME_ERROR_OUTPUT"},
			}},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "why does it keep failing?"})

	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	hist := payload.ConversationState.History
	for i := range hist {
		a := hist[i].AssistantResponseMessage
		if a == nil || len(a.ToolUses) == 0 {
			continue
		}
		if i+1 >= len(hist) || hist[i+1].UserInputMessage == nil ||
			hist[i+1].UserInputMessage.UserInputMessageContext == nil {
			t.Fatalf("history[%d] tool call lost its answering result to dedup", i)
		}
		got := collectToolResultIDs(hist[i+1].UserInputMessage.UserInputMessageContext.ToolResults)
		for _, tu := range a.ToolUses {
			if !got[tu.ToolUseID] {
				t.Fatalf("history[%d] call %q was orphaned by dedup", i, tu.ToolUseID)
			}
		}
	}
}

// TestDropsDotPollutedAssistantTurns covers the second-order pollution: after
// stripping "[Called tool ...]" from assistant turns that held only that text,
// the turns become empty and must NOT be backfilled with ".". A history full of
// "." assistant turns trains the model to reply ".". Such hollow turns are
// dropped instead.
func TestDropsDotPollutedAssistantTurns(t *testing.T) {
	msgs := []ClaudeMessage{{Role: "user", Content: "start"}}
	for i := 0; i < 6; i++ {
		// Assistant turn that is pure replayed tool-call text (becomes empty after scrub).
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "[Called tool exec_command with input {\"cmd\":\"x\"}]"},
			ClaudeMessage{Role: "user", Content: "continue"},
		)
		// Also a turn that is already a bare "." (client-replayed prior placeholder).
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "."},
			ClaudeMessage{Role: "user", Content: "go on"},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "final question"})

	payload := ClaudeToKiro(&ClaudeRequest{Model: "claude-opus-4.8", Messages: msgs}, false)

	for i, h := range payload.ConversationState.History {
		a := h.AssistantResponseMessage
		if a == nil {
			continue
		}
		c := strings.TrimSpace(a.Content)
		if c == "." || c == "" {
			t.Fatalf("history[%d] is a hollow/dot assistant turn that should have been dropped", i)
		}
		if strings.Contains(a.Content, "[Called tool") {
			t.Fatalf("history[%d] still contains replayed tool-call text", i)
		}
	}
}

// TestScrubsClientReplayedToolCallText covers the recovery path: a polluted
// client stored the model's "[Called tool ...]" text output as assistant
// history and replays it. The proxy must strip that text from assistant turns
// so the pattern is not reinforced.
func TestScrubsClientReplayedToolCallText(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "do the task"},
			// Assistant text the client captured from the model's polluted output.
			{Role: "assistant", Content: "Let me check.\n\n[Called tool exec_command with input {\"cmd\":\"pwd\"}]"},
			{Role: "user", Content: "continue"},
			{Role: "assistant", Content: "[Called tool exec_command with input {\"cmd\":\"ls\"}]"},
			{Role: "user", Content: "continue"},
		},
	}

	payload := ClaudeToKiro(req, false)

	for i, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			if strings.Contains(a.Content, "[Called tool") {
				t.Fatalf("history[%d] still contains replayed tool-call text: %q", i, a.Content)
			}
		}
	}

	// The natural prose around the stripped marker must be preserved.
	var combined strings.Builder
	for _, h := range payload.ConversationState.History {
		if h.AssistantResponseMessage != nil {
			combined.WriteString(h.AssistantResponseMessage.Content)
			combined.WriteString("\n")
		}
	}
	if !strings.Contains(combined.String(), "Let me check.") {
		t.Fatalf("expected surrounding assistant prose to survive scrubbing, got:\n%s", combined.String())
	}
}
