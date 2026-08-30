package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kiro-go/config"
)

// seedRateKey creates an enabled key and clears the request-log ring so a test
// can assert on the single entry its own call produces.
func seedRateKey(t *testing.T, name string) config.ApiKeyEntry {
	t.Helper()
	mustInitConfig(t)
	requestLog.reset()
	created, err := config.AddApiKey(config.ApiKeyEntry{Name: name, Key: "sk-" + name, Enabled: true})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return created
}

// The default rate must leave billing exactly as it was before the setting
// existed: an operator who never touches it sees no change.
func TestCreditRateDefaultsToExactBilling(t *testing.T) {
	created := seedRateKey(t, "rate-default")

	if got := config.GetCreditRate(); got != 1 {
		t.Fatalf("expected default rate 1, got %v", got)
	}

	h := &Handler{}
	h.recordSuccessForApiKey(created.ID, 100, 200, 2.0, "claude-test", nil, "claude", time.Time{})

	got := config.GetApiKeyEntry(created.ID)
	if got.CreditsUsed != 2.0 {
		t.Fatalf("expected CreditsUsed=2.0 at rate 1, got %v", got.CreditsUsed)
	}
}

// The multiplier scales the customer debit and nothing else. Token counters in
// particular must stay at the real count: the client counts those itself and the
// usage block this proxy returns reports them unscaled.
func TestCreditRateScalesCreditsNotTokens(t *testing.T) {
	created := seedRateKey(t, "rate-scaled")
	if err := config.UpdateCreditRate(1.5); err != nil {
		t.Fatalf("set rate: %v", err)
	}

	h := &Handler{}
	h.recordSuccessForApiKey(created.ID, 100, 200, 2.0, "claude-test", nil, "claude", time.Time{})

	got := config.GetApiKeyEntry(created.ID)
	if got.CreditsUsed != 3.0 {
		t.Fatalf("expected CreditsUsed=3.0 (2.0 x 1.5), got %v", got.CreditsUsed)
	}
	if got.TokensUsed != 300 {
		t.Fatalf("expected TokensUsed=300 unscaled, got %d", got.TokensUsed)
	}
}

// The /check page shows both a per-request log and the running total, so the
// charge written to the ledger and the charge written to the log must be the same
// number. sourceCost stays at the real upstream spend so Profit is the true margin.
func TestCreditRateKeepsLedgerAndLogReconciled(t *testing.T) {
	created := seedRateKey(t, "rate-log")
	if err := config.UpdateCreditRate(1.5); err != nil {
		t.Fatalf("set rate: %v", err)
	}

	h := &Handler{}
	h.recordSuccessForApiKeyWithCost(created.ID, 10, 20, 2.0, 2.0, "claude-test", nil, "claude", time.Time{})

	entries := requestLog.snapshot()
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	e := entries[0]

	got := config.GetApiKeyEntry(created.ID)
	if e.Credits != got.CreditsUsed {
		t.Fatalf("log charge %v does not reconcile with ledger %v", e.Credits, got.CreditsUsed)
	}
	if e.Credits != 3.0 {
		t.Fatalf("expected logged charge 3.0, got %v", e.Credits)
	}
	if e.SourceCost != 2.0 {
		t.Fatalf("expected SourceCost to stay at real spend 2.0, got %v", e.SourceCost)
	}
	if e.Profit != 1.0 {
		t.Fatalf("expected Profit=1.0 (3.0 charged - 2.0 cost), got %v", e.Profit)
	}
	if e.TotalTokens != 30 {
		t.Fatalf("expected TotalTokens=30 unscaled, got %d", e.TotalTokens)
	}
}

