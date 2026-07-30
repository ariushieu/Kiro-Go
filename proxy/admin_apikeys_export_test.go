package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedExportKeys inserts four entries covering: unlimited, under-limit,
// over-token-limit, and expired. Returns their IDs in insertion order.
func seedExportKeys(t *testing.T) (unlimited, under, overTok, expired string) {
	t.Helper()

	e1, err := config.AddApiKey(config.ApiKeyEntry{Name: "unlimited", Key: "sk-unlimited-secret", Enabled: true})
	if err != nil {
		t.Fatalf("seed unlimited: %v", err)
	}
	e2, err := config.AddApiKey(config.ApiKeyEntry{Name: "under", Key: "sk-under-secret", Enabled: true, TokenLimit: 1000, CreditLimit: 10})
	if err != nil {
		t.Fatalf("seed under: %v", err)
	}
	e3, err := config.AddApiKey(config.ApiKeyEntry{Name: "overtok", Key: "sk-overtok-secret", Enabled: true, TokenLimit: 100})
	if err != nil {
		t.Fatalf("seed overtok: %v", err)
	}
	e4, err := config.AddApiKey(config.ApiKeyEntry{Name: "expired", Key: "sk-expired-secret", Enabled: true, ExpiresAt: time.Now().Unix() - 3600})
	if err != nil {
		t.Fatalf("seed expired: %v", err)
	}

	// Drive usage counters. under: 500/1000 tokens, 5/10 credits. overtok: 200/100 tokens.
	if err := config.RecordApiKeyUsage(e2.ID, 500, 5); err != nil {
		t.Fatalf("usage under: %v", err)
	}
	if err := config.RecordApiKeyUsage(e3.ID, 200, 0); err != nil {
		t.Fatalf("usage overtok: %v", err)
	}
	return e1.ID, e2.ID, e3.ID, e4.ID
}

