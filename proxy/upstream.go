package proxy

import (
	"context"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"strings"
	"sync/atomic"
	"time"
)

// customUpstreamHistory removes the synthetic system-priming pair used by the
// native Kiro protocol. OpenAI/Anthropic custom upstreams receive the same text
// through their real system field, where it has the intended instruction priority.
func customUpstreamHistory(payload *KiroPayload) []KiroHistoryMessage {
	if payload == nil {
		return nil
	}
	history := payload.ConversationState.History
	if strings.TrimSpace(payload.SystemPrompt) == "" || len(history) < 2 {
		return history
	}
	first := history[0].UserInputMessage
	second := history[1].AssistantResponseMessage
	if first != nil && second != nil &&
		strings.TrimSpace(first.Content) == strings.TrimSpace(payload.SystemPrompt) &&
		strings.TrimSpace(second.Content) == "I will follow these instructions." {
		return history[2:]
	}
	return history
}

// Custom-upstream first-byte-timeout retries. A gateway in front of a custom
// upstream (Cloudflare, LiteLLM, one-api, …) gives up when its own origin is slow
// to produce the first byte and returns a "first_byte_timeout" error. That failure
// happens before any content reaches us, so re-issuing the SAME request to the SAME
// upstream is safe (it duplicates nothing) and usually succeeds once the origin has
// warmed up.
const (
	maxUpstreamFirstByteRetries = 2
	upstreamFirstByteRetryDelay = 1500 * time.Millisecond
)

// callCustomUpstreamWithRetry runs one custom-upstream attempt (call) and retries it,
// on the same upstream, when the failure is a first-byte timeout AND nothing has yet
// been streamed to the client. The emit guard is the safety net: if a future upstream
// ever reported this error after already sending tokens, retrying would duplicate the
// answer, so we only retry while the response is still empty.
func callCustomUpstreamWithRetry(ctx context.Context, account *config.Account, callback *KiroStreamCallback, call func(*KiroStreamCallback) error) error {
	var emitted atomic.Bool
	guarded := guardEmitCallback(callback, &emitted)

	var err error
	for attempt := 0; ; attempt++ {
		err = call(guarded)
		if err == nil {
			return nil
		}
		if attempt >= maxUpstreamFirstByteRetries {
			return err
		}
		if !isUpstreamFirstByteTimeoutMessage(err.Error()) || emitted.Load() {
			return err
		}
		if ctx != nil && ctx.Err() != nil {
			return err
		}
		logger.Warnf("[Upstream] First-byte timeout on %s (attempt %d/%d), retrying same upstream: %v",
			accountEmailForLog(account), attempt+1, maxUpstreamFirstByteRetries+1, err)
		select {
		case <-time.After(upstreamFirstByteRetryDelay):
		case <-ctxDone(ctx):
			return err
		}
	}
}

// guardEmitCallback wraps callback so the first real content event (text or tool use)
// flips emitted. It leaves every other hook untouched so accounting stays identical.
func guardEmitCallback(callback *KiroStreamCallback, emitted *atomic.Bool) *KiroStreamCallback {
	if callback == nil {
		return &KiroStreamCallback{OnText: func(string, bool) { emitted.Store(true) }}
	}
	wrapped := *callback
	origText := callback.OnText
	wrapped.OnText = func(text string, isThinking bool) {
		emitted.Store(true)
		if origText != nil {
			origText(text, isThinking)
		}
	}
	origTool := callback.OnToolUse
	wrapped.OnToolUse = func(tu KiroToolUse) {
		emitted.Store(true)
		if origTool != nil {
			origTool(tu)
		}
	}
	return &wrapped
}

func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

// CallUpstreamAPI is the single dispatch point used by request handlers. Kiro
// remains the default for legacy accounts; additional backends implement the
// same callback contract so response formatting and accounting stay shared.
//
// ctx carries the inbound request's cancellation so a client that hangs up stops
// the upstream stream instead of leaving it to run (and bill) unread.
func CallUpstreamAPI(ctx context.Context, account *config.Account, model string, payload *KiroPayload, callback *KiroStreamCallback) error {
	if account == nil {
		return fmt.Errorf("missing upstream account")
	}
	maskedCallback, flushMask := maskUpstreamIdentity(callback, payload, model)
	defer flushMask()
	callback = maskedCallback
	switch account.EffectiveBackend() {
	case config.BackendKiro:
		return CallKiroAPI(ctx, account, payload, callback)
	case config.BackendOpenAICompatible:
		switch account.EffectiveAPIFormat() {
		case config.APIFormatOpenAI:
			return callCustomUpstreamWithRetry(ctx, account, callback, func(cb *KiroStreamCallback) error {
				return CallOpenAICompatibleAPI(ctx, account, model, payload, cb)
			})
		case config.APIFormatAnthropic:
			return callCustomUpstreamWithRetry(ctx, account, callback, func(cb *KiroStreamCallback) error {
				return CallAnthropicCompatibleAPI(ctx, account, model, payload, cb)
			})
		default:
			return fmt.Errorf("unsupported custom upstream apiFormat %q", account.APIFormat)
		}
	default:
		return fmt.Errorf("unsupported upstream backend %q", account.Backend)
	}
}
