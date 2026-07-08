package proxy

import (
	"sync"
	"time"
)

// adminAuthGuard is a per-client-IP failed-login throttle for the admin API.
//
// The public /v1/* endpoints are covered by dosGuard, but /admin/api/* is not
// (isGuardedAPIPath excludes it), and the admin password unlocks everything —
// account tokens, key creation, configuration. Without a throttle an attacker can
// brute-force the password online at full request rate. This guard locks out an IP
// for lockout after it accumulates maxFails wrong-password attempts within window,
// and clears the counter on a successful auth.
//
// All state is in-memory and bounded by a background janitor that evicts stale
// entries, so the map cannot be turned into a memory-exhaustion vector.
type adminAuthGuard struct {
	mu       sync.Mutex
	attempts map[string]*adminAttempt
	maxFails int
	window   time.Duration
	lockout  time.Duration
}

type adminAttempt struct {
	fails       int
	windowStart time.Time
	lockedUntil time.Time
}

// newAdminAuthGuard builds a guard with the given policy. maxFails <= 0 disables it.
func newAdminAuthGuard(maxFails int, window, lockout time.Duration) *adminAuthGuard {
	g := &adminAuthGuard{
		attempts: make(map[string]*adminAttempt),
		maxFails: maxFails,
		window:   window,
		lockout:  lockout,
	}
	if maxFails > 0 {
		go g.janitor()
	}
	return g
}

// loadAdminAuthGuard reads the policy from the environment with safe defaults:
// 10 wrong attempts within 5 minutes → locked out for 15 minutes.
func loadAdminAuthGuard() *adminAuthGuard {
	maxFails := envInt("KIRO_ADMIN_MAX_FAILS", 10)
	window := time.Duration(envInt("KIRO_ADMIN_FAIL_WINDOW_SEC", 300)) * time.Second
	lockout := time.Duration(envInt("KIRO_ADMIN_LOCKOUT_SEC", 900)) * time.Second
	return newAdminAuthGuard(maxFails, window, lockout)
}

// locked reports whether ip is currently locked out, and if so for how much longer.
func (g *adminAuthGuard) locked(ip string) (bool, time.Duration) {
	if g == nil || g.maxFails <= 0 || ip == "" {
		return false, 0
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.attempts[ip]
	if a == nil {
		return false, 0
	}
	if now.Before(a.lockedUntil) {
		return true, a.lockedUntil.Sub(now)
	}
	return false, 0
}

// recordFailure registers a wrong-password attempt from ip and locks the IP out
// once it exceeds maxFails within window.
func (g *adminAuthGuard) recordFailure(ip string) {
	if g == nil || g.maxFails <= 0 || ip == "" {
		return
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.attempts[ip]
	if a == nil {
		a = &adminAttempt{windowStart: now}
		g.attempts[ip] = a
	}
	// Reset the counting window if the previous one has elapsed (and we aren't
	// currently in a lockout).
	if now.After(a.windowStart.Add(g.window)) && now.After(a.lockedUntil) {
		a.fails = 0
		a.windowStart = now
	}
	a.fails++
	if a.fails >= g.maxFails {
		a.lockedUntil = now.Add(g.lockout)
		// Start a fresh window after the lockout so the IP isn't permanently locked.
		a.fails = 0
		a.windowStart = a.lockedUntil
	}
}

// recordSuccess clears any failure state for ip after a successful auth.
func (g *adminAuthGuard) recordSuccess(ip string) {
	if g == nil || g.maxFails <= 0 || ip == "" {
		return
	}
	g.mu.Lock()
	delete(g.attempts, ip)
	g.mu.Unlock()
}

// janitor periodically evicts entries that are neither locked nor within an active
// counting window, so the attempts map cannot grow without bound under a flood.
func (g *adminAuthGuard) janitor() {
	interval := g.window + g.lockout
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		g.mu.Lock()
		for ip, a := range g.attempts {
			if now.After(a.lockedUntil) && now.After(a.windowStart.Add(g.window)) {
				delete(g.attempts, ip)
			}
		}
		g.mu.Unlock()
	}
}