// A free request must stay free rather than pick up a charge from the multiplier.
func TestCreditRateLeavesZeroChargeAlone(t *testing.T) {
	created := seedRateKey(t, "rate-zero")
	if err := config.UpdateCreditRate(2); err != nil {
		t.Fatalf("set rate: %v", err)
	}

	h := &Handler{}
	h.recordSuccessForApiKey(created.ID, 50, 50, 0, "claude-test", nil, "claude", time.Time{})

	if got := config.GetApiKeyEntry(created.ID); got.CreditsUsed != 0 {
		t.Fatalf("expected CreditsUsed=0, got %v", got.CreditsUsed)
	}
}

// The real upstream spend is the operator's cost basis. It is recorded per key so a
// billing period can be reconciled after a restart, but it must never reach the
// customer: this asserts against the raw response bytes rather than a decoded struct,
// so adding a leaking field to any of these payloads fails here even if the field is
// one nobody thought to check for.
func TestSourceCreditsNeverLeakToCustomer(t *testing.T) {
	created := seedRateKey(t, "rate-leak")
	if err := config.UpdateCreditRate(1.5); err != nil {
		t.Fatalf("set rate: %v", err)
	}

	h := &Handler{usage: newUsageStats()}
	h.recordSuccessForApiKeyWithCost(created.ID, 100, 200, 2.0, 2.0, "claude-test", nil, "claude", time.Time{})

	// Anything that would let a customer back out the multiplier or the real cost.
	forbidden := []string{"sourceCost", "sourceCredits", "sourceCreditsUsed", "profit", "margin", "creditRate"}

	for _, tc := range []struct {
		name    string
		path    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"info", "/v1/key/info", h.apiKeySelfInfo},
		{"logs", "/v1/key/logs", h.apiKeySelfLogs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+created.Key)
			rec := httptest.NewRecorder()
			tc.handler(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			for _, field := range forbidden {
				if strings.Contains(strings.ToLower(body), strings.ToLower(field)) {
					t.Fatalf("%s leaked %q to the customer: %s", tc.path, field, body)
				}
			}
		})
	}

	// The admin view is where both numbers are supposed to be visible.
	adminRec := httptest.NewRecorder()
	h.apiGetUsageSummary(adminRec, httptest.NewRequest(http.MethodGet, "/admin/api/usage-summary", nil))
	var summary struct {
		Keys []struct {
			CreditsUsed       float64 `json:"creditsUsed"`
			SourceCreditsUsed float64 `json:"sourceCreditsUsed"`
			Margin            float64 `json:"margin"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(adminRec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode usage summary: %v", err)
	}
	if len(summary.Keys) != 1 {
		t.Fatalf("expected 1 key in summary, got %d", len(summary.Keys))
	}
	k := summary.Keys[0]
	if k.CreditsUsed != 3.0 || k.SourceCreditsUsed != 2.0 || k.Margin != 1.0 {
		t.Fatalf("admin must see charged 3.0 / real 2.0 / margin 1.0, got %+v", k)
	}

	// Moving keys between servers must carry the cost basis, otherwise a migration
	// silently resets every key's real spend to zero and the margin reads as 100%.
	entry := config.GetApiKeyEntry(created.ID)
	if exported := toApiKeyExportView(*entry, false); exported.SourceCreditsUsed != 2.0 {
		t.Fatalf("export dropped the cost basis: got %v want 2.0", exported.SourceCreditsUsed)
	}
}

// Out-of-range rates are refused, not clamped: a typo like 15 instead of 1.5 would
// drain a customer's key in a handful of requests.
func TestUpdateCreditRateRejectsOutOfRange(t *testing.T) {
	mustInitConfig(t)

	for _, rate := range []float64{0, 0.5, -1, config.MaxCreditRate + 0.1, 15} {
		if err := config.UpdateCreditRate(rate); err == nil {
			t.Fatalf("expected rate %v to be rejected", rate)
		}
	}
	if got := config.GetCreditRate(); got != 1 {
		t.Fatalf("expected rate to stay at default after rejections, got %v", got)
	}

	if err := config.UpdateCreditRate(config.MaxCreditRate); err != nil {
		t.Fatalf("expected MaxCreditRate to be accepted: %v", err)
	}
}
