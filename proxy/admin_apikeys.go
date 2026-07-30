package proxy

import (
	"encoding/json"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxApiKeyImportBytes caps an import payload. Generous for thousands of keys while
// still bounding how much an admin-authenticated request can buffer in memory.
const maxApiKeyImportBytes = 8 << 20 // 8 MiB

// apiKeyView is the response payload for listing/inspecting API keys. The Key field
// is masked so admins can identify entries without exposing the secret.
type apiKeyView struct {
	ID            string  `json:"id"`
	Name          string  `json:"name,omitempty"`
	KeyMasked     string  `json:"keyMasked"`
	Enabled       bool    `json:"enabled"`
	Migrated      bool    `json:"migrated,omitempty"`
	CreatedAt     int64   `json:"createdAt"`
	LastUsedAt    int64   `json:"lastUsedAt,omitempty"`
	ExpiresAt     int64   `json:"expiresAt,omitempty"`
	TokenLimit    int64   `json:"tokenLimit,omitempty"`
	CreditLimit   float64 `json:"creditLimit,omitempty"`
	TokensUsed    int64   `json:"tokensUsed"`
	CreditsUsed   float64 `json:"creditsUsed"`
	RequestsCount int64   `json:"requestsCount"`
	RPMLimit      int      `json:"rpmLimit,omitempty"`
	IPLimit       int      `json:"ipLimit,omitempty"`
	IPAllowlist   []string `json:"ipAllowlist,omitempty"`
	TPMLimit      int      `json:"tpmLimit,omitempty"`

	// BoundAccountIDs restricts routing to a fixed set of accounts (empty = shared pool).
	BoundAccountIDs []string `json:"boundAccountIds,omitempty"`

	// Models is the per-key model allowlist (empty = use client's model). A client model
	// in the list passes through; one not in the list is remapped to the first entry.
	Models []string `json:"models,omitempty"`

	// Lifetime totals — never cleared by "Reset Usage", only by "Reset All".
	LifetimeTokens   int64   `json:"lifetimeTokens"`
	LifetimeCredits  float64 `json:"lifetimeCredits"`
	LifetimeRequests int64   `json:"lifetimeRequests"`
}

func toApiKeyView(e config.ApiKeyEntry) apiKeyView {
	return apiKeyView{
		ID:            e.ID,
		Name:          e.Name,
		KeyMasked:     config.MaskApiKey(e.Key),
		Enabled:       e.Enabled,
		Migrated:      e.Migrated,
		CreatedAt:     e.CreatedAt,
		LastUsedAt:    e.LastUsedAt,
		ExpiresAt:     e.ExpiresAt,
		TokenLimit:    e.TokenLimit,
		CreditLimit:   e.CreditLimit,
		TokensUsed:    e.TokensUsed,
		CreditsUsed:   e.CreditsUsed,
		RequestsCount: e.RequestsCount,
		RPMLimit:      e.RPMLimit,
		IPLimit:       e.IPLimit,
		IPAllowlist:   e.IPAllowlist,
		TPMLimit:      e.TPMLimit,

		BoundAccountIDs: e.BoundAccountIDs,
		Models:          e.Models,

		LifetimeTokens:   e.LifetimeTokens,
		LifetimeCredits:  e.LifetimeCredits,
		LifetimeRequests: e.LifetimeRequests,
	}
}

func (h *Handler) apiListApiKeys(w http.ResponseWriter, r *http.Request) {
	entries := config.ListApiKeys()
	out := make([]apiKeyView, len(entries))
	for i, e := range entries {
		out[i] = toApiKeyView(e)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"apiKeys": out})
}

func (h *Handler) apiGetApiKey(w http.ResponseWriter, r *http.Request, id string) {
	entry := config.GetApiKeyEntry(id)
	if entry == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
		return
	}
	json.NewEncoder(w).Encode(toApiKeyView(*entry))
}

type apiKeyCreateRequest struct {
	Name        string  `json:"name,omitempty"`
	Key         string  `json:"key,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`
	ExpiresAt   int64   `json:"expiresAt,omitempty"`
	RPMLimit    int      `json:"rpmLimit,omitempty"`
	IPLimit     int      `json:"ipLimit,omitempty"`
	IPAllowlist []string `json:"ipAllowlist,omitempty"`
	TPMLimit    int      `json:"tpmLimit,omitempty"`

	BoundAccountIDs []string `json:"boundAccountIds,omitempty"`
	// Models is the per-key model allowlist. Model is a legacy single-value alias folded
	// into Models when Models is empty.
	Models []string `json:"models,omitempty"`
	Model  string   `json:"model,omitempty"`
}

