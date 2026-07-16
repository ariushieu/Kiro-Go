# Requirements Document — Multi-Provider Pool

## Introduction

Kiro-Go hiện là reverse proxy khóa cứng vào **một upstream duy nhất là Kiro/AWS**: mọi
request Claude/OpenAI vào đều được dịch sang định dạng Kiro (CodeWhisperer/Q), gọi
`CallKiroAPI` qua AWS Event Stream, rồi map kết quả ngược lại. Toàn bộ khái niệm "account"
trong `config` + `pool` đều giả định là tài khoản Kiro với OAuth/AWS token.

Spec này mở rộng proxy để pool có thể chứa **nhiều loại backend khác nhau**:

- **Kiro** (hiện tại, giữ nguyên) — tài khoản AWS/Kiro, Event Stream.
- **Anthropic API** — API key trực tiếp tới `api.anthropic.com` (định dạng gần như passthrough
  vì request vào đã là Claude Messages format).
- **OpenAI-compatible** — API key tới `api.openai.com` hoặc bất kỳ endpoint tương thích
  OpenAI (OpenRouter, Groq, DeepSeek, LM Studio, vLLM, ...), cấu hình qua `baseURL`.

Mục tiêu: người vận hành thêm một "account" chỉ bằng API key + baseURL, pool tự route request
tới đúng backend theo model, và toàn bộ cơ chế hiện có (weighted round-robin, cooldown, failover,
per-key binding, thống kê) hoạt động thống nhất cho mọi provider.

**Ngoài phạm vi (Non-goals):** Không reverse-engineer tài khoản subscription web (ChatGPT
Plus/Max, Claude.ai Pro) qua session/cookie. Các tài khoản đó không có API chính thức, dễ bị
khóa và vi phạm ToS — xem Requirement 10. Chỉ hỗ trợ backend dùng **API key tĩnh**.

## Glossary

- **Provider / Backend**: loại upstream của một account — `kiro`, `anthropic`, `openai`.
- **UpstreamClient**: interface trừu tượng hóa lời gọi upstream + parse stream, tách khỏi
  `CallKiroAPI`. Mỗi provider có một implementation.
- **Passthrough**: khi request vào và backend cùng định dạng (Claude→Anthropic,
  OpenAI→OpenAI), chỉ cần forward + đổi auth header, không dịch payload.
- **Cross-format**: khi request vào và backend khác định dạng (ví dụ OpenAI→Anthropic),
  cần dịch qua lại.
- **Model routing**: chọn account có provider phục vụ được model được yêu cầu.
- **StreamCallback**: `KiroStreamCallback` hiện tại (`OnText`/`OnToolUse`/`OnComplete`/
  `OnCredits`/`OnContextUsage`) — hợp đồng sự kiện mà mọi provider phải phát ra.

## Requirements

### Requirement 1: Trường Provider trên Account

**User Story:** Là quản trị viên, tôi muốn khai báo loại backend cho mỗi account để pool biết
gọi upstream nào.

#### Acceptance Criteria
1. WHEN load config THEN mỗi `Account` SHALL có trường `Provider` với giá trị `"kiro"`,
   `"anthropic"`, hoặc `"openai"`.
2. WHERE `Provider` rỗng trong config cũ THEN hệ thống SHALL migrate mặc định thành `"kiro"`
   (backward-compatible, không phá account hiện có).
3. WHEN `Provider` là `anthropic` hoặc `openai` THEN account SHALL dùng các trường
   `ApiKey` (bearer key) và `BaseURL` (endpoint gốc), không dùng OAuth/AWS token fields.
4. IF `Provider` không thuộc tập giá trị hợp lệ THEN hệ thống SHALL từ chối lưu account và
   trả lỗi rõ ràng.

### Requirement 2: UpstreamClient interface

**User Story:** Là lập trình viên, tôi muốn một interface chung để thêm provider mới mà không
phải sửa các streaming handler.

#### Acceptance Criteria
1. WHEN gọi upstream THEN handler SHALL đi qua một interface `UpstreamClient` thay vì gọi
   trực tiếp `CallKiroAPI`.
2. WHEN account có `Provider="kiro"` THEN hệ thống SHALL dùng implementation bọc `CallKiroAPI`
   hiện tại, giữ nguyên hành vi bit-for-bit.
3. WHEN thêm một provider mới THEN chỉ SHALL cần thêm một implementation `UpstreamClient` và
   đăng ký nó, không sửa `handler.go` streaming logic.
4. WHERE mọi provider THEN implementation SHALL phát sự kiện qua cùng `KiroStreamCallback`
   contract (`OnText`, `OnToolUse`, `OnComplete`, `OnError`, và tùy chọn `OnCredits`/
   `OnContextUsage`).

### Requirement 3: Anthropic passthrough backend

**User Story:** Là người vận hành, tôi muốn cắm API key Anthropic thật để phục vụ model Claude.

