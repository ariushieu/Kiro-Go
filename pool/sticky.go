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