func (h *Handler) apiCreateApiKey(w http.ResponseWriter, r *http.Request) {
	var req apiKeyCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	entry, err := createApiKeyFromRequest(req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Return the cleartext key exactly once on creation so the operator can copy it.
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"id":      entry.ID,
		"key":     entry.Key,
		"apiKey":  toApiKeyView(entry),
	})
}

func createApiKeyFromRequest(req apiKeyCreateRequest) (config.ApiKeyEntry, error) {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	keyValue := req.Key
	if keyValue == "" {
		keyValue = config.GenerateApiKeyValue()
	}

	return config.AddApiKey(config.ApiKeyEntry{
		Name:            req.Name,
		Key:             keyValue,
		Enabled:         enabled,
		TokenLimit:      req.TokenLimit,
		CreditLimit:     req.CreditLimit,
		ExpiresAt:       req.ExpiresAt,
		RPMLimit:        req.RPMLimit,
		IPLimit:         req.IPLimit,
		IPAllowlist:     sanitizeIPAllowlist(req.IPAllowlist),
		TPMLimit:        req.TPMLimit,
		BoundAccountIDs: req.BoundAccountIDs,
		Models:          mergeModelList(req.Models, req.Model),
	})
}

// mergeModelList folds a legacy single-value model into the allowlist: the list wins
// when non-empty, otherwise a non-empty legacy value becomes a one-element list.
func mergeModelList(list []string, legacy string) []string {
	if len(list) > 0 {
		return list
	}
	if strings.TrimSpace(legacy) != "" {
		return []string{legacy}
	}
	return nil
}

type apiKeyBulkCreateRequest struct {
	Count       int     `json:"count"`
	NamePrefix  string  `json:"namePrefix,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`
	ExpiresAt   int64   `json:"expiresAt,omitempty"`
	RPMLimit    int      `json:"rpmLimit,omitempty"`
	IPLimit     int      `json:"ipLimit,omitempty"`
	IPAllowlist []string `json:"ipAllowlist,omitempty"`
	TPMLimit    int      `json:"tpmLimit,omitempty"`

	BoundAccountIDs []string `json:"boundAccountIds,omitempty"`
	// Models is the per-key model allowlist; Model is a legacy single-value alias.
	Models []string `json:"models,omitempty"`
	Model  string   `json:"model,omitempty"`
}

func (h *Handler) apiBulkCreateApiKeys(w http.ResponseWriter, r *http.Request) {
	var req apiKeyBulkCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.Count <= 0 || req.Count > 100 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "count must be between 1 and 100"})
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	prefix := req.NamePrefix
	if prefix == "" {
		prefix = "API Key"
	}

	ipAllowlist := sanitizeIPAllowlist(req.IPAllowlist)
	models := mergeModelList(req.Models, req.Model)
	entries := make([]config.ApiKeyEntry, req.Count)
	for i := range entries {
		entries[i] = config.ApiKeyEntry{
			Name:            prefix + " " + strconv.Itoa(i+1),
			Key:             config.GenerateApiKeyValue(),
			Enabled:         enabled,
			TokenLimit:      req.TokenLimit,
			CreditLimit:     req.CreditLimit,
			ExpiresAt:       req.ExpiresAt,
			RPMLimit:        req.RPMLimit,
			IPLimit:         req.IPLimit,
			IPAllowlist:     ipAllowlist,
			TPMLimit:        req.TPMLimit,
			BoundAccountIDs: req.BoundAccountIDs,
			Models:          models,
		}
	}
	created, err := config.AddApiKeys(entries)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	keys := make([]string, len(created))
	views := make([]apiKeyView, len(created))
	for i, entry := range created {
		keys[i] = entry.Key
		views[i] = toApiKeyView(entry)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   len(created),
		"keys":    keys,
		"apiKeys": views,
	})
}

type apiKeyBulkDeleteRequest struct {
	IDs []string `json:"ids"`
}

