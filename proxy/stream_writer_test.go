package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockstepWriter is a ResponseWriter that records writes under its own lock, so the
// race detector can prove streamWriter serializes the handler goroutine against the
// heartbeat goroutine.
type lockstepWriter struct {
	mu     sync.Mutex
	chunks []string
	header http.Header
}

func newLockstepWriter() *lockstepWriter {
	return &lockstepWriter{header: make(http.Header)}
}

func (l *lockstepWriter) Header() http.Header { return l.header }
func (l *lockstepWriter) WriteHeader(int)     {}
func (l *lockstepWriter) Flush()              {}

func (l *lockstepWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.chunks = append(l.chunks, string(p))
	return len(p), nil
}

func (l *lockstepWriter) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.chunks, "")
}

// TestStreamWriterDoesNotPingBeforeArm is the guard for the status-code trap: a
// heartbeat writes bytes, and bytes commit the 200 status line. If a ping escaped
// before the handler decided it was really streaming, every later error path
// (503 empty pool, 504 stalled upstream) would silently degrade to a 200 carrying a
// truncated SSE body.
func TestStreamWriterDoesNotPingBeforeArm(t *testing.T) {
	inner := newLockstepWriter()
	sw := newStreamWriter(inner, inner)
	defer sw.stop()

	// Force the idle condition, then drive the heartbeat check directly rather than
	// sleeping for a real interval.
	markIdle(sw)
	sw.pingIfIdle()

	if got := inner.joined(); got != "" {
		t.Fatalf("expected no bytes before arm, got %q", got)
	}
	if sw.wroteAnything() {
		t.Fatalf("wroteAnything must stay false before arm")
	}
}

func TestStreamWriterPingsOnceArmedAndIdle(t *testing.T) {
	inner := newLockstepWriter()
	sw := newStreamWriter(inner, inner)
	defer sw.stop()

	sw.arm()
	markIdle(sw)
	if !sw.pingIfIdle() {
		t.Fatalf("pingIfIdle reported a broken connection on a healthy writer")
	}

	if got := inner.joined(); got != ": ping\n\n" {
		t.Fatalf("expected one SSE comment, got %q", got)
	}
	if !sw.wroteAnything() {
		t.Fatalf("a delivered ping commits the status line, so wroteAnything must be true")
	}
}

// TestStreamWriterSkipsPingWhenRecentlyActive covers the mid-stream case: while
// tokens are flowing the connection needs no synthetic traffic, and extra comments
// would just add noise.
func TestStreamWriterSkipsPingWhenRecentlyActive(t *testing.T) {
	inner := newLockstepWriter()
	sw := newStreamWriter(inner, inner)
	defer sw.stop()

	sw.arm()
	if _, err := sw.Write([]byte("event: x\ndata: {}\n\n")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	sw.pingIfIdle()

	if strings.Contains(inner.joined(), ": ping") {
		t.Fatalf("expected no ping right after real content, got %q", inner.joined())
	}
}

// TestStreamWriterPingsAgainAfterMidStreamSilence pins why the heartbeat is driven by
// time-since-last-write instead of stopping at the first real write: a model can go
// quiet again between two tool calls, and that gap needs covering too.
func TestStreamWriterPingsAgainAfterMidStreamSilence(t *testing.T) {
	inner := newLockstepWriter()
	sw := newStreamWriter(inner, inner)
	defer sw.stop()

	sw.arm()
	if _, err := sw.Write([]byte("event: x\ndata: {}\n\n")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	// The model stops producing mid-answer.
	markIdle(sw)
	sw.pingIfIdle()

	if !strings.Contains(inner.joined(), ": ping") {
		t.Fatalf("expected a ping after mid-stream silence, got %q", inner.joined())
	}
}

// TestStreamWriterConcurrentWritesAreSerialized is meaningful under `go test -race`:
// the heartbeat goroutine and the request goroutine share one ResponseWriter, and
// unsynchronized writes would both race and interleave partial SSE frames.
func TestStreamWriterConcurrentWritesAreSerialized(t *testing.T) {
	inner := newLockstepWriter()
	sw := newStreamWriter(inner, inner)
	sw.arm()
	sw.startHeartbeat()
	defer sw.stop()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sw.Write([]byte("data: chunk\n\n"))
			sw.Flush()
			sw.pingIfIdle()
		}()
	}
	wg.Wait()

	// Every frame must survive intact; interleaving would corrupt the delimiters.
	body := inner.joined()
	if strings.Count(body, "data: chunk\n\n") != 50 {
		t.Fatalf("expected 50 intact frames, got %q", body)
	}
}

func TestStreamWriterStopIsIdempotent(t *testing.T) {
	inner := newLockstepWriter()
	sw := newStreamWriter(inner, inner)
	sw.startHeartbeat()
	sw.stop()
	sw.stop() // must not panic on a second close
}

// TestStreamWriterReportsBrokenConnection makes the heartbeat goroutine exit instead
// of spinning against a dead socket.
func TestStreamWriterReportsBrokenConnection(t *testing.T) {
	sw := newStreamWriter(errWriter{}, nopFlusher{})
	defer sw.stop()

	sw.arm()
	markIdle(sw)
	if sw.pingIfIdle() {
		t.Fatalf("expected pingIfIdle to report failure when the write errors")
	}
}

type errWriter struct{}

func (errWriter) Header() http.Header       { return make(http.Header) }
func (errWriter) WriteHeader(int)           {}
func (errWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }

type nopFlusher struct{}

func (nopFlusher) Flush() {}

