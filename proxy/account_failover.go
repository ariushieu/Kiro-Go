package proxy

import (
	"context"
	"errors"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

const maxAccountRetryAttempts = 3

// isClientGoneError reports that the request was abandoned rather than failing on
// the upstream's side: the caller disconnected, or our own idle watchdog gave up on
// a silent stream (see watchStreamIdle).
//
// These must NOT be retried on another account. A retry re-runs the whole generation
// upstream — real tokens, real cost — to produce a response that either nobody is
// listening for, or that will stall exactly the same way. Before this check, a client
// that hung up mid-stream quietly triggered up to maxAccountRetryAttempts full
// generations.
func isClientGoneError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, errStreamIdle) {
		return true
	}
	// Textual fallback for errors that crossed a boundary which dropped the wrap
	// (e.g. an error rebuilt from a string).
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "stream idle for more than")
}

// isUpstreamTimeoutMessage matches a stalled upstream: a deadline exceeded while
// waiting on response headers or the body. Distinct from isClientGoneError because
// the account itself may be perfectly healthy, so it earns at most a short cooldown
// and never a ban.
func isUpstreamTimeoutMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "client.timeout exceeded") ||
		strings.Contains(msg, "timeout awaiting response headers")
}

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "429") || strings.Contains(msg, "quota") || strings.Contains(msg, "throttl")
}

func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "402") && strings.Contains(msg, "overage")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

func isAuthErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

// isCustomUpstreamErrorMessage matches the error strings produced by the custom
// (non-Kiro) upstream callers in upstream_openai.go / upstream_anthropic.go /
// upstream.go. Those strings embed the third-party provider's raw response body
// (up to 4KB), which must never reach a customer: it leaks that we resell a proxy,
// names the provider, and its wording ("your credit balance is too low", "invalid
// api key") gets misread by client SDKs as the CUSTOMER's own key/billing being
// broken. Keep in sync with the fmt.Errorf strings in those three files.
func isCustomUpstreamErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "-compatible upstream") ||
		strings.Contains(msg, "-compatible baseurl") ||
		strings.Contains(msg, "-compatible request has no messages") ||
		strings.Contains(msg, "-compatible sse ") ||
		strings.Contains(msg, "unsupported custom upstream apiformat") ||
		strings.Contains(msg, "unsupported upstream backend")
}

// isCustomUpstreamAuthBanMessage narrowly matches a custom upstream rejecting OUR
// credential, i.e. only HTTP 401. It deliberately does NOT match 403/"forbidden"/
// "unauthorized" the way isAuthErrorMessage does: resold providers return 403 for
// per-model permission, region blocks and plan limits, so matching those would
// permanently BAN a perfectly healthy account on the first request for a model it
// isn't entitled to. Everything other than 401 is a soft failure (cooldown+rotate).
func isCustomUpstreamAuthBanMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "invalid api key") ||
		strings.Contains(msg, "invalid_api_key")
}

// isProxyErrorMessage matches outbound-proxy / dial failures: a missing required
// proxy (require-proxy), a dead or refusing proxy, or a connect timeout on the
// proxy hop. These are infrastructure failures, not account bans — the account
// is cooled down and the request rotates to the next account. NOTE: keep this
// case ABOVE isAuthErrorMessage in handleAccountFailure so a proxy connect
// failure is never misread as an auth ban and disable the account.
func isProxyErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "require-proxy") ||
		strings.Contains(msg, "proxyconnect") ||
		strings.Contains(msg, "socks") ||
		strings.Contains(msg, "connection refused") ||
		(strings.Contains(msg, "dial tcp") && (strings.Contains(msg, "timeout") ||
			strings.Contains(msg, "refused") ||
			strings.Contains(msg, "connectex") ||
			strings.Contains(msg, "no such host")))
}

// statusForUpstreamError maps an upstream error to the HTTP status the client should see.
// Quota/throttle → 429, overage → 402, auth → 401, everything else → 500.
func statusForUpstreamError(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	msg := err.Error()
	switch {
	case isQuotaErrorMessage(msg):
		return http.StatusTooManyRequests
	case isOverageErrorMessage(msg):
		return http.StatusPaymentRequired
	case isAuthErrorMessage(msg):
		return http.StatusUnauthorized
	default:
		return http.StatusInternalServerError
	}
}

