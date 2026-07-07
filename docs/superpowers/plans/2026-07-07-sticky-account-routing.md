# Sticky Account Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pin each conversation to the account that first served it (for ~5 min) so consecutive turns hit the same account's warm upstream prompt cache, cutting credit burn.

**Architecture:** A per-request sticky key is derived from the stable request prefix (system + first user message), independent of the cache profile. An in-memory pin store on `AccountPool` maps `key → accountID` with a 5-min TTL. On attempt 0 the retry loop tries the pinned account first (if still usable); attempts 1–2 and re-pinning on success are unchanged failover. Nothing persists.

**Tech Stack:** Go stdlib (`crypto/sha256`, `sync`, `time`), `github.com/google/uuid` (already present). No new dependencies.

## Global Constraints

- Never write config on the request hot path — the pin store is in-memory only, no `Save()`.
- Account selection always goes through the pool, never by iterating `config.GetAccounts()` in request handling.
- Pin biases attempt 0 only; attempts 1–2 use `GetNextForModelExcluding` unchanged.
- Comments may be English or Chinese; match the surrounding file.
- Tests live beside their code (`*_test.go`).
- Reuse `canonicalizeCacheValue` / `writeHashChunk` / `isAnthropicBillingHeaderBlock` from `proxy/cache_tracker.go` — do not re-implement hashing or billing-header detection.

---

### Task 1: Sticky key derivation (`proxy`)

Derive a stable `[32]byte` key from the request prefix, for both Claude and OpenAI shapes. Zero-value key means "no key → skip stickiness". Billing-header text blocks are filtered so the key is stable across `x-anthropic-billing-header` drift.

**Files:**
- Create: `proxy/sticky_key.go`
- Test: `proxy/sticky_key_test.go`

**Interfaces:**
- Consumes: `canonicalizeCacheValue(interface{}) string`, `writeHashChunk(hashWriter, string)`, `isAnthropicBillingHeaderBlock(interface{}) bool` (all in `proxy/cache_tracker.go`); `ClaudeRequest`/`ClaudeMessage`, `OpenAIRequest`/`OpenAIMessage` (in `proxy/translator.go`).
- Produces:
  - `func claudeStickyKey(req *ClaudeRequest) [32]byte`
  - `func openAIStickyKey(req *OpenAIRequest) [32]byte`
  - `func isZeroStickyKey(key [32]byte) bool`

- [ ] **Step 1: Write the failing test**

```go
package proxy

import "testing"

func TestClaudeStickyKeyStableAcrossBillingHeaderDrift(t *testing.T) {
	build := func(billing string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-x",
			System: []interface{}{
				map[string]interface{}{"type": "text", "text": billing},
				map[string]interface{}{"type": "text", "text": "You are a helpful assistant."},
			},
			Messages: []ClaudeMessage{
				{Role: "user", Content: "hello"},
			},
		}
	}
	a := claudeStickyKey(build("x-anthropic-billing-header: cc_version=1"))
	b := claudeStickyKey(build("x-anthropic-billing-header: cc_version=2"))
	if a != b {
		t.Fatalf("billing-header drift changed the sticky key: %x != %x", a, b)
	}
	if isZeroStickyKey(a) {
		t.Fatalf("expected non-zero key for a real prefix")
	}
}

func TestClaudeStickyKeyDiffersOnSystemAndFirstMessage(t *testing.T) {
	base := &ClaudeRequest{
		System:   "prompt A",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	diffSystem := &ClaudeRequest{
		System:   "prompt B",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	diffMsg := &ClaudeRequest{
		System:   "prompt A",
		Messages: []ClaudeMessage{{Role: "user", Content: "different"}},
	}
	k := claudeStickyKey(base)
	if k == claudeStickyKey(diffSystem) {
		t.Fatal("expected different key when system differs")
	}
	if k == claudeStickyKey(diffMsg) {
		t.Fatal("expected different key when first user message differs")
	}
}

func TestClaudeStickyKeySameConversationSameKey(t *testing.T) {
	turn1 := &ClaudeRequest{
		System:   "prompt A",
		Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
	}
	turn2 := &ClaudeRequest{
		System: "prompt A",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
			{Role: "user", Content: "next turn"},
		},
	}
	if claudeStickyKey(turn1) != claudeStickyKey(turn2) {
		t.Fatal("expected same key across turns of one conversation")
	}
}

func TestStickyKeyZeroWhenEmptyPrefix(t *testing.T) {
	if !isZeroStickyKey(claudeStickyKey(&ClaudeRequest{})) {
		t.Fatal("expected zero key for empty Claude request")
	}
	if !isZeroStickyKey(openAIStickyKey(&OpenAIRequest{})) {
		t.Fatal("expected zero key for empty OpenAI request")
	}
}

func TestOpenAIStickyKeyUsesSystemAndFirstUser(t *testing.T) {
	req := &OpenAIRequest{
		Messages: []OpenAIMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "u1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "u2"},
		},
	}
	same := &OpenAIRequest{
		Messages: []OpenAIMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "u1"},
		},
	}
	if openAIStickyKey(req) != openAIStickyKey(same) {
		t.Fatal("expected same key regardless of later turns")
	}
	if isZeroStickyKey(openAIStickyKey(req)) {
		t.Fatal("expected non-zero key")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./proxy/ -run TestClaudeStickyKey -v`
