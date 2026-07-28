package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeChunkBasicProgression(t *testing.T) {
	prev := ""

	if got := normalizeChunk("abc", &prev); got != "abc" {
		t.Fatalf("expected first chunk to pass through, got %q", got)
	}
	if got := normalizeChunk("abcde", &prev); got != "de" {
		t.Fatalf("expected appended delta, got %q", got)
	}
}

func TestNormalizeChunkPrefixRewindDoesNotReplay(t *testing.T) {
	prev := ""

	_ = normalizeChunk("abcde", &prev)
	if got := normalizeChunk("abc", &prev); got != "" {
		t.Fatalf("expected rewind chunk to be ignored, got %q", got)
	}
	if prev != "abcde" {
		t.Fatalf("expected previous snapshot to remain longest version, got %q", prev)
	}
	if got := normalizeChunk("abcdef", &prev); got != "f" {
		t.Fatalf("expected only unseen suffix after rewind, got %q", got)
	}
}

func TestNormalizeChunkOverlapDelta(t *testing.T) {
	prev := "hello world"

	if got := normalizeChunk("world!!!", &prev); got != "!!!" {
		t.Fatalf("expected overlap suffix delta, got %q", got)
	}
}

func TestParseEventStreamFinishesPendingToolUseOnEOF(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "mcpIdaProMcpStatus",
		"input":     `{"server":"ida-pro-mcp"}`,
	}))

	var toolUses []KiroToolUse
	var completed bool
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
		OnComplete: func(_, _ int) {
			completed = true
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !completed {
		t.Fatalf("expected stream completion callback")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected pending tool use to be emitted on EOF, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID != "toolu_1" || toolUses[0].Name != "mcpIdaProMcpStatus" {
		t.Fatalf("unexpected tool use: %#v", toolUses[0])
	}
	if got := toolUses[0].Input["server"]; got != "ida-pro-mcp" {
		t.Fatalf("expected parsed tool input, got %#v", toolUses[0].Input)
	}
}

func TestParseEventStreamNilCallbackIsNoOp(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "thinking"}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.25}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"name":  "mcpIdaProMcpStatus",
			"input": `{"server":"ida-pro-mcp"}`,
			"stop":  true,
		}),
	}, nil))

	if err := parseEventStream(stream, nil); err != nil {
		t.Fatalf("expected nil callback to be a no-op, got %v", err)
	}
}

func TestParseEventStreamNilCallbackFieldsAreNoOp(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "hello",
	}))

	if err := parseEventStream(stream, &KiroStreamCallback{}); err != nil {
		t.Fatalf("expected empty callback to be a no-op, got %v", err)
	}
}

func TestHandleToolUseEventGeneratesMissingToolUseID(t *testing.T) {
	var toolUses []KiroToolUse
	current := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":"ida-pro-mcp"}`,
		"stop":  true,
	}, nil, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})

	if current != nil {
		t.Fatalf("expected stopped tool use to clear current state")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID == "" {
		t.Fatalf("expected generated tool use id")
	}
	if toolUses[0].Name != "mcpIdaProMcpStatus" {
		t.Fatalf("unexpected tool name: %q", toolUses[0].Name)
	}
}

func TestHandleToolUseEventReplacesGeneratedIDWhenRealIDArrives(t *testing.T) {
	var toolUses []KiroToolUse
	callback := &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	}

	current := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":`,
	}, nil, callback)
	current = handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_real",
		"name":      "mcpIdaProMcpStatus",
		"input":     `"ida-pro-mcp"}`,
		"stop":      true,
	}, current, callback)

	if current != nil {
		t.Fatalf("expected stopped tool use to clear current state")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one completed tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID != "toolu_real" {
		t.Fatalf("expected real tool id to replace generated id, got %q", toolUses[0].ToolUseID)
	}
	if got := toolUses[0].Input["server"]; got != "ida-pro-mcp" {
		t.Fatalf("expected joined tool input, got %#v", toolUses[0].Input)
	}
}

func TestBuildKiroTransportUsesExplicitProxyURL(t *testing.T) {
	transport := buildKiroTransport("http://proxy.local:8080")
	req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://proxy.local:8080")
}

func TestBuildKiroTransportFallsBackToEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://env-proxy.local:2323")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	transport := buildKiroTransport("")
	req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://env-proxy.local:2323")
}

// TestInitKiroHttpClientKeepsShortRestTimeout pins the asymmetry between the two
// clients: the streaming client must have NO total timeout, while REST keeps a short
// one.
//
// http.Client.Timeout covers reading the response body, so any value on the streaming
// client is a hard cap on total stream duration. The old 5-minute cap killed requests
// where the model reasoned for minutes before its first token — and since nothing had
// been written yet, the handler silently retried on another account, billing the same
// generation up to maxAccountRetryAttempts times. Liveness for streams comes from
// ResponseHeaderTimeout plus the per-frame idle watchdog instead.
func TestInitKiroHttpClientKeepsShortRestTimeout(t *testing.T) {
	InitKiroHttpClient("")
	t.Cleanup(func() { InitKiroHttpClient("") })

	streamClient := kiroHttpStore.Load()
	restClient := kiroRestHttpStore.Load()

	if streamClient.Timeout != 0 {
		t.Fatalf("streaming client must have no total timeout, got %s", streamClient.Timeout)
	}
	streamTransport, ok := streamClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", streamClient.Transport)
	}
	if streamTransport.ResponseHeaderTimeout != streamResponseHeaderTimeout {
		t.Fatalf("expected header timeout %s, got %s", streamResponseHeaderTimeout, streamTransport.ResponseHeaderTimeout)
	}
	if restClient.Timeout != 30*time.Second {
		t.Fatalf("expected REST timeout to stay 30s, got %s", restClient.Timeout)
	}
}

// TestGetClientForProxyHasNoStreamTimeout guards the per-account proxy path, which
// builds its own client and would otherwise reintroduce the 5-minute cap for every
// account that has a proxy configured.
func TestGetClientForProxyHasNoStreamTimeout(t *testing.T) {
	client := GetClientForProxy("http://stream-timeout-probe.local:3128")
	if client.Timeout != 0 {
		t.Fatalf("proxied streaming client must have no total timeout, got %s", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", client.Transport)
	}
	if transport.ResponseHeaderTimeout != streamResponseHeaderTimeout {
		t.Fatalf("expected header timeout %s, got %s", streamResponseHeaderTimeout, transport.ResponseHeaderTimeout)
	}
}

func TestResolveAccountProxyURLStrictBlocksWhenRequired(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateRequireProxy(true); err != nil {
		t.Fatalf("set require-proxy: %v", err)
	}
	acc := &config.Account{ID: "a1"} // no per-account proxy, no global proxy
	_, err := ResolveAccountProxyURLStrict(acc)
	if err == nil {
		t.Fatalf("expected error when require-proxy on and no proxy configured")
	}
	if !strings.Contains(err.Error(), "require-proxy") {
		t.Fatalf("error should contain marker, got: %v", err)
	}
}

func TestResolveAccountProxyURLStrictAllowsWithProxy(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateRequireProxy(true); err != nil {
		t.Fatalf("set require-proxy: %v", err)
	}
	acc := &config.Account{ID: "a1", ProxyURL: "socks5h://1.2.3.4:1080"}
	got, err := ResolveAccountProxyURLStrict(acc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "socks5h://1.2.3.4:1080" {
		t.Fatalf("expected account proxy, got %q", got)
	}
}

func TestBuildKiroTransportSetsDialTimeout(t *testing.T) {
	transport := buildKiroTransport("http://proxy.local:8080")
	if transport.DialContext == nil {
		t.Fatalf("expected DialContext to be set for dial timeout")
	}
}

func TestSetPayloadProfileArnForAccountUsesAccountArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: "arn:aws:codewhisperer:profile/stale"}

	setPayloadProfileArnForAccount(payload, &config.Account{ProfileArn: " arn:aws:codewhisperer:profile/current "})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/current" {
		t.Fatalf("expected current account profile ARN, got %q", payload.ProfileArn)
	}
}

