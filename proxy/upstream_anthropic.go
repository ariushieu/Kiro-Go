package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/google/uuid"
)

const maxAnthropicUpstreamErrorBytes = 4096

// defaultAnthropicUpstreamMaxTokens is the max_tokens sent to an Anthropic-format
// custom upstream when the client did NOT specify one. The Anthropic Messages API
// requires the field, so we cannot simply omit it like the OpenAI path does.
//
// This used to be 4096, which silently truncated long answers mid-sentence: any
// client that omits max_tokens (most OpenAI-style SDKs, and Claude clients relying
// on the API default) got cut off at 4096 output tokens while the response still
// reported a normal end_turn. 32000 matches what Claude Code itself requests and is
// within the output ceiling of current Claude models.
const defaultAnthropicUpstreamMaxTokens = 32000

type anthropicCompatibleRequest struct {
	Model       string          `json:"model"`
	Messages    []ClaudeMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature float64         `json:"temperature,omitempty"`
	TopP        float64         `json:"top_p,omitempty"`
	Stream      bool            `json:"stream"`
	Tools       []ClaudeTool    `json:"tools,omitempty"`
}

type anthropicToolAccumulator struct {
	id          string
	name        string
	initial     interface{}
	partialJSON strings.Builder
}

// CallAnthropicCompatibleAPI translates the shared request representation to
// Anthropic Messages. Authorization is sent in both forms used in practice:
// Claude Code gateways commonly consume Bearer tokens, while the native
// Anthropic API consumes x-api-key.
func CallAnthropicCompatibleAPI(account *config.Account, model string, payload *KiroPayload, callback *KiroStreamCallback) error {
	upstreamReq, err := kiroPayloadToAnthropic(model, payload)
	if err != nil {
		return err
	}
	body, err := json.Marshal(upstreamReq)
	if err != nil {
		return err
	}
	endpoint, err := anthropicMessagesURL(account.BaseURL)
	if err != nil {
		return err
	}

	proxyAttempts := 0
	for {
		proxyURL, poolKey, err := SelectProxyForAccount(account)
		if err != nil {
			return err
		}
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+account.ApiKey)
		req.Header.Set("x-api-key", account.ApiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")

		resp, err := GetClientForProxy(proxyURL).Do(req)
		if err != nil {
			if isProxyErrorMessage(err.Error()) && poolKey != "" && proxyAttempts < maxProxySwapAttempts {
				config.MarkProxyUnhealthy(poolKey)
				proxyAttempts++
				continue
			}
			return err
		}
		if poolKey != "" {
			config.MarkProxyHealthy(poolKey)
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			limited, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnthropicUpstreamErrorBytes))
			msg := strings.TrimSpace(string(limited))
			if retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After")); retryAfter != "" {
				msg += "; retry after " + retryAfter
			}
			return fmt.Errorf("HTTP %d from Anthropic-compatible upstream: %s", resp.StatusCode, msg)
		}

		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			return consumeAnthropicCompatibleSSE(resp.Body, account, payload, callback)
		}
		return consumeAnthropicCompatibleJSON(resp.Body, account, payload, callback)
	}
}

func anthropicMessagesURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid Anthropic-compatible baseURL")
	}
	path := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(path, "/messages"):
	case strings.HasSuffix(path, "/v1"):
		path += "/messages"
	default:
		path += "/v1/messages"
	}
	u.Path = path
	return u.String(), nil
}

func kiroPayloadToAnthropic(model string, payload *KiroPayload) (*anthropicCompatibleRequest, error) {
	if payload == nil {
		return nil, fmt.Errorf("missing upstream payload")
	}
	req := &anthropicCompatibleRequest{
		Model:     strings.TrimSpace(model),
		MaxTokens: defaultAnthropicUpstreamMaxTokens,
		Stream:    true,
	}
	if req.Model == "" {
		req.Model = payload.ConversationState.CurrentMessage.UserInputMessage.ModelID
	}
	if cfg := payload.InferenceConfig; cfg != nil {
		if cfg.MaxTokens > 0 {
			req.MaxTokens = cfg.MaxTokens
		}
		req.Temperature = cfg.Temperature
		req.TopP = cfg.TopP
	}

	for _, history := range payload.ConversationState.History {
		if history.UserInputMessage != nil {
			req.Messages = append(req.Messages, anthropicMessageForUser(*history.UserInputMessage))
		}
		if history.AssistantResponseMessage != nil {
			req.Messages = append(req.Messages, anthropicMessageForAssistant(*history.AssistantResponseMessage, payload.ToolNameMap))
		}
	}
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	req.Messages = append(req.Messages, anthropicMessageForUser(current))
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("Anthropic-compatible request has no messages")
	}
	if current.UserInputMessageContext != nil {
		for _, tool := range current.UserInputMessageContext.Tools {
			req.Tools = append(req.Tools, ClaudeTool{
				Name:        restoreToolName(tool.ToolSpecification.Name, payload.ToolNameMap),
				Description: tool.ToolSpecification.Description,
				InputSchema: tool.ToolSpecification.InputSchema.JSON,
			})
		}
	}
	return req, nil
}

