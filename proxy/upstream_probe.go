package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Probes are admin-triggered and synchronous behind a click, so unlike the
// streaming path they get hard deadlines: a gateway that hangs must fail the
// button rather than hold the admin request open.
const (
	upstreamProbeListTimeout = 20 * time.Second
	upstreamProbeChatTimeout = 60 * time.Second
	maxUpstreamProbeBody     = 8 << 10
)

// upstreamModelsURL derives the model-listing endpoint. Both the OpenAI and the
// Anthropic API expose /v1/models, so the two formats share this.
func upstreamModelsURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid baseURL")
	}
	path := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(path, "/models"):
	case strings.HasSuffix(path, "/chat/completions"):
		path = strings.TrimSuffix(path, "/chat/completions") + "/models"
	case strings.HasSuffix(path, "/messages"):
		path = strings.TrimSuffix(path, "/messages") + "/models"
	case strings.HasSuffix(path, "/v1"):
		path += "/models"
	default:
		path += "/v1/models"
	}
	u.Path = path
	return u.String(), nil
}

// ListCustomUpstreamModels asks a custom upstream which models it serves.
//
// Plenty of resold gateways do not implement /v1/models at all. That is not a
// configuration error, so a 404/405 comes back as (nil, nil) — an empty list
// with no error — and the caller reports the step as skipped rather than failed.
func ListCustomUpstreamModels(ctx context.Context, account *config.Account) ([]string, error) {
	if account == nil {
		return nil, fmt.Errorf("missing upstream account")
	}
	endpoint, err := upstreamModelsURL(account.BaseURL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamProbeListTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	applyUpstreamAuthHeaders(req, account)
	req.Header.Set("Accept", "application/json")

	proxyURL, _, err := SelectProxyForAccount(account)
	if err != nil {
		return nil, err
	}
	resp, err := GetCustomUpstreamClientForProxy(proxyURL).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamProbeBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, upstreamProbeSummary(body))
	}

	// OpenAI shape is {"data":[{"id":...}]}; some gateways return a bare array,
	// and Anthropic returns the same {"data":[...]} envelope.
	var envelope struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	ids := make([]string, 0, 8)
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Data) > 0 {
		for _, m := range envelope.Data {
			if id := strings.TrimSpace(m.ID); id != "" {
				ids = append(ids, id)
			} else if name := strings.TrimSpace(m.Name); name != "" {
				ids = append(ids, name)
			}
		}
		return ids, nil
	}
	var bare []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &bare); err == nil {
		for _, m := range bare {
			if id := strings.TrimSpace(m.ID); id != "" {
				ids = append(ids, id)
			} else if name := strings.TrimSpace(m.Name); name != "" {
				ids = append(ids, name)
			}
		}
		return ids, nil
	}
	// Reachable and authorised, but in a shape we do not recognise. Treat it like
	// a missing endpoint so the button still reports the chat result.
	return nil, nil
}

// applyUpstreamAuthHeaders mirrors the auth scheme each format's call path uses,
// so a probe fails for the same reasons a real request would.
func applyUpstreamAuthHeaders(req *http.Request, account *config.Account) {
	if account.EffectiveAPIFormat() == config.APIFormatAnthropic {
		req.Header.Set("Authorization", "Bearer "+account.ApiKey)
		req.Header.Set("x-api-key", account.ApiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		return
	}
	req.Header.Set("Authorization", "Bearer "+account.ApiKey)
}

// ProbeCustomUpstreamChat sends the smallest possible non-streaming completion
// and reports the assistant text. This is the only check that proves the
// configured model is actually callable with this key.
func ProbeCustomUpstreamChat(ctx context.Context, account *config.Account, model string) (string, error) {
	if account == nil {
		return "", fmt.Errorf("missing upstream account")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("no model to test")
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamProbeChatTimeout)
	defer cancel()

	var (
		endpoint string
		payload  []byte
		err      error
	)
	if account.EffectiveAPIFormat() == config.APIFormatAnthropic {
		endpoint, err = anthropicMessagesURL(account.BaseURL)
		if err != nil {
			return "", err
		}
		payload, err = json.Marshal(map[string]interface{}{
			"model":      model,
			"max_tokens": 16,
			"stream":     false,
			"messages": []map[string]interface{}{
				{"role": "user", "content": "say ok"},
			},
		})
	} else {
		endpoint, err = openAIChatCompletionsURL(account.BaseURL)
		if err != nil {
			return "", err
		}
		payload, err = json.Marshal(map[string]interface{}{
			"model":      model,
			"max_tokens": 16,
			"stream":     false,
			"messages": []map[string]interface{}{
				{"role": "user", "content": "say ok"},
			},
		})
	}
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	applyUpstreamAuthHeaders(req, account)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	proxyURL, _, err := SelectProxyForAccount(account)
	if err != nil {
		return "", err
	}
	resp, err := GetCustomUpstreamClientForProxy(proxyURL).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamProbeBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, upstreamProbeSummary(body))
	}
	return extractProbeReplyText(body, account.EffectiveAPIFormat()), nil
}

// extractProbeReplyText pulls the assistant text out of either response shape.
// An empty string is not an error: some gateways answer a 16-token request with
// only a stop reason, and the 2xx alone already proves the model is callable.
func extractProbeReplyText(body []byte, format string) string {
	if format == config.APIFormatAnthropic {
		var out struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(body, &out); err == nil {
			var sb strings.Builder
			for _, block := range out.Content {
				if block.Type == "" || block.Type == "text" {
					sb.WriteString(block.Text)
				}
			}
			return strings.TrimSpace(sb.String())
		}
		return ""
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err == nil && len(out.Choices) > 0 {
		return strings.TrimSpace(out.Choices[0].Message.Content)
	}
	return ""
}

// upstreamProbeSummary condenses an error body into one short line. Unlike the
// customer-facing path (which masks third-party errors entirely — see
// clientFacingUpstreamError) the admin running this test needs the provider's
// own wording to fix their configuration, so this is deliberately not masked.
// It is only ever returned from the password-gated admin API.
func upstreamProbeSummary(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "empty response body"
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		if msg := strings.TrimSpace(envelope.Error.Message); msg != "" {
			text = msg
		} else if msg := strings.TrimSpace(envelope.Message); msg != "" {
			text = msg
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	const maxLen = 300
	if len(text) > maxLen {
		return text[:maxLen] + "…"
	}
	return text
}
