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