Expected: FAIL — `undefined: claudeStickyKey` / `openAIStickyKey` / `isZeroStickyKey`.

- [ ] **Step 3: Write the implementation**

```go
package proxy

import "crypto/sha256"

// stickyKeyFromParts hashes the stable request prefix (system + first user
// message) into a routing key. Billing-header text blocks are stripped so the
// key stays stable across x-anthropic-billing-header drift. Returns the zero
// value when both parts are empty, signalling "no sticky key".
func stickyKeyFromParts(system, firstUser interface{}) [32]byte {
	system = stripBillingBlocks(system)
	firstUser = stripBillingBlocks(firstUser)
	if isEmptyStickyPart(system) && isEmptyStickyPart(firstUser) {
		return [32]byte{}
	}
	hasher := sha256.New()
	writeHashChunk(hasher, canonicalizeCacheValue(system))
	writeHashChunk(hasher, canonicalizeCacheValue(firstUser))
	var key [32]byte
	copy(key[:], hasher.Sum(nil))
	return key
}

// stripBillingBlocks removes x-anthropic-billing-header text blocks from a
// []interface{} content value. Non-array values are returned unchanged.
func stripBillingBlocks(part interface{}) interface{} {
	blocks, ok := part.([]interface{})
	if !ok {
		return part
	}
	out := make([]interface{}, 0, len(blocks))
	for _, b := range blocks {
		if isAnthropicBillingHeaderBlock(b) {
			continue
		}
		out = append(out, b)
	}
	return out
}

func isEmptyStickyPart(part interface{}) bool {
	switch v := part.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []interface{}:
		return len(v) == 0
	default:
		return false
	}
}

func isZeroStickyKey(key [32]byte) bool {
	return key == [32]byte{}
}

func firstClaudeUserContent(messages []ClaudeMessage) interface{} {
	for _, m := range messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return nil
}

func claudeStickyKey(req *ClaudeRequest) [32]byte {
	if req == nil {
		return [32]byte{}
	}
	return stickyKeyFromParts(req.System, firstClaudeUserContent(req.Messages))
}

func firstOpenAIRoleContent(messages []OpenAIMessage, role string) interface{} {
	for _, m := range messages {
		if m.Role == role {
			return m.Content
		}
	}
	return nil
}

func openAIStickyKey(req *OpenAIRequest) [32]byte {
	if req == nil {
		return [32]byte{}
	}
	system := firstOpenAIRoleContent(req.Messages, "system")
	firstUser := firstOpenAIRoleContent(req.Messages, "user")
	return stickyKeyFromParts(system, firstUser)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./proxy/ -run 'TestClaudeStickyKey|TestOpenAIStickyKey|TestStickyKeyZero' -v`
