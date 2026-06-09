package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
)

// ApiKeyImportResult is the per-key outcome of a bulk API-key import.
type ApiKeyImportResult struct {
	// MaskedKey shows only the head/tail of the key so the full secret is not
	// echoed back to the browser or logs.
	MaskedKey    string  `json:"maskedKey"`
	AccountID    string  `json:"accountId,omitempty"`
	Email        string  `json:"email,omitempty"`
	Subscription string  `json:"subscription,omitempty"`
	UsageCurrent float64 `json:"usageCurrent"`
	UsageLimit   float64 `json:"usageLimit"`
	Imported     bool    `json:"imported"`
	Skipped      bool    `json:"skipped"` // duplicate of an existing account
	InfoOK       bool    `json:"infoOk"`  // whether usage/credit lookup succeeded
	Error        string  `json:"error,omitempty"`
}

// maskKey obscures the middle of an API key for display.
func maskKey(key string) string {
	k := strings.TrimSpace(key)
	if len(k) <= 10 {
		return "****"
	}
	return k[:6] + "…" + k[len(k)-4:]
}

// ImportApiKeys parses a newline-separated list of Kiro API keys, skips any that
// duplicate an existing account, creates an enabled api_key account for each new
// key, and best-effort fetches usage/credit so the caller can show
// used/limit per account.
func (h *Handler) ImportApiKeys(rawText, region, authRegion, apiRegion string) []ApiKeyImportResult {
	if region == "" {
		region = "us-east-1"
	}
	if authRegion == "" {
		authRegion = region
	}
	if apiRegion == "" {
		apiRegion = region
	}

	// Snapshot existing keys for duplicate detection.
	existing := make(map[string]bool)
	for _, a := range config.GetAccounts() {
		if a.KiroApiKey != "" {
			existing[a.KiroApiKey] = true
		}
	}

	// Dedupe within the input itself too.
	seenInBatch := make(map[string]bool)

	var results []ApiKeyImportResult
	reloadNeeded := false

	for _, line := range strings.Split(rawText, "\n") {
		key := strings.TrimSpace(line)
		if key == "" {
			continue
		}

		res := ApiKeyImportResult{MaskedKey: maskKey(key)}

		if existing[key] || seenInBatch[key] {
			res.Skipped = true
			results = append(results, res)
			continue
		}
		seenInBatch[key] = true

		account := config.Account{
			ID:         auth.GenerateAccountID(),
			KiroApiKey: key,
			AccessToken: key, // mirror for pool compatibility
			AuthMethod: "api_key",
			Region:     region,
			AuthRegion: authRegion,
			ApiRegion:  apiRegion,
			ExpiresAt:  0,
			Enabled:    true,
			MachineId:  config.GenerateMachineId(),
		}

		// Best-effort: fetch usage/credit + email before persisting.
		if info, err := RefreshAccountInfo(&account); err == nil && info != nil {
			res.InfoOK = true
			account.Email = info.Email
			account.UserId = info.UserId
			account.SubscriptionType = info.SubscriptionType
			account.SubscriptionTitle = info.SubscriptionTitle
			account.DaysRemaining = info.DaysRemaining
			account.UsageCurrent = info.UsageCurrent
			account.UsageLimit = info.UsageLimit
			account.UsagePercent = info.UsagePercent
			account.NextResetDate = info.NextResetDate
			account.LastRefresh = info.LastRefresh
			res.Email = info.Email
			res.Subscription = info.SubscriptionTitle
			res.UsageCurrent = info.UsageCurrent
			res.UsageLimit = info.UsageLimit
		} else if err != nil {
			res.Error = "info: " + err.Error()
		}

		if err := config.AddAccount(account); err != nil {
			res.Error = "save: " + err.Error()
			results = append(results, res)
			continue
		}

		res.Imported = true
		res.AccountID = account.ID
		reloadNeeded = true

		// Cache model list asynchronously for the new account.
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ApiKeyImport] Model cache failed for %s: %v", maskKey(acc.KiroApiKey), err)
			}
		}(account)

		results = append(results, res)
	}

	if reloadNeeded {
		h.pool.Reload()
	}

	imported, skipped := 0, 0
	for _, r := range results {
		if r.Imported {
			imported++
		}
		if r.Skipped {
			skipped++
		}
	}
	logger.Infof("[ApiKeyImport] %d keys: %d imported, %d skipped", len(results), imported, skipped)
	return results
}

// apiImportApiKeys handles POST /admin/api/auth/apikeys-batch.
//
// Body:
//
//	{
//	  "keys": "one API key per line",
//	  "region": "us-east-1",      // optional
//	  "authRegion": "us-east-1",  // optional
//	  "apiRegion": "us-east-1"     // optional
//	}
//
// Each new key becomes an enabled api_key account; duplicates of existing
// accounts are skipped. The response reports per-key outcome plus used/limit
// credit for each imported account.
func (h *Handler) apiImportApiKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys       string `json:"keys"`
		Region     string `json:"region"`
		AuthRegion string `json:"authRegion"`
		ApiRegion  string `json:"apiRegion"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.Keys) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "keys is required"})
		return
	}

	results := h.ImportApiKeys(req.Keys, req.Region, req.AuthRegion, req.ApiRegion)

	imported, skipped, infoFailed := 0, 0, 0
	for _, r := range results {
		if r.Imported {
			imported++
		}
		if r.Skipped {
			skipped++
		}
		if r.Imported && !r.InfoOK {
			infoFailed++
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"total":      len(results),
		"imported":   imported,
		"skipped":    skipped,
		"infoFailed": infoFailed,
		"results":    results,
	})
}
