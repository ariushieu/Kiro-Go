# Sticky account routing — design

Date: 2026-07-07

## Problem

The pool round-robins every request across accounts (`pool/account.go`
`GetNextForModelExcluding`). Kiro/AWS prompt caching is **per-account**, so
consecutive turns of one conversation land on different accounts, each seeing a
cold cache. AWS then bills full input tokens per turn → high credit burn (the
observed 197M-input-token / high-credit pattern with Claude Code traffic).

Note: `proxy/cache_tracker.go` only *reports* cache usage to the client; it does
not control the upstream cache. The real cache lives on the account at the
upstream, which is why routing — not the tracker — is the lever.

## Goal

Pin a conversation to the account that first served it, for as long as the
upstream prompt cache stays warm (~5 min). Same conversation → same account →
cache hits → lower credits. This is a **first-choice bias** layered on top of
the existing retry/failover loop, not a replacement for it.

## Decisions locked (from user)

- **Sticky key**: hash of the stable prefix (system prompt + first user
  message), derived independently of the cache profile so it works for both
  Claude and OpenAI formats.
- **TTL / storage**: in-memory, 5-min TTL matching `defaultPromptCacheTTL`,
  refreshed on each hit. Lost on restart (upstream cache is cold then anyway).
  Not persisted (respects "never write config on the hot path").
- **Fallback**: on pin-miss, fall back to normal round-robin and **re-pin** to
  whatever account succeeds (self-healing migration).
- **Retry interaction**: pin biases **attempt 0 only**; attempts 1–2 always use
  `GetNextForModelExcluding`. Re-pin on the first successful attempt.

## Components

### a) Sticky key derivation (`proxy`)

`stickyKey(system, firstUserMessage) → [32]byte`. Canonicalizes the stable
prefix and SHA-256s it, reusing `canonicalizeCacheValue` / `writeHashChunk` from
`cache_tracker.go` for consistent hashing. Independent of the cache profile.
Empty/unhashable prefix → zero-value sentinel meaning "no key".

### b) Pin store (`pool`, new type on `AccountPool`)

In-memory `map[[32]byte]pinEntry` where `pinEntry = {accountID string,
expiresAt time.Time}`, guarded by its own mutex.

- `GetPinnedForModel(key, model) *config.Account` — returns the pinned account
  **only if** currently usable: not cooled down, not quota-blocked, token not
  near expiry, supports the model (same guards as `GetNextForModelExcluding`).
  Refreshes TTL on hit. Drops expired entries lazily. Returns nil otherwise.
- `SetPin(key, accountID)` — upserts the pin with a fresh 5-min TTL.

Lives on `AccountPool` so it can read cooldown/quota state directly.

### c) Handler integration (`proxy`, shared helper)

A helper picks the account per attempt:
- attempt 0: if `GetPinnedForModel` returns non-nil → use it; else
  `GetNextForModelExcluding`.
- attempt ≥ 1: always `GetNextForModelExcluding` (pin never retried after
  failing; excluded set unchanged).

After a successful request, call `SetPin(key, account.ID)`. Applied to all four
handlers (Claude stream/non-stream, OpenAI stream/non-stream) via one shared
helper — the logic is identical, so no copy-paste.

Boundaries: derivation knows nothing about accounts; the pin store takes a key
and knows nothing about hashing; the handler wires them. Each testable alone.

## Data flow (happy path)

```
Turn 1: stickyKey = K; no pin → round-robin → account A; success → SetPin(K, A)
Turn 2: stickyKey = K; pin → A (usable) → serve from A → upstream CACHE HIT;
        SetPin(K, A) refreshes TTL
Turn N: A cooled down → pin returns nil → round-robin → B; success →
        SetPin(K, B)  (conversation migrates and re-pins)
```

## Edge cases

- **Empty prefix**: zero-value key → helper skips stickiness → pure round-robin.
- **Pinned account fails mid-request**: existing retry loop handles it; re-pin
  only on the account that actually succeeds.
- **Restart**: pin map empty, upstream cache cold — nothing lost.
- **Map growth**: bounded by lazy pruning on lookup + 5-min TTL.

## Testing

- `stickyKey`: same prefix → same hash; differing system or first message →
  different hash; billing-header noise ignored (reuses canonicalization).
- Pin store: set→get returns account; expired → nil; cooled-down/quota-blocked
  pinned account → nil (falls back); TTL refreshes on hit.
- Handler: turn 2 reuses turn-1 account; pinned-account-unusable falls back and
  re-pins. Uses existing `auth/testhooks.go` seams to stub the upstream call.
