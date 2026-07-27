package proxy

import (
	"errors"
	"net/http"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
		{name: "auth", fn: isAuthErrorMessage, msg: "Authentication failed - token invalid or expired"},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}

func TestClientFacingUpstreamError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantMsg    string
	}{
		{name: "nil", err: nil, wantStatus: http.StatusServiceUnavailable, wantMsg: noAccountsClientMessage},
		{name: "quota", err: errors.New("HTTP 429: quota exhausted"), wantStatus: http.StatusTooManyRequests, wantMsg: rateLimitedClientMessage},
		{name: "overage", err: errors.New("HTTP 402 from Kiro IDE: OVERAGE limit exceeded"), wantStatus: http.StatusServiceUnavailable, wantMsg: noAccountsClientMessage},
		{name: "suspension", err: errors.New("Your User ID temporarily is suspended"), wantStatus: http.StatusServiceUnavailable, wantMsg: noAccountsClientMessage},
		{name: "profile", err: errors.New("no available Kiro profile"), wantStatus: http.StatusServiceUnavailable, wantMsg: noAccountsClientMessage},
		{name: "proxy", err: errors.New("proxyconnect tcp: dial tcp 1.2.3.4:1080: connect: connection refused"), wantStatus: http.StatusServiceUnavailable, wantMsg: noAccountsClientMessage},
		{name: "auth", err: errors.New("Authentication failed - token invalid or expired"), wantStatus: http.StatusServiceUnavailable, wantMsg: noAccountsClientMessage},
		{name: "passthrough", err: errors.New("Improperly formed request"), wantStatus: http.StatusInternalServerError, wantMsg: "Improperly formed request"},

		// Custom (non-Kiro) upstream errors carry the third-party provider's raw body.
		// Every shape produced by upstream_openai.go / upstream_anthropic.go /
		// upstream.go must be masked as maintenance, never passed through.
		{
			name:       "custom upstream HTTP body",
			err:        errors.New(`HTTP 500 from OpenAI-compatible upstream: {"error":{"message":"upstream provider xyz.example.com is down"}}`),
			wantStatus: http.StatusServiceUnavailable,
			wantMsg:    noAccountsClientMessage,
		},
		{
			name:       "custom upstream anthropic HTTP body",
			err:        errors.New(`HTTP 529 from Anthropic-compatible upstream: {"type":"overloaded_error"}`),
			wantStatus: http.StatusServiceUnavailable,
			wantMsg:    noAccountsClientMessage,
		},
		{
			name:       "custom upstream billing wording must not reach customer",
			err:        errors.New("OpenAI-compatible upstream error: Your credit balance is too low to access the API"),
			wantStatus: http.StatusServiceUnavailable,
			wantMsg:    noAccountsClientMessage,
		},
		{
			name:       "custom upstream anthropic sse error",
			err:        errors.New("Anthropic-compatible upstream error: unknown upstream error"),
			wantStatus: http.StatusServiceUnavailable,
			wantMsg:    noAccountsClientMessage,
		},
		{
			name:       "custom upstream malformed sse",
			err:        errors.New("invalid OpenAI-compatible SSE chunk: unexpected end of JSON input"),
			wantStatus: http.StatusServiceUnavailable,
			wantMsg:    noAccountsClientMessage,
		},
		{
			name:       "custom upstream bad baseURL",
			err:        errors.New("invalid Anthropic-compatible baseURL"),
			wantStatus: http.StatusServiceUnavailable,
			wantMsg:    noAccountsClientMessage,
		},
		{
			name:       "custom upstream misconfigured backend",
			err:        errors.New(`unsupported custom upstream apiFormat "gemini"`),
			wantStatus: http.StatusServiceUnavailable,
			wantMsg:    noAccountsClientMessage,
		},
		{
			name:       "custom upstream unknown backend",
			err:        errors.New(`unsupported upstream backend "vertex"`),
			wantStatus: http.StatusServiceUnavailable,
			wantMsg:    noAccountsClientMessage,
		},
	}

	for _, tc := range tests {
		status, msg := clientFacingUpstreamError(tc.err)
		if status != tc.wantStatus || msg != tc.wantMsg {
			t.Fatalf("%s: clientFacingUpstreamError() = (%d, %q), want (%d, %q)", tc.name, status, msg, tc.wantStatus, tc.wantMsg)
		}
	}
}

// TestCustomUpstreamFailureDoesNotBanOnNon401 pins the failover policy for custom
// (non-Kiro) upstreams: only a hard 401 disables the account. A resold provider
// answering 403 for a model the plan doesn't cover, or 500/overloaded, must leave
// the account enabled — otherwise one request for an unentitled model permanently
// bans a healthy account and drains the pool.
func TestCustomUpstreamFailureDoesNotBanOnNon401(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantBan bool
	}{
		{
			name:    "403 model permission must not ban",
			err:     errors.New(`HTTP 403 from OpenAI-compatible upstream: {"error":{"message":"You do not have access to model claude-opus-4-8","type":"forbidden"}}`),
			wantBan: false,
		},
		{
			name:    "unauthorized wording in body must not ban",
			err:     errors.New(`HTTP 403 from Anthropic-compatible upstream: {"error":{"message":"unauthorized for this model"}}`),
			wantBan: false,
		},
		{
			name:    "500 must not ban",
			err:     errors.New(`HTTP 500 from OpenAI-compatible upstream: internal error`),
			wantBan: false,
		},
		{
			name:    "401 bad key does ban",
			err:     errors.New(`HTTP 401 from OpenAI-compatible upstream: {"error":{"message":"invalid api key"}}`),
			wantBan: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mustInitConfig(t)
			acct := config.Account{
				ID:       "custom-1",
				Nickname: "reseller",
				Email:    "reseller@example.com",
				Enabled: true,
				Backend: config.BackendOpenAICompatible,
				ApiKey:  "sk-upstream",
				BaseURL: "https://provider.example.com/v1",
				Models:  []string{"claude-sonnet-5"},
			}
			if err := config.AddAccount(acct); err != nil {
				t.Fatalf("add account: %v", err)
			}
			p := accountpool.GetPool()
			p.Reload()
			h := &Handler{pool: p}

			h.handleAccountFailure(&acct, tc.err)

			var got *config.Account
			for _, a := range config.GetAccounts() {
				if a.ID == acct.ID {
					cp := a
					got = &cp
					break
				}
			}
			if got == nil {
				t.Fatalf("account disappeared")
			}
			if tc.wantBan && got.Enabled {
				t.Fatalf("expected account to be banned for %q, still enabled", tc.err)
			}
			if !tc.wantBan && !got.Enabled {
				t.Fatalf("account was banned for a non-401 failure (%q); banReason=%q", tc.err, got.BanReason)
			}
		})
	}
}

func TestIsProxyErrorMessage(t *testing.T) {
	hits := []string{
		"require-proxy: no proxy configured for account",
		"proxyconnect tcp: dial tcp 1.2.3.4:1080: connect: connection refused",
		"socks connect tcp: i/o timeout",
		"dial tcp 1.2.3.4:8080: connectex: A connection attempt failed",
	}
	for _, m := range hits {
		if !isProxyErrorMessage(m) {
			t.Fatalf("expected proxy-error match for %q", m)
		}
	}
	misses := []string{
		"HTTP 401 unauthorized",
		"quota exhausted on KiroIDE",
		"temporarily_suspended",
	}
	for _, m := range misses {
		if isProxyErrorMessage(m) {
			t.Fatalf("did not expect proxy-error match for %q", m)
		}
	}
}
