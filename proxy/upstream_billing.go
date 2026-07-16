package proxy

import (
	"kiro-go/config"
	"math"
)

// UpstreamUsage contains the billable token classes returned by a custom
// upstream. CacheCreationInputTokens is used as a 5-minute write fallback when
// the provider does not include the more detailed 5m/1h breakdown.
type UpstreamUsage struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	CacheCreation5mTokens    int
	CacheCreation1hTokens    int
}

type UpstreamBilling struct {
	SourceCostUSD float64
	ChargeUSD     float64
	ProfitUSD     float64
}

// CalculateUpstreamBilling uses integer micro-USD internally. At USD N per
// million tokens, one token costs exactly N micro-USD, which avoids cumulative
// binary floating-point drift in the persisted customer counters.
func CalculateUpstreamBilling(account *config.Account, usage UpstreamUsage) UpstreamBilling {
	if account == nil || account.Pricing == nil {
		return UpstreamBilling{}
	}
	p := account.Pricing
	write5m := usage.CacheCreation5mTokens
	write1h := usage.CacheCreation1hTokens
	if write5m == 0 && write1h == 0 {
		write5m = usage.CacheCreationInputTokens
	}

	sourceMicro := math.Ceil(
		float64(max(usage.InputTokens, 0))*p.InputPerMillion +
			float64(max(usage.OutputTokens, 0))*p.OutputPerMillion +
			float64(max(usage.CacheReadInputTokens, 0))*p.CacheReadPerMillion +
			float64(max(write5m, 0))*p.CacheWrite5mPerMillion +
			float64(max(write1h, 0))*p.CacheWrite1hPerMillion,
	)
	if sourceMicro <= 0 {
		return UpstreamBilling{}
	}
	markup := p.Markup
	if markup < 1 {
		markup = 1
	}
	chargeMicro := math.Ceil(sourceMicro * markup)
	minChargeMicro := math.Ceil(p.MinChargeUSD * 1_000_000)
	if chargeMicro < minChargeMicro {
		chargeMicro = minChargeMicro
	}
	// Customer debits are rounded upward to the nearest $0.001. This keeps the
	// ledger readable and prevents tiny requests from losing money to rounding.
	chargeMicro = math.Ceil(chargeMicro/1000) * 1000

	source := sourceMicro / 1_000_000
	charge := chargeMicro / 1_000_000
	return UpstreamBilling{SourceCostUSD: source, ChargeUSD: charge, ProfitUSD: charge - source}
}

func emitUpstreamBilling(account *config.Account, usage UpstreamUsage, callback *KiroStreamCallback) {
	if callback == nil {
		return
	}
	billing := CalculateUpstreamBilling(account, usage)
	if callback.OnSourceCost != nil {
		callback.OnSourceCost(billing.SourceCostUSD)
	}
	if callback.OnCredits != nil {
		callback.OnCredits(billing.ChargeUSD)
	}
}