// apiKeyExtendRequest shifts expiry on a set of keys.
//
// Seconds is the delta; the panel's quick buttons send 1800, 3600, 86400 and so
// on. All targets every configured key, which is why it is an explicit flag
// rather than "empty IDs means everything" — an accidentally empty selection
// must not silently rewrite the whole key list.
type apiKeyExtendRequest struct {
	IDs     []string `json:"ids"`
	All     bool     `json:"all"`
	Seconds int64    `json:"seconds"`
}

// maxApiKeyExtendSeconds caps a single call at ~10 years. Without a bound a typo
// in the custom-amount box (or a units mixup) can push expiry past the year
// 275760 limit of a JS Date and render the key list unreadable.
const maxApiKeyExtendSeconds = 10 * 365 * 24 * 60 * 60

func (h *Handler) apiExtendApiKeys(w http.ResponseWriter, r *http.Request) {
	var req apiKeyExtendRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.Seconds == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "seconds must be non-zero"})
		return
	}
	if req.Seconds > maxApiKeyExtendSeconds || req.Seconds < -maxApiKeyExtendSeconds {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "seconds is out of range"})
		return
	}

	ids := req.IDs
	if req.All {
		entries := config.ListApiKeys()
		ids = make([]string, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "no api key ids provided"})
		return
	}

	res, err := config.ExtendApiKeys(ids, req.Seconds)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	logger.Infof("[ApiKeyExtend] %+d s on %d key(s): %d extended, %d never-expires skipped, %d not found",
		req.Seconds, len(ids), len(res.Extended), len(res.SkippedNeverExpires), len(res.NotFound))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":             true,
		"extended":            len(res.Extended),
		"skippedNeverExpires": len(res.SkippedNeverExpires),
		"notFound":            len(res.NotFound),
		"expiresAt":           res.Extended,
	})
}

func (h *Handler) apiBulkDeleteApiKeys(w http.ResponseWriter, r *http.Request) {
	var req apiKeyBulkDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	deleted, err := config.DeleteApiKeys(req.IDs)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "deleted": deleted})
}

type apiKeyUpdateRequest struct {
	Name        *string   `json:"name,omitempty"`
	Key         *string   `json:"key,omitempty"`
	Enabled     *bool     `json:"enabled,omitempty"`
	TokenLimit  *int64    `json:"tokenLimit,omitempty"`
	CreditLimit *float64  `json:"creditLimit,omitempty"`
	ExpiresAt   *int64    `json:"expiresAt,omitempty"`
	RPMLimit    *int      `json:"rpmLimit,omitempty"`
	IPLimit     *int      `json:"ipLimit,omitempty"`
	IPAllowlist *[]string `json:"ipAllowlist,omitempty"`
	TPMLimit    *int      `json:"tpmLimit,omitempty"`

	BoundAccountIDs *[]string `json:"boundAccountIds,omitempty"`

	// Models is the per-key model allowlist. Model is a legacy single-value alias kept
	// for older API clients; it is folded into a one-element allowlist below.
	Models *[]string `json:"models,omitempty"`
	Model  *string   `json:"model,omitempty"`
}

func (h *Handler) apiUpdateApiKey(w http.ResponseWriter, r *http.Request, id string) {
	existing := config.GetApiKeyEntry(id)
	if existing == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
		return
	}

	var req apiKeyUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	patch := *existing
	if req.Name != nil {
		patch.Name = *req.Name
	}
	if req.Key != nil {
		patch.Key = *req.Key
	}
	if req.Enabled != nil {
		patch.Enabled = *req.Enabled
	}
	if req.TokenLimit != nil {
		patch.TokenLimit = *req.TokenLimit
	}
	if req.CreditLimit != nil {
		patch.CreditLimit = *req.CreditLimit
	}
	if req.ExpiresAt != nil {
		patch.ExpiresAt = *req.ExpiresAt
	}
	if req.RPMLimit != nil {
		patch.RPMLimit = *req.RPMLimit
	}
	if req.IPLimit != nil {
		patch.IPLimit = *req.IPLimit
	}
	if req.IPAllowlist != nil {
		patch.IPAllowlist = sanitizeIPAllowlist(*req.IPAllowlist)
	}
	if req.TPMLimit != nil {
		patch.TPMLimit = *req.TPMLimit
	}
	if req.BoundAccountIDs != nil {
		patch.BoundAccountIDs = *req.BoundAccountIDs
	}
	// Models is authoritative when present; Model is the legacy single-value alias.
	if req.Models != nil {
		patch.Models = *req.Models
	} else if req.Model != nil {
		if m := *req.Model; m != "" {
			patch.Models = []string{m}
		} else {
			patch.Models = nil
		}
	}

	if err := config.UpdateApiKey(id, patch); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// If the key value or IP limits changed, drop the tracked IP allow-set so stale
	// slots don't linger.
	if h.ipLimiter != nil && (req.Key != nil || req.IPLimit != nil || req.IPAllowlist != nil) {
		h.ipLimiter.forget(id)
	}

	updated := config.GetApiKeyEntry(id)
	if updated == nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to reload entry"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"apiKey":  toApiKeyView(*updated),
	})
}

