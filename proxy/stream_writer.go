package proxy

import (
	"net/http"
	"sync"
	"time"
)

// streamWriter wraps an SSE ResponseWriter so a background heartbeat can share it
// with the request goroutine.
//
// Two problems it solves:
//
//  1. While the upstream model is still reasoning, nothing is written for as long as
//     it takes to produce the first token. Intermediaries treat a silent connection
//     as dead — Cloudflare drops it at ~100s — and clients apply their own idle
//     timeouts, so a genuinely healthy long request fails with no error anywhere.
//     Emitting an SSE comment (": ping") resets every idle timer along the path and
//     is ignored by conforming SSE parsers, so clients need no changes.
//
//  2. That heartbeat runs on its own goroutine, so every write must be serialized.
//     Concurrent writes to an http.ResponseWriter are a data race, and interleaved
//     bytes would corrupt the frame a client is mid-way through parsing.
//
// It implements both http.ResponseWriter and http.Flusher, so it substitutes for the
// original writer without touching the handlers' many sendSSE call sites. The
// heartbeat is driven by time since the last real write rather than stopping at the
// first one, because silence also happens mid-stream — a model reasoning between two
// tool calls can go quiet for just as long as it does before its first token.
type streamWriter struct {
	inner   http.ResponseWriter
	flusher http.Flusher

	mu sync.Mutex
	// lastWrite is when real (non-heartbeat) content was last written. Guarded by mu.
	lastWrite time.Time

	// armed gates the heartbeat. Until the handler commits to streaming, a ping
	// would implicitly write a 200 header and make later error paths unable to set
	// a real status (503 for an empty pool, 504 for a stalled upstream). Guarded by mu.
	armed bool
	// sentBytes records whether anything at all (heartbeat or content) has reached
	// the client, i.e. whether the status line is already locked in.
	sentBytes bool

	stopOnce sync.Once
	done     chan struct{}
}

func newStreamWriter(w http.ResponseWriter, flusher http.Flusher) *streamWriter {
	return &streamWriter{
		inner:     w,
		flusher:   flusher,
		lastWrite: time.Now(),
		done:      make(chan struct{}),
	}
}

func (s *streamWriter) Header() http.Header { return s.inner.Header() }

func (s *streamWriter) WriteHeader(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inner.WriteHeader(status)
}

func (s *streamWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastWrite = time.Now()
	s.sentBytes = true
	return s.inner.Write(p)
}

func (s *streamWriter) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flusher.Flush()
}

// arm allows the heartbeat to start emitting. Call it only once the handler is
// committed to a 200 streaming response — that is, right before the upstream call
// whose latency the heartbeat exists to cover. Any error path reached BEFORE arming
// can still send a proper HTTP status.
func (s *streamWriter) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
	s.lastWrite = time.Now()
}

// wroteAnything reports whether the response status is already committed, so callers
// know whether an error can still be delivered as an HTTP status code.
func (s *streamWriter) wroteAnything() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sentBytes
}

// startHeartbeat pings whenever the stream has been quiet for a full interval, until
// stop is called. Pings only begin after arm. Safe to call once per stream.
func (s *streamWriter) startHeartbeat() {
	go func() {
		// Tick faster than the interval so a ping lands close to the moment the
		// connection actually goes quiet, rather than up to 2x late.
		ticker := time.NewTicker(streamHeartbeatInterval / 3)
		defer ticker.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
				if !s.pingIfIdle() {
					// The connection is gone; nothing left to keep alive.
					return
				}
			}
		}
	}()
}

// pingIfIdle writes a heartbeat comment when the stream has been quiet for at least
// one interval. It reports false only when the connection is broken, so the caller
// can stop pinging.
func (s *streamWriter) pingIfIdle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Not armed yet: the handler may still need to send a real HTTP error status,
	// which writing here would foreclose.
	if !s.armed {
		return true
	}
	if time.Since(s.lastWrite) < streamHeartbeatInterval {
		return true
	}
	// A comment line carries no event and no data, so SSE clients discard it.
	// Deliberately does not advance lastWrite past its own cadence: heartbeats are
	// not real content, but they do reset the idle window they are meant to fill.
	if _, err := s.inner.Write([]byte(": ping\n\n")); err != nil {
		return false
	}
	s.flusher.Flush()
	s.sentBytes = true
	s.lastWrite = time.Now()
	return true
}

func (s *streamWriter) stop() {
	s.stopOnce.Do(func() { close(s.done) })
}