Expected: PASS (all five tests).

- [ ] **Step 5: Vet**

Run: `go vet ./proxy/`
Expected: no output.

---

### Task 2: Pin store on `AccountPool` (`pool`)

Add an in-memory pin map with a 5-min TTL and the selection methods. Usability guards mirror `GetNextForModelExcluding` exactly.

**Files:**
- Modify: `pool/account.go` — add `pins`/`pinsMu` fields to the `AccountPool` struct (`pool/account.go:16-24`) and initialize `pins` in `GetPool` (`pool/account.go:33-40`).
- Create: `pool/sticky.go`
- Test: `pool/sticky_test.go`

**Interfaces:**
- Consumes: `config.Account`, `isQuotaBlocked(config.Account, bool) bool`, `config.GetAllowOverUsage() bool`, `p.accountHasModel(id, model)`, `p.cooldowns`, `tokenRefreshSkewSeconds` (all in `pool`).
- Produces:
  - `const stickyPinTTL = 5 * time.Minute`
  - `type pinEntry struct { accountID string; expiresAt time.Time }`
  - `func (p *AccountPool) GetPinnedForModel(key [32]byte, model string) *config.Account`
  - `func (p *AccountPool) SetPin(key [32]byte, accountID string)`
  - `func (p *AccountPool) GetForAttempt(key [32]byte, model string, excluded map[string]bool, attempt int) *config.Account`

- [ ] **Step 1: Add struct fields and init**

In `pool/account.go`, add two fields to the `AccountPool` struct (after `modelLists`):

```go
	modelLists    map[string]map[string]bool // accountID → set of modelIDs (from ListAvailableModels)
	pins          map[[32]byte]pinEntry      // sticky routing: prefix hash → account
	pinsMu        sync.Mutex
```

In `GetPool`, add `pins` to the initializer:

```go
		pool = &AccountPool{
			cooldowns:   make(map[string]time.Time),
			errorCounts: make(map[string]int),
			modelLists:  make(map[string]map[string]bool),
			pins:        make(map[[32]byte]pinEntry),
		}
```

- [ ] **Step 2: Write the failing test**