#### Acceptance Criteria
1. WHEN request Claude Messages tới account `anthropic` THEN hệ thống SHALL forward payload
   tới `{BaseURL}/v1/messages` với header `x-api-key` + `anthropic-version`, không dịch qua Kiro.
2. WHEN Anthropic trả SSE THEN hệ thống SHALL parse các event Anthropic (`content_block_delta`,
   `message_delta`, ...) và ánh xạ vào `KiroStreamCallback`.
3. WHEN request đến ở định dạng OpenAI nhưng route tới account `anthropic` THEN hệ thống SHALL
   dịch OpenAI→Claude trước khi forward (cross-format).
4. WHEN Anthropic trả lỗi (4xx/5xx) THEN hệ thống SHALL map sang phân loại lỗi của failover
   (quota/auth/transient) để pool xử lý nhất quán.

### Requirement 4: OpenAI-compatible backend

**User Story:** Là người vận hành, tôi muốn cắm API key OpenAI hoặc endpoint tương thích
(OpenRouter, Groq, DeepSeek, vLLM) qua `baseURL`.

#### Acceptance Criteria
1. WHEN request tới account `openai` THEN hệ thống SHALL gọi `{BaseURL}/chat/completions`
   với header `Authorization: Bearer {ApiKey}`.
2. WHEN request đến ở định dạng Claude nhưng route tới account `openai` THEN hệ thống SHALL
   dịch Claude→OpenAI trước khi forward, và dịch response OpenAI→Claude khi trả về.
3. WHEN OpenAI trả SSE (`chat.completion.chunk`) THEN hệ thống SHALL parse `choices[].delta`
   (content + tool_calls) và ánh xạ vào `KiroStreamCallback`.
4. WHERE `BaseURL` do người dùng cấu hình THEN hệ thống SHALL chuẩn hóa (bỏ trailing slash,
   yêu cầu scheme https trừ khi host là loopback) để tránh lỗi ghép URL.

### Requirement 5: Model routing theo provider

**User Story:** Là người dùng API, tôi muốn gọi `gpt-4o` được route tới account OpenAI và
`claude-*` tới account Kiro hoặc Anthropic.

#### Acceptance Criteria
1. WHEN chọn account cho một model THEN pool SHALL chỉ chọn account có provider phục vụ được
   model đó.
2. WHERE nhiều provider phục vụ cùng một model (ví dụ Claude qua cả Kiro lẫn Anthropic) THEN
   pool SHALL cân bằng tải theo weighted round-robin hiện có, giữ nguyên cooldown/quota skip.
3. WHEN không có account nào phục vụ được model THEN hệ thống SHALL trả lỗi rõ ràng
   (không im lặng route sai provider).
4. WHERE model alias/normalization hiện áp cho Kiro (`translator.go`) THEN việc route SHALL
   dựa trên tên model đã chuẩn hóa, và alias cross-provider (ví dụ `gpt-4o`→Claude) chỉ áp
   khi không có account OpenAI thật phục vụ model đó.

### Requirement 6: Failover và cooldown thống nhất

**User Story:** Là người vận hành, tôi muốn cơ chế retry/cooldown/disable hoạt động giống nhau
cho mọi provider.

#### Acceptance Criteria
1. WHEN một upstream provider lỗi THEN `handleAccountFailure` SHALL phân loại lỗi
   (quota → cooldown, auth → disable, transient → retry) dựa trên response của provider đó.
2. WHERE error classification hiện là string-matching cho Kiro THEN mỗi provider SHALL cung
   cấp mapping riêng (HTTP status + error body) về cùng tập action.
3. WHEN retry qua các account THEN vòng lặp retry hiện có (`excluded` set,
   `maxAccountRetryAttempts`) SHALL hoạt động không đổi, kể cả khi các account khác provider.

### Requirement 7: Admin UI + config cho provider mới

**User Story:** Là quản trị viên, tôi muốn thêm/sửa account non-Kiro qua admin panel.

#### Acceptance Criteria
1. WHEN thêm account qua `/admin/api/*` THEN hệ thống SHALL chấp nhận `provider`, `apiKey`,
   `baseURL` và validate theo Requirement 1.
2. WHEN hiển thị account list THEN admin panel SHALL cho thấy provider của mỗi account.
3. WHERE account non-Kiro không có usage/subscription từ AWS THEN UI SHALL không hiển thị
   các field không áp dụng (usage credits, overage, ban-by-quota) hoặc hiển thị "N/A".
4. WHEN lưu account THEN `apiKey` SHALL được xử lý như secret (không log giá trị, mask trong UI).

### Requirement 8: Không rò rỉ khái niệm Kiro sang provider khác

**User Story:** Là lập trình viên, tôi muốn code các nhánh non-Kiro không vô tình gọi logic
đặc thù AWS.

#### Acceptance Criteria
1. WHEN account non-Kiro chạy qua background refresh THEN hệ thống SHALL không gọi
   `RefreshToken` (OIDC), `RefreshAccountInfo`, hay profile ARN resolution.
