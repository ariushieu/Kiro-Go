package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

// TestAnthropicUpstreamDefaultMaxTokens pins the max_tokens sent when the client
// omits it. The Anthropic Messages API requires the field, so a low default silently
// truncates long answers mid-sentence — the old 4096 did exactly that. A client that
// DOES set max_tokens must still win.
func TestAnthropicUpstreamDefaultMaxTokens(t *testing.T) {
	t.Run("client omits max_tokens", func(t *testing.T) {
		payload := OpenAIToKiro(&OpenAIRequest{
			Model: "claude-fable-5", Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
		}, false)
		req, err := kiroPayloadToAnthropic("claude-fable-5", payload)
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		if req.MaxTokens != defaultAnthropicUpstreamMaxTokens {
			t.Fatalf("max_tokens = %d, want default %d", req.MaxTokens, defaultAnthropicUpstreamMaxTokens)
		}
		if req.MaxTokens <= 4096 {
			t.Fatalf("default max_tokens %d is low enough to truncate long answers", req.MaxTokens)
		}
	})

	t.Run("client max_tokens wins", func(t *testing.T) {
		payload := OpenAIToKiro(&OpenAIRequest{
			Model: "claude-fable-5", Messages: []OpenAIMessage{{Role: "user", Content: "hi"}}, MaxTokens: 128,
		}, false)
		req, err := kiroPayloadToAnthropic("claude-fable-5", payload)
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		if req.MaxTokens != 128 {
			t.Fatalf("max_tokens = %d, want the client's 128", req.MaxTokens)
		}
	})
}

// TestAnthropicUpstreamMaxTokensStopReasonIsParsed covers the truncation signal path:
// a stream ending with stop_reason "max_tokens" must still deliver everything the
// upstream sent (no dropped text, usage intact, OnComplete fired). Before stop_reason
// was parsed at all, a truncated answer was indistinguishable from a complete one.
func TestAnthropicUpstreamMaxTokensStopReasonIsParsed(t *testing.T) {
	mustInitConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"a long answer cut off mid-sen\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":9}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	account := &config.Account{
		ID: "anthropic-trunc", Backend: config.BackendOpenAICompatible, APIFormat: config.APIFormatAnthropic,
		ApiKey: "upstream-secret", BaseURL: server.URL, Models: []string{"claude-fable-5"},
	}
	payload := OpenAIToKiro(&OpenAIRequest{
		Model: "claude-fable-5", Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}, false)

	var text string
	outputTokens, completes := 0, 0
	err := CallUpstreamAPI(context.Background(), account, "claude-fable-5", payload, &KiroStreamCallback{
		OnText:     func(value string, isThinking bool) { text += value },
		OnComplete: func(in, out int) { outputTokens = out; completes++ },
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if text != "a long answer cut off mid-sen" {
		t.Fatalf("truncated stream lost text: %q", text)
	}
	if outputTokens != 9 {
		t.Fatalf("outputTokens = %d, want 9", outputTokens)
	}
	if completes != 1 {
		t.Fatalf("OnComplete fired %d times, want 1", completes)
	}
}

func TestAnthropicCompatibleSSEDispatch(t *testing.T) {
	mustInitConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("unexpected authorization header %q", got)
		}
		if got := r.Header.Get("x-api-key"); got != "upstream-secret" {
			t.Errorf("unexpected x-api-key header %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("unexpected anthropic-version %q", got)
		}
		var req anthropicCompatibleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != "claude-fable-5" || !req.Stream || req.MaxTokens != 32 {
			t.Errorf("unexpected request model=%q stream=%v max=%d", req.Model, req.Stream, req.MaxTokens)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":0}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello \"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"reason\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_1\",\"name\":\"weather\",\"input\":{}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	account := &config.Account{
		ID: "anthropic", Backend: config.BackendOpenAICompatible, APIFormat: config.APIFormatAnthropic,
		ApiKey: "upstream-secret", BaseURL: server.URL, Models: []string{"claude-fable-5"},
		Pricing: &config.UpstreamPricing{InputPerMillion: 10, OutputPerMillion: 50, Markup: 1.4, MinChargeUSD: 0.001},
	}
	payload := OpenAIToKiro(&OpenAIRequest{
		Model: "claude-fable-5", Messages: []OpenAIMessage{{Role: "user", Content: "hi"}}, MaxTokens: 32,
	}, false)

	var text, thinking string
	var tools []KiroToolUse
	inputTokens, outputTokens, completes := 0, 0, 0
	sourceCost, charge := 0.0, 0.0
	err := CallUpstreamAPI(context.Background(), account, "claude-fable-5", payload, &KiroStreamCallback{
		OnText: func(value string, isThinking bool) {
			if isThinking {
				thinking += value
			} else {
				text += value
			}
		},
		OnToolUse:    func(tool KiroToolUse) { tools = append(tools, tool) },
		OnSourceCost: func(value float64) { sourceCost = value },
		OnCredits:    func(value float64) { charge = value },
		OnComplete: func(in, out int) {
			inputTokens, outputTokens = in, out
			completes++
		},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if text != "hello " || thinking != "reason" {
		t.Fatalf("unexpected content text=%q thinking=%q", text, thinking)
	}
	if len(tools) != 1 || tools[0].Name != "weather" || tools[0].Input["city"] != "Paris" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	if inputTokens != 12 || outputTokens != 5 || completes != 1 {
		t.Fatalf("unexpected completion usage=%d/%d count=%d", inputTokens, outputTokens, completes)
	}
	assertBillingNear(t, sourceCost, 0.00037)
	assertBillingNear(t, charge, 0.001)
}

func TestAnthropicCompatibleHTTPErrorDoesNotLeakKey(t *testing.T) {
	mustInitConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"type":"error","error":{"message":"busy"}}`))
	}))
	defer server.Close()
	account := &config.Account{
		Backend: config.BackendOpenAICompatible, APIFormat: config.APIFormatAnthropic,
		ApiKey: "do-not-leak", BaseURL: server.URL + "/v1", Models: []string{"claude-fable-5"},
	}
	payload := OpenAIToKiro(&OpenAIRequest{Model: "claude-fable-5", Messages: []OpenAIMessage{{Role: "user", Content: "hi"}}}, false)
	err := CallUpstreamAPI(context.Background(), account, "claude-fable-5", payload, &KiroStreamCallback{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("expected HTTP 503, got %v", err)
	}
	if strings.Contains(err.Error(), account.ApiKey) {
		t.Fatal("upstream key leaked in error")
	}
}

func TestAnthropicMessagesURL(t *testing.T) {
	for input, want := range map[string]string{
		"https://example.com":             "https://example.com/v1/messages",
		"https://example.com/v1":          "https://example.com/v1/messages",
		"https://example.com/v1/messages": "https://example.com/v1/messages",
	} {
		got, err := anthropicMessagesURL(input)
		if err != nil || got != want {
			t.Errorf("anthropicMessagesURL(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}
