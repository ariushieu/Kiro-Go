package proxy

import (
	"fmt"
	"kiro-go/config"
)

// CallUpstreamAPI is the single dispatch point used by request handlers. Kiro
// remains the default for legacy accounts; additional backends implement the
// same callback contract so response formatting and accounting stay shared.
func CallUpstreamAPI(account *config.Account, model string, payload *KiroPayload, callback *KiroStreamCallback) error {
	if account == nil {
		return fmt.Errorf("missing upstream account")
	}
	switch account.EffectiveBackend() {
	case config.BackendKiro:
		return CallKiroAPI(account, payload, callback)
	case config.BackendOpenAICompatible:
		switch account.EffectiveAPIFormat() {
		case config.APIFormatOpenAI:
			return CallOpenAICompatibleAPI(account, model, payload, callback)
		case config.APIFormatAnthropic:
			return CallAnthropicCompatibleAPI(account, model, payload, callback)
		default:
			return fmt.Errorf("unsupported custom upstream apiFormat %q", account.APIFormat)
		}
	default:
		return fmt.Errorf("unsupported upstream backend %q", account.Backend)
	}
}