```go
package pool

import (
	"kiro-go/config"
	"testing"
	"time"
)

func newPinTestPool(accs ...config.Account) *AccountPool {
	p := &AccountPool{
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}
	p.accounts = accs
	return p
}

func TestSetPinThenGetReturnsAccount(t *testing.T) {
	p := newPinTestPool(config.Account{ID: "A"})
	key := [32]byte{1}
	p.SetPin(key, "A")
	acc := p.GetPinnedForModel(key, "model-x")
	if acc == nil || acc.ID != "A" {
		t.Fatalf("expected pinned account A, got %v", acc)
	}
}

func TestGetPinnedZeroKeyReturnsNil(t *testing.T) {
	p := newPinTestPool(config.Account{ID: "A"})
	if p.GetPinnedForModel([32]byte{}, "model-x") != nil {
		t.Fatal("zero key must never resolve to an account")
	}
}

func TestGetPinnedExpiredReturnsNil(t *testing.T) {
	p := newPinTestPool(config.Account{ID: "A"})
	key := [32]byte{2}
	p.pins = map[[32]byte]pinEntry{key: {accountID: "A", expiresAt: time.Now().Add(-time.Minute)}}
	if p.GetPinnedForModel(key, "model-x") != nil {
		t.Fatal("expired pin must return nil")
	}
	if _, ok := p.pins[key]; ok {
		t.Fatal("expired pin should be pruned on lookup")
	}
}

func TestGetPinnedCooledDownAccountReturnsNil(t *testing.T) {
	p := newPinTestPool(config.Account{ID: "A"})
	key := [32]byte{3}
	p.SetPin(key, "A")
	p.cooldowns["A"] = time.Now().Add(time.Hour)
	if p.GetPinnedForModel(key, "model-x") != nil {
		t.Fatal("cooled-down pinned account must return nil")
	}
}

func TestGetPinnedQuotaBlockedAccountReturnsNil(t *testing.T) {
	p := newPinTestPool(config.Account{ID: "A", UsageCurrent: 10, UsageLimit: 10})
	key := [32]byte{4}
	p.SetPin(key, "A")
	if p.GetPinnedForModel(key, "model-x") != nil {
		t.Fatal("quota-blocked pinned account must return nil")
	}
}

func TestGetPinnedRefreshesTTLOnHit(t *testing.T) {
	p := newPinTestPool(config.Account{ID: "A"})
	key := [32]byte{5}
	p.pins = map[[32]byte]pinEntry{key: {accountID: "A", expiresAt: time.Now().Add(time.Second)}}
	if p.GetPinnedForModel(key, "model-x") == nil {
		t.Fatal("expected hit")
	}
	if p.pins[key].expiresAt.Before(time.Now().Add(stickyPinTTL - time.Minute)) {
		t.Fatal("expected TTL to be refreshed toward stickyPinTTL")
	}
}

func TestGetForAttemptZeroUsesPin(t *testing.T) {
	p := newPinTestPool(config.Account{ID: "A"}, config.Account{ID: "B"})
	key := [32]byte{6}
	p.SetPin(key, "A")
	acc := p.GetForAttempt(key, "model-x", nil, 0)
	if acc == nil || acc.ID != "A" {
		t.Fatalf("attempt 0 should use pin A, got %v", acc)
	}
}

func TestGetForAttemptNonZeroIgnoresPin(t *testing.T) {
	p := newPinTestPool(config.Account{ID: "A"})
	key := [32]byte{7}
	p.SetPin(key, "A")
	excluded := map[string]bool{"A": true}
	if acc := p.GetForAttempt(key, "model-x", excluded, 1); acc != nil && acc.ID == "A" {
		t.Fatal("attempt >= 1 must not return the pinned (excluded) account")
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./pool/ -run 'TestSetPin|TestGetPinned|TestGetForAttempt' -v`
Expected: FAIL — `undefined: pinEntry` / `SetPin` / `GetPinnedForModel` / `GetForAttempt` / `stickyPinTTL`.

- [ ] **Step 4: Write the implementation**

Create `pool/sticky.go`:

```go
package pool

import (
	"kiro-go/config"
	"time"
)

const stickyPinTTL = 5 * time.Minute

type pinEntry struct {
	accountID string
	expiresAt time.Time
}

// SetPin upserts a sticky pin with a fresh TTL. Zero keys are ignored.
func (p *AccountPool) SetPin(key [32]byte, accountID string) {
	if key == ([32]byte{}) || accountID == "" {
		return
	}
	p.pinsMu.Lock()
	defer p.pinsMu.Unlock()
	if p.pins == nil {
		p.pins = make(map[[32]byte]pinEntry)
	}
	p.pins[key] = pinEntry{accountID: accountID, expiresAt: time.Now().Add(stickyPinTTL)}
}

// GetPinnedForModel returns the pinned account for key only if it is currently
// usable (not cooled down, token not near expiry, not quota-blocked, supports
// the model). Expired pins are pruned. On a hit the TTL is refreshed.
func (p *AccountPool) GetPinnedForModel(key [32]byte, model string) *config.Account {
	if key == ([32]byte{}) {
		return nil
	}

	p.pinsMu.Lock()
	entry, ok := p.pins[key]
	if !ok || time.Now().After(entry.expiresAt) {
		if ok {
			delete(p.pins, key)
		}
		p.pinsMu.Unlock()
		return nil
	}
	accountID := entry.accountID
	p.pinsMu.Unlock()

	p.mu.RLock()
	acc := p.findUsableLocked(accountID, model)
	p.mu.RUnlock()
	if acc == nil {
		return nil
	}

	p.pinsMu.Lock()
	if e, ok := p.pins[key]; ok {
		e.expiresAt = time.Now().Add(stickyPinTTL)
		p.pins[key] = e
	}
	p.pinsMu.Unlock()
	return acc
}

// findUsableLocked returns the account with id if it is currently selectable,
// applying the same guards as GetNextForModelExcluding. Caller must hold p.mu.
func (p *AccountPool) findUsableLocked(id, model string) *config.Account {
	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	for i := range p.accounts {
		acc := &p.accounts[i]
		if acc.ID != id {
			continue
		}
		if !p.accountHasModel(acc.ID, model) {
			return nil
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			return nil
		}
		if acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds {
			return nil
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			return nil
		}
		return acc
	}
	return nil
}

// GetForAttempt biases attempt 0 toward the sticky pin, falling back to the
// weighted round-robin. Attempts >= 1 always round-robin (pin never retried).
func (p *AccountPool) GetForAttempt(key [32]byte, model string, excluded map[string]bool, attempt int) *config.Account {
	if attempt == 0 {
		if acc := p.GetPinnedForModel(key, model); acc != nil {
			return acc
		}
	}
	return p.GetNextForModelExcluding(model, excluded)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./pool/ -run 'TestSetPin|TestGetPinned|TestGetForAttempt' -v`
