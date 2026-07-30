package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"kiro-go/config"
	"kiro-go/logger"
)

var (
	errAccountNotFound        = errors.New("account not found")
	errUpstreamAPIKeyRequired = errors.New("apiKey is required")
	errUnsupportedProbeFormat = errors.New("unsupported apiFormat")
)

// upstreamTestRequest is the admin panel's test-connection payload.
//
// APIKey is optional: when AccountID names an existing custom-upstream account
// and APIKey is blank, the stored secret is used. That is what lets the panel
// re-test a saved account without ever holding the cleartext key in the browser.
type upstreamTestRequest struct {
	AccountID string `json:"accountId"`
	BaseURL   string `json:"baseURL"`
	APIKey    string `json:"apiKey"`
	APIFormat string `json:"apiFormat"`
	Model     string `json:"model"`
	ProxyURL  string `json:"proxyURL"`
}

// maskUpstreamApiKey renders a stored upstream secret for display. Empty stays
// empty so the panel can tell "no key set" apart from "key set but hidden".
func maskUpstreamApiKey(key string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	return maskKey(key)
}

// upstreamTestStep is one check's outcome. Status is "ok", "failed" or
// "skipped"; skipped covers a gateway that simply does not implement /v1/models.
type upstreamTestStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type upstreamTestResponse struct {
	Success bool               `json:"success"`
	Steps   []upstreamTestStep `json:"steps"`
	Models  []string           `json:"models,omitempty"`
	Reply   string             `json:"reply,omitempty"`
	Error   string             `json:"error,omitempty"`
}

// apiTestUpstreamConnection probes a custom upstream's credentials before the
// admin commits them. It runs two checks: list the served models, then send a
// minimal completion. The second is the one that proves the configured model is
// callable; the first is best-effort because many resold gateways omit it.
func (h *Handler) apiTestUpstreamConnection(w http.ResponseWriter, r *http.Request) {
	var req upstreamTestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeUpstreamTestError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	account, err := h.buildUpstreamProbeAccount(req)
	if err != nil {
		writeUpstreamTestError(w, http.StatusBadRequest, err.Error())
		return
	}

	steps := make([]upstreamTestStep, 0, 2)
	resp := upstreamTestResponse{}

	models, listErr := ListCustomUpstreamModels(r.Context(), account)
	switch {
	case listErr != nil:
		// A failed listing is reported but not fatal: the chat probe below is the
		// authoritative check, and some gateways guard /v1/models separately.
		steps = append(steps, upstreamTestStep{Step: "listModels", Status: "failed", Detail: listErr.Error()})
	case len(models) == 0:
		steps = append(steps, upstreamTestStep{Step: "listModels", Status: "skipped"})
	default:
		steps = append(steps, upstreamTestStep{Step: "listModels", Status: "ok"})
		resp.Models = models
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = firstConfiguredProbeModel(account, models)
	}
	if model == "" {
		steps = append(steps, upstreamTestStep{
			Step: "chat", Status: "failed", Detail: "no model configured to test",
		})
		resp.Steps = steps
		resp.Error = "no model configured to test"
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
		return
	}

	reply, chatErr := ProbeCustomUpstreamChat(r.Context(), account, model)
	if chatErr != nil {
		steps = append(steps, upstreamTestStep{Step: "chat", Status: "failed", Detail: chatErr.Error()})
		resp.Steps = steps
		resp.Error = chatErr.Error()
		logger.Warnf("[UpstreamTest] %s (%s) model %s failed: %v",
			account.BaseURL, account.EffectiveAPIFormat(), model, chatErr)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
		return
	}

	steps = append(steps, upstreamTestStep{Step: "chat", Status: "ok", Detail: model})
	resp.Success = true
	resp.Steps = steps
	resp.Reply = reply
	logger.Infof("[UpstreamTest] %s (%s) model %s OK",
		account.BaseURL, account.EffectiveAPIFormat(), model)
	json.NewEncoder(w).Encode(resp)
}

// buildUpstreamProbeAccount assembles a throwaway Account for the probe. The
// base URL goes through config.NormalizeUpstreamBaseURL so this endpoint cannot
// be used to reach plain-http internal hosts — it enforces the same https-only
// (loopback excepted) rule as account creation.
func (h *Handler) buildUpstreamProbeAccount(req upstreamTestRequest) (*config.Account, error) {
	var stored *config.Account
	if id := strings.TrimSpace(req.AccountID); id != "" {
		for _, a := range config.GetAccounts() {
			if a.ID == id {
				copied := a
				stored = &copied
				break
			}
		}
		if stored == nil {
			return nil, errAccountNotFound
		}
	}

	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" && stored != nil {
		baseURL = stored.BaseURL
	}
	normalized, err := config.NormalizeUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, err
	}

	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" && stored != nil {
		apiKey = stored.ApiKey
	}
	if apiKey == "" {
		return nil, errUpstreamAPIKeyRequired
	}

	apiFormat := strings.ToLower(strings.TrimSpace(req.APIFormat))
	if apiFormat == "" && stored != nil {
		apiFormat = stored.EffectiveAPIFormat()
	}
	if apiFormat == "" {
		apiFormat = config.APIFormatOpenAI
	}
	if apiFormat != config.APIFormatOpenAI && apiFormat != config.APIFormatAnthropic {
		return nil, errUnsupportedProbeFormat
	}

	proxyURL := strings.TrimSpace(req.ProxyURL)
	if proxyURL == "" && stored != nil {
		proxyURL = stored.ProxyURL
	}

	probe := &config.Account{
		Backend:   config.BackendOpenAICompatible,
		APIFormat: apiFormat,
		BaseURL:   normalized,
		ApiKey:    apiKey,
		ProxyURL:  proxyURL,
	}
	if stored != nil {
		probe.Models = stored.Models
	}
	return probe, nil
}

// firstConfiguredProbeModel picks what to send when the caller named no model:
// prefer a model the admin has configured on the account, and only fall back to
// something the upstream advertised.
func firstConfiguredProbeModel(account *config.Account, listed []string) string {
	if account != nil {
		for _, m := range account.Models {
			if m = strings.TrimSpace(m); m != "" && m != "*" {
				return m
			}
		}
	}
	for _, m := range listed {
		if m = strings.TrimSpace(m); m != "" {
			return m
		}
	}
	return ""
}

func writeUpstreamTestError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(upstreamTestResponse{Error: message})
}
