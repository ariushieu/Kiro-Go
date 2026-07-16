# Design — Multi-Provider Pool

## Overview

Mục tiêu là tách proxy khỏi giả định "mọi account đều là Kiro". Hiện tại luồng là:

```
client → Handler.ServeHTTP → auth → translator (ClaudeToKiro/OpenAIToKiro)
       → pool chọn account → CallKiroAPI (AWS Event Stream) → KiroStreamCallback → client
```

Điểm nghẽn: `CallKiroAPI` bị gọi trực tiếp ở 9 chỗ (`handler.go` ×5, `responses_handler.go` ×2,
`proxy_import.go` ×1, và định nghĩa ở `kiro.go`), và translator luôn dịch **sang** Kiro. Ta
chèn một tầng trừu tượng `UpstreamClient` vào giữa "pool chọn account" và "gọi upstream", giữ
`KiroStreamCallback` làm hợp đồng sự kiện chung.

Nguyên tắc chủ đạo: **request vào đã là Claude hoặc OpenAI format**. Nếu backend cùng format →
passthrough (dễ). Chỉ khi lệch format mới cần dịch. Điều này khiến Anthropic backend gần như
miễn phí về công sức, còn Kiro (đường khó nhất, dịch sang Event Stream) đã có sẵn.

Phạm vi sửa đổi:
- `config/config.go` — thêm `Provider`, `ApiKey`, `BaseURL` trên `Account` + migrate + validate.
- `proxy/upstream.go` (mới) — interface `UpstreamClient` + registry.
- `proxy/upstream_kiro.go` (mới) — wrapper quanh `CallKiroAPI`.
- `proxy/upstream_anthropic.go` (mới) — Anthropic SSE client.
- `proxy/upstream_openai.go` (mới) — OpenAI-compatible SSE client.
- `proxy/handler.go`, `responses_handler.go`, `proxy_import.go` — thay `CallKiroAPI(...)` bằng
  `dispatchUpstream(account, ...)`.
- `pool/account.go` — model routing biết provider.
- `proxy/account_failover.go` — mapping lỗi per-provider.
- `web/` — form thêm/sửa account có provider.

## Architecture

```
                         ┌─────────────────────────────┐
client → ServeHTTP ──────┤ pool.GetNextForModel(...)   │  (đã biết provider)
   │  (Claude/OpenAI in) └─────────────┬───────────────┘
   ▼                                   ▼
 translator                    dispatchUpstream(account, req, callback)
 (chỉ dịch khi cần)                    │
                                       ▼
                    ┌──────────────────┼───────────────────┐
                    ▼                  ▼                    ▼
             upstreamKiro       upstreamAnthropic     upstreamOpenAI
             (CallKiroAPI)      (POST /v1/messages)   (POST /chat/completions)
                    │                  │                    │
                    └──────────────────┴────────────────────┘
                                       ▼
                             KiroStreamCallback (OnText/OnToolUse/OnComplete/…)
                                       ▼
                              streaming handler → client
```

Điểm mấu chốt về format: `dispatchUpstream` nhận **request gốc đã parse** (Claude hoặc OpenAI
struct) + cờ định dạng client mong đợi. Mỗi `UpstreamClient` tự quyết định có cần dịch không:

| Client vào \ Backend | Kiro                | Anthropic            | OpenAI                |
|----------------------|---------------------|----------------------|-----------------------|
| Claude Messages      | ClaudeToKiro (cũ)   | passthrough          | Claude→OpenAI         |
| OpenAI Chat          | OpenAIToKiro (cũ)   | OpenAI→Claude        | passthrough           |

Response luôn về `KiroStreamCallback`; streaming handler ở `handler.go` giữ nguyên vì nó chỉ
tiêu thụ callback, không quan tâm nguồn.

## Components and Interfaces

### 1. Config: Provider trên Account (Req 1)

```go
// config/config.go — thêm vào struct Account
Provider string `json:"provider,omitempty"` // "kiro" (default) | "anthropic" | "openai"
ApiKey   string `json:"apiKey,omitempty"`   // bearer key cho anthropic/openai (secret)
BaseURL  string `json:"baseURL,omitempty"`  // endpoint gốc cho anthropic/openai
```

