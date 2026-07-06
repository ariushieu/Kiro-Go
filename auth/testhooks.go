package auth

import "net/http"

// SetOIDCTokenURLForTest replaces the OIDC token URL builder. Test-only.
func SetOIDCTokenURLForTest(fn func(region string) string) {
	if fn == nil {
		return
	}
	oidcTokenURL = fn
}

// GetOIDCTokenURLForTest returns the current OIDC token URL builder so tests
// can restore it after replacement.
func GetOIDCTokenURLForTest() func(region string) string { return oidcTokenURL }

// SetGlobalAuthClientForTest swaps the global auth HTTP client. The package's
// init() installs a client whose Transport calls http.ProxyFromEnvironment, and
// that function caches env vars on first call — which corrupts later tests
// that rely on t.Setenv("HTTPS_PROXY", ...). Tests that need to issue an HTTP
// request against a httptest server should install a client whose Transport
// has Proxy=nil to keep env-proxy state clean. Returns the previous client so
// callers can restore it.
func SetGlobalAuthClientForTest(c *http.Client) *http.Client {
	old := httpClientStore.Load()
	if c != nil {
		httpClientStore.Store(c)
	}
	return old
}

// SetKiroPortalBaseURL sets the Kiro portal sign-in URL for testing.
func SetKiroPortalBaseURL(url string) {
	if url != "" {
		kiroPortalSignInURL = url
	}
}

// SetExternalIdpTokenURLFnForTest sets a custom external IdP token URL resolver for testing.
// The function receives an issuer URL and returns the token endpoint URL.
func SetExternalIdpTokenURLFnForTest(fn func(issuerURL string, client *http.Client) (string, error)) {
	if fn != nil {
		externalIdpTokenURLFn = fn
	}
}

// SetKiroLoopbackPortsForTest overrides the loopback port list used by
// bindKiroLoopback. Pass []int{0} to force an ephemeral random port and avoid
// port conflicts during parallel test runs. Returns the previous override so
// callers can restore it. Test-only.
func SetKiroLoopbackPortsForTest(ports []int) []int {
	old := kiroLoopbackPortsOverride
	kiroLoopbackPortsOverride = ports
	return old
}
