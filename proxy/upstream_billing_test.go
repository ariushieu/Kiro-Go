package proxy

import (
	"kiro-go/config"
	"math"
	"testing"
)

func TestCalculateUpstreamBillingFableSample(t *testing.T) {
	account := &config.Account{Pricing: &config.UpstreamPricing{
		InputPerMillion: 10, OutputPerMillion: 50,
		CacheReadPerMillion: 1, CacheWrite5mPerMillion: 12.5,
		CacheWrite1hPerMillion: 20, Markup: 1.4, MinChargeUSD: 0.001,
	}}
	got := CalculateUpstreamBilling(account, UpstreamUsage{InputTokens: 8679, OutputTokens: 151})
	assertBillingNear(t, got.SourceCostUSD, 0.09434)
	assertBillingNear(t, got.ChargeUSD, 0.133)
	assertBillingNear(t, got.ProfitUSD, 0.03866)
}

func TestCalculateUpstreamBillingCacheAndMinimumCharge(t *testing.T) {
	account := &config.Account{Pricing: &config.UpstreamPricing{
		InputPerMillion: 10, OutputPerMillion: 50,
		CacheReadPerMillion: 1, CacheWrite5mPerMillion: 12.5,
		CacheWrite1hPerMillion: 20, Markup: 1.4, MinChargeUSD: 0.001,
	}}
	got := CalculateUpstreamBilling(account, UpstreamUsage{
		InputTokens: 1, OutputTokens: 1, CacheReadInputTokens: 10,
		CacheCreation5mTokens: 2, CacheCreation1hTokens: 3,
	})
	assertBillingNear(t, got.SourceCostUSD, 0.000155)
	assertBillingNear(t, got.ChargeUSD, 0.001)
	assertBillingNear(t, got.ProfitUSD, 0.000845)
}

func TestCalculateUpstreamBillingDisabledWithoutPricing(t *testing.T) {
	got := CalculateUpstreamBilling(&config.Account{}, UpstreamUsage{InputTokens: 1000, OutputTokens: 1000})
	if got != (UpstreamBilling{}) {
		t.Fatalf("expected zero billing without pricing, got %#v", got)
	}
}

func assertBillingNear(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.0000001 {
		t.Fatalf("got %.8f, want %.8f", got, want)
	}
}
