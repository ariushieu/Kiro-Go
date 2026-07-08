package proxy

import (
	"sort"
	"sync"
	"time"
)

// usageStats holds per-API-key usage statistics for the self-service usage dashboard.
// It is IN-MEMORY ONLY (lost on restart) and bounded by the number of configured keys,
// so it needs no background eviction. It complements the persisted cumulative counters
// on config.ApiKeyEntry (TokensUsed/CreditsUsed/RequestsCount) by adding the two things
// those counters lack: a per-model breakdown and a since-local-midnight daily total.
type usageStats struct {
	mu   sync.Mutex
	keys map[string]*keyUsage
}

// modelStat is the per-model tally for one key. Failures are counted separately from
// Requests (a failed request increments Failures, a successful one increments Requests).
type modelStat struct {
	Requests  int64
	Failures  int64
	InputTok  int64
	CacheTok  int64
	OutputTok int64
}

// keyUsage is the full in-memory usage for a single API key.
type keyUsage struct {
	perModel  map[string]*modelStat
	dailyTok  int64
	dailyDate string // local calendar day "2006-01-02"; a mismatch resets dailyTok
}

// modelStatView is a flattened, exported snapshot of one model's tally.
type modelStatView struct {
	Model     string `json:"model"`
	Requests  int64  `json:"requests"`
	Failures  int64  `json:"failures"`
	InputTok  int64  `json:"inputTok"`
	CacheTok  int64  `json:"cacheTok"`
	OutputTok int64  `json:"outputTok"`
}

// keyUsageView is the read snapshot returned to the dashboard handler.
type keyUsageView struct {
	DailyTokens int64           `json:"dailyTokens"`
	ByModel     []modelStatView `json:"byModel"`
}

func newUsageStats() *usageStats {
	return &usageStats{keys: make(map[string]*keyUsage)}
}

// todayString returns the current local calendar day. Local time is used so "daily"
// aligns with the operator's wall clock ("since local midnight").
func todayString() string {
	return time.Now().Format("2006-01-02")
}

// getOrInit returns the keyUsage for id, creating it if absent, and rolls the daily
// bucket over when the calendar day has changed. Caller MUST hold the write lock.
func (u *usageStats) getOrInit(id string) *keyUsage {
	ku := u.keys[id]
	if ku == nil {
		ku = &keyUsage{perModel: make(map[string]*modelStat), dailyDate: todayString()}
		u.keys[id] = ku
		return ku
	}
	if today := todayString(); ku.dailyDate != today {
		ku.dailyTok = 0
		ku.dailyDate = today
	}
	return ku
}

// recordSuccess tallies a successful request against a key+model. An empty apiKeyID
// (legacy single-key or unauthenticated path) is ignored. Model is normalised to
// "unknown" when empty so the breakdown never has a blank row.
func (u *usageStats) recordSuccess(apiKeyID, model string, inTok, cacheTok, outTok int64) {
	if apiKeyID == "" {
		return
	}
	if model == "" {
		model = "unknown"
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	ku := u.getOrInit(apiKeyID)
	ms := ku.perModel[model]
	if ms == nil {
		ms = &modelStat{}
		ku.perModel[model] = ms
	}
	ms.Requests++
	ms.InputTok += inTok
	ms.CacheTok += cacheTok
	ms.OutputTok += outTok
	ku.dailyTok += inTok + outTok
}

// recordFailure tallies a failed request against a key+model. Failures do not touch
// token counts or the daily total (no tokens were billed).
func (u *usageStats) recordFailure(apiKeyID, model string) {
	if apiKeyID == "" {
		return
	}
	if model == "" {
		model = "unknown"
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	ku := u.getOrInit(apiKeyID)
	ms := ku.perModel[model]
	if ms == nil {
		ms = &modelStat{}
		ku.perModel[model] = ms
	}
	ms.Failures++
}

// snapshot returns a copy of the usage for id. The ByModel slice is sorted by total
// tokens descending (busiest model first) for stable display. An unknown id returns
// a zero-valued view with today's date and an empty breakdown.
func (u *usageStats) snapshot(apiKeyID string) keyUsageView {
	u.mu.Lock()
	defer u.mu.Unlock()
	view := keyUsageView{ByModel: []modelStatView{}}
	ku := u.keys[apiKeyID]
	if ku == nil {
		return view
	}
	// Roll the daily bucket over on read too, so a stale day reads as 0.
	if today := todayString(); ku.dailyDate != today {
		ku.dailyTok = 0
		ku.dailyDate = today
	}
	view.DailyTokens = ku.dailyTok
	for model, ms := range ku.perModel {
		view.ByModel = append(view.ByModel, modelStatView{
			Model:     model,
			Requests:  ms.Requests,
			Failures:  ms.Failures,
			InputTok:  ms.InputTok,
			CacheTok:  ms.CacheTok,
			OutputTok: ms.OutputTok,
		})
	}
	sort.Slice(view.ByModel, func(i, j int) bool {
		ti := view.ByModel[i].InputTok + view.ByModel[i].OutputTok
		tj := view.ByModel[j].InputTok + view.ByModel[j].OutputTok
		return ti > tj
	})
	return view
}
