package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// validateExternalIdpURL tests
// ---------------------------------------------------------------------------

func TestValidateExternalIdpURL_ValidMicrosoftHosts(t *testing.T) {
	validURLs := []string{
		"https://login.microsoftonline.com/common/v2.0",
		"https://login.microsoftonline.com/tenant-id/v2.0",
		"https://login.microsoftonline.us/tenant/v2.0",
		"https://login.microsoftonline.cn/tenant/v2.0",
		"https://login.microsoftonline.com",
	}

	for _, rawURL := range validURLs {
		if err := validateExternalIdpURL(rawURL); err != nil {
			t.Errorf("expected valid, got error for %s: %v", rawURL, err)
		}
	}
}

func TestValidateExternalIdpURL_InvalidHosts(t *testing.T) {
	invalidURLs := []string{
		"https://example.com",
		"https://evil.com/microsoftonline.com",
		"https://microsoftonline.com.evil.com",
		"https://login-microsoftonline.com",
	}

	for _, rawURL := range invalidURLs {
		if err := validateExternalIdpURL(rawURL); err == nil {
			t.Errorf("expected error, got nil for %s", rawURL)
		}
	}
}

func TestValidateExternalIdpURL_RejectsHTTP(t *testing.T) {
	if err := validateExternalIdpURL("http://login.microsoftonline.com"); err == nil {
		t.Error("expected error for HTTP scheme, got nil")
	}
}

func TestValidateExternalIdpURL_RejectsIPLiterals(t *testing.T) {
	ipURLs := []string{
		"https://192.168.1.1/tenant/v2.0",
		"https://10.0.0.1",
		"https://[::1]/tenant",
	}

	for _, rawURL := range ipURLs {
		if err := validateExternalIdpURL(rawURL); err == nil {
			t.Errorf("expected error for IP literal, got nil for %s", rawURL)
		}
	}
}

func TestValidateExternalIdpURL_EmptyHost(t *testing.T) {
	if err := validateExternalIdpURL("https://"); err == nil {
		t.Error("expected error for empty host, got nil")
	}
}

// ---------------------------------------------------------------------------
// PKCE tests (from iam_sso.go — verify they're callable from kiro_sso.go)
// ---------------------------------------------------------------------------

func TestGenerateCodeVerifier(t *testing.T) {
	v := generateCodeVerifier()
	if len(v) < 43 {
		t.Errorf("code verifier too short: %d chars (expected >= 43)", len(v))
	}
	// Verify base64url characters only
	for _, c := range v {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			t.Errorf("invalid base64url char in verifier: %c", c)
		}
	}
}

func TestGenerateCodeChallenge(t *testing.T) {
	verifier := generateCodeVerifier()
	challenge := generateCodeChallenge(verifier)
	if len(challenge) != 43 {
		t.Errorf("expected 43-char challenge, got %d", len(challenge))
	}
	// Challenge must differ from verifier
	if challenge == verifier {
		t.Error("challenge should differ from verifier")
	}
}

// ---------------------------------------------------------------------------
// Session lifecycle tests
// ---------------------------------------------------------------------------

func TestKiroSsoSessionExpiry(t *testing.T) {
	// Tạo session thủ công với ExpiresAt đã qua
	s := &KiroSsoSession{
		ID:        "test-expired",
		ResultCh:  make(chan KiroSsoResult, 1),
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	}

	kiroSsoSessionsMu.Lock()
	kiroSsoSessions[s.ID] = s
	kiroSsoSessionsMu.Unlock()

	// Poll phải báo lỗi vì session đã hết hạn
	status, _, err := PollKiroSsoLogin("test-expired")
	if err == nil {
		t.Error("expected error for expired session")
	}
	if status != "" {
		t.Errorf("expected empty status, got %s", status)
	}

	// Session phải bị xóa khỏi map
	kiroSsoSessionsMu.RLock()
	_, exists := kiroSsoSessions["test-expired"]
	kiroSsoSessionsMu.RUnlock()
	if exists {
		t.Error("expired session should be removed from map")
	}
}

