# Implementation Plan — Kiro SSO External IdP Hardening

## Overview

Kế hoạch triển khai hardening cho flow Kiro Hosted SSO / External IdP (Microsoft Entra),
dựa trên requirements.md và design.md. Các task được nhóm theo file/khu vực, ưu tiên các
must-fix (port, single-shot, state) trước, sau đó tới robustness và tài liệu.

## Tasks

- [ ] 1. Loopback port allocation theo tập port chính thức Kiro
  - Thêm `kiroLoopbackPorts` (3128, 4649, 6588, 8008, 9091, 49153, 50153, 51153, 52153, 53153) và hàm `bindKiroLoopback()` trong `auth/kiro_sso.go`
  - Thêm test-override để dùng port `:0` khi chạy test
  - Thay `net.Listen("tcp","127.0.0.1:0")` trong `StartKiroSsoLogin` bằng `bindKiroLoopback()`; lỗi khi tất cả port bận
  - _Requirements: 1.1, 1.2, 1.3, 1.4_

- [ ] 2. Bind IPv4 + IPv6 best-effort trên cùng port
  - Tách hàm `serveLoopback` bind `127.0.0.1` (bắt buộc) và `[::1]` (best-effort, log debug nếu fail)
  - Giữ host `localhost` trong redirect_uri Leg-1 và Leg-2
  - _Requirements: 8.2_

- [ ] 3. Single-shot chỉ commit sau discovery thành công
  - Bỏ set `leg2Processing` trong `handleLoopback`
  - Trong `handleExternalIdpDescriptor`, commit cờ (có re-check dưới lock) sau khi discovery + validate endpoint OK, trước khi 302
  - _Requirements: 2.1, 2.2, 2.3_

- [ ] 4. Nới lỏng validate state ở Leg-1
  - Thay khối fail-on-mismatch bằng log debug; không return/pushError
  - Giữ nguyên check nghiêm ngặt `s.IdPState` ở `handleOAuthCallback`
  - _Requirements: 3.1, 3.2, 3.3_

- [ ] 5. Kiểm tra error của rand.Read khi sinh PKCE verifier Leg-2
  - Nếu fail → writeSSOErrorPage + pushError
  - _Requirements: 5.3_

- [ ] 6. Thêm scope vào token exchange
  - Truyền `s.IdPScopes` vào `exchangeExternalIdpCode`; set `scope` khi non-empty
  - _Requirements: 5.1_

- [ ] 7. Cache token endpoint trong Account
  - Thêm field `IdPTokenEndpoint` vào `config.Account` (`config/config.go`)
  - `apiPollKiroSso` lưu token endpoint từ result; `RefreshExternalIdpToken` nhận tokenEndpoint, chỉ discovery khi rỗng
  - Đảm bảo `KiroSsoResult`/session mang token endpoint ra ngoài
  - _Requirements: 5.2_

- [ ] 8. LimitReader + Accept header cho discovery và token
  - `discoverOIDCEndpoints`: set `Accept: application/json`, đọc qua `io.LimitReader(body, 1<<20)`
  - `exchangeExternalIdpCode` và `RefreshExternalIdpToken`: bọc body bằng `io.LimitReader`
  - Giữ không echo body discovery vào error
  - _Requirements: 8.3, 8.4, 8.5_

- [ ] 9. Xử lý social code ở root thay vì 204 im lặng
  - Nhánh fallback trong `handleLoopback`: nếu có `code` → error page "social chưa hỗ trợ" + pushError
  - _Requirements: 8.1_

- [ ] 10. external_idp: bỏ gọi usage API và auto-ban trong RefreshAccountInfo
  - `proxy/kiro_api.go`: early-return cho external_idp, gọi ResolveProfileArn best-effort, không set BANNED
  - _Requirements: 4.1, 4.3, 4.4_

- [ ] 11. apiPollKiroSso: bỏ màn ban-rồi-unban
  - Xóa block re-enable thừa; giữ fetchAndCacheAccountModels trong goroutine background
  - _Requirements: 4.2_

- [ ] 12. State REAUTH_REQUIRED khi refresh token hết hạn
  - Thêm helper `isInvalidGrant`; trong xử lý refresh failure (background/handleAccountFailure) đánh dấu external_idp → BanStatus="REAUTH_REQUIRED", BanReason="Re-authentication required", Enabled=false
  - Transient (5xx/network) không đánh dấu
  - _Requirements: 6.1, 6.3_

- [ ] 13. UI hiển thị trạng thái REAUTH_REQUIRED
  - `web/app.js` + `web/locales/en.json`, `zh.json`: nhãn + nút khởi động lại SSO cho account cần re-auth
  - _Requirements: 6.2_

- [ ] 14. Session reaper định kỳ + shutdown an toàn
  - Thay goroutine one-shot bằng ticker (sync.Once), chạy `cleanupExpiredKiroSsoSessions` mỗi phút
  - `shutdownLoopbackServer` dùng `Shutdown(ctx)` timeout 2s, fallback Close()
  - http.Server set `ReadHeaderTimeout: 5s`
  - _Requirements: 7.1, 7.2, 7.3, 7.4_

- [ ] 15. Cập nhật tài liệu cấu hình admin
  - Ghi rõ yêu cầu Entra: requestedAccessTokenVersion=2, scope codewhisperer:* + offline_access; thêm mục troubleshooting token version
  - _Requirements: 9.1, 9.2_

- [ ] 16. Mở rộng test và verify build
  - Bổ sung unit test theo Testing Strategy trong design (port binding, state Leg-1, single-shot, scope, cached endpoint, isInvalidGrant, social code, LimitReader)
  - Thêm proxy test: RefreshAccountInfo external_idp không ban; apiPollKiroSso không ban-rồi-unban
  - Chạy `go build ./...` và `go test ./...`, sửa lỗi nếu có
  - _Requirements: 1-8_

## Task Dependency Graph

```json
{
  "waves": [
    { "wave": 1, "tasks": ["1", "3", "4", "5", "6", "7", "9", "10", "14", "15"] },
    { "wave": 2, "tasks": ["2", "8", "11", "12"] },
    { "wave": 3, "tasks": ["13"] },
    { "wave": 4, "tasks": ["16"] }
  ],
  "dependencies": {
    "2": ["1"],
    "8": ["7"],
    "11": ["10"],
    "12": ["7"],
    "13": ["12"],
    "16": ["1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14"]
  }
}
```

Diễn giải: task 7 là tiền đề cho 8/12; task 10 là tiền đề cho 11; task 12 là tiền đề cho 13;
task 2 phụ thuộc 1. Task 16 (test + build) chạy cuối cùng. Task 15 (tài liệu) độc lập.

## Notes

- Phải-fix trước (ảnh hưởng flow chạy được với portal thật): task 1, 3, 4.
- Các Open Questions trong design (Entra localhost vs 127.0.0.1, ListAvailableProfiles với
  token Entra, portal có từ chối port ngoài tập 10) cần verify với token thật nhưng không
  chặn việc triển khai các task này.
- Sau mỗi nhóm thay đổi, chạy `go build ./...` để bắt lỗi sớm; chạy `go test ./...` ở task 16.
- Giữ hành vi auth của IdC/BuilderID/social hiện có — chỉ tách nhánh cho external_idp.