func anthropicMessageForUser(message KiroUserInputMessage) ClaudeMessage {
	ctx := message.UserInputMessageContext
	if len(message.Images) == 0 && (ctx == nil || len(ctx.ToolResults) == 0) {
		return ClaudeMessage{Role: "user", Content: message.Content}
	}
	blocks := make([]ClaudeContentBlock, 0, len(message.Images)+1)
	hasToolResults := ctx != nil && len(ctx.ToolResults) > 0
	contentIsFallback := strings.TrimSpace(message.Content) == minimalFallbackUserContent && hasToolResults
	if !contentIsFallback && strings.TrimSpace(message.Content) != "" {
		blocks = append(blocks, ClaudeContentBlock{Type: "text", Text: message.Content})
	}
	for _, image := range message.Images {
		format := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(image.Format)), "image/")
		if format == "jpg" {
			format = "jpeg"
		}
		blocks = append(blocks, ClaudeContentBlock{
			Type:   "image",
			Source: &ImageSource{Type: "base64", MediaType: "image/" + format, Data: image.Source.Bytes},
		})
	}
	if ctx != nil {
		for _, result := range ctx.ToolResults {
			parts := make([]string, 0, len(result.Content))
			for _, part := range result.Content {
				if strings.TrimSpace(part.Text) != "" {
					parts = append(parts, part.Text)
				}
			}
			blocks = append(blocks, ClaudeContentBlock{
				Type: "tool_result", ToolUseID: result.ToolUseID, Content: strings.Join(parts, "\n"),
			})
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, ClaudeContentBlock{Type: "text", Text: minimalFallbackUserContent})
	}
	return ClaudeMessage{Role: "user", Content: blocks}
}

func anthropicMessageForAssistant(message KiroAssistantResponseMessage, names map[string]string) ClaudeMessage {
	if len(message.ToolUses) == 0 {
		return ClaudeMessage{Role: "assistant", Content: message.Content}
	}
	blocks := make([]ClaudeContentBlock, 0, len(message.ToolUses)+1)
	if strings.TrimSpace(message.Content) != "" {
		blocks = append(blocks, ClaudeContentBlock{Type: "text", Text: message.Content})
	}
	for _, toolUse := range message.ToolUses {
		blocks = append(blocks, ClaudeContentBlock{
			Type: "tool_use", ID: toolUse.ToolUseID,
			Name: restoreToolName(toolUse.Name, names), Input: toolUse.Input,
		})
	}
	return ClaudeMessage{Role: "assistant", Content: blocks}
}

