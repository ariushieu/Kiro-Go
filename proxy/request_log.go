package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// RequestLogEntry is a single served API request, captured for the admin "API Log" view.
// It records which API key was used, the model, token/credit cost and the serving account.
type RequestLogEntry struct {
	Time         int64   `json:"time"` // Unix seconds
	APIKeyID     string  `json:"apiKeyId,omitempty"`
	APIKeyName   string  `json:"apiKeyName,omitempty"`
	APIKeyMasked string  `json:"apiKeyMasked,omitempty"`
	Model        string  `json:"model,omitempty"`
	AccountID    string  `json:"accountId,omitempty"`
	AccountEmail string  `json:"accountEmail,omitempty"`
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	TotalTokens  int     `json:"totalTokens"`
	Credits      float64 `json:"credits"`
}

// requestLogCapacity is the number of recent request entries retained in memory.
// Per-request only — counters/limits persist on the ApiKeyEntry; this is just a live feed.
const requestLogCapacity = 1000

// requestLogBuffer is a fixed-size in-memory ring of recent requests. Oldest entries are
// overwritten once full. Not persisted: it is a runtime diagnostic feed, gone on restart.
type requestLogBuffer struct {
	mu   sync.Mutex
	ring []RequestLogEntry
	next int
	full bool
}

var requestLog = &requestLogBuffer{ring: make([]RequestLogEntry, requestLogCapacity)}

func (b *requestLogBuffer) add(e RequestLogEntry) {
	b.mu.Lock()
	b.ring[b.next] = e
	b.next = (b.next + 1) % requestLogCapacity
	if b.next == 0 {
		b.full = true
	}
	b.mu.Unlock()
}

// snapshot returns retained entries newest-first.
func (b *requestLogBuffer) snapshot() []RequestLogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []RequestLogEntry
	if !b.full {
		out = make([]RequestLogEntry, b.next)
		copy(out, b.ring[:b.next])
	} else {
		out = make([]RequestLogEntry, 0, requestLogCapacity)
		out = append(out, b.ring[b.next:]...)
		out = append(out, b.ring[:b.next]...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

// logRequest appends a served-request entry to the in-memory feed. apiKeyID may be empty
// (unauthenticated / legacy single-key path), in which case key fields are left blank.
func logRequest(e RequestLogEntry) {
	if e.Time == 0 {
		e.Time = time.Now().Unix()
	}
	e.TotalTokens = e.InputTokens + e.OutputTokens
	requestLog.add(e)
}

// apiGetRequestLogs GET /admin/api/request-logs - returns the recent per-request feed.
// Optional query params:
//   - apiKeyId: only entries for that key
//   - limit:    cap the number of returned entries (default 200, max requestLogCapacity)
func (h *Handler) apiGetRequestLogs(w http.ResponseWriter, r *http.Request) {
	entries := requestLog.snapshot()

	if keyID := r.URL.Query().Get("apiKeyId"); keyID != "" {
		filtered := entries[:0:0]
		for _, e := range entries {
			if e.APIKeyID == keyID {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > requestLogCapacity {
		limit = requestLogCapacity
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"logs": entries})
}

// apiKeyUsageView is the per-key usage summary returned by apiGetUsageSummary.
type apiKeyUsageView struct {
	ID            string  `json:"id"`
	Name          string  `json:"name,omitempty"`
	KeyMasked     string  `json:"keyMasked"`
	Enabled       bool    `json:"enabled"`
	TokenLimit    int64   `json:"tokenLimit"`
	CreditLimit   float64 `json:"creditLimit"`
	TokensUsed    int64   `json:"tokensUsed"`
	CreditsUsed   float64 `json:"creditsUsed"`
	RequestsCount int64   `json:"requestsCount"`
	TokensRemain  int64   `json:"tokensRemain"`  // -1 = unlimited
	CreditsRemain float64 `json:"creditsRemain"` // -1 = unlimited
	OverToken     bool    `json:"overToken"`
	OverCredit    bool    `json:"overCredit"`
	Expired       bool    `json:"expired"`
	LastUsedAt    int64   `json:"lastUsedAt,omitempty"`
	ExpiresAt     int64   `json:"expiresAt,omitempty"`
}

// apiGetUsageSummary GET /admin/api/usage-summary - per-key credit/token usage vs limits.
// Backs the admin "Usage Check" view so operators can see, at a glance, how much each key
// has consumed and how much quota remains.
func (h *Handler) apiGetUsageSummary(w http.ResponseWriter, r *http.Request) {
	entries := config.ListApiKeys()
	out := make([]apiKeyUsageView, 0, len(entries))

	var totalTokens, totalLimitTokens int64
	var totalCredits, totalLimitCredits float64
	var totalRequests int64

	for _, e := range entries {
		overToken, overCredit := config.ApiKeyOverLimit(e)
		tokensRemain := int64(-1)
		if e.TokenLimit > 0 {
			tokensRemain = e.TokenLimit - e.TokensUsed
			if tokensRemain < 0 {
				tokensRemain = 0
			}
		}
		creditsRemain := float64(-1)
		if e.CreditLimit > 0 {
			creditsRemain = e.CreditLimit - e.CreditsUsed
			if creditsRemain < 0 {
				creditsRemain = 0
			}
		}
		out = append(out, apiKeyUsageView{
			ID:            e.ID,
			Name:          e.Name,
			KeyMasked:     config.MaskApiKey(e.Key),
			Enabled:       e.Enabled,
			TokenLimit:    e.TokenLimit,
			CreditLimit:   e.CreditLimit,
			TokensUsed:    e.TokensUsed,
			CreditsUsed:   e.CreditsUsed,
			RequestsCount: e.RequestsCount,
			TokensRemain:  tokensRemain,
			CreditsRemain: creditsRemain,
			OverToken:     overToken,
			OverCredit:    overCredit,
			Expired:       config.ApiKeyExpired(e),
			LastUsedAt:    e.LastUsedAt,
			ExpiresAt:     e.ExpiresAt,
		})
		totalTokens += e.TokensUsed
		totalLimitTokens += e.TokenLimit
		totalCredits += e.CreditsUsed
		totalLimitCredits += e.CreditLimit
		totalRequests += e.RequestsCount
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"keys": out,
		"totals": map[string]interface{}{
			"tokensUsed":   totalTokens,
			"tokenLimit":   totalLimitTokens,
			"creditsUsed":  totalCredits,
			"creditLimit":  totalLimitCredits,
			"requests":     totalRequests,
			"requestCount": totalRequests,
		},
	})
}
