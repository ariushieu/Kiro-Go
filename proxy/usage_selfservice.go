package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"time"
)

// usageKeyInfo is the non-secret key metadata surfaced on the /usage dashboard. The
// raw key is never included — only the masked form.
type usageKeyInfo struct {
	Masked      string  `json:"masked"`
	Name        string  `json:"name,omitempty"`
	Enabled     bool    `json:"enabled"`
	CreatedAt   int64   `json:"createdAt"`
	LastUsedAt  int64   `json:"lastUsedAt,omitempty"`
	ExpiresAt   int64   `json:"expiresAt,omitempty"`
	Expired     bool    `json:"expired"`
	RPMLimit    int     `json:"rpmLimit"`
	TPMLimit    int     `json:"tpmLimit"`
	TokenLimit  int64   `json:"tokenLimit"`
	CreditLimit float64 `json:"creditLimit"`
}

// usageLifetime carries both the current-period counters (cleared by "Reset Usage")
// and the true grand totals (only cleared by "Reset All"), so the dashboard can show
// the running total that survives a routine per-cycle reset.
type usageLifetime struct {
	Requests         int64   `json:"requests"`
	TokensUsed       int64   `json:"tokensUsed"`
	CreditsUsed      float64 `json:"creditsUsed"`
	LifetimeRequests int64   `json:"lifetimeRequests"`
	LifetimeTokens   int64   `json:"lifetimeTokens"`
	LifetimeCredits  float64 `json:"lifetimeCredits"`
}

// usageLogView is one request-log row shaped for the /usage page. It maps the internal
// RequestLogEntry into the field names usage.js expects (status "success"/"error",
// duration in ms). IP and cache-token columns are not tracked per-entry in this fork,
// so they are left zero/blank and render as "—" on the page.
type usageLogView struct {
	Time         int64  `json:"time"`
	Model        string `json:"model,omitempty"`
	Endpoint     string `json:"endpoint,omitempty"`
	Status       string `json:"status"`
	IP           string `json:"ip,omitempty"`
	InputTokens  int    `json:"inputTokens"`
	CacheTokens  int    `json:"cacheTokens"`
	OutputTokens int    `json:"outputTokens"`
	Duration     int64  `json:"duration"`
}

// usageLogsForKey returns the request-log entries attributed to apiKeyID, newest first,
// capped at limit (0 = no cap). It reads the in-memory ring buffer directly and ignores
// any admin-clear watermark: a customer still sees their own history on /usage.
func (h *Handler) usageLogsForKey(apiKeyID string, limit int) []usageLogView {
	out := make([]usageLogView, 0, limit)
	if apiKeyID == "" {
		return out
	}
	for _, e := range requestLog.snapshot() { // snapshot is newest-first
		if e.APIKeyID != apiKeyID {
			continue
		}
		status := "success"
		if e.Status != "ok" {
			status = "error"
		}
		out = append(out, usageLogView{
			Time:         e.Time,
			Model:        e.Model,
			Endpoint:     e.Endpoint,
			Status:       status,
			InputTokens:  e.InputTokens,
			OutputTokens: e.OutputTokens,
			Duration:     e.DurationMs,
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// serveUsagePage serves the public self-service usage dashboard HTML. No auth: the page
// itself shows nothing until the visitor enters a valid key and the POST /usage call
// succeeds.
func (h *Handler) serveUsagePage(w http.ResponseWriter, r *http.Request) {
	setWebSecurityHeaders(w)
	http.ServeFile(w, r, "web/usage.html")
}

// apiUsageSelfService returns the usage snapshot for the API key supplied in the
// Authorization/X-Api-Key header. It requires NO admin password so a shared key's owner
// can self-check. To blunt key-guessing floods it is gated by the per-IP reject limiter;
// the key space makes enumeration hopeless, so this only caps volume. The response never
// contains the raw key — only the masked form.
func (h *Handler) apiUsageSelfService(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if h.guard != nil {
		ip := h.guard.clientIP(r)
		if !h.guard.allowIP(ip) {
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{"error": "Too many requests, please retry shortly"})
			return
		}
	}

	provided := extractProvidedKey(r)
	if provided == "" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Missing API key"})
		return
	}
	entry := config.FindApiKeyByValue(provided)
	if entry == nil {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid API key"})
		return
	}

	info := usageKeyInfo{
		Masked:      config.MaskApiKey(entry.Key),
		Name:        entry.Name,
		Enabled:     entry.Enabled,
		CreatedAt:   entry.CreatedAt,
		LastUsedAt:  entry.LastUsedAt,
		ExpiresAt:   entry.ExpiresAt,
		Expired:     config.ApiKeyExpired(*entry),
		RPMLimit:    entry.RPMLimit,
		TPMLimit:    entry.TPMLimit,
		TokenLimit:  entry.TokenLimit,
		CreditLimit: entry.CreditLimit,
	}

	var usageView keyUsageView
	if h.usage != nil {
		usageView = h.usage.snapshot(entry.ID)
	}

	resp := map[string]interface{}{
		"key": info,
		"lifetime": usageLifetime{
			Requests:         entry.RequestsCount,
			TokensUsed:       entry.TokensUsed,
			CreditsUsed:      entry.CreditsUsed,
			LifetimeRequests: entry.LifetimeRequests,
			LifetimeTokens:   entry.LifetimeTokens,
			LifetimeCredits:  entry.LifetimeCredits,
		},
		"daily": map[string]interface{}{
			"tokens": usageView.DailyTokens,
		},
		"byModel":  usageView.ByModel,
		"logs":     h.usageLogsForKey(entry.ID, 200),
		"baseURL":  selfServiceBaseURL(r),
		"serverTs": time.Now().Unix(),
	}
	json.NewEncoder(w).Encode(resp)
}
