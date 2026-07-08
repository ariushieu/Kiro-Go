package proxy

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// dosGuard bundles the application-layer DoS protections that sit in front of the
// expensive public API endpoints:
//
//   - a global cap on the number of in-flight guarded requests (bounds goroutines
//     and upstream connections for the whole server);
//   - a per-client-IP token-bucket REJECT limiter (anti brute-force key guessing and
//     anti flood — unlike the per-key RPM throttle this rejects immediately instead of
//     delaying, because an unauthenticated/abusive flood must not hold connections open);
//   - a per-API-key cap on the number of requests that may be simultaneously SLEEPING
//     in the RPM delay (so a single key cannot build an unbounded backlog of delayed
//     requests that pin goroutines for up to rpmThrottleMaxWait each);
//   - a request body size limit (applied by the caller via http.MaxBytesReader).
//
// Everything is in-memory and self-contained (no external rate-limit dependency), to
// match the hand-rolled rpmThrottle. The per-IP map is bounded by a background janitor
// that evicts idle buckets, so the map itself cannot be turned into a memory-exhaustion
// vector.
type dosGuard struct {
	maxBodyBytes int64
	trustProxy   bool
	// trustedHops is how many reverse-proxy hops sit in front of us (>=1 when
	// trustProxy). The client IP is taken this many entries from the RIGHT of
	// X-Forwarded-For — see clientIP for why left-most is unsafe.
	trustedHops int

	// Global concurrency: a buffered channel used as a counting semaphore.
	globalSlots chan struct{}

	// Per-IP reject limiter.
	ipRPM   int
	ipMu    sync.Mutex
	ipState map[string]*ipBucket

	// Per-key in-flight (sleeping) cap.
	keyMaxWaiters int
	keyMu         sync.Mutex
	keyWaiters    map[string]int
}

type ipBucket struct {
	tokens float64
	last   time.Time
}

// dosGuardConfig is resolved once at startup from environment variables. Zero/negative
// values disable the corresponding protection so operators can opt out per-knob.
type dosGuardConfig struct {
	MaxBodyBytes  int64
	MaxConcurrent int
	IPRPM         int
	KeyMaxWaiters int
	TrustProxy    bool
	TrustedHops   int
}

