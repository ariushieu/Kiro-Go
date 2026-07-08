package proxy

import (
	"testing"
	"time"
)

func TestRPMThrottleUnlimited(t *testing.T) {
	th := newRPMThrottle()
	for i := 0; i < 100; i++ {
		if w := th.reserve("k", 0); w != 0 {
			t.Fatalf("limit 0 should never wait, got %v", w)
		}
	}
}

func TestRPMThrottleFirstRequestImmediate(t *testing.T) {
	th := newRPMThrottle()
	if w := th.reserve("k", 60); w != 0 {
		t.Fatalf("first request should not wait, got %v", w)
	}
}

// An idle key banks tokens up to the burst size, so a sudden burst is served
// immediately up to `limit` requests — matching the user's "idle then burst" case.
func TestRPMThrottleAllowsBurstUpToLimit(t *testing.T) {
	th := newRPMThrottle()
	// Seed the bucket, then simulate it sitting idle long enough to refill fully.
	th.reserve("k", 60)
	th.buckets["k"].last = time.Now().Add(-2 * time.Minute) // plenty of idle time
	th.buckets["k"].tokens = 0

	// First 60 requests in the burst should be immediate (bucket refilled to 60).
	for i := 0; i < 60; i++ {
		if w := th.reserve("k", 60); w != 0 {
			t.Fatalf("burst request %d should be immediate, got %v", i, w)
		}
	}
	// The 61st request exhausts the bucket and must be delayed ~1s (60rpm => 1s/token).
	w := th.reserve("k", 60)
	if w <= 0 {
		t.Fatalf("request beyond burst should be delayed, got %v", w)
	}
	if w > 2*time.Second {
		t.Fatalf("expected ~1s delay at 60rpm, got %v", w)
	}
}

// Sustained spamming past the burst grows the delay roughly linearly.
func TestRPMThrottleDelaysSustainedSpam(t *testing.T) {
	th := newRPMThrottle()
	th.reserve("k", 60) // start with near-full bucket

	// Drain the burst instantly.
	for i := 0; i < 60; i++ {
		th.reserve("k", 60)
	}
	w1 := th.reserve("k", 60)
	w2 := th.reserve("k", 60)
	if w2 <= w1 {
		t.Fatalf("each extra spammed request should wait longer: w1=%v w2=%v", w1, w2)
	}
}

func TestRPMThrottleWaitCapped(t *testing.T) {
	th := newRPMThrottle()
	th.reserve("k", 60)
	// Hammer well past the cap; no single reserve should exceed the max wait.
	var maxSeen time.Duration
	for i := 0; i < 10000; i++ {
		if w := th.reserve("k", 60); w > maxSeen {
			maxSeen = w
		}
	}
	if maxSeen > rpmThrottleMaxWait {
		t.Fatalf("wait should be capped at %v, saw %v", rpmThrottleMaxWait, maxSeen)
	}
}

func TestRPMThrottleKeysIndependent(t *testing.T) {
	th := newRPMThrottle()
	// Drain key A completely.
	th.reserve("a", 1)
	th.reserve("a", 1) // second within the minute will be delayed
	// Key B is untouched and should be immediate.
	if w := th.reserve("b", 1); w != 0 {
		t.Fatalf("independent key B should not be delayed by key A, got %v", w)
	}
}