Lưu ý xung đột tên: `Account` đã có `KiroApiKey` (khác nghĩa — headless Kiro auth). Dùng tên
`ApiKey` cho key non-Kiro để tách bạch. Config-level đã có `ApiKeys[]` (key của *client* gọi
proxy) — không liên quan.

Migrate trong `Load()` (nơi đã migrate `ApiKey→ApiKeys[]` và `allowOverage→OverageStatus`):

```go
if a.Provider == "" {
    a.Provider = "kiro"
}
```

Validate trong setter thêm/sửa account:
```go
switch a.Provider {
case "kiro":                     // yêu cầu token fields như cũ
case "anthropic", "openai":      // yêu cầu ApiKey != "" và BaseURL hợp lệ
default: return fmt.Errorf("invalid provider %q", a.Provider)
}
```

`BaseURL` mặc định khi rỗng: `anthropic` → `https://api.anthropic.com`, `openai` →
`https://api.openai.com/v1`. Chuẩn hóa: trim trailing `/`, bắt buộc scheme `https` trừ khi host
là loopback (Req 4.4).

### 2. UpstreamClient interface (Req 2)

```go
// proxy/upstream.go
type UpstreamRequest struct {
    Model        string
    ClientFormat string          // "claude" | "openai" — định dạng client mong đợi ở output
    ClaudePayload *KiroPayload   // đã dựng (đường Kiro dùng trực tiếp)
    RawClaude    *ClaudeRequest  // request gốc nếu client gửi Claude
    RawOpenAI    *OpenAIRequest  // request gốc nếu client gửi OpenAI
    Thinking     bool
}

type UpstreamClient interface {
    Name() string
    Call(account *config.Account, req *UpstreamRequest, cb *KiroStreamCallback) error
    // ClassifyError map lỗi provider → action cho failover (Req 6)
    ClassifyError(err error) failureAction
}

func dispatchUpstream(account *config.Account, req *UpstreamRequest, cb *KiroStreamCallback) error {
    switch account.Provider {
    case "anthropic": return anthropicClient.Call(account, req, cb)
    case "openai":    return openaiClient.Call(account, req, cb)
    default:          return kiroClient.Call(account, req, cb)
    }
}
```

`failureAction` là enum nội bộ (`actionCooldown`, `actionDisable`, `actionRetry`) mà
`handleAccountFailure` đã ngầm thực hiện qua string-match — ta hiện thực hóa nó thành kiểu.

### 3. upstreamKiro — wrapper (Req 2.2)

Chỉ gọi lại `CallKiroAPI(account, req.ClaudePayload, cb)`. Không đổi hành vi. `ClassifyError`
gói logic string-match hiện có trong `account_failover.go`/`pool.RecordError`.

### 4. upstreamAnthropic (Req 3)

- Nếu `req.ClientFormat=="claude"` → dùng lại body Claude gốc (`req.RawClaude`), chỉ set stream=true.
- Nếu `openai` → dịch `RawOpenAI`→Claude (hàm mới `openAIToClaude`, đối xứng với alias hiện có).
- POST `{BaseURL}/v1/messages`, headers: `x-api-key`, `anthropic-version: 2023-06-01`,
  `content-type: application/json`, `accept: text/event-stream`.
- Parse SSE: `event: content_block_delta` → `OnText(text, isThinking)`; `content_block_start`
  type `tool_use` + `input_json_delta` gộp → `OnToolUse`; `message_delta.usage` → `OnComplete`.
  Thinking: `content_block` type `thinking` → `OnText(..., true)`.
- Dùng `GetClientForProxy(account.ProxyURL)` để tôn trọng per-account proxy như Kiro.

### 5. upstreamOpenAI (Req 4)

- Nếu `req.ClientFormat=="openai"` → dùng lại `RawOpenAI`.
- Nếu `claude` → dịch Claude→OpenAI (hàm mới `claudeToOpenAI`).
- POST `{BaseURL}/chat/completions`, header `Authorization: Bearer {ApiKey}`, `stream:true`,
  `stream_options.include_usage:true` để lấy token count.
- Parse SSE `chat.completion.chunk`: `choices[0].delta.content` → `OnText`;
  `delta.tool_calls[]` (gộp theo index, arguments nối chuỗi) → `OnToolUse` khi finish;
  `usage` (chunk cuối) → `OnComplete`.
