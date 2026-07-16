# Implementation Plan — Multi-Provider Pool

## Overview

Kế hoạch triển khai để pool có thể chứa nhiều loại backend (Kiro, Anthropic API, OpenAI-compatible
API) thay vì chỉ Kiro. Dựa trên requirements.md và design.md. Thứ tự ưu tiên: nền tảng (config +
interface) → wrapper Kiro (không đổi hành vi) → Anthropic (dễ nhất) → OpenAI (nặng nhất) → UI.

Mỗi task phải build + `go test ./...` xanh trước khi sang task sau.

## Tasks

- [ ] 1. Thêm Provider/ApiKey/BaseURL vào Account + migrate + validate
  - Thêm 3 field vào struct `Account` trong `config/config.go`
  - Migrate trong `Load()`: `Provider == "" → "kiro"` (cạnh migrate ApiKeys/OverageStatus hiện có)
  - Validate provider trong setter thêm/sửa account: kiro | anthropic | openai, reject giá trị khác
  - Normalize `BaseURL`: default theo provider, trim trailing `/`, bắt buộc https trừ loopback
  - Bump `config.Version` + cập nhật `version.json`
  - Test: migrate rỗng→kiro; reject provider lạ; normalize BaseURL
  - _Requirements: 1.1, 1.2, 1.3, 1.4_

- [ ] 2. Định nghĩa UpstreamClient interface + dispatchUpstream + failureAction
  - Tạo `proxy/upstream.go`: `UpstreamRequest`, `UpstreamClient`, `failureAction` enum, `dispatchUpstream`
  - Chưa đổi call site — chỉ dựng khung để build được
  - _Requirements: 2.1, 2.2, 2.3, 6.1_

- [ ] 3. upstreamKiro wrapper + thay toàn bộ call site
  - Tạo `proxy/upstream_kiro.go`: `Call` gọi `CallKiroAPI`; `ClassifyError` gói matcher hiện có
  - Thay 9 call site `CallKiroAPI(...)` (handler.go ×5, responses_handler.go ×2, proxy_import.go ×1)
    bằng `dispatchUpstream(account, req, cb)`
  - Chạy full `go test ./...` — hành vi Kiro KHÔNG được đổi (regression gate)
  - _Requirements: 2.2, 8.1_

- [ ] 4. Bỏ logic Kiro cho account non-Kiro
  - `ensureValidToken`: `Provider != "kiro"` → return nil (key tĩnh, không refresh)
  - Background refresh goroutine: skip account non-Kiro
  - Payload truncation ~2MB: giữ trong đường Kiro, non-Kiro không áp
  - Test: account non-kiro không bị gọi RefreshToken/RefreshAccountInfo
  - _Requirements: 8.1, 8.2, 8.3_

- [ ] 5. Cross-format translator: openAIToClaude + claudeToOpenAI
  - Thêm vào `proxy/translator.go`: dịch OpenAI Chat request → Claude Messages và ngược lại
  - Bao gồm: messages, system prompt, tools/tool_calls, stream flag
  - Test round-trip cơ bản: text + một tool call mỗi chiều
  - _Requirements: 3.2, 4.2_

- [ ] 6. upstreamAnthropic (passthrough + cross-format)
  - Tạo `proxy/upstream_anthropic.go`: POST `{BaseURL}/v1/messages`, headers x-api-key +
    anthropic-version; parse SSE → KiroStreamCallback (text, thinking, tool_use, usage)
  - ClientFormat claude → dùng RawClaude; openai → openAIToClaude trước
  - Dùng `GetClientForProxy(account.ProxyURL)`; không log ApiKey
  - `ClassifyError`: 429→cooldown, 401/403→disable, 5xx/network→retry
  - Test: httptest.Server phát SSE mẫu, assert callback
  - _Requirements: 3.1, 3.2, 3.3, 6.2_