func TestSetPayloadProfileArnForAccountPreservesExplicitPayloadArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: " arn:aws:codewhisperer:profile/explicit "}

	setPayloadProfileArnForAccount(payload, &config.Account{})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/explicit" {
		t.Fatalf("expected explicit payload profile ARN to be preserved, got %q", payload.ProfileArn)
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

func awsEventStreamFrame(t *testing.T, eventType string, payload map[string]interface{}) []byte {
	t.Helper()
	return awsEventStreamFrameWithHeaders(t, map[string]string{":event-type": eventType}, payload)
}

func awsEventStreamFrameWithHeaders(t *testing.T, strHeaders map[string]string, payload map[string]interface{}) []byte {
	t.Helper()

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	var headers []byte
	for name, value := range strHeaders {
		v := []byte(value)
		headers = append(headers, byte(len(name)))
		headers = append(headers, []byte(name)...)
		headers = append(headers, byte(7))
		headers = append(headers, byte(len(v)>>8), byte(len(v)))
		headers = append(headers, v...)
	}

	totalLength := 12 + len(headers) + len(payloadBytes) + 4
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	frame = append(frame, headers...)
	frame = append(frame, payloadBytes...)
	frame = append(frame, 0, 0, 0, 0)
	return frame
}

func TestParseEventStreamSurfacesMidStreamException(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "partial"}),
		awsEventStreamFrameWithHeaders(t, map[string]string{
			":message-type":   "exception",
			":exception-type":  "ThrottlingException",
			":content-type":    "application/json",
		}, map[string]interface{}{"message": "Too many requests"}),
	}, nil))

	var text string
	var completed bool
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText:     func(t string, _ bool) { text += t },
		OnComplete: func(_, _ int) { completed = true },
	})
	if err == nil {
		t.Fatal("expected mid-stream exception frame to surface an error, got nil")
	}
	if !isQuotaErrorMessage(err.Error()) {
		t.Fatalf("expected ThrottlingException to classify as quota error, got %q", err.Error())
	}
	if text != "partial" {
		t.Fatalf("expected partial content before exception, got %q", text)
	}
	if completed {
		t.Fatal("OnComplete must not fire when the stream ends in an exception")
	}
}