// setStreamIdleTimeout shrinks the idle budget so watchdog tests finish in
// milliseconds instead of minutes. Returns a restore func.
func setStreamIdleTimeout(d time.Duration) func() {
	previous := streamIdleTimeout
	streamIdleTimeout = d
	return func() { streamIdleTimeout = previous }
}

// markIdle backdates the last-write clock so pingIfIdle sees a stale stream, taking
// the same lock the heartbeat goroutine uses.
func markIdle(sw *streamWriter) {
	sw.mu.Lock()
	sw.lastWrite = time.Now().Add(-time.Hour)
	sw.mu.Unlock()
}

// TestWatchStreamIdleCancelsWithCause is the core of the timeout fix: the streaming
// client has no total timeout, so a silent socket must be detected here or the request
// hangs indefinitely. The cause must be errStreamIdle rather than a bare
// context.Canceled, otherwise failover cannot tell a stall from a client hang-up.
func TestWatchStreamIdleCancelsWithCause(t *testing.T) {
	restore := setStreamIdleTimeout(60 * time.Millisecond)
	defer restore()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	// A reader that never produces bytes models an upstream that went silent.
	tracker := newActivityReader(strings.NewReader(""))
	tracker.last.Store(time.Now().Add(-time.Hour).UnixNano())
	stop := watchStreamIdle(tracker, cancel)
	defer stop()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("watchdog did not cancel a silent stream")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, errStreamIdle) {
		t.Fatalf("expected errStreamIdle cause, got %v", cause)
	}
}

// TestWatchStreamIdleLeavesActiveStreamAlone is the other half: a long answer that
// keeps producing must never be cut off, which is exactly what the old total timeout
// got wrong.
func TestWatchStreamIdleLeavesActiveStreamAlone(t *testing.T) {
	restore := setStreamIdleTimeout(80 * time.Millisecond)
	defer restore()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)

	tracker := newActivityReader(strings.NewReader(""))
	stop := watchStreamIdle(tracker, cancel)
	defer stop()

	// Simulate steady output for several idle windows.
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		tracker.touch()
		time.Sleep(20 * time.Millisecond)
	}

	if err := ctx.Err(); err != nil {
		t.Fatalf("watchdog cancelled an actively producing stream: %v (cause %v)", err, context.Cause(ctx))
	}
}

func TestActivityReaderTracksProgress(t *testing.T) {
	tracker := newActivityReader(strings.NewReader("hello"))
	tracker.last.Store(time.Now().Add(-time.Hour).UnixNano())

	buf := make([]byte, 5)
	if _, err := io.ReadFull(tracker, buf); err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if tracker.idleFor() > time.Second {
		t.Fatalf("a successful read must reset the idle clock, idleFor=%s", tracker.idleFor())
	}
}

// TestIsClientGoneError separates "abandoned" from "upstream broke". Only the former
// must skip the retry loop; misclassifying a real upstream failure here would stop
// failover from rotating to a healthy account.
func TestIsClientGoneError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"client cancelled", context.Canceled, true},
		{"idle watchdog", errStreamIdle, true},
		{"wrapped idle", errors.New("read tcp: " + errStreamIdle.Error()), true},
		{"upstream 500", errors.New("HTTP 500 from Kiro IDE: high load"), false},
		{"quota", errors.New("quota exhausted on AmazonQ"), false},
		{"deadline exceeded is a stall, not a hang-up", context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClientGoneError(tc.err); got != tc.want {
				t.Fatalf("isClientGoneError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsUpstreamTimeoutMessage covers the errors that must earn a short cooldown
// rather than falling through to the generic error path (or, worse, a ban).
func TestIsUpstreamTimeoutMessage(t *testing.T) {
	timeouts := []string{
		"context deadline exceeded",
		`Post "https://q.us-east-1.amazonaws.com": context deadline exceeded (Client.Timeout exceeded while reading body)`,
		"net/http: timeout awaiting response headers",
	}
	for _, msg := range timeouts {
		if !isUpstreamTimeoutMessage(msg) {
			t.Fatalf("expected %q to be classified as an upstream timeout", msg)
		}
	}

	notTimeouts := []string{
		"HTTP 500 from Kiro IDE: high load",
		"quota exhausted on AmazonQ",
		"proxyconnect tcp: connection refused",
	}
	for _, msg := range notTimeouts {
		if isUpstreamTimeoutMessage(msg) {
			t.Fatalf("did not expect %q to be classified as an upstream timeout", msg)
		}
	}
}

// TestUpstreamTimeoutIsMaskedAsMaintenance keeps our internal timeout wording out of
// customer-facing responses, matching how the other pool-internal faults are handled.
func TestUpstreamTimeoutIsMaskedAsMaintenance(t *testing.T) {
	status, msg := clientFacingUpstreamError(errors.New("context deadline exceeded (Client.Timeout exceeded while reading body)"))
	if status != http.StatusGatewayTimeout {
		t.Fatalf("expected 504 for an upstream stall, got %d", status)
	}
	if msg != noAccountsClientMessage() {
		t.Fatalf("expected the maintenance message, got %q", msg)
	}
}

// TestStreamHandlerSetsAccelBufferingHeader pins the nginx opt-out. Without it a
// location with proxy_buffering on would hold both tokens and heartbeats, defeating
// the whole fix while looking fine locally.
func TestStreamHandlerSetsAccelBufferingHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newStreamWriter(rec, rec)
	defer sw.stop()

	// The handlers set this before any write; assert the value the handlers use.
	sw.Header().Set("X-Accel-Buffering", "no")
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("expected X-Accel-Buffering=no to reach the underlying writer, got %q", got)
	}
}
