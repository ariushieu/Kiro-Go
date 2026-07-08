package proxy

import (
	"testing"
	"time"
)

func TestIPLimiterUnlimited(t *testing.T) {
	l := newIPLimiter()
	// limit 0 means unlimited: any number of distinct IPs pass.
	for i := 0; i < 100; i++ {
		ip := "10.0.0." + string(rune('0'+i%10))
		if !l.allow("k1", ip, 0) {
			t.Fatalf("limit 0 must always allow, blocked at i=%d", i)
		}
	}
}

func TestIPLimiterEmptyInputsAllow(t *testing.T) {
	l := newIPLimiter()
	if !l.allow("", "1.1.1.1", 1) {
		t.Fatal("empty keyID must allow")
	}
	if !l.allow("k1", "", 1) {
		t.Fatal("empty ip must allow (cannot attribute)")
	}
}

func TestIPLimiterCapsDistinctIPs(t *testing.T) {
	l := newIPLimiter()
	if !l.allow("k1", "1.1.1.1", 2) {
		t.Fatal("first IP should pass")
	}
	if !l.allow("k1", "2.2.2.2", 2) {
		t.Fatal("second IP should pass (at limit)")
	}
	// Already-seen IPs keep working even at the cap.
	if !l.allow("k1", "1.1.1.1", 2) {
		t.Fatal("already-seen IP must keep working")
	}
	// A third distinct IP is over the cap.
	if l.allow("k1", "3.3.3.3", 2) {
		t.Fatal("third distinct IP must be blocked")
	}
	// Rejected IP must not consume a slot, so it stays blocked while others are active.
	if l.allow("k1", "3.3.3.3", 2) {
		t.Fatal("blocked IP must remain blocked (no slot consumed)")
	}
}

func TestIPLimiterPerKeyIsolation(t *testing.T) {
	l := newIPLimiter()
	if !l.allow("k1", "1.1.1.1", 1) {
		t.Fatal("k1 first IP should pass")
	}
	// Different key has its own independent budget.
	if !l.allow("k2", "9.9.9.9", 1) {
		t.Fatal("k2 first IP should pass independently of k1")
	}
	if l.allow("k1", "2.2.2.2", 1) {
		t.Fatal("k1 second IP must be blocked")
	}
}

func TestIPLimiterPrunesStaleIPs(t *testing.T) {
	l := newIPLimiter()
	// Seed an IP with a lastSeen far in the past so it is pruned on the next call.
	l.keys["k1"] = map[string]time.Time{
		"1.1.1.1": time.Now().Add(-2 * ipLimitWindow),
	}
	// A new IP should get the freed slot because the stale one is pruned.
	if !l.allow("k1", "2.2.2.2", 1) {
		t.Fatal("stale IP should have been pruned, freeing a slot")
	}
	if _, ok := l.keys["k1"]["1.1.1.1"]; ok {
		t.Fatal("stale IP should have been removed")
	}
}

func TestIPLimiterForget(t *testing.T) {
	l := newIPLimiter()
	l.allow("k1", "1.1.1.1", 1)
	if l.allow("k1", "2.2.2.2", 1) {
		t.Fatal("second IP should be blocked before forget")
	}
	l.forget("k1")
	if !l.allow("k1", "2.2.2.2", 1) {
		t.Fatal("after forget the IP set should be cleared")
	}
}

func TestIPMatchesAllowlistExact(t *testing.T) {
	list := []string{"203.0.113.7", "2001:db8::1"}
	if !ipMatchesAllowlist("203.0.113.7", list) {
		t.Fatal("exact IPv4 should match")
	}
	if !ipMatchesAllowlist("2001:db8::1", list) {
		t.Fatal("exact IPv6 should match")
	}
	if ipMatchesAllowlist("203.0.113.8", list) {
		t.Fatal("non-listed IP must not match")
	}
}

func TestIPMatchesAllowlistCIDR(t *testing.T) {
	list := []string{"203.0.113.0/24", "2001:db8::/32"}
	if !ipMatchesAllowlist("203.0.113.55", list) {
		t.Fatal("IP inside CIDR range should match")
	}
	if ipMatchesAllowlist("203.0.114.1", list) {
		t.Fatal("IP outside CIDR range must not match")
	}
	if !ipMatchesAllowlist("2001:db8:abcd::5", list) {
		t.Fatal("IPv6 inside CIDR should match")
	}
}

func TestIPMatchesAllowlistEdgeCases(t *testing.T) {
	if ipMatchesAllowlist("1.1.1.1", nil) {
		t.Fatal("empty allowlist must not match")
	}
	if ipMatchesAllowlist("not-an-ip", []string{"1.1.1.1"}) {
		t.Fatal("unparseable client IP must not match")
	}
	// One malformed entry must not disable the whole list.
	if !ipMatchesAllowlist("1.1.1.1", []string{"garbage", "1.1.1.1"}) {
		t.Fatal("valid entry after a malformed one should still match")
	}
	// Whitespace around entries is tolerated.
	if !ipMatchesAllowlist("1.1.1.1", []string{"  1.1.1.1  "}) {
		t.Fatal("entry with surrounding whitespace should match")
	}
}

func TestSanitizeIPAllowlist(t *testing.T) {
	in := []string{" 203.0.113.7 ", "203.0.113.0/24", "garbage", "", "203.0.113.7", "999.1.1.1"}
	got := sanitizeIPAllowlist(in)
	want := []string{"203.0.113.7", "203.0.113.0/24"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("at %d expected %q, got %q (full: %v)", i, want[i], got[i], got)
		}
	}
}
