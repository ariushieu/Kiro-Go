package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/google/uuid"
)

const maxOpenAIUpstreamErrorBytes = 4096

type openAICompatibleRequest struct {
	Model         string                   `json:"model"`
	Messages      []map[string]interface{} `json:"messages"`
	MaxTokens     int                      `json:"max_tokens,omitempty"`
	Temperature   float64                  `json:"temperature,omitempty"`
	TopP          float64                  `json:"top_p,omitempty"`
	Stream        bool                     `json:"stream"`
	StreamOptions map[string]bool          `json:"stream_options,omitempty"`
	Tools         []map[string]interface{} `json:"tools,omitempty"`
}

type openAIToolAccumulator struct {
	id        string
	name      string
	arguments strings.Builder
	emitted   bool
}

// CallOpenAICompatibleAPI translates the common KiroPayload representation to
// Chat Completions, consumes either SSE or a JSON response, and emits the same
// callback events as the Kiro Event Stream adapter.
func CallOpenAICompatibleAPI(account *config.Account, model string, payload *KiroPayload, callback *KiroStreamCallback) error {
	upstreamReq, err := kiroPayloadToOpenAI(model, payload)
	if err != nil {
		return err
	}
	body, err := json.Marshal(upstreamReq)
	if err != nil {
		return err
	}
	endpoint, err := openAIChatCompletionsURL(account.BaseURL)
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
			limited, _ := io.ReadAll(io.LimitReader(resp.Body, maxOpenAIUpstreamErrorBytes))
			msg := strings.TrimSpace(string(limited))
			if retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After")); retryAfter != "" {
				msg += "; retry after " + retryAfter
			}
			return fmt.Errorf("HTTP %d from OpenAI-compatible upstream: %s", resp.StatusCode, msg)
		}

		if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			return consumeOpenAICompatibleSSE(resp.Body, account, payload, callback)
		}
		return consumeOpenAICompatibleJSON(resp.Body, account, payload, callback)
	}
}

func openAIChatCompletionsURL(base string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid OpenAI-compatible baseURL")
	}
	path := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
	case strings.HasSuffix(path, "/v1"):
		path += "/chat/completions"
	default:
		path += "/v1/chat/completions"
	}
	u.Path = path
	return u.String(), nil
}

func kiroPayloadToOpenAI(model string, payload *KiroPayload) (*openAICompatibleRequest, error) {
	if payload == nil {
		return nil, fmt.Errorf("missing upstream payload")
	}
	req := &openAICompatibleRequest{
		Model:         strings.TrimSpace(model),
		Stream:        true,
		StreamOptions: map[string]bool{"include_usage": true},
	}
	if req.Model == "" {
		req.Model = payload.ConversationState.CurrentMessage.UserInputMessage.ModelID
	}
	if cfg := payload.InferenceConfig; cfg != nil {
		req.MaxTokens = cfg.MaxTokens
		req.Temperature = cfg.Temperature
		req.TopP = cfg.TopP
	}

	for _, history := range payload.ConversationState.History {
		if history.UserInputMessage != nil {
			req.Messages = append(req.Messages, openAIMessagesForUser(*history.UserInputMessage)...)
		}
		if history.AssistantResponseMessage != nil {
			req.Messages = append(req.Messages, openAIMessageForAssistant(*history.AssistantResponseMessage, payload.ToolNameMap))
		}
	}
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	req.Messages = append(req.Messages, openAIMessagesForUser(current)...)
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("OpenAI-compatible request has no messages")
	}
	if current.UserInputMessageContext != nil {
		for _, tool := range current.UserInputMessageContext.Tools {
			name := restoreToolName(tool.ToolSpecification.Name, payload.ToolNameMap)
			req.Tools = append(req.Tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name": name, "description": tool.ToolSpecification.Description,
					"parameters": tool.ToolSpecification.InputSchema.JSON,
				},
			})
		}
	}
	return req, nil
}

func openAIMessagesForUser(message KiroUserInputMessage) []map[string]interface{} {
	var result []map[string]interface{}
	if ctx := message.UserInputMessageContext; ctx != nil {
		for _, toolResult := range ctx.ToolResults {
			parts := make([]string, 0, len(toolResult.Content))
			for _, part := range toolResult.Content {
				if strings.TrimSpace(part.Text) != "" {
					parts = append(parts, part.Text)
				}
			}
			result = append(result, map[string]interface{}{
				"role": "tool", "tool_call_id": toolResult.ToolUseID,
				"content": strings.Join(parts, "\n"),
			})
		}
	}
	hasToolResults := message.UserInputMessageContext != nil && len(message.UserInputMessageContext.ToolResults) > 0
	contentIsFallback := strings.TrimSpace(message.Content) == minimalFallbackUserContent && hasToolResults
	if (!contentIsFallback && strings.TrimSpace(message.Content) != "") || len(message.Images) > 0 {
		result = append(result, map[string]interface{}{
			"role": "user", "content": openAIUserContent(message.Content, message.Images),
		})
	}
	return result
}