// clientFacingUpstreamError 决定重试耗尽后返回给【客户】的状态码和文案。
// 账号池内部故障（账号 token/鉴权、overage、封号、profile ARN、出站代理）与客户无关 —
// 原样返回 401/402 会被客户误读成"自己的 key/账单坏了"，所以统一伪装成 503 维护文案。
// 配额耗尽保留 429（调用方仍会带 Retry-After）让客户端 SDK 正常退避，但替换上游原文。
// 其余错误原样透传。真实原因始终记录在管理端请求日志里。
func clientFacingUpstreamError(err error) (status int, message string) {
	if err == nil {
		return http.StatusServiceUnavailable, noAccountsClientMessage()
	}
	msg := err.Error()
	switch {
	case isQuotaErrorMessage(msg):
		return http.StatusTooManyRequests, rateLimitedClientMessage()
	case isUpstreamTimeoutMessage(msg):
		// 上游卡住是内部故障：原文里带着我们的超时细节，对客户毫无意义，
		// 一律走维护文案（504 让客户端知道可以重试）。
		return http.StatusGatewayTimeout, noAccountsClientMessage()
	case isOverageErrorMessage(msg),
		isSuspensionErrorMessage(msg),
		isProfileUnavailableErrorMessage(msg),
		isProxyErrorMessage(msg),
		isAuthErrorMessage(msg),
		// 自定义（非 Kiro）上游的错误串里带着第三方原文 body，同样属于内部故障，
		// 一律伪装成维护文案，绝不透传给客户。
		isCustomUpstreamErrorMessage(msg):
		return http.StatusServiceUnavailable, noAccountsClientMessage()
	default:
		return statusForUpstreamError(err), msg
	}
}

func errorTypeForOpenAIStatus(status int) string {
	switch status {
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	default:
		return "server_error"
	}
}

// applyRetryAfterHeader sets Retry-After on quota errors, using the upstream-supplied
// value when the message carries one ("retry after 30"), else a 60s default.
func applyRetryAfterHeader(w http.ResponseWriter, err error) {
	if w == nil || err == nil || !isQuotaErrorMessage(err.Error()) {
		return
	}
	if retryAfter := retryAfterFromError(err.Error()); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
		return
	}
	w.Header().Set("Retry-After", "60")
}

func retryAfterFromError(msg string) string {
	idx := strings.LastIndex(strings.ToLower(msg), "retry after ")
	if idx < 0 {
		return ""
	}
	value := strings.TrimSpace(msg[idx+len("retry after "):])
	if semi := strings.Index(value, ";"); semi >= 0 {
		value = strings.TrimSpace(value[:semi])
	}
	return value
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	if account.EffectiveBackend() != config.BackendKiro {
		switch {
		case isClientGoneError(err):
			// Caller left / stream went silent — not the account's fault.
			logger.Warnf("[AccountFailover] Request abandoned on %s: %v", account.Email, err)
		case isUpstreamTimeoutMessage(errMsg):
			logger.Warnf("[AccountFailover] Upstream timeout for %s: %v", account.Email, err)
			h.pool.RecordError(account.ID, false)
		case isProxyErrorMessage(errMsg):
			logger.Warnf("[AccountFailover] Proxy/dial failure for %s: %v", account.Email, err)
			h.pool.RecordError(account.ID, false)
		case isQuotaErrorMessage(errMsg):
			h.pool.RecordError(account.ID, true)
		case isCustomUpstreamAuthBanMessage(errMsg):
			// Only a hard 401 (our key is wrong) bans the account. NOTE: do NOT widen
			// this to isAuthErrorMessage — it also matches 403/"forbidden", which
			// resold providers return for per-model permission and plan limits, and
			// that would ban a healthy account on one unentitled model request.
			h.disableAccount(account, "BANNED", "Custom upstream rejected our API key (HTTP 401)")
		default:
			logger.Warnf("[AccountFailover] Custom upstream failure for %s (cooldown, not banned): %v", account.Email, err)
			h.pool.RecordError(account.ID, false)
		}
		return
	}
	switch {
	case isClientGoneError(err):
		// The caller left or the stream went silent. Nothing about the account is
		// known to be wrong, so do not penalise it at all — counting this as an
		// error would push healthy accounts into cooldown whenever users cancel.
		logger.Warnf("[AccountFailover] Request abandoned on %s: %v", account.Email, err)
	case isUpstreamTimeoutMessage(errMsg):
		// Upstream stalled. Cool down so the next request rotates away, but never
		// ban: the credentials are fine and the endpoint usually recovers.
		logger.Warnf("[AccountFailover] Upstream timeout for %s: %v", account.Email, err)
		h.pool.RecordError(account.ID, false)
	case isProxyErrorMessage(errMsg):
		// Proxy/dial failure — cool down and rotate; never disable the account
		// and never fall through to a direct connection.
		logger.Warnf("[AccountFailover] Proxy/dial failure for %s: %v", account.Email, err)
		h.pool.RecordError(account.ID, false)
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false)
	case isQuotaErrorMessage(errMsg):
		h.pool.RecordError(account.ID, true)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case isAuthErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordError(account.ID, false)
	}
}