func TestKiroSsoCancel(t *testing.T) {
	s := &KiroSsoSession{
		ID:        "test-cancel",
		ResultCh:  make(chan KiroSsoResult, 1),
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	kiroSsoSessionsMu.Lock()
	kiroSsoSessions["test-cancel"] = s
	kiroSsoSessionsMu.Unlock()

	CancelKiroSsoLogin("test-cancel")

	// Session phải bị xóa
	kiroSsoSessionsMu.RLock()
	_, exists := kiroSsoSessions["test-cancel"]
	kiroSsoSessionsMu.RUnlock()
	if exists {
		t.Error("cancelled session should be removed from map")
	}
}

func TestKiroSsoSessionNotFound(t *testing.T) {
	status, _, err := PollKiroSsoLogin("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
	if status != "" {
		t.Errorf("expected empty status, got %s", status)
	}
}

// ---------------------------------------------------------------------------
// Token refresh tests (with mock IdP)
// ---------------------------------------------------------------------------

func TestRefreshExternalIdpToken_Success(t *testing.T) {
	// This test drives a local httptest token endpoint (not an allow-listed IdP host),
	// so disable the refresh-path SSRF allow-list check for the duration.
	validateExternalIdpEndpoints = false
	defer func() { validateExternalIdpEndpoints = true }()
	// Mock IdP token endpoint
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		if r.URL.Path != "/token" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer server.Close()

	// Dùng SetExternalIdpTokenURLFnForTest để mock resolver
	SetExternalIdpTokenURLFnForTest(func(issuerURL string, client *http.Client) (string, error) {
		return server.URL + "/token", nil
	})

	accessToken, refreshToken, expiresAt, _, err := RefreshExternalIdpToken(
		"old-refresh", "https://login.microsoftonline.com/test/v2.0", "", "test-client", "openid offline_access", nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if accessToken != "new-access" {
		t.Errorf("expected 'new-access', got '%s'", accessToken)
	}
	if refreshToken != "new-refresh" {
		t.Errorf("expected 'new-refresh', got '%s'", refreshToken)
	}
	if expiresAt <= time.Now().Unix() {
		t.Error("expiresAt should be in the future")
	}
}

func TestRefreshExternalIdpToken_ErrorResponse(t *testing.T) {
	validateExternalIdpEndpoints = false
	defer func() { validateExternalIdpEndpoints = true }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()

	SetExternalIdpTokenURLFnForTest(func(issuerURL string, client *http.Client) (string, error) {
		return server.URL + "/token", nil
	})

	_, _, _, _, err := RefreshExternalIdpToken(
		"bad-refresh", "https://login.microsoftonline.com/test/v2.0", "", "test-client", "", nil,
	)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("expected error to contain status code, got: %v", err)
	}
}

func TestRefreshExternalIdpToken_MissingIssuerURL(t *testing.T) {
	_, _, _, _, err := RefreshExternalIdpToken("token", "", "", "client", "", nil)
	if err == nil {
		t.Fatal("expected error for missing issuer URL")
	}
}

// TestRefreshExternalIdpToken_RejectsNonAllowlistedIssuer asserts the SSRF guard on
// the refresh path: an issuer that is not on the allow-list (e.g. cloud metadata) is
// refused before any network request is made (M2).
func TestRefreshExternalIdpToken_RejectsNonAllowlistedIssuer(t *testing.T) {
	// Validation is ON by default; do not disable it here.
	_, _, _, _, err := RefreshExternalIdpToken(
		"refresh", "http://169.254.169.254/latest", "", "test-client", "", nil,
	)
	if err == nil {
		t.Fatal("expected refresh to be rejected for a non-allow-listed issuer URL")
	}
	if !strings.Contains(err.Error(), "issuer_url") {
		t.Fatalf("expected an issuer_url validation error, got: %v", err)
	}
}

func TestRefreshExternalIdpToken_MissingClientID(t *testing.T) {
	_, _, _, _, err := RefreshExternalIdpToken("token", "https://login.microsoftonline.com/test", "", "", "", nil)
	if err == nil {
		t.Fatal("expected error for missing client ID")
	}
}

// ---------------------------------------------------------------------------
// StartKiroSsoLogin test (validates basic start flow)
// ---------------------------------------------------------------------------

func TestStartKiroSsoLogin(t *testing.T) {
	sessionID, authorizeURL, loopbackPort, err := StartKiroSsoLogin("test@company.com", "")
	if err != nil {
		t.Fatalf("unexpected error starting login: %v", err)
	}
	if sessionID == "" {
		t.Error("expected non-empty sessionID")
	}
	if authorizeURL == "" {
		t.Error("expected non-empty authorizeURL")
	}
	if !strings.Contains(authorizeURL, "app.kiro.dev/signin") {
		t.Errorf("authorizeURL should use app.kiro.dev/signin, got: %s", authorizeURL)
	}
	if !strings.Contains(authorizeURL, "redirect_from=KiroIDE") {
		t.Errorf("authorizeURL should contain redirect_from=KiroIDE, got: %s", authorizeURL)
	}
	if strings.Contains(authorizeURL, "client_id=") {
		t.Errorf("authorizeURL should NOT contain client_id, got: %s", authorizeURL)
	}
	if strings.Contains(authorizeURL, "response_type=") {
		t.Errorf("authorizeURL should NOT contain response_type, got: %s", authorizeURL)
	}
	if !strings.Contains(authorizeURL, "redirect_uri=http") {
		t.Errorf("authorizeURL should contain redirect_uri, got: %s", authorizeURL)
	}
	if loopbackPort <= 0 || loopbackPort > 65535 {
		t.Errorf("invalid loopback port: %d", loopbackPort)
	}

	// Cleanup
	CancelKiroSsoLogin(sessionID)
}

func TestStartKiroSsoLogin_NoLoginHint(t *testing.T) {
	sessionID, authorizeURL, _, err := StartKiroSsoLogin("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sessionID == "" {
		t.Error("expected non-empty sessionID")
	}
	if authorizeURL == "" {
		t.Error("expected non-empty authorizeURL")
	}
	if !strings.Contains(authorizeURL, "redirect_from=KiroIDE") {
		t.Error("authorizeURL should contain redirect_from=KiroIDE")
	}

	CancelKiroSsoLogin(sessionID)
}

// ---------------------------------------------------------------------------
// OIDC discovery client-injection test (proxy-leak fix)
// ---------------------------------------------------------------------------

func TestDiscoverOIDCEndpointsUsesPassedClient(t *testing.T) {
	var gotHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			w.WriteHeader(404)
			return
		}
		gotHeader = r.Header.Get("X-Proxy-Marker")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"authorization_endpoint":"https://login.microsoftonline.com/a","token_endpoint":"https://login.microsoftonline.com/t","issuer":"x"}`))
	}))
	defer server.Close()

	marker := &markerRoundTripper{next: http.DefaultTransport, marker: "yes"}
	client := &http.Client{Transport: marker}

	_, tokenEndpoint, err := discoverOIDCEndpoints(server.URL, client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokenEndpoint != "https://login.microsoftonline.com/t" {
		t.Fatalf("unexpected token endpoint: %q", tokenEndpoint)
	}
	if gotHeader != "yes" {
		t.Fatalf("discovery did not use the passed client (marker header missing)")
	}
}

type markerRoundTripper struct {
	next   http.RoundTripper
	marker string
}

func (m *markerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("X-Proxy-Marker", m.marker)
	return m.next.RoundTrip(r)
}

// TestRefreshExternalIdpTokenDiscoveryUsesPassedClient drives the REAL refresh
// path: empty tokenEndpoint → resolveExternalIdpTokenEndpoint → production seam
// (externalIdpTokenURLFn) → discoverOIDCEndpoints. It asserts the proxy-aware
// client threaded from oidc.go actually reaches the discovery server. This is the
// path the leak lived on — the direct-call test above bypasses the seam.
func TestRefreshExternalIdpTokenDiscoveryUsesPassedClient(t *testing.T) {
	// Drives local httptest discovery + token servers, so disable the refresh-path
	// SSRF allow-list check for the duration.
	validateExternalIdpEndpoints = false
	defer func() { validateExternalIdpEndpoints = true }()
	// Restore the production seam after the test (sibling tests mutate it).
	old := externalIdpTokenURLFn
	defer func() { externalIdpTokenURLFn = old }()
	// Reinstate the production closure so we exercise the real forwarding path,
	// not a prior test's fixed-URL stub.
	externalIdpTokenURLFn = func(issuerURL string, client *http.Client) (string, error) {
		_, tokenEndpoint, err := discoverOIDCEndpoints(issuerURL, client)
		return tokenEndpoint, err
	}

	var discoveryHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			discoveryHeader = r.Header.Get("X-Proxy-Marker")
			w.Header().Set("Content-Type", "application/json")
			// token_endpoint points back at this same server's /token.
			w.Write([]byte(`{"authorization_endpoint":"` + "https://login.microsoftonline.com/a" +
				`","token_endpoint":"` + serverTokenURL + `","issuer":"x"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	// token_endpoint must resolve to a live handler; run a second server for it so
	// the discovery doc can reference a stable URL captured before ListenAndServe.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":3600}`))
	}))
	defer tokenSrv.Close()
	serverTokenURL = tokenSrv.URL

	marker := &markerRoundTripper{next: http.DefaultTransport, marker: "yes"}
	client := &http.Client{Transport: marker}

	_, _, _, _, err := RefreshExternalIdpToken(
		"old-refresh", server.URL, "", "test-client", "openid", client,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if discoveryHeader != "yes" {
		t.Fatalf("refresh-path discovery did not use the passed client (marker header missing) — leak survives")
	}
}

