package proxy

import (
	"sync"
	"time"
)

// rpmThrottleMaxWait caps how long a single request may be delayed. Beyond this,
// the request proceeds anyway rather than holding a connection open indefinitely
// (the client would otherwise time out). This protects against one key queuing up
// an unbounded backlog of waiting requests.
const rpmThrottleMaxWait = 60 * time.Second

// rpmThrottle paces requests per API key using a token-bucket algorithm. It does
// NOT reject requests; when a key runs out of tokens, the next request is delayed
// just long enough to refill one token, so traffic is smoothed instead of bursting
// all at once.
//
// Burst behaviour: each key accumulates up to `limit` tokens while idle, so a key
// that has been quiet can immediately serve a burst of up to `limit` requests, then
// settles into the steady per-minute pace. This matches how OpenAI-style limits
// feel to clients while never returning an error.
//
// State is in-memory only and keyed by API key ID, so it is bounded by the number
// of configured keys and needs no background cleanup.
type rpmThrottle struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

type tokenBucket struct {
	tokens float64   // currently available tokens (can briefly go below 0 to schedule waits)
	last   time.Time // last time tokens were refilled
}

func newRPMThrottle() *rpmThrottle {
	return &rpmThrottle{buckets: make(map[string]*tokenBucket)}
}

// reserve accounts for one request against keyID and returns how long the caller
// should wait before proceeding. A limit of 0 (or negative) means unlimited and
// always returns 0. The returned wait is capped at rpmThrottleMaxWait.
func (l *rpmThrottle) reserve(keyID string, limit int) time.Duration {
	if limit <= 0 || keyID == "" {
		return 0
	}
	ratePerSec := float64(limit) / 60.0 // tokens refilled per second
	burst := float64(limit)             // max tokens a key can bank while idle
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[keyID]
	if b == nil {
		// First request for this key starts with a full bucket and is never delayed.
		l.buckets[keyID] = &tokenBucket{tokens: burst - 1, last: now}
		return 0
	}

	// Refill tokens for the time elapsed since the last request, capped at burst.
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * ratePerSec
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now

	// Spend one token. If that drops us below zero, the deficit determines the wait.
	b.tokens--
	if b.tokens >= 0 {
		return 0
	}
	wait := time.Duration(-b.tokens / ratePerSec * float64(time.Second))
	if wait > rpmThrottleMaxWait {
		wait = rpmThrottleMaxWait
	}
	return wait
}

// refund returns one previously-reserved token to keyID's bucket, capped at burst.
// Called when a request that reserved a slot is rejected before actually waiting
// (e.g. the per-key in-flight cap is hit), so the throttle's effective rate does
// not silently drift below the configured limit.
func (l *rpmThrottle) refund(keyID string, limit int) {
	if limit <= 0 || keyID == "" {
		return
	}
	burst := float64(limit)
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[keyID]
	if b == nil {
		return
	}
	b.tokens++
	if b.tokens > burst {
		b.tokens = burst
	}
}
