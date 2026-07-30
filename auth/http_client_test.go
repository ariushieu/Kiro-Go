package auth

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

func TestBuildAuthTransportUsesExplicitProxyURL(t *testing.T) {
	transport := buildAuthTransport("http://proxy.local:8080")
	req := &http.Request{URL: mustParseURL(t, "https://oidc.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://proxy.local:8080")
}

// Asserts on function identity rather than calling Proxy with a t.Setenv'd
// HTTPS_PROXY. net/http reads the proxy environment through a sync.Once, so the
// first real HTTP request anywhere in this test binary freezes the cached value and
// any later t.Setenv is silently ignored — the env-based version of this test passed
// or failed depending on test order. What matters here is only that the transport
// delegates to the environment at all.
func TestBuildAuthTransportFallsBackToEnvironmentProxy(t *testing.T) {
	transport := buildAuthTransport("")

	if transport.Proxy == nil {
		t.Fatal("transport must fall back to the environment proxy, got a nil Proxy")
	}
	want := reflect.ValueOf(http.ProxyFromEnvironment).Pointer()
	got := reflect.ValueOf(transport.Proxy).Pointer()
	if got != want {
		t.Fatal("Proxy is not http.ProxyFromEnvironment")
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("invalid test URL: %v", err)
	}
	return parsed
}

func assertProxyURL(t *testing.T, got *url.URL, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected proxy URL %q, got nil", want)
	}
	if got.String() != want {
		t.Fatalf("expected proxy URL %q, got %q", want, got.String())
	}
}