2. WHEN account non-Kiro cần "token hợp lệ" THEN `ensureValidToken` SHALL no-op (API key
   tĩnh luôn hợp lệ) thay vì cố refresh.
3. WHERE payload truncation (~2MB Kiro cap) THEN giới hạn đó SHALL chỉ áp cho provider Kiro;
   provider khác dùng giới hạn riêng của họ hoặc không giới hạn.

### Requirement 9: Thống kê và per-key binding hoạt động cho mọi provider

**User Story:** Là quản trị viên, tôi muốn thống kê usage và binding API key hoạt động bất kể
provider.

#### Acceptance Criteria
1. WHEN request hoàn tất qua bất kỳ provider THEN `UpdateStats`/`UpdateAccountStats`/
   `RecordApiKeyUsage` SHALL cập nhật như hiện tại (qua `markDirtyLocked` + background flush).
2. WHERE một API key bind vào các account cụ thể (`BoundAccountIDs`) THEN binding SHALL hoạt
   động không phân biệt provider của account.
3. WHEN provider trả token usage THEN hệ thống SHALL dùng số thật; nếu không có, SHALL fallback
   ước lượng như đường Kiro hiện tại.

### Requirement 10: Credit thống nhất — quy đổi cost đa nguồn về một đơn vị

**User Story:** Là người vận hành, tôi muốn gom nhiều nguồn (Kiro credit, OpenAI $/token,
Anthropic $/token) rồi phân phối lại cho end-user bằng **một đơn vị credit chung**, để việc trừ
quota của user không phụ thuộc nguồn nào phục vụ request.

#### Acceptance Criteria
1. WHEN một request hoàn tất qua bất kỳ provider THEN hệ thống SHALL quy đổi chi phí thực của
   request đó về **một đơn vị credit thống nhất** trước khi trừ vào key của end-user (khớp lựa
   chọn "credit thống nhất").
2. WHERE mỗi provider/model có cách tính giá khác nhau THEN hệ thống SHALL có một bảng quy đổi
   cấu hình được: `costPerInputToken` / `costPerOutputToken` (hoặc hệ số credit) theo
   provider+model, với giá trị mặc định hợp lý.
3. WHEN Kiro báo `OnCredits` (credit thực của Kiro) THEN hệ thống SHALL dùng số đó làm cơ sở quy
   đổi cho nguồn Kiro; WHEN provider chỉ trả token (OpenAI/Anthropic) THEN hệ thống SHALL tính
   credit = tokens × hệ số theo bảng.
4. WHEN trừ quota key THEN `RecordApiKeyUsage` SHALL nhận credit đã quy đổi thống nhất (không
   phải con số thô đặc thù từng nguồn), giữ nguyên `TokenLimit`/`CreditLimit` hiện có.
5. WHERE người vận hành muốn markup THEN bảng quy đổi SHALL cho phép một hệ số nhân (ví dụ bán
   lại 1.2×) để tách giá vốn khỏi giá phân phối cho user.

### Requirement 11: Fallback chéo nguồn khi nguồn bind hết quota/lỗi

**User Story:** Là người vận hành, tôi muốn khi nguồn mà key đang ưu tiên bị hết quota hoặc lỗi,
request tự chuyển sang nguồn khác (kể cả khác provider) thay vì fail.

#### Acceptance Criteria
1. WHEN một key có `BoundAccountIDs` và tất cả account bind đều không dùng được (quota/cooldown/
   lỗi) THEN hệ thống SHALL fallback sang shared pool như hành vi hiện tại (khớp lựa chọn
   "fallback sang nguồn khác").
2. WHERE fallback xảy ra THEN account được chọn CÓ THỂ khác provider với account bind ban đầu,
   miễn là phục vụ được model yêu cầu (theo Requirement 5).
3. WHEN fallback chọn nguồn khác provider THEN việc trừ credit user SHALL vẫn theo đơn vị thống
   nhất (Requirement 10), để user không bị tính giá lệch chỉ vì rơi sang nguồn đắt/rẻ hơn.
4. WHERE người vận hành muốn giới hạn fallback chỉ trong một tập nguồn THEN đây là mở rộng tương
   lai (per-key allowed-provider set) — spec này giữ hành vi fallback rộng như hiện tại.

### Requirement 12: Ranh giới an toàn — không hỗ trợ subscription web

**User Story:** Là người vận hành, tôi muốn hiểu rõ giới hạn để không đặt kỳ vọng sai.

#### Acceptance Criteria
1. WHERE tài khoản subscription web (ChatGPT Plus/Max, Claude.ai Pro) THEN spec này SHALL
   KHÔNG hỗ trợ import qua session/cookie/reverse-engineering.
2. WHEN người dùng hỏi về loại account đó THEN tài liệu SHALL nêu rõ rủi ro (ToS, dễ bị khóa,
   không API chính thức) và khuyến nghị dùng API key.
