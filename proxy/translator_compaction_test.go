package proxy

import (
	"strings"
	"testing"
)

// TestClaudeToKiroKeepsCompletedToolCyclesStructured covers a long agent session
// whose history holds completed tool cycles (assistant tool_use + user
// tool_result), followed by a plain-text instruction.
//
// Completed cycles must keep their native structured form. Flattening them into
// prose was the cause of the "agent stops after every step" behaviour: an
// assistant turn reduced to "running build" with no tool call attached models a
// turn that announced intent and then ended, and a history full of those teaches
// the model to do exactly that.
func TestClaudeToKiroKeepsCompletedToolCyclesStructured(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "run the build"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "running build"},
				map[string]interface{}{"type": "tool_use", "id": "t1", "name": "exec_command", "input": map[string]interface{}{"cmd": "make"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": "build ok"},
			}},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "t2", "name": "exec_command", "input": map[string]interface{}{"cmd": "test"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t2", "content": "tests pass"},
			}},
			// Final plain-text instruction (the compaction request).
			{Role: "user", Content: "Summarize everything that happened above."},
		},
	}

	payload := ClaudeToKiro(req, false)

	// Both completed cycles must survive structurally.
	gotCalls := map[string]bool{}
	gotResults := map[string]bool{}
	for _, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			for _, tu := range a.ToolUses {
				gotCalls[tu.ToolUseID] = true
			}
		}
		if u := h.UserInputMessage; u != nil && u.UserInputMessageContext != nil {
			for _, tr := range u.UserInputMessageContext.ToolResults {
				gotResults[tr.ToolUseID] = true
			}
		}
	}
	for _, id := range []string{"t1", "t2"} {
		if !gotCalls[id] {
			t.Fatalf("tool call %s was stripped from history; assistant turn now only announces intent", id)
		}
		if !gotResults[id] {
			t.Fatalf("tool result %s lost its structured form in history", id)
		}
	}

	// Every structured call must be answered, and every result must answer a call —
	// a half-pair is what the upstream rejects as "Improperly formed request".
	assertToolPairingIntact(t, payload)

	// Current message is plain text, so it carries no structured tool results.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) > 0 {
		t.Fatalf("current message should not carry structured toolResults for a plain instruction")
	}
	if !strings.Contains(cur.Content, "Summarize everything") {
		t.Fatalf("expected current content to be the compaction instruction, got %q", cur.Content)
	}

	// Regression guard: tool calls must never appear as imitable prose.
	for i, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil && strings.Contains(a.Content, "[Called tool") {
			t.Fatalf("history[%d] assistant content contains mimicable tool-invocation text: %q", i, a.Content)
		}
	}
}

// TestClaudeToKiroFlattensOrphanedToolResults covers real client-side context
// compaction: the tool_result survives but the assistant tool_use that produced
// it was dropped. That half-pair is what the upstream rejects, so it must be
// flattened to text — with its output preserved.
func TestClaudeToKiroFlattensOrphanedToolResults(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			// History begins mid-cycle: the tool_use turn is gone.
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "vanished", "content": "ORPHAN_OUTPUT"},
			}},
			{Role: "assistant", Content: "Based on that, the build is fine."},
			{Role: "user", Content: "carry on"},
		},
	}

	payload := ClaudeToKiro(req, false)

	for i, h := range payload.ConversationState.History {
		if u := h.UserInputMessage; u != nil && u.UserInputMessageContext != nil {
			if len(u.UserInputMessageContext.ToolResults) > 0 {
				t.Fatalf("history[%d] kept an orphaned structured toolResult; upstream rejects half-pairs", i)
			}
		}
	}

	var combined strings.Builder
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil {
			combined.WriteString(h.UserInputMessage.Content)
			combined.WriteString("\n")
		}
	}
	if !strings.Contains(combined.String(), "ORPHAN_OUTPUT") {
		t.Fatalf("orphaned tool output must survive as text, got:\n%s", combined.String())
	}

	assertToolPairingIntact(t, payload)
}

