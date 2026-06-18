# Requirements Document

## Introduction

Implementation hiện tại của Kiro Hosted SSO / External IdP (Microsoft Entra) authentication
đã hoạt động về mặt cơ chế nhưng có một số điểm lệch khỏi reference (zsecducna/cli-cache-proxy-api)
và khỏi hành vi thực tế của Kiro portal/Microsoft Entra mà có thể làm hỏng flow hoặc gây
lỗi vận hành. Spec này gom các phát hiện từ quá trình review + research tài liệu chính thức
của Kiro thành các yêu cầu hardening cụ thể.

Nguồn tham chiếu đã verify:
- Kiro Okta IdP docs: tập 10 port loopback cố định (3128, 4649, 6588, 8008, 9091, 49153,
  50153, 51153, 52153, 53153); path `/oauth/callback`.
- Kiro Microsoft Entra IdP docs: đăng ký `http://localhost/oauth/callback` (không port);
  yêu cầu access token v2; scope `offline_access` cho refresh token.
- Microsoft Q&A + RFC 8252 §7.3: Entra bỏ qua port cho loopback redirect.
- zsec social.go: không match state ở Leg-1; bind cả IPv4+IPv6; LimitReader; single-shot
  commit sau discovery.

## Glossary

- **Leg-1**: Bước đầu — browser tới Kiro portal (app.kiro.dev/signin), portal phát hiện email enterprise và redirect về loopback với IdP descriptor.
- **Leg-2**: Bước hai — proxy redirect browser tới Microsoft Entra authorize endpoint bằng PKCE mới, nhận code về `/oauth/callback`.
- **external_idp**: AuthMethod cho tài khoản đăng nhập qua external IdP (Microsoft Entra).
- **Single-shot**: Cơ chế chỉ cho phép xử lý một descriptor external IdP cho mỗi phiên.
- **profileArn**: ARN profile CodeWhisperer cần cho request runtime, resolve qua ListAvailableProfiles.
- **REAUTH_REQUIRED**: Trạng thái account khi refresh token hết hạn, cần đăng nhập lại.

## Requirements

### Requirement 1: Loopback port khớp tập port chính thức của Kiro

**User Story:** Là người dùng đăng nhập SSO, tôi muốn loopback server bind đúng port mà
Kiro portal chấp nhận, để Leg-1 redirect không bị portal từ chối.

#### Acceptance Criteria
1. WHEN khởi tạo SSO login THEN hệ thống SHALL thử bind lần lượt các port chính thức của
   Kiro theo thứ tự: 3128, 4649, 6588, 8008, 9091, 49153, 50153, 51153, 52153, 53153.
2. WHEN một port trong danh sách trống THEN hệ thống SHALL dùng port đó cho cả Leg-1 và Leg-2.
3. IF tất cả 10 port đều bận THEN hệ thống SHALL trả về lỗi rõ ràng cho người dùng thay vì
   fallback sang port ngẫu nhiên.
4. WHEN xây redirect_uri cho cả hai leg THEN port sử dụng SHALL là port đã bind thành công.

### Requirement 2: Single-shot chỉ tiêu thụ sau khi descriptor hợp lệ

**User Story:** Là người dùng, tôi muốn một request lạc/giả mạo tới loopback không phá được
phiên login đang chạy.

#### Acceptance Criteria
1. WHEN nhận external IdP descriptor ở Leg-1 THEN hệ thống SHALL chỉ commit cờ single-shot
   (leg2Processing) SAU KHI OIDC discovery và validate endpoint thành công.
2. IF descriptor fail validate (thiếu client_id, issuer_url, discovery lỗi) THEN single-shot
   SHALL không bị tiêu thụ và phiên SHALL vẫn còn khả dụng cho descriptor hợp lệ tiếp theo.
3. WHEN có hai descriptor đến đồng thời THEN chỉ descriptor đầu tiên SHALL được redirect, các
   descriptor sau SHALL nhận 204 (re-check dưới lock).

### Requirement 3: Nới lỏng validate state ở Leg-1

**User Story:** Là người dùng, tôi muốn Leg-1 không bị từ chối khi Kiro portal trả về state
khác với state proxy gửi đi.

#### Acceptance Criteria
1. WHEN nhận descriptor ở Leg-1 THEN hệ thống SHALL KHÔNG fail flow chỉ vì state không khớp
   state đã gửi.
2. IF state mismatch ở Leg-1 THEN hệ thống SHALL log ở mức debug/warn nhưng vẫn tiếp tục flow.
3. WHEN nhận callback ở Leg-2 (/oauth/callback) THEN hệ thống SHALL vẫn validate nghiêm ngặt
   IdP state (s.IdPState) do proxy tự sinh, mismatch → 204.

### Requirement 4: Bỏ auto-ban và bỏ gọi usage API cho external_idp

**User Story:** Là quản trị viên, tôi muốn tài khoản external_idp mới tạo không bị ban oan
do GetUsageLimits fail.

