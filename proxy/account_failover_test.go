package proxy

import (
	"errors"
	"net/http"
	"testing"
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
	}

	for _, tc := range tests {
		status, msg := clientFacingUpstreamError(tc.err)
		if status != tc.wantStatus || msg != tc.wantMsg {
			t.Fatalf("%s: clientFacingUpstreamError() = (%d, %q), want (%d, %q)", tc.name, status, msg, tc.wantStatus, tc.wantMsg)
		}
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
