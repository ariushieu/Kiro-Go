package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kiro-go/config"
)

func TestOpenAICompatibleSSEDispatch(t *testing.T) {
	mustInitConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("unexpected authorization header %q", got)
		}
		var req openAICompatibleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != "gpt-4.1" || !req.Stream {
			t.Errorf("unexpected request model=%q stream=%v", req.Model, req.Stream)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"Paris\\\"}\"}}]}}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5,\"total_tokens\":17}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	account := &config.Account{
		ID: "openai", Backend: config.BackendOpenAICompatible,
		ApiKey: "upstream-secret", BaseURL: server.URL + "/v1", Models: []string{"gpt-4.1"},
	}
	payload := OpenAIToKiro(&OpenAIRequest{
		Model: "gpt-4.1", Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
		Tools: []OpenAITool{{Type: "function", Function: struct {
			Name        string      `json:"name"`
			Description string      `json:"description"`
			Parameters  interface{} `json:"parameters"`
		}{Name: "weather", Parameters: map[string]interface{}{"type": "object"}}}},
	}, false)

	var text string
	var tools []KiroToolUse
	inputTokens, outputTokens := 0, 0
	err := CallUpstreamAPI(account, "gpt-4.1", payload, &KiroStreamCallback{
		OnText: func(s string, thinking bool) {
			if !thinking {
				text += s
			}
		},
		OnToolUse:  func(tool KiroToolUse) { tools = append(tools, tool) },
		OnComplete: func(in, out int) { inputTokens, outputTokens = in, out },
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if text != "hello " {
		t.Fatalf("unexpected text %q", text)
	}
	if len(tools) != 1 || tools[0].Name != "weather" || tools[0].Input["city"] != "Paris" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	if inputTokens != 12 || outputTokens != 5 {
		t.Fatalf("unexpected usage %d/%d", inputTokens, outputTokens)
	}
}

func TestOpenAICompatibleHTTPErrorDoesNotLeakKey(t *testing.T) {
	mustInitConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer server.Close()
	account := &config.Account{Backend: config.BackendOpenAICompatible, ApiKey: "do-not-leak", BaseURL: server.URL, Models: []string{"gpt-4.1"}}
	payload := OpenAIToKiro(&OpenAIRequest{Model: "gpt-4.1", Messages: []OpenAIMessage{{Role: "user", Content: "hi"}}}, false)
	err := CallUpstreamAPI(account, "gpt-4.1", payload, &KiroStreamCallback{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("expected HTTP 401, got %v", err)
	}
	if strings.Contains(err.Error(), account.ApiKey) {
		t.Fatal("upstream key leaked in error")
	}
}