- [ ] 7. upstreamOpenAI (passthrough + cross-format)
  - Tạo `proxy/upstream_openai.go`: POST `{BaseURL}/chat/completions`, Bearer key,
    stream + stream_options.include_usage; parse chunk → callback (content, reasoning_content,
    tool_calls gộp theo index, usage)
  - ClientFormat openai → dùng RawOpenAI; claude → claudeToOpenAI trước
  - `ClassifyError`: 429→cooldown, 401→disable, 5xx→retry
  - Test: httptest.Server phát chunk mẫu, assert callback (kể cả tool call arguments nối chuỗi)
  - _Requirements: 4.1, 4.2, 4.3, 6.2_

- [ ] 8. Model routing biết provider
  - Thêm `Models []string` (override tùy chọn) vào Account
  - `pool.accountHasModel` rẽ nhánh: kiro giữ modelLists; non-kiro so prefix provider + override
  - Route quyết định trước khi alias Kiro-hóa; `gpt-4o` ưu tiên account OpenAI thật (Req 5.4)
  - Test: accountHasModel non-kiro; gpt-4o route tới OpenAI over alias Kiro
  - _Requirements: 5.1, 5.2, 5.3, 5.4_

- [ ] 9. Failover gọi ClassifyError per-provider
  - Refactor `handleAccountFailure` hỏi `UpstreamClient.ClassifyError` theo provider account
  - Kiro giữ nguyên matcher; giữ vòng retry excluded + maxAccountRetryAttempts
  - Test: lỗi 429 anthropic → cooldown; 401 → disable
  - _Requirements: 6.1, 6.2, 6.3_

- [ ] 10. Bảng quy đổi credit thống nhất (cost → credit)
  - Thêm cấu hình `CreditPricing` vào config: map[provider+model] → {costPerInputToken, costPerOutputToken}
    + hệ số markup toàn cục (mặc định 1.0), có getter/setter persist như các setting khác
  - Thêm hàm `ComputeCredits(provider, model string, inTok, outTok int, kiroCredits float64) float64`:
    Kiro → dùng `kiroCredits` (OnCredits) làm cơ sở; non-Kiro → (inTok×costIn + outTok×costOut); nhân markup
  - Ở mọi streaming handler, thay `credits` thô bằng `ComputeCredits(...)` trước khi gọi `RecordApiKeyUsage`
  - Test: Kiro giữ số OnCredits × markup; OpenAI/Anthropic tính theo token × hệ số; markup áp đúng
  - _Requirements: 10.1, 10.2, 10.3, 10.4, 10.5_

- [ ] 11. Fallback chéo nguồn giữ đơn vị credit thống nhất
  - Xác nhận `nextAccountForKey` đã fallback shared pool khi bound account hết dùng (hành vi hiện có)
  - Đảm bảo credit trừ user luôn qua `ComputeCredits` của provider THỰC SỰ phục vụ request (kể cả sau fallback)
  - Test: key bind account Kiro hết quota → fallback account OpenAI → credit trừ theo bảng OpenAI × markup
  - _Requirements: 11.1, 11.2, 11.3_

- [ ] 12. Admin UI cho multi-provider + bảng giá credit
  - Form account: dropdown provider; anthropic/openai → hiện apiKey + baseURL, ẩn OAuth/region
  - List: badge provider; usage/overage hiện N/A cho non-kiro
  - Mask apiKey trong response admin API
  - Trang cấu hình bảng quy đổi credit (per provider+model cost + markup), hiển thị credit thống nhất trên usage
  - _Requirements: 7.1, 7.2, 7.3, 10.2, 10.5_

## Out of scope (ghi chú)

- Gom tài khoản subscription web (ChatGPT Plus/Max, Claude.ai) qua session/cookie: KHÔNG làm.
  Không có API chính thức, phải reverse-engineer, rủi ro khóa tài khoản + vi phạm ToS. Chỉ hỗ trợ
  API key thật (OpenAI, Anthropic, OpenRouter, và endpoint OpenAI-compatible bất kỳ).