func envInt(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envInt64(name string, def int64) int64 {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envBool(name string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// loadDosGuardConfig reads the tunables from the environment with safe defaults.
func loadDosGuardConfig() dosGuardConfig {
	return dosGuardConfig{
		MaxBodyBytes:  envInt64("KIRO_MAX_BODY_BYTES", 10*1024*1024), // 10 MiB
		MaxConcurrent: envInt("KIRO_MAX_CONCURRENT", 256),
		IPRPM:         envInt("KIRO_IP_RPM", 120),
		KeyMaxWaiters: envInt("KIRO_PER_KEY_INFLIGHT", 8),
		TrustProxy:    envBool("KIRO_TRUST_PROXY", false),
		TrustedHops:   envInt("KIRO_TRUSTED_PROXY_HOPS", 1),
	}
}

func newDosGuard(cfg dosGuardConfig) *dosGuard {
	hops := cfg.TrustedHops
	if hops < 1 {
		hops = 1
	}
	g := &dosGuard{
		maxBodyBytes:  cfg.MaxBodyBytes,
		trustProxy:    cfg.TrustProxy,
		trustedHops:   hops,
		ipRPM:         cfg.IPRPM,
		ipState:       make(map[string]*ipBucket),
		keyMaxWaiters: cfg.KeyMaxWaiters,
		keyWaiters:    make(map[string]int),
	}
	if cfg.MaxConcurrent > 0 {
		g.globalSlots = make(chan struct{}, cfg.MaxConcurrent)
	}
	if cfg.IPRPM > 0 {
		go g.janitor()
	}
	return g
}

// clientIP returns the best-effort client IP for rate-limiting and IP allow-listing.
//
// When trustProxy is false (default — VPS exposed directly), only RemoteAddr is
// trusted, because any client can forge X-Forwarded-For / X-Real-IP and thereby
// escape per-IP limits or frame another IP.
//
// When trustProxy is true (deployment sits behind a reverse proxy), the client IP is
// taken trustedHops entries from the RIGHT of X-Forwarded-For — NOT the left-most.
// The left-most entry is attacker-controlled: common reverse proxies (e.g. nginx's
// $proxy_add_x_forwarded_for) APPEND the connecting IP rather than overwrite the
// header, so a client that sends "X-Forwarded-For: <spoofed>" produces
// "X-Forwarded-For: <spoofed>, <real-ip>". Trusting the left-most value would let an
// attacker rotate it to bypass the per-IP limiter entirely, or set it to an
// allow-listed IP to defeat an API key's IPAllowlist. Each trusted proxy in the chain
// appends exactly one entry, so the entry trustedHops from the right is the address
// the outermost trusted proxy actually observed. Operators with N chained proxies set
// KIRO_TRUSTED_PROXY_HOPS=N. Left-most is only safe when the proxy is guaranteed to
// REPLACE the header, which cannot be assumed in general.
func (g *dosGuard) clientIP(r *http.Request) string {
	if g.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			ips := make([]string, 0, len(parts))
			for _, p := range parts {
				if s := strings.TrimSpace(p); s != "" {
					ips = append(ips, s)
				}
			}
			if len(ips) > 0 {
				hops := g.trustedHops
				if hops < 1 {
					hops = 1
				}
				idx := len(ips) - hops
				if idx < 0 {
					idx = 0
				}
				return ips[idx]
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			return xr
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// allowIP reports whether a request from ip may proceed. It uses a token bucket
// (capacity == ipRPM, refill == ipRPM per minute) and REJECTS (returns false) when
// the bucket is empty, rather than delaying. ipRPM <= 0 disables the limiter.
func (g *dosGuard) allowIP(ip string) bool {
	if g.ipRPM <= 0 || ip == "" {
		return true
	}
	ratePerSec := float64(g.ipRPM) / 60.0
	burst := float64(g.ipRPM)
	now := time.Now()

	g.ipMu.Lock()
	defer g.ipMu.Unlock()

	b := g.ipState[ip]
	if b == nil {
		// First request from this IP: full bucket, spend one.
		g.ipState[ip] = &ipBucket{tokens: burst - 1, last: now}
		return true
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * ratePerSec
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// acquireGlobal reserves one of the global concurrency slots without blocking.
// Returns (release, true) when a slot was taken; the caller MUST invoke release
// exactly once. Returns (nil, false) when the server is at capacity. A nil/disabled
// semaphore always admits.
func (g *dosGuard) acquireGlobal() (func(), bool) {
	if g.globalSlots == nil {
		return func() {}, true
	}
	select {
	case g.globalSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-g.globalSlots }) }, true
	default:
		return nil, false
	}
}

// enterKeyWait registers that a request for keyID is about to SLEEP in the RPM delay.
// Returns false when keyID already has keyMaxWaiters requests sleeping, signalling the
// caller to reject instead of piling on another held connection. On true, the caller
// MUST call leaveKeyWait(keyID) once the sleep completes. A keyMaxWaiters <= 0 disables
// the cap, and an empty keyID is never capped.
func (g *dosGuard) enterKeyWait(keyID string) bool {
	if g.keyMaxWaiters <= 0 || keyID == "" {
		return true
	}
	g.keyMu.Lock()
	defer g.keyMu.Unlock()
	if g.keyWaiters[keyID] >= g.keyMaxWaiters {
		return false
	}
	g.keyWaiters[keyID]++
	return true
}

func (g *dosGuard) leaveKeyWait(keyID string) {
	if g.keyMaxWaiters <= 0 || keyID == "" {
		return
	}
	g.keyMu.Lock()
	defer g.keyMu.Unlock()
	if n := g.keyWaiters[keyID]; n <= 1 {
		delete(g.keyWaiters, keyID)
	} else {
		g.keyWaiters[keyID] = n - 1
	}
}

// janitor periodically evicts idle per-IP buckets so the ipState map can't grow
// without bound under a spoofed/rotating-IP flood. A bucket is idle once enough time
// has passed for it to have fully refilled (so dropping it loses no state a fresh
// bucket wouldn't recreate identically).
func (g *dosGuard) janitor() {
	idleFor := 5 * time.Minute
	ticker := time.NewTicker(idleFor)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-idleFor)
		g.ipMu.Lock()
		for ip, b := range g.ipState {
			if b.last.Before(cutoff) {
				delete(g.ipState, ip)
			}
		}
		g.ipMu.Unlock()
	}
}

// isGuardedAPIPath reports whether a path is one of the public, upstream-hitting API
// endpoints that the DoS guard should protect. Admin, health and static paths are
// excluded: admin is password-gated and not publicly shared, and health/static are
// cheap and must stay reachable for liveness probes.
func isGuardedAPIPath(path string) bool {
	switch path {
	case "/v1/messages", "/messages", "/anthropic/v1/messages",
		"/v1/messages/count_tokens", "/messages/count_tokens",
		"/v1/chat/completions", "/chat/completions",
		"/v1/responses", "/responses",
		"/v1/stats":
		return true
	}
	return false
}

// isOpenAIStylePath reports whether a guarded path expects OpenAI-shaped error bodies.
// The remaining guarded paths use Claude-shaped errors.
func isOpenAIStylePath(path string) bool {
	switch path {
	case "/v1/chat/completions", "/chat/completions", "/v1/responses", "/responses":
		return true
	}
	return false
}