// apiKeyExportView is one exported row. By default it is a masked usage report:
// KeyMasked is always set and Key is omitted, so the file is safe to hand to a
// customer. When the operator opts into includeSecrets, Key carries the cleartext
// value and the row becomes a restorable backup — see apiExportApiKeys.
//
// Everything needed to reconstruct the key is included (limits, rate/IP caps,
// bindings, lifetime counters) so an export + import round-trip is lossless.
type apiKeyExportView struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	KeyMasked string `json:"keyMasked"`
	// Key is the cleartext value, present only on an includeSecrets export.
	Key               string   `json:"key,omitempty"`
	Enabled           bool     `json:"enabled"`
	RequestsCount     int64    `json:"requestsCount"`
	TokensUsed        int64    `json:"tokensUsed"`
	CreditsUsed       float64  `json:"creditsUsed"`
	TokenLimit        int64    `json:"tokenLimit"`
	CreditLimit       float64  `json:"creditLimit"`
	ExpiresAt         int64    `json:"expiresAt"`
	CreatedAt         int64    `json:"createdAt"`
	LastUsedAt        int64    `json:"lastUsedAt"`
	TokenPercentUsed  float64  `json:"tokenPercentUsed"`
	CreditPercentUsed float64  `json:"creditPercentUsed"`
	OverToken         bool     `json:"overToken"`
	OverCredit        bool     `json:"overCredit"`
	Expired           bool     `json:"expired"`
	RPMLimit          int      `json:"rpmLimit,omitempty"`
	IPLimit           int      `json:"ipLimit,omitempty"`
	IPAllowlist       []string `json:"ipAllowlist,omitempty"`
	TPMLimit          int      `json:"tpmLimit,omitempty"`
	BoundAccountIDs   []string `json:"boundAccountIds,omitempty"`
	Models            []string `json:"models,omitempty"`
	LifetimeTokens    int64    `json:"lifetimeTokens"`
	LifetimeCredits   float64  `json:"lifetimeCredits"`
	LifetimeRequests  int64    `json:"lifetimeRequests"`
}

func toApiKeyExportView(e config.ApiKeyEntry, includeSecrets bool) apiKeyExportView {
	overToken, overCredit := config.ApiKeyOverLimit(e)
	tokenPct := 0.0
	if e.TokenLimit > 0 {
		tokenPct = float64(e.TokensUsed) / float64(e.TokenLimit) * 100
	}
	creditPct := 0.0
	if e.CreditLimit > 0 {
		creditPct = e.CreditsUsed / e.CreditLimit * 100
	}
	v := apiKeyExportView{
		ID:                e.ID,
		Name:              e.Name,
		KeyMasked:         config.MaskApiKey(e.Key),
		Enabled:           e.Enabled,
		RequestsCount:     e.RequestsCount,
		TokensUsed:        e.TokensUsed,
		CreditsUsed:       e.CreditsUsed,
		TokenLimit:        e.TokenLimit,
		CreditLimit:       e.CreditLimit,
		ExpiresAt:         e.ExpiresAt,
		CreatedAt:         e.CreatedAt,
		LastUsedAt:        e.LastUsedAt,
		TokenPercentUsed:  tokenPct,
		CreditPercentUsed: creditPct,
		OverToken:         overToken,
		OverCredit:        overCredit,
		Expired:           config.ApiKeyExpired(e),
		RPMLimit:          e.RPMLimit,
		IPLimit:           e.IPLimit,
		IPAllowlist:       e.IPAllowlist,
		TPMLimit:          e.TPMLimit,
		BoundAccountIDs:   e.BoundAccountIDs,
		Models:            e.Models,
		LifetimeTokens:    e.LifetimeTokens,
		LifetimeCredits:   e.LifetimeCredits,
		LifetimeRequests:  e.LifetimeRequests,
	}
	if includeSecrets {
		v.Key = e.Key
	}
	return v
}

