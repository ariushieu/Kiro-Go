package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"kiro-go/config"
)

// firstByteTimeoutSSE is the error a gateway in front of a custom upstream emits when
// its own origin is too slow to produce a first byte. It arrives as an SSE `error`
// event, which consumeAnthropicCompatibleSSE turns into an "...upstream error: ..."
// Go error — the string the retry matcher keys on.
const firstByteTimeoutSSE = "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"first_byte_timeout: upstream produced no response within 45s\"}}\n\n"

func anthropicSuccessSSE() string {
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"warmed up\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

func newFirstByteRetryAccount(baseURL string) *config.Account {
	return &config.Account{
		ID: "fb-retry", Backend: config.BackendOpenAICompatible, APIFormat: config.APIFormatAnthropic,
		ApiKey: "upstream-secret", BaseURL: baseURL, Models: []string{"claude-fable-5"},
	}
}

func firstByteRetryPayload() *KiroPayload {
	return OpenAIToKiro(&OpenAIRequest{
		Model: "claude-fable-5", Messages: []OpenAIMessage{{Role: "user", Content: "hi"}},
	}, false)
}

// TestFirstByteTimeoutRetriesSameUpstream: the first two attempts time out on the
// first byte, the third succeeds. The dispatcher must retry the same upstream and
// deliver the eventual answer without surfacing the transient failures.
func TestFirstByteTimeoutRetriesSameUpstream(t *testing.T) {
	mustInitConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n < 3 {
			_, _ = w.Write([]byte(firstByteTimeoutSSE))
			return
		}
		_, _ = w.Write([]byte(anthropicSuccessSSE()))
	}))
	defer server.Close()

	var text string
	completes := 0
	err := CallUpstreamAPI(context.Background(), newFirstByteRetryAccount(server.URL), "claude-fable-5",
		firstByteRetryPayload(), &KiroStreamCallback{
			OnText:     func(v string, _ bool) { text += v },
			OnComplete: func(_, _ int) { completes++ },
		})
	if err != nil {
		t.Fatalf("dispatch after retries: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream called %d times, want 3 (2 retries + success)", got)
	}
	if text != "warmed up" {
		t.Fatalf("final text = %q, want %q", text, "warmed up")
	}
	if completes != 1 {
		t.Fatalf("OnComplete fired %d times, want 1", completes)
	}
}

// TestFirstByteTimeoutExhaustsRetries: every attempt times out. The dispatcher must
// stop after maxUpstreamFirstByteRetries retries and return the failure.
func TestFirstByteTimeoutExhaustsRetries(t *testing.T) {
	mustInitConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(firstByteTimeoutSSE))
	}))
	defer server.Close()

	err := CallUpstreamAPI(context.Background(), newFirstByteRetryAccount(server.URL), "claude-fable-5",
		firstByteRetryPayload(), &KiroStreamCallback{})
	if err == nil || !strings.Contains(err.Error(), "first_byte_timeout") {
		t.Fatalf("expected a first-byte-timeout error, got %v", err)
	}
	if got := calls.Load(); got != int32(maxUpstreamFirstByteRetries+1) {
		t.Fatalf("upstream called %d times, want %d (1 + %d retries)", got, maxUpstreamFirstByteRetries+1, maxUpstreamFirstByteRetries)
	}
}

// TestFirstByteTimeoutAfterContentDoesNotRetry: once any text has streamed to the
// client, a first-byte-timeout error must NOT trigger a retry — replaying would
// duplicate the answer. The stream must be attempted exactly once.
func TestFirstByteTimeoutAfterContentDoesNotRetry(t *testing.T) {
	mustInitConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"))
		_, _ = w.Write([]byte(firstByteTimeoutSSE))
	}))
	defer server.Close()

	var text string
	err := CallUpstreamAPI(context.Background(), newFirstByteRetryAccount(server.URL), "claude-fable-5",
		firstByteRetryPayload(), &KiroStreamCallback{
			OnText: func(v string, _ bool) { text += v },
		})
	if err == nil || !strings.Contains(err.Error(), "first_byte_timeout") {
		t.Fatalf("expected the first-byte-timeout error to surface, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times, want 1 (no retry after content emitted)", got)
	}
	if text != "partial" {
		t.Fatalf("text = %q, want the single partial emission %q", text, "partial")
	}
}

// TestNonFirstByteErrorDoesNotRetry: an unrelated upstream failure (HTTP 503 body)
// must pass straight through without the first-byte retry loop kicking in.
func TestNonFirstByteErrorDoesNotRetry(t *testing.T) {
	mustInitConfig(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"type":"error","error":{"message":"busy"}}`))
	}))
	defer server.Close()

	err := CallUpstreamAPI(context.Background(), newFirstByteRetryAccount(server.URL), "claude-fable-5",
		firstByteRetryPayload(), &KiroStreamCallback{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("expected HTTP 503 to pass through, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times, want 1 (no retry on a non-first-byte error)", got)
	}
}