// TestClaudeToKiroDropsUnansweredToolCall covers the mirror case: an assistant
// tool_use whose result never arrived (client dropped it during compaction). The
// unanswered call must be removed rather than sent as a half-pair — and must not
// be narrated as imitable text.
func TestClaudeToKiroDropsUnansweredToolCall(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "run the build"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "Starting the build now."},
				map[string]interface{}{"type": "tool_use", "id": "never_answered", "name": "exec_command", "input": map[string]interface{}{"cmd": "make"}},
			}},
			// No tool_result — the next turn is ordinary user text.
			{Role: "user", Content: "actually, stop"},
		},
	}

	payload := ClaudeToKiro(req, false)

	for i, h := range payload.ConversationState.History {
		if a := h.AssistantResponseMessage; a != nil {
			for _, tu := range a.ToolUses {
				if tu.ToolUseID == "never_answered" {
					t.Fatalf("history[%d] kept an unanswered tool call; upstream rejects half-pairs", i)
				}
			}
			if strings.Contains(a.Content, "[Called tool") {
				t.Fatalf("history[%d] narrated the dropped call as imitable text: %q", i, a.Content)
			}
		}
	}

	assertToolPairingIntact(t, payload)
}

// TestClaudeToKiroKeepsActiveToolTurnStructured verifies the in-progress tool
// case: the last assistant turn issues a tool_use and the final user message
// delivers the matching tool_result. That active turn stays structured.
func TestClaudeToKiroKeepsActiveToolTurnStructured(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Tools: []ClaudeTool{{Name: "exec_command", Description: "run", InputSchema: map[string]interface{}{"type": "object"}}},
		Messages: []ClaudeMessage{
			{Role: "user", Content: "run ls"},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "t9", "name": "exec_command", "input": map[string]interface{}{"cmd": "ls"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t9", "content": "file1 file2"},
			}},
		},
	}

	payload := ClaudeToKiro(req, false)

	hist := payload.ConversationState.History
	if len(hist) == 0 {
		t.Fatalf("expected non-empty history")
	}
	last := hist[len(hist)-1].AssistantResponseMessage
	if last == nil || len(last.ToolUses) != 1 || last.ToolUses[0].ToolUseID != "t9" {
		t.Fatalf("expected last history assistant to keep the active structured tool use t9")
	}

	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected current message to keep the matching structured tool result")
	}
	if cur.UserInputMessageContext.ToolResults[0].ToolUseID != "t9" {
		t.Fatalf("expected current tool result to answer t9, got %q", cur.UserInputMessageContext.ToolResults[0].ToolUseID)
	}
}

// assertToolPairingIntact verifies the invariant the upstream cares about: every
// structured tool call in history is answered by the immediately following turn
// (or by the current message, for the final assistant turn), and every
// structured tool result answers a call. Any half-pair is a malformed request.
func assertToolPairingIntact(t *testing.T, payload *KiroPayload) {
	t.Helper()

	history := payload.ConversationState.History
	currentIDs := map[string]bool{}
	if ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext; ctx != nil {
		for _, tr := range ctx.ToolResults {
			currentIDs[tr.ToolUseID] = true
		}
	}

	answered := map[string]bool{}
	for i := range history {
		a := history[i].AssistantResponseMessage
		if a == nil || len(a.ToolUses) == 0 {
			continue
		}
		for _, tu := range a.ToolUses {
			if strings.TrimSpace(tu.ToolUseID) == "" || strings.TrimSpace(tu.Name) == "" {
				t.Fatalf("history[%d] has a tool call with an empty id/name: %+v", i, tu)
			}
		}

		next := currentIDs
		if i+1 < len(history) {
			next = map[string]bool{}
			if u := history[i+1].UserInputMessage; u != nil && u.UserInputMessageContext != nil {
				for _, tr := range u.UserInputMessageContext.ToolResults {
					next[tr.ToolUseID] = true
				}
			}
		}
		for _, tu := range a.ToolUses {
			if !next[tu.ToolUseID] {
				t.Fatalf("history[%d] tool call %s is unanswered by the following turn", i, tu.ToolUseID)
			}
			answered[tu.ToolUseID] = true
		}
	}

	for i := range history {
		u := history[i].UserInputMessage
		if u == nil || u.UserInputMessageContext == nil {
			continue
		}
		for _, tr := range u.UserInputMessageContext.ToolResults {
			if !answered[tr.ToolUseID] {
				t.Fatalf("history[%d] tool result %s answers no tool call", i, tr.ToolUseID)
			}
		}
	}
	for id := range currentIDs {
		if !answered[id] {
			t.Fatalf("current message tool result %s answers no tool call in history", id)
		}
	}
}