// apiExportApiKeys handles POST /admin/api/api-keys/export.
// Body: {"ids": [...], "includeSecrets": false}; empty/missing ids = all.
//
// The default is a masked usage report. includeSecrets=true returns cleartext key
// values so the file can be restored on another server via apiImportApiKeys — that
// file is a credential dump for every listed customer, so the request is logged.
func (h *Handler) apiExportApiKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs            []string `json:"ids"`
		IncludeSecrets bool     `json:"includeSecrets"`
	}
	// Empty/invalid body = export all, masked.
	_ = json.NewDecoder(r.Body).Decode(&req)

	entries := config.ListApiKeys()
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool, len(req.IDs))
		for _, id := range req.IDs {
			idSet[id] = true
		}
		filtered := entries[:0]
		for _, e := range entries {
			if idSet[e.ID] {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	views := make([]apiKeyExportView, len(entries))
	for i, e := range entries {
		views[i] = toApiKeyExportView(e, req.IncludeSecrets)
	}

	if req.IncludeSecrets {
		logger.Infof("[ApiKeyExport] cleartext export of %d key(s)", len(views))
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"version":        config.Version,
		"exportedAt":     time.Now().Unix(),
		"includeSecrets": req.IncludeSecrets,
		"apiKeys":        views,
	})
}

// apiKeyImportEntry is one row accepted by apiImportApiKeys. It deliberately mirrors
// apiKeyExportView rather than reusing config.ApiKeyEntry, so request bodies cannot
// set internal fields (Migrated) that are not the caller's to decide.
type apiKeyImportEntry struct {
	Name string `json:"name,omitempty"`
	Key  string `json:"key,omitempty"`
	// KeyMasked is not imported. It is read only to tell a masked usage report apart
	// from a real backup, so we can return a useful error instead of "0 imported".
	KeyMasked   string   `json:"keyMasked,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
	TokenLimit  int64    `json:"tokenLimit,omitempty"`
	CreditLimit float64  `json:"creditLimit,omitempty"`
	ExpiresAt   int64    `json:"expiresAt,omitempty"`
	CreatedAt   int64    `json:"createdAt,omitempty"`
	LastUsedAt  int64    `json:"lastUsedAt,omitempty"`
	RPMLimit    int      `json:"rpmLimit,omitempty"`
	IPLimit     int      `json:"ipLimit,omitempty"`
	IPAllowlist []string `json:"ipAllowlist,omitempty"`
	TPMLimit    int      `json:"tpmLimit,omitempty"`

	BoundAccountIDs []string `json:"boundAccountIds,omitempty"`
	Models          []string `json:"models,omitempty"`
	Model           string   `json:"model,omitempty"`

	// Usage counters are carried over so a restored key resumes its quota rather
	// than handing the customer a fresh allowance.
	TokensUsed       int64   `json:"tokensUsed,omitempty"`
	CreditsUsed      float64 `json:"creditsUsed,omitempty"`
	RequestsCount    int64   `json:"requestsCount,omitempty"`
	LifetimeTokens   int64   `json:"lifetimeTokens,omitempty"`
	LifetimeCredits  float64 `json:"lifetimeCredits,omitempty"`
	LifetimeRequests int64   `json:"lifetimeRequests,omitempty"`
}

// decodeApiKeyImport accepts both shapes an operator is likely to paste:
//
//	{"apiKeys": [...]}   — the export envelope, downloaded from this panel
//	[...]                — a bare array, e.g. `jq '.apiKeys' data/config.json`
func decodeApiKeyImport(body []byte) ([]apiKeyImportEntry, error) {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") {
		var entries []apiKeyImportEntry
		if err := json.Unmarshal(body, &entries); err != nil {
			return nil, err
		}
		return entries, nil
	}
	var envelope struct {
		ApiKeys []apiKeyImportEntry `json:"apiKeys"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	return envelope.ApiKeys, nil
}