- Reasoning: `delta.reasoning_content` (DeepSeek/một số endpoint) → `OnText(..., true)`.

### 6. Model routing (Req 5)

`pool.accountHasModel` hiện dựa `modelLists[accountID]` (fetch từ Kiro `ListAvailableModels`).
Non-Kiro không có API list đó. Bổ sung:

- Account non-Kiro khai báo model phục vụ. Đơn giản nhất: một danh sách model tĩnh trên account
  (`Models []string`) HOẶC suy theo prefix provider (`openai`→`gpt-*,o1-*`; `anthropic`→`claude-*`).
  Chọn **prefix + optional override list** để không bắt người dùng liệt kê tay.
- `accountHasModel` rẽ nhánh theo provider: Kiro giữ `modelLists`; non-Kiro so prefix/override.
- Alias cross-provider (`gpt-4o`→Claude ở `modelAliases`) **chỉ áp cho đường Kiro**. Khi có
  account OpenAI thật, `gpt-4o` route thẳng tới nó (Req 5.4). Route quyết định *trước* khi
  alias Kiro-hóa tên.

Selection call (`GetNextForModel*`) không đổi chữ ký; chỉ `accountHasModel` thông minh hơn.

### 7. Failover per-provider (Req 6)

`handleAccountFailure` hiện match chuỗi lỗi Kiro. Refactor để hỏi `UpstreamClient.ClassifyError`
của provider tương ứng:
- Anthropic: HTTP 429 / `rate_limit_error` → cooldown; 401/403 → disable; 5xx/network → retry.
- OpenAI: 429 → cooldown; 401 → disable; 5xx → retry.
- Kiro: giữ nguyên matcher hiện có.

Vòng lặp retry (`excluded`, `maxAccountRetryAttempts=3`) không đổi.

### 8. Bỏ logic Kiro cho non-Kiro (Req 8)

- `ensureValidToken`: nếu `Provider != "kiro"` → return nil ngay (key tĩnh).
- Background refresh goroutine: skip account non-Kiro khỏi vòng `RefreshToken`/`RefreshAccountInfo`.
- Payload truncation (~2MB): chỉ trong đường Kiro (`translator`/`kiro.go`), non-Kiro không đụng.

### 9. Admin UI (Req 7)

- Form account thêm dropdown `provider`. Khi chọn anthropic/openai → hiện `apiKey` + `baseURL`,
  ẩn field OAuth/region.
- List account hiện badge provider. Field usage/overage hiện "N/A" cho non-Kiro.
- `apiKey` mask trong response admin API (giống cách token hiện được xử lý).

### 10. Credit thống nhất — quy đổi cost đa nguồn (Req 10, 11)

Đây là điểm mấu chốt của mục tiêu "gom nhiều nguồn, phân phối lại credit cho user". Vấn đề: mỗi
nguồn tính giá theo đơn vị khác nhau — Kiro trả `OnCredits` (credit riêng của Kiro), OpenAI/
Anthropic chỉ trả token và tính tiền theo `$/1M token` khác nhau giữa các model. Nếu trừ thẳng
số thô vào key user thì user bị tính lệch tùy nguồn phục vụ.

Giải pháp: một tầng **quy đổi về credit thống nhất** đặt *ngay trước* `RecordApiKeyUsage`.

```
provider trả về (credits HOẶC tokens)
        │
        ▼
  costToUnifiedCredits(provider, model, inTok, outTok, kiroCredits)
        │  tra bảng PricingTable + markup
        ▼
  RecordApiKeyUsage(keyID, tokens, unifiedCredits)   ← số credit user bị trừ
```

Bảng giá cấu hình ở config (global), không phải trên từng account:

```go
// config: bảng quy đổi credit
type ModelPricing struct {
    Provider          string  `json:"provider"`          // "kiro" | "anthropic" | "openai"
    Model             string  `json:"model"`             // khớp chính xác hoặc prefix "*"
    CreditPerInputTok float64 `json:"creditPerInputTok"` // credit / input token
    CreditPerOutputTok float64 `json:"creditPerOutputTok"`
    KiroCreditFactor  float64 `json:"kiroCreditFactor,omitempty"` // nhân vào OnCredits của Kiro
}
// + một Markup float64 toàn cục (mặc định 1.0) để bán lại (Req 10.5)
```

