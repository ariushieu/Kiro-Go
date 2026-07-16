package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

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
	err := CallUpstreamAPI(account, "claude-fable-5", payload, &KiroStreamCallback{
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
	err := CallUpstreamAPI(account, "claude-fable-5", payload, &KiroStreamCallback{})
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