// apiRestoreApiKeys handles POST /admin/api/api-keys/import. It restores keys from an
// includeSecrets export (or a raw config.json apiKeys array) — the other half of
// migrating to a new server without hand-editing config.json.
//
// Not to be confused with apiImportApiKeys in apikey_batch.go, which imports *Kiro
// account* credentials as new upstream accounts.
//
// Duplicates of existing keys are skipped, so importing the same file twice is safe.
func (h *Handler) apiRestoreApiKeys(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxApiKeyImportBytes))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Request body too large or unreadable"})
		return
	}

	entries, err := decodeApiKeyImport(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(entries) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "no api keys found in payload"})
		return
	}

	// A masked export has keyMasked but no key. Importing it would create nothing, so
	// say what is actually wrong instead of reporting a successful no-op.
	usable, masked := 0, 0
	for _, e := range entries {
		if strings.TrimSpace(e.Key) != "" {
			usable++
		} else if strings.TrimSpace(e.KeyMasked) != "" {
			masked++
		}
	}
	if usable == 0 {
		msg := "no usable key values found in payload"
		if masked > 0 {
			msg = "this export is masked (keyMasked only) and cannot be imported — re-export with \"include real keys\" enabled"
		}
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
		return
	}

	toImport := make([]config.ApiKeyEntry, 0, len(entries))
	for _, e := range entries {
		// Default to enabled: a key omitting the field is almost always meant to work.
		enabled := true
		if e.Enabled != nil {
			enabled = *e.Enabled
		}
		toImport = append(toImport, config.ApiKeyEntry{
			Name:             e.Name,
			Key:              e.Key,
			Enabled:          enabled,
			CreatedAt:        e.CreatedAt,
			LastUsedAt:       e.LastUsedAt,
			ExpiresAt:        e.ExpiresAt,
			TokenLimit:       e.TokenLimit,
			CreditLimit:      e.CreditLimit,
			RPMLimit:         e.RPMLimit,
			IPLimit:          e.IPLimit,
			IPAllowlist:      sanitizeIPAllowlist(e.IPAllowlist),
			TPMLimit:         e.TPMLimit,
			BoundAccountIDs:  e.BoundAccountIDs,
			Models:           mergeModelList(e.Models, e.Model),
			TokensUsed:       e.TokensUsed,
			CreditsUsed:      e.CreditsUsed,
			RequestsCount:    e.RequestsCount,
			LifetimeTokens:   e.LifetimeTokens,
			LifetimeCredits:  e.LifetimeCredits,
			LifetimeRequests: e.LifetimeRequests,
		})
	}

	result, err := config.ImportApiKeys(toImport)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	views := make([]apiKeyView, len(result.Imported))
	for i, e := range result.Imported {
		views[i] = toApiKeyView(e)
	}
	logger.Infof("[ApiKeyImport] %d row(s): %d imported, %d skipped (duplicate), %d invalid",
		len(entries), len(result.Imported), result.Skipped, result.Invalid)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"total":    len(entries),
		"imported": len(result.Imported),
		"skipped":  result.Skipped,
		"invalid":  result.Invalid,
		"apiKeys":  views,
	})
}

func (h *Handler) apiDeleteApiKey(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteApiKey(id); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiResetApiKeyUsage(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.ResetApiKeyUsage(id); err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	// Clear the tracked IP allow-set so a reset also frees all IP slots.
	if h.ipLimiter != nil {
		h.ipLimiter.forget(id)
	}
	updated := config.GetApiKeyEntry(id)
	if updated == nil {
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"apiKey":  toApiKeyView(*updated),
	})
}

// apiResetApiKeyUsageAll wipes BOTH the current-period and lifetime counters, resetting
// the key as if it were new. Unlike apiResetApiKeyUsage (which keeps the lifetime total),
// this is the destructive "Reset All" action.
func (h *Handler) apiResetApiKeyUsageAll(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.ResetApiKeyUsageAll(id); err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	// Clear the tracked IP allow-set so a full reset also frees all IP slots.
	if h.ipLimiter != nil {
		h.ipLimiter.forget(id)
	}
	updated := config.GetApiKeyEntry(id)
	if updated == nil {
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"apiKey":  toApiKeyView(*updated),
	})
}