// serverTokenURL is set by TestRefreshExternalIdpTokenDiscoveryUsesPassedClient
// before the discovery server serves its document (closure capture ordering).
var serverTokenURL string

// ---------------------------------------------------------------------------
// Test hooks
// ---------------------------------------------------------------------------

func TestSetKiroPortalBaseURL(t *testing.T) {
	old := kiroPortalSignInURL
	defer func() { kiroPortalSignInURL = old }()

	SetKiroPortalBaseURL("http://localhost:9999")
	if kiroPortalSignInURL != "http://localhost:9999" {
		t.Errorf("expected 'http://localhost:9999', got '%s'", kiroPortalSignInURL)
	}

	// Empty string should not change
	SetKiroPortalBaseURL("")
	if kiroPortalSignInURL != "http://localhost:9999" {
		t.Errorf("empty string should not change base URL, got '%s'", kiroPortalSignInURL)
	}
}

func TestSetExternalIdpTokenURLFnForTest(t *testing.T) {
	old := externalIdpTokenURLFn
	defer func() { externalIdpTokenURLFn = old }()

	called := false
	SetExternalIdpTokenURLFnForTest(func(issuerURL string, client *http.Client) (string, error) {
		called = true
		return "https://custom.example.com/token", nil
	})

	url, err := externalIdpTokenURLFn("https://login.microsoftonline.com/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("custom function should be called")
	}
	if url != "https://custom.example.com/token" {
		t.Errorf("expected custom URL, got %s", url)
	}

	// Nil should not change
	SetExternalIdpTokenURLFnForTest(nil)
	if !called {
		t.Error("nil should not overwrite function")
	}
}