Expected: PASS (all eight tests).

- [ ] **Step 6: Run the full pool package to check no regressions**

Run: `go test ./pool/`
Expected: PASS.

- [ ] **Step 7: Vet**

Run: `go vet ./pool/`
Expected: no output.

---

### Task 3: Wire sticky routing into the four handlers (`proxy`)

Compute the sticky key at each API entry point, thread it into the stream/non-stream handlers, use `GetForAttempt` in the retry loop, and re-pin on success.

**Files:**
- Modify: `proxy/handler.go`
  - `handleClaudeChat` entry (`proxy/handler.go:843-854`) — compute key, pass to both handlers.
  - `handleClaudeStream` signature (`proxy/handler.go:868`) + retry loop (`proxy/handler.go:910-911`) + success block (`proxy/handler.go:1277-1278`).
  - `handleClaudeNonStream` signature (`proxy/handler.go:1454`) + retry loop (`proxy/handler.go:1459-1460`) + success block (`proxy/handler.go:1528-1529`).
  - `handleOpenAIChat` entry (`proxy/handler.go:1619-1626`) — compute key, pass to both handlers.
  - `handleOpenAIStream` signature (`proxy/handler.go:1630`) + retry loop (`proxy/handler.go:1649-1650`) + success block (`proxy/handler.go:1979-1980`).
  - `handleOpenAINonStream` signature (`proxy/handler.go:2023`) + retry loop (`proxy/handler.go:2028-2029`) + success block (`proxy/handler.go:2086-2087`).

**Interfaces:**
- Consumes: `claudeStickyKey`, `openAIStickyKey` (Task 1); `h.pool.GetForAttempt`, `h.pool.SetPin` (Task 2).
- Produces: no new exported symbols — internal wiring only.

Note: `CallKiroAPI` is a free function and not injectable, so end-to-end handler behavior is covered by the pool-level `GetForAttempt`/`SetPin` tests in Task 2 rather than a network test here. This task's verification is build + full suite + `go vet`.

- [ ] **Step 1: Compute the Claude key at the entry point**

In `handleClaudeChat`, immediately after the `cacheProfile := ...` line (`proxy/handler.go:843`), add:

```go
	stickyKey := claudeStickyKey(effectiveReq)
```

Update both dispatch calls (`proxy/handler.go:851` and `:853`) to pass it as the final argument:

```go
	if req.Stream {
		h.handleClaudeStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, stickyKey)
	} else {
		h.handleClaudeNonStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, stickyKey)
	}
```

- [ ] **Step 2: Update the two Claude handler signatures**

`proxy/handler.go:868`:

```go
func (h *Handler) handleClaudeStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, stickyKey [32]byte) {
```

`proxy/handler.go:1454`:

```go
func (h *Handler) handleClaudeNonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, stickyKey [32]byte) {
```

- [ ] **Step 3: Use `GetForAttempt` in the two Claude retry loops**