func TestSelectProxyOverrideWins(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.AddProxyToPool("http://pool:3128"); err != nil {
		t.Fatalf("add pool proxy: %v", err)
	}
	acc := &config.Account{ID: "a1", ProxyURL: "socks5h://override:1080"}
	got, poolKey, err := SelectProxyForAccount(acc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "socks5h://override:1080" {
		t.Fatalf("expected account override, got %q", got)
	}
	if poolKey != "" {
		t.Fatalf("expected empty poolKey for override, got %q", poolKey)
	}
}

func TestSelectProxyRoundRobinSkipsIneligible(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	const (
		unhealthy = "http://unhealthy:3128"
		disabled  = "http://disabled:3128"
		healthy   = "http://healthy:3128"
	)
	for _, u := range []string{unhealthy, disabled, healthy} {
		if err := config.AddProxyToPool(u); err != nil {
			t.Fatalf("add %q: %v", u, err)
		}
	}
	if _, err := config.MarkProxyUnhealthy(unhealthy); err != nil {
		t.Fatalf("mark unhealthy: %v", err)
	}
	if err := config.SetProxyPoolDisabled(disabled, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// The unhealthy entry was just failed, so it is in cooldown and ineligible;
	// the disabled entry is never eligible. Only the healthy one may be picked.
	for i := 0; i < 5; i++ {
		got, poolKey, err := SelectProxyForAccount(&config.Account{ID: "a1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != healthy {
			t.Fatalf("iteration %d: expected healthy proxy, got %q", i, got)
		}
		if poolKey != healthy {
			t.Fatalf("iteration %d: expected poolKey==URL, got %q", i, poolKey)
		}
	}
}

func TestSelectProxyCooldownHalfOpen(t *testing.T) {
	cooldown := int64(config.ProxyUnhealthyCooldown.Seconds())
	now := time.Now().Unix()

	inCooldown := config.PooledProxy{URL: "http://x:3128", Healthy: false, LastFailAt: now - cooldown + 5}
	if proxyPoolEligible(inCooldown, now) {
		t.Fatalf("expected unhealthy entry within cooldown to be ineligible")
	}

	elapsed := config.PooledProxy{URL: "http://x:3128", Healthy: false, LastFailAt: now - cooldown - 1}
	if !proxyPoolEligible(elapsed, now) {
		t.Fatalf("expected unhealthy entry past cooldown to be eligible again")
	}

	disabled := config.PooledProxy{URL: "http://x:3128", Healthy: true, DisabledPermanent: true}
	if proxyPoolEligible(disabled, now) {
		t.Fatalf("expected DisabledPermanent entry to never be eligible")
	}

	healthy := config.PooledProxy{URL: "http://x:3128", Healthy: true}
	if !proxyPoolEligible(healthy, now) {
		t.Fatalf("expected healthy entry to be eligible")
	}
}

func TestSelectProxyGlobalFallback(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateProxySettings("http://global:8080"); err != nil {
		t.Fatalf("set global proxy: %v", err)
	}
	got, poolKey, err := SelectProxyForAccount(&config.Account{ID: "a1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "http://global:8080" {
		t.Fatalf("expected global proxy, got %q", got)
	}
	if poolKey != "" {
		t.Fatalf("expected empty poolKey for global, got %q", poolKey)
	}
}

func TestSelectProxyRequireProxyError(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateRequireProxy(true); err != nil {
		t.Fatalf("set require-proxy: %v", err)
	}
	_, _, err := SelectProxyForAccount(&config.Account{ID: "a1"})
	if err == nil {
		t.Fatalf("expected error when require-proxy on and no proxy configured")
	}
	if !strings.Contains(err.Error(), "require-proxy") {
		t.Fatalf("error should contain marker, got: %v", err)
	}
}

func TestSelectProxyDirect(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	got, poolKey, err := SelectProxyForAccount(&config.Account{ID: "a1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" || poolKey != "" {
		t.Fatalf("expected direct (empty, empty), got (%q, %q)", got, poolKey)
	}
}

func TestShouldSwapProxy(t *testing.T) {
	proxyErr := fmt.Errorf("proxyconnect tcp: dial tcp 1.2.3.4:3128: connect: connection refused")
	httpErr := fmt.Errorf("HTTP 401 from Kiro IDE: unauthorized")

	// proxy transport error + pool key + under cap → swap.
	if !shouldSwapProxy(proxyErr, "http://pool:3128", 0) {
		t.Fatal("expected swap for proxy error with poolKey under cap")
	}
	// nil error (no transport failure) → never swap.
	if shouldSwapProxy(nil, "http://pool:3128", 0) {
		t.Fatal("expected no swap for nil error")
	}
	// non-proxy transport error → never swap.
	if shouldSwapProxy(fmt.Errorf("some other error"), "http://pool:3128", 0) {
		t.Fatal("expected no swap for non-proxy error")
	}
	// HTTP-status error string → never swap (a working proxy must not be marked unhealthy).
	if shouldSwapProxy(httpErr, "http://pool:3128", 0) {
		t.Fatal("expected no swap for HTTP-status error")
	}
	// empty poolKey (override / global / direct) → never swap.
	if shouldSwapProxy(proxyErr, "", 0) {
		t.Fatal("expected no swap for empty poolKey")
	}
	// at cap → stop swapping.
	if shouldSwapProxy(proxyErr, "http://pool:3128", maxProxySwapAttempts) {
		t.Fatal("expected no swap once attempts reach the cap")
	}
}

func TestMaskProxyForLog(t *testing.T) {
	cases := map[string]string{
		"":                                "direct",
		"socks5h://1.2.3.4:1080":          "socks5h://1.2.3.4:1080",
		"http://user:secret@1.2.3.4:8080": "http://user:***@1.2.3.4:8080",
	}
	for in, want := range cases {
		if got := maskProxyForLog(in); got != want {
			t.Fatalf("maskProxyForLog(%q) = %q, want %q", in, got, want)
		}
	}
}