Quy tắc quy đổi (`costToUnifiedCredits`):
- **Kiro**: `credit = OnCredits × KiroCreditFactor × Markup`. Giữ nguyên nguồn sự thật là số
  Kiro trả về (Req 10.3), chỉ nhân hệ số.
- **OpenAI/Anthropic**: `credit = (inTok × CreditPerInputTok + outTok × CreditPerOutputTok) × Markup`.
- Không tìm thấy dòng khớp trong bảng → dùng một default an toàn (log warn) để không trừ nhầm 0.

Điểm neo trong code: các streaming handler đã gom `inputTokens/outputTokens/credits` trước khi
gọi `config.RecordApiKeyUsage(apiKeyID, int64(inputTokens+outputTokens), credits)`
([handler.go:1682](../../proxy/handler.go)). Ta chèn `credits = costToUnifiedCredits(...)` tại
đúng các điểm đó (5 handler + responses ×2). `TokenLimit`/`CreditLimit` trên `ApiKeyEntry` không
đổi ý nghĩa — chỉ nguồn số `credits` được chuẩn hóa (Req 10.4).

**Fallback chéo nguồn (Req 11):** cơ chế đã có sẵn — `nextAccountForKey` fallback từ
`GetNextForModelBoundExcluding` sang `GetNextForModelExcluding` khi account bind không dùng được
([handler.go:1149](../../proxy/handler.go)). Với multi-provider, fallback có thể rơi sang account
khác provider miễn phục vụ được model (Req 11.2). Vì credit đã thống nhất (Req 10), user bị trừ
như nhau bất kể fallback sang nguồn đắt/rẻ (Req 11.3). Không cần code mới cho fallback — chỉ cần
đảm bảo quy đổi credit chạy *sau* khi biết account nào thực sự phục vụ.

**Tier/gói plan:** hiện tại quota là per-key (`TokenLimit`/`CreditLimit` trên từng
`ApiKeyEntry`). Người dùng xác nhận: giữ per-key bây giờ, thêm khái niệm "gói plan" (template
quota + pricing dùng chung) là mở rộng tương lai — không nằm trong spec này.

## Data Models

`Account` thêm 3 field (mục 1). Thêm `[]ModelPricing` + `Markup` vào `Config` (global, mục 10).
Không đổi schema khác. Bump `config.Version` và cập nhật `version.json` khi release (theo
convention CLAUDE.md).

## Error Handling

- Config validate provider → lỗi trả về admin API, không lưu.
- URL ghép sai (thiếu scheme) → chặn ở `BaseURL` normalize.
- Provider SSE lỗi giữa chừng → `OnError` → cùng đường xử lý như Kiro stream lỗi.
- Secret: không log `ApiKey`; mask trong debug summary (giống `summarizeKiroPayload`).

## Testing Strategy

- `config`: test migrate `provider` rỗng→kiro; validate reject provider lạ; normalize BaseURL.
- `upstream_anthropic_test.go` / `upstream_openai_test.go`: dùng `httptest.Server` phát SSE mẫu,
  assert callback nhận đúng text/tool/usage. Dùng seam giống `auth/testhooks.go` để inject client.
- `pool`: test `accountHasModel` cho non-Kiro (prefix + override); route `gpt-4o` ưu tiên account
  OpenAI thật over alias Kiro.
- Cross-format: test `openAIToClaude` và `claudeToOpenAI` round-trip cơ bản (text + tool call).
- Regression: đường Kiro qua `dispatchUpstream` cho kết quả y hệt gọi `CallKiroAPI` trực tiếp.

## Migration & Rollout

1. Thêm field + migrate (an toàn, account cũ = kiro).
2. Thêm interface + wrapper Kiro; thay call site; chạy full `go test ./...` — hành vi Kiro không đổi.
3. Thêm Anthropic (dễ nhất, passthrough) → test end-to-end với key thật.
4. Thêm OpenAI (cross-format nặng hơn).
5. Admin UI cuối cùng.

Mỗi bước build + test xanh trước khi sang bước sau.