#### Acceptance Criteria
1. WHEN RefreshAccountInfo chạy cho account external_idp THEN hệ thống SHALL không gọi
   GetUsageLimits HOẶC nếu gọi thì SHALL không bao giờ set BanStatus=BANNED.
2. WHEN tạo account external_idp trong apiPollKiroSso THEN hệ thống SHALL không thực hiện
   màn ban-rồi-unban; logic re-enable thừa SHALL được loại bỏ.
3. WHEN account external_idp cần profileArn THEN hệ thống SHALL resolve qua
   ListAvailableProfiles (không phụ thuộc profileArn từ token Microsoft).
4. WHERE việc detect lỗi token hiện dựa vào substring ("403","401","invalid","expired") THE
   hệ thống SHALL giữ hành vi đó cho non-external_idp nhưng tách nhánh rõ ràng cho external_idp.

### Requirement 5: Thêm scope vào token exchange; cache token endpoint

**User Story:** Là người dùng, tôi muốn token exchange/refresh khớp đúng reference và hiệu quả.

#### Acceptance Criteria
1. WHEN đổi authorization code lấy token THEN hệ thống SHALL set `scope` trong form nếu scopes
   không rỗng (khớp zsec ExchangeExternalIdpCode).
2. WHEN refresh token external_idp THEN hệ thống SHALL ưu tiên dùng token endpoint đã cache
   (lưu cùng account) trước khi chạy OIDC discovery lại.
3. WHEN sinh PKCE verifier 96-byte THEN hệ thống SHALL kiểm tra error trả về của rand.Read.

### Requirement 6: State re-authentication khi refresh token hết hạn

**User Story:** Là người dùng, tôi muốn biết rõ khi cần đăng nhập lại thay vì thấy account
bị ban không rõ lý do.

#### Acceptance Criteria
1. WHEN refresh token external_idp trả về invalid_grant hoặc lỗi refresh-token-expired THEN
   hệ thống SHALL đánh dấu account với trạng thái riêng (ví dụ BanReason="Re-authentication required").
2. WHEN account ở trạng thái cần re-auth THEN UI/admin SHALL hiển thị tín hiệu để người dùng
   chạy lại SSO login.
3. IF lỗi refresh là transient (5xx/network) THEN hệ thống SHALL không đánh dấu re-auth.

### Requirement 7: Session cleanup đáng tin cậy

**User Story:** Là người vận hành, tôi muốn loopback server luôn được shutdown để không rò port.

#### Acceptance Criteria
1. WHEN có session SSO đang tồn tại THEN hệ thống SHALL chạy reaper định kỳ (ticker) để dọn
   các session hết hạn, không phụ thuộc vào lần StartKiroSsoLogin kế tiếp.
2. WHEN session timeout/cancel/error/success THEN loopback server SHALL được shutdown.
3. WHEN tạo loopback http.Server THEN hệ thống SHALL set ReadHeaderTimeout (ví dụ 5s).
4. WHERE có thể THE hệ thống SHALL dùng Shutdown(ctx) có timeout thay vì Close() đột ngột.

### Requirement 8: Bổ sung tính năng còn thiếu so với reference

**User Story:** Là người dùng với nhiều loại tài khoản, tôi muốn flow không treo im lặng và
hoạt động trên mọi cách resolve localhost.

#### Acceptance Criteria
1. WHEN nhận social/cognito code ở root (không phải external_idp) THEN hệ thống SHALL trả về
   thông báo lỗi rõ ràng "loại tài khoản không hỗ trợ" thay vì 204 im lặng dẫn tới timeout.
2. WHEN bind loopback THEN hệ thống SHALL bind cả 127.0.0.1 và (best-effort) [::1] cho cùng
   port; HOẶC dùng 127.0.0.1 trong redirect_uri thay vì localhost.
3. WHEN đọc response OIDC discovery và token THEN hệ thống SHALL dùng io.LimitReader (ví dụ 1MB).
4. WHEN gửi request OIDC discovery THEN hệ thống SHALL set header Accept: application/json.
5. WHEN tạo lỗi từ discovery THEN hệ thống SHALL KHÔNG echo response body (giữ hành vi hiện tại).

### Requirement 9: Tài liệu cấu hình admin (token v2)

**User Story:** Là quản trị viên, tôi muốn biết yêu cầu cấu hình phía Entra để token hoạt động.

#### Acceptance Criteria
1. WHERE tài liệu hướng dẫn external_idp THE hệ thống SHALL ghi rõ Entra phải set
   requestedAccessTokenVersion=2 và cấu hình scope codewhisperer:* + offline_access.
2. WHEN token bị API từ chối THEN tài liệu troubleshooting SHALL liệt kê token version v1/v2
   là một nguyên nhân khả dĩ.

## Non-Goals
- Không triển khai social/cognito login đầy đủ (chỉ xử lý lỗi rõ ràng cho trường hợp này).
- Không thay đổi flow auth của IdC/BuilderID/social hiện có.