func openAIUserContent(text string, images []KiroImage) interface{} {
	if len(images) == 0 {
		return text
	}
	parts := make([]map[string]interface{}, 0, len(images)+1)
	if strings.TrimSpace(text) != "" {
		parts = append(parts, map[string]interface{}{"type": "text", "text": text})
	}
	for _, image := range images {
		format := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(image.Format)), "image/")
		if format == "jpg" {
			format = "jpeg"
		}
		parts = append(parts, map[string]interface{}{
			"type":      "image_url",
			"image_url": map[string]string{"url": "data:image/" + format + ";base64," + image.Source.Bytes},
		})
	}
	return parts
}

func openAIMessageForAssistant(message KiroAssistantResponseMessage, names map[string]string) map[string]interface{} {
	result := map[string]interface{}{"role": "assistant", "content": message.Content}
	if len(message.ToolUses) == 0 {
		return result
	}
	toolCalls := make([]map[string]interface{}, 0, len(message.ToolUses))
	for _, toolUse := range message.ToolUses {
		arguments, _ := json.Marshal(toolUse.Input)
		toolCalls = append(toolCalls, map[string]interface{}{
			"id": toolUse.ToolUseID, "type": "function",
			"function": map[string]string{
				"name": restoreToolName(toolUse.Name, names), "arguments": string(arguments),
			},
		})
	}
	result["tool_calls"] = toolCalls
	return result
}

func restoreToolName(name string, names map[string]string) string {
	if original, ok := names[name]; ok {
		return original
	}
	return name
}

func consumeOpenAICompatibleSSE(body io.Reader, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	tools := make(map[int]*openAIToolAccumulator)
	usage := UpstreamUsage{}
	reportedInputTokens := 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *OpenAIUsage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("invalid OpenAI-compatible SSE chunk: %w", err)
		}
		if chunk.Error != nil {
			return fmt.Errorf("OpenAI-compatible upstream error: %s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			usage = upstreamUsageFromOpenAI(*chunk.Usage)
			reportedInputTokens = chunk.Usage.PromptTokens
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" && callback != nil && callback.OnText != nil {
				callback.OnText(choice.Delta.Content, false)
			}
			if choice.Delta.ReasoningContent != "" && callback != nil && callback.OnText != nil {
				callback.OnText(choice.Delta.ReasoningContent, true)
			}
			for _, part := range choice.Delta.ToolCalls {
				acc := tools[part.Index]
				if acc == nil {
					acc = &openAIToolAccumulator{}
					tools[part.Index] = acc
				}
				if part.ID != "" {
					acc.id = part.ID
				}
				acc.name += part.Function.Name
				acc.arguments.WriteString(part.Function.Arguments)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	emitOpenAITools(tools, payload, callback)
	emitUpstreamBilling(account, usage, callback)
	if callback != nil && callback.OnComplete != nil {
		callback.OnComplete(reportedInputTokens, usage.OutputTokens)
	}
	return nil
}

func consumeOpenAICompatibleJSON(body io.Reader, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	var response struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string                           `json:"id"`
					Function struct{ Name, Arguments string } `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage OpenAIUsage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(body).Decode(&response); err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("OpenAI-compatible upstream error: %s", response.Error.Message)
	}
	tools := make(map[int]*openAIToolAccumulator)
	for _, choice := range response.Choices {
		if choice.Message.Content != "" && callback != nil && callback.OnText != nil {
			callback.OnText(choice.Message.Content, false)
		}
		if choice.Message.ReasoningContent != "" && callback != nil && callback.OnText != nil {
			callback.OnText(choice.Message.ReasoningContent, true)
		}
		for i, tool := range choice.Message.ToolCalls {
			acc := &openAIToolAccumulator{id: tool.ID, name: tool.Function.Name}
			acc.arguments.WriteString(tool.Function.Arguments)
			tools[i] = acc
		}
	}
	emitOpenAITools(tools, payload, callback)
	emitUpstreamBilling(account, upstreamUsageFromOpenAI(response.Usage), callback)
	if callback != nil && callback.OnComplete != nil {
		callback.OnComplete(response.Usage.PromptTokens, response.Usage.CompletionTokens)
	}
	return nil
}

func upstreamUsageFromOpenAI(usage OpenAIUsage) UpstreamUsage {
	result := UpstreamUsage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens}
	if usage.PromptTokensDetails != nil {
		result.CacheReadInputTokens = usage.PromptTokensDetails.CachedTokens
		result.InputTokens -= result.CacheReadInputTokens
		if result.InputTokens < 0 {
			result.InputTokens = 0
		}
	}
	return result
}

func emitOpenAITools(tools map[int]*openAIToolAccumulator, payload *KiroPayload, callback *KiroStreamCallback) {
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
		if tool == nil || tool.emitted {
			continue
		}
		tool.emitted = true
		var input map[string]interface{}
		if err := json.Unmarshal([]byte(tool.arguments.String()), &input); err != nil || input == nil {
			input = map[string]interface{}{"raw": tool.arguments.String()}
		}
		id := tool.id
		if id == "" {
			id = "call_" + uuid.New().String()
		}
		name := tool.name
		if payload != nil {
			name = restoreToolName(name, payload.ToolNameMap)
		}
		callback.OnToolUse(KiroToolUse{ToolUseID: id, Name: name, Input: input})
	}
}