// decodeExport posts an export request and returns the rows by ID. When
// includeSecrets is false it also asserts the raw key values (seeded as "sk-*-secret")
// never reach the response; when true it asserts the opposite, since a cleartext
// export is only useful if the secret IS present.
func decodeExport(t *testing.T, ids []string, includeSecrets bool) map[string]apiKeyExportView {
	t.Helper()
	body := map[string]interface{}{"includeSecrets": includeSecrets}
	if ids != nil {
		body["ids"] = ids
	}
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/admin/api/api-keys/export", strings.NewReader(string(b)))
	w := httptest.NewRecorder()

	h := &Handler{}
	h.apiExportApiKeys(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	hasSecret := strings.Contains(w.Body.String(), "secret")
	if !includeSecrets && hasSecret {
		t.Fatalf("raw key value leaked in masked output: %s", w.Body.String())
	}
	if includeSecrets && !hasSecret {
		t.Fatalf("includeSecrets export omitted the raw key: %s", w.Body.String())
	}

	var out struct {
		Version        string             `json:"version"`
		ExportedAt     int64              `json:"exportedAt"`
		IncludeSecrets bool               `json:"includeSecrets"`
		ApiKeys        []apiKeyExportView `json:"apiKeys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ExportedAt == 0 {
		t.Fatalf("expected exportedAt to be set")
	}
	if out.IncludeSecrets != includeSecrets {
		t.Fatalf("includeSecrets echo: want %v, got %v", includeSecrets, out.IncludeSecrets)
	}
	byID := make(map[string]apiKeyExportView, len(out.ApiKeys))
	for _, v := range out.ApiKeys {
		byID[v.ID] = v
	}
	return byID
}

func TestExportApiKeysAllMaskedAndDerived(t *testing.T) {
	mustInitConfig(t)
	_, under, overTok, expired := seedExportKeys(t)

	got := decodeExport(t, nil, false)
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(got))
	}

	u := got[under]
	if !strings.HasPrefix(u.KeyMasked, "sk-") || !strings.Contains(u.KeyMasked, "***") {
		t.Fatalf("under key not masked: %q", u.KeyMasked)
	}
	if u.Key != "" {
		t.Fatalf("masked export must omit Key, got %q", u.Key)
	}
	if u.TokenPercentUsed != 50 {
		t.Fatalf("under tokenPercentUsed: want 50, got %v", u.TokenPercentUsed)
	}
	if u.CreditPercentUsed != 50 {
		t.Fatalf("under creditPercentUsed: want 50, got %v", u.CreditPercentUsed)
	}
	if u.OverToken || u.OverCredit || u.Expired {
		t.Fatalf("under should not be over/expired: %+v", u)
	}

	o := got[overTok]
	if !o.OverToken {
		t.Fatalf("overtok should be OverToken: %+v", o)
	}
	if o.TokenPercentUsed != 200 {
		t.Fatalf("overtok tokenPercentUsed: want 200, got %v", o.TokenPercentUsed)
	}

	e := got[expired]
	if !e.Expired {
		t.Fatalf("expired key should have Expired=true: %+v", e)
	}
	// Unlimited: no limits => percent 0.
	for _, v := range got {
		if v.Name == "unlimited" && (v.TokenPercentUsed != 0 || v.CreditPercentUsed != 0) {
			t.Fatalf("unlimited should have 0 percents: %+v", v)
		}
	}
}

func TestExportApiKeysFilterByIDs(t *testing.T) {
	mustInitConfig(t)
	unlimited, under, _, _ := seedExportKeys(t)

	got := decodeExport(t, []string{unlimited, under}, false)
	if len(got) != 2 {
		t.Fatalf("expected 2 filtered entries, got %d", len(got))
	}
	if _, ok := got[unlimited]; !ok {
		t.Fatalf("expected unlimited in filtered result")
	}
	if _, ok := got[under]; !ok {
		t.Fatalf("expected under in filtered result")
	}
}

// TestExportApiKeysIncludeSecrets covers the opt-in path: cleartext key present, and
// the masked column still populated so the CSV report keeps working either way.
func TestExportApiKeysIncludeSecrets(t *testing.T) {
	mustInitConfig(t)
	_, under, _, _ := seedExportKeys(t)

	got := decodeExport(t, []string{under}, true)
	u := got[under]
	if u.Key != "sk-under-secret" {
		t.Fatalf("expected cleartext key, got %q", u.Key)
	}
	if !strings.Contains(u.KeyMasked, "***") {
		t.Fatalf("KeyMasked should still be set alongside Key: %q", u.KeyMasked)
	}
}

// TestExportApiKeysCarriesRestorableFields guards the fields the export used to drop.
// Without them an export/import round-trip silently loses rate limits and bindings.
func TestExportApiKeysCarriesRestorableFields(t *testing.T) {
	mustInitConfig(t)

	entry, err := config.AddApiKey(config.ApiKeyEntry{
		Name:        "full",
		Key:         "sk-full-secret",
		Enabled:     true,
		TokenLimit:  5000,
		RPMLimit:    30,
		IPLimit:     3,
		IPAllowlist: []string{"203.0.113.4"},
		TPMLimit:    9000,
		Models:      []string{"claude-sonnet-4-5"},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := config.RecordApiKeyUsage(entry.ID, 700, 1.5); err != nil {
		t.Fatalf("usage: %v", err)
	}

	got := decodeExport(t, []string{entry.ID}, true)[entry.ID]
	if got.RPMLimit != 30 || got.IPLimit != 3 || got.TPMLimit != 9000 {
		t.Fatalf("rate/IP limits not exported: %+v", got)
	}
	if len(got.IPAllowlist) != 1 || got.IPAllowlist[0] != "203.0.113.4" {
		t.Fatalf("ipAllowlist not exported: %+v", got.IPAllowlist)
	}
	if len(got.Models) != 1 || got.Models[0] != "claude-sonnet-4-5" {
		t.Fatalf("models not exported: %+v", got.Models)
	}
	if got.LifetimeTokens != 700 || got.LifetimeCredits != 1.5 || got.LifetimeRequests != 1 {
		t.Fatalf("lifetime counters not exported: %+v", got)
	}
}