In `handleClaudeStream` (`proxy/handler.go:911`) and `handleClaudeNonStream` (`proxy/handler.go:1460`), replace:

```go
		account := h.pool.GetNextForModelExcluding(model, excluded)
```

with:

```go
		account := h.pool.GetForAttempt(stickyKey, model, excluded, attempt)
```

- [ ] **Step 4: Re-pin on success in the two Claude handlers**

In `handleClaudeStream`, immediately after `h.pool.RecordSuccess(account.ID)` (`proxy/handler.go:1277`), add:

```go
		h.pool.SetPin(stickyKey, account.ID)
```

In `handleClaudeNonStream`, immediately after `h.pool.RecordSuccess(account.ID)` (`proxy/handler.go:1528`), add the same line.

- [ ] **Step 5: Compute the OpenAI key at the entry point**

In `handleOpenAIChat`, after `kiroPayload := OpenAIToKiro(&req, thinking)` (`proxy/handler.go:1619`), add:

```go
	stickyKey := openAIStickyKey(&req)
```

Update both dispatch calls (`proxy/handler.go:1623` and `:1625`) to pass it:

```go
	if req.Stream {
		h.handleOpenAIStream(w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID, stickyKey)
	} else {
		h.handleOpenAINonStream(w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID, stickyKey)
	}
```

- [ ] **Step 6: Update the two OpenAI handler signatures**

`proxy/handler.go:1630`:

```go
func (h *Handler) handleOpenAIStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string, stickyKey [32]byte) {
```

`proxy/handler.go:2023`:

```go
func (h *Handler) handleOpenAINonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string, stickyKey [32]byte) {
```

- [ ] **Step 7: Use `GetForAttempt` in the two OpenAI retry loops**

In `handleOpenAIStream` (`proxy/handler.go:1650`) and `handleOpenAINonStream` (`proxy/handler.go:2029`), replace:

```go
		account := h.pool.GetNextForModelExcluding(model, excluded)
```

with:

```go
		account := h.pool.GetForAttempt(stickyKey, model, excluded, attempt)
```

- [ ] **Step 8: Re-pin on success in the two OpenAI handlers**

In `handleOpenAIStream`, immediately after `h.pool.RecordSuccess(account.ID)` (`proxy/handler.go:1979`), add:

```go
		h.pool.SetPin(stickyKey, account.ID)
```

In `handleOpenAINonStream`, immediately after `h.pool.RecordSuccess(account.ID)` (`proxy/handler.go:2086`), add the same line.

- [ ] **Step 9: Build**

Run: `go build -o kiro-go .`
Expected: builds with no errors (confirms all four signatures and call sites are consistent).

- [ ] **Step 10: Full test suite + vet**

Run: `go test ./... && go vet ./...`
Expected: all packages PASS, vet clean.

- [ ] **Step 11: Commit**

(Per user's version-control rule, only commit when the user explicitly asks. Leave changes pending otherwise.)

```bash
git add proxy/sticky_key.go proxy/sticky_key_test.go pool/sticky.go pool/sticky_test.go pool/account.go proxy/handler.go
git commit -m "feat(pool): sticky account routing to preserve upstream prompt cache"
```

---

## Notes on coverage vs. the spec

- **Spec "Handler: turn 2 reuses turn-1 account; pinned-account-unusable falls back and re-pins."** — Covered at the pool boundary by `TestGetForAttemptZeroUsesPin` (reuse) and the cooled-down/quota-blocked pin tests (fallback). `SetPin`-on-success is exercised by `TestSetPinThenGetReturnsAccount` + `GetForAttempt`. A network-level handler test is intentionally omitted because `CallKiroAPI` is not injectable.
- **Spec "stickyKey: same prefix → same hash; differing system/first message → different; billing-header noise ignored."** — Covered by Task 1's five tests.
- **Spec "Pin store: set→get; expired → nil; cooled/quota-blocked → nil; TTL refresh."** — Covered by Task 2's eight tests.