func consumeAnthropicCompatibleSSE(body io.Reader, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	tools := make(map[int]*anthropicToolAccumulator)
	usage := UpstreamUsage{}
	completed := false
	stopReason := ""

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Type         string             `json:"type"`
			Index        int                `json:"index"`
			Message      *ClaudeResponse    `json:"message"`
			ContentBlock ClaudeContentBlock `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage ClaudeUsage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("invalid Anthropic-compatible SSE event: %w", err)
		}
		// delta.stop_reason rides on message_delta; capture it before the type switch
		// so a truncated answer is at least visible in the admin log.
		if event.Delta.StopReason != "" {
			stopReason = event.Delta.StopReason
		}
		if event.Error != nil || event.Type == "error" {
			msg := "unknown upstream error"
			if event.Error != nil && event.Error.Message != "" {
				msg = event.Error.Message
			}
			return fmt.Errorf("Anthropic-compatible upstream error: %s", msg)
		}
		switch event.Type {
		case "message_start":
			if event.Message != nil {
				usage = upstreamUsageFromClaude(event.Message.Usage)
			}
		case "content_block_start":
			switch event.ContentBlock.Type {
			case "text":
				emitAnthropicText(callback, event.ContentBlock.Text, false)
			case "thinking":
				emitAnthropicText(callback, event.ContentBlock.Thinking, true)
			case "tool_use":
				tools[event.Index] = &anthropicToolAccumulator{
					id: event.ContentBlock.ID, name: event.ContentBlock.Name, initial: event.ContentBlock.Input,
				}
			}
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				emitAnthropicText(callback, event.Delta.Text, false)
			case "thinking_delta":
				emitAnthropicText(callback, event.Delta.Thinking, true)
			case "input_json_delta":
				acc := tools[event.Index]
				if acc == nil {
					acc = &anthropicToolAccumulator{}
					tools[event.Index] = acc
				}
				acc.partialJSON.WriteString(event.Delta.PartialJSON)
			}
		case "message_delta":
			if event.Usage.OutputTokens > 0 {
				usage.OutputTokens = event.Usage.OutputTokens
			}
		case "message_stop":
			emitAnthropicTools(tools, payload, callback)
			emitUpstreamBilling(account, usage, callback)
			if callback != nil && callback.OnComplete != nil {
				callback.OnComplete(usage.InputTokens, usage.OutputTokens)
			}
			completed = true
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !completed {
		emitAnthropicTools(tools, payload, callback)
		emitUpstreamBilling(account, usage, callback)
		if callback != nil && callback.OnComplete != nil {
			callback.OnComplete(usage.InputTokens, usage.OutputTokens)
		}
	}
	warnUpstreamTruncation(account, stopReason, usage.OutputTokens)
	return nil
}

// warnUpstreamTruncation logs when a custom Anthropic upstream cut the answer short.
// max_tokens truncation is otherwise invisible: the stream ends normally and the
// customer just sees a reply that stops mid-sentence, so without this line there is
// nothing in the admin log to distinguish it from a genuinely short answer.
func warnUpstreamTruncation(account *config.Account, stopReason string, outputTokens int) {
	if stopReason != "max_tokens" {
		return
	}
	email := ""
	if account != nil {
		email = account.Email
	}
	logger.Warnf("[Upstream] Anthropic-compatible upstream truncated the response at max_tokens (account=%s, outputTokens=%d) — raise the client's max_tokens or defaultAnthropicUpstreamMaxTokens", email, outputTokens)
}

func consumeAnthropicCompatibleJSON(body io.Reader, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	var response ClaudeResponse
	if err := json.NewDecoder(body).Decode(&response); err != nil {
		return err
	}
	for _, block := range response.Content {
		switch block.Type {
		case "text":
			emitAnthropicText(callback, block.Text, false)
		case "thinking":
			emitAnthropicText(callback, block.Thinking, true)
		case "tool_use":
			if callback != nil && callback.OnToolUse != nil {
				callback.OnToolUse(KiroToolUse{
					ToolUseID: block.ID, Name: restoreToolName(block.Name, payload.ToolNameMap),
					Input: anthropicToolInput(block.Input, ""),
				})
			}
		}
	}
	emitUpstreamBilling(account, upstreamUsageFromClaude(response.Usage), callback)
	if callback != nil && callback.OnComplete != nil {
		callback.OnComplete(response.Usage.InputTokens, response.Usage.OutputTokens)
	}
	warnUpstreamTruncation(account, response.StopReason, response.Usage.OutputTokens)
	return nil
}

func upstreamUsageFromClaude(usage ClaudeUsage) UpstreamUsage {
	result := UpstreamUsage{
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
	}
	if usage.CacheCreation != nil {
		result.CacheCreation5mTokens = usage.CacheCreation.Ephemeral5mInputTokens
		result.CacheCreation1hTokens = usage.CacheCreation.Ephemeral1hInputTokens
	}
	return result
}

func emitAnthropicText(callback *KiroStreamCallback, value string, thinking bool) {
	if value != "" && callback != nil && callback.OnText != nil {
		callback.OnText(value, thinking)
	}
}

func emitAnthropicTools(tools map[int]*anthropicToolAccumulator, payload *KiroPayload, callback *KiroStreamCallback) {
	if callback == nil || callback.OnToolUse == nil {
		return
	}
	indexes := make([]int, 0, len(tools))
	for index := range tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		tool := tools[index]
		id := tool.id
		if id == "" {
			id = "tool_" + uuid.NewString()
		}
		callback.OnToolUse(KiroToolUse{
			ToolUseID: id, Name: restoreToolName(tool.name, payload.ToolNameMap),
			Input: anthropicToolInput(tool.initial, tool.partialJSON.String()),
		})
	}
}

func anthropicToolInput(initial interface{}, partial string) map[string]interface{} {
	if strings.TrimSpace(partial) != "" {
		var parsed map[string]interface{}
		if json.Unmarshal([]byte(partial), &parsed) == nil {
			return parsed
		}
	}
	if value, ok := initial.(map[string]interface{}); ok && value != nil {
		return value
	}
	if initial != nil {
		encoded, _ := json.Marshal(initial)
		var parsed map[string]interface{}
		if json.Unmarshal(encoded, &parsed) == nil && parsed != nil {
			return parsed
		}
	}
	return map[string]interface{}{}
}
