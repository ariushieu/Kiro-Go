# Design — Kiro SSO External IdP Hardening

## Overview

Tài liệu này mô tả thiết kế kỹ thuật để hardening flow Kiro Hosted SSO / External IdP
(Microsoft Entra) trong Kiro-Go, dựa trên review code hiện tại và research tài liệu chính
thức của Kiro + Microsoft + RFC 8252 + reference zsecducna/cli-cache-proxy-api.

Phạm vi sửa đổi tập trung ở 3 file: `auth/kiro_sso.go` (chính), `auth/oidc.go`,
`proxy/handler.go` và `proxy/kiro_api.go`. Bổ sung tài liệu hướng dẫn.

## Architecture

Flow giữ nguyên kiến trúc 2-leg PKCE:

```
Browser ──(Leg-1 PKCE)──> app.kiro.dev/signin
   │                              │
   │   portal phát hiện email enterprise → 302 về loopback root
   ▼                              ▼
loopback :PORT  ◄── /?login_option=external_idp&issuer_url=...&client_id=...
   │  handleExternalIdpDescriptor: validate → OIDC discovery → commit single-shot
   │  → 302 browser tới Microsoft Entra authorize (Leg-2 PKCE mới)
   ▼
Microsoft Entra ──(user auth)──> 302 về loopback /oauth/callback?code=...&state=...
   │  handleOAuthCallback: validate IdP state → exchange code lấy token
   ▼
ResultCh ──> apiPollKiroSso tạo account external_idp
```

Các thay đổi là cải tiến cục bộ trong từng bước, không đổi kiến trúc tổng thể.

## Components and Interfaces

### 1. Port allocation (Req 1)

Thêm danh sách port chính thức và hàm bind tuần tự:

```go
// auth/kiro_sso.go
var kiroLoopbackPorts = []int{3128, 4649, 6588, 8008, 9091, 49153, 50153, 51153, 52153, 53153}

func bindKiroLoopback() (net.Listener, int, error) {
    for _, p := range kiroLoopbackPorts {
        ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
        if err == nil {
            return ln, p, nil
        }
    }
    return nil, 0, fmt.Errorf("tất cả port loopback Kiro (%v) đều đang bận", kiroLoopbackPorts)
}
```

`StartKiroSsoLogin` thay `net.Listen("tcp", "127.0.0.1:0")` bằng `bindKiroLoopback()`.

**Test hook:** thêm biến `kiroLoopbackPortsOverride` để test có thể ép dùng port :0 ngẫu nhiên,
tránh xung đột port khi chạy test song song.

### 2. IPv6 loopback + redirect_uri (Req 8.2)

Quyết định thiết kế: **dùng `127.0.0.1` trong redirect_uri** thay vì `localhost`. Đây là cách
đơn giản và tất định nhất, tránh phải bind kép IPv4+IPv6 và tránh việc browser resolve
`localhost` thành `::1`.

- Leg-1 redirect_uri: `http://127.0.0.1:<port>`
- Leg-2 redirect_uri: `http://127.0.0.1:<port>/oauth/callback`

Lưu ý tương thích: tài liệu Entra đăng ký `http://localhost/oauth/callback`. Vì Entra áp dụng
RFC 8252 (bỏ qua port) nhưng KHÔNG coi `localhost` và `127.0.0.1` là tương đương về host, cần
xác nhận: nếu Entra yêu cầu đúng host `localhost`, ta phải giữ `localhost` và bind cả `[::1]`.

→ **Thiết kế chọn:** giữ host `localhost` trong redirect_uri (khớp đăng ký Entra), và bind
best-effort cả `127.0.0.1` lẫn `[::1]` trên cùng port (giống zsec). Nếu bind `[::1]` fail thì
log debug và tiếp tục.

```go
func serveLoopback(srv *http.Server, port int) (cleanup func()) {
    // bind 127.0.0.1 (bắt buộc) + [::1] (best-effort), cùng port
}
```

### 3. Single-shot commit sau discovery (Req 2)

`handleLoopback` hiện set `leg2Processing=true` trước khi gọi `handleExternalIdpDescriptor`.
Đổi: chuyển toàn bộ logic discovery vào trước, chỉ set cờ sau khi thành công, re-check dưới lock.

```go
// trong handleExternalIdpDescriptor, sau khi discovery + validate endpoint OK:
s.loopbackMu.Lock()
if s.leg2Processing {
    s.loopbackMu.Unlock()
    w.WriteHeader(http.StatusNoContent)
    return
}
s.leg2Processing = true
s.loopbackMu.Unlock()
// ... rồi mới 302 redirect
```

`handleLoopback` chỉ dispatch theo path/query, không set cờ nữa.

### 4. Nới lỏng state Leg-1 (Req 3)

Trong `handleExternalIdpDescriptor`, thay khối fail-on-mismatch bằng log:

```go
state := query.Get("state")
if state != "" && state != s.State {
    logger.Debugf("[KiroSSO] Leg-1 state khác state đã gửi (portal có thể tự sinh state); tiếp tục")
}
// KHÔNG return/pushError
```

Leg-2 (`handleOAuthCallback`) giữ nguyên check nghiêm ngặt `s.IdPState`.

### 5. external_idp không auto-ban, không gọi usage (Req 4)

`proxy/kiro_api.go` — `RefreshAccountInfo`: early-return cho external_idp ngay đầu hàm:

```go
if account.AuthMethod == "external_idp" {
    // Microsoft-issued token không hỗ trợ getUsageLimits; resolve profileArn riêng.
    info := &config.AccountInfo{LastRefresh: time.Now().Unix(), Email: account.Email, UserId: account.UserId}
    if _, err := ResolveProfileArn(account); err != nil {
        logger.Debugf("[RefreshAccountInfo] external_idp %s: ResolveProfileArn chưa sẵn sàng: %v", account.Email, err)
    }
    return info, nil
}
```

`proxy/handler.go` — `apiPollKiroSso`: xóa màn ban-rồi-unban, thay bằng gọi
`fetchAndCacheAccountModels` (đã gồm ResolveProfileArn gián tiếp qua ensureValidToken/ListModels).
Vẫn giữ goroutine background nhưng bỏ block re-enable.

### 6. scope trong exchange + cache token endpoint + rand error (Req 5)

- `exchangeExternalIdpCode`: thêm `if scopes != "" { payload.Set("scope", scopes) }` → cần
  truyền thêm tham số `scopes` vào hàm (lấy từ `s.IdPScopes`).
- Cache token endpoint: thêm field `IdPTokenEndpoint string` vào `config.Account` (json
  `idpTokenEndpoint,omitempty`). `apiPollKiroSso` lưu lại từ `result`. `RefreshExternalIdpToken`
  nhận thêm tham số tokenEndpoint; nếu rỗng mới discovery.
- `rand.Read`: kiểm tra error, nếu fail thì pushError + writeSSOErrorPage.

### 7. Re-auth state khi refresh fail (Req 6)

`auth/oidc.go` hoặc nơi gọi refresh: phân loại lỗi. Thêm helper:

```go
func isInvalidGrant(err error) bool {
    msg := strings.ToLower(err.Error())
    return strings.Contains(msg, "invalid_grant") || strings.Contains(msg, "aadsts70008") // expired/revoked
}
```

Trong `backgroundRefresh`/`handleAccountFailure`: nếu external_idp và isInvalidGrant → set
`BanStatus="REAUTH_REQUIRED"`, `BanReason="Re-authentication required"`, `Enabled=false`.
Transient (5xx/network) → không đánh dấu.

`web/app.js` + locales: hiển thị nhãn cho trạng thái REAUTH_REQUIRED với nút khởi động lại SSO.

### 8. Social code rõ ràng (Req 8.1)

Trong `handleLoopback`, nhánh fallback cuối: nếu query có `code` (social) → writeSSOErrorPage
với thông báo "Tài khoản social (Google/GitHub) chưa được hỗ trợ ở luồng này" + pushError, thay
vì 204 im lặng.

### 9. LimitReader + Accept (Req 8.3, 8.4)

- `discoverOIDCEndpoints`: dùng `http.NewRequest` + set `Accept: application/json`, đọc qua
  `io.LimitReader(resp.Body, 1<<20)` rồi `json.Unmarshal`.
- `exchangeExternalIdpCode` / `RefreshExternalIdpToken`: bọc body bằng `io.LimitReader(resp.Body, 1<<20)`.

### 10. Session reaper (Req 7)

Thay goroutine one-shot bằng ticker chạy nền (khởi động một lần khi package init hoặc khi
handler khởi động):

```go
func startKiroSsoReaper() {
    go func() {
        t := time.NewTicker(1 * time.Minute)
        defer t.Stop()
        for range t.C {
            cleanupExpiredKiroSsoSessions()
        }
    }()
}
```

`shutdownLoopbackServer` dùng `Shutdown(ctx)` với timeout 2s, fallback `Close()`. `http.Server`
set `ReadHeaderTimeout: 5 * time.Second`.

Dùng `sync.Once` để reaper chỉ chạy một lần dù `StartKiroSsoLogin` gọi nhiều lần.

## Data Models

`config.Account` thêm 1 field:
```go
IdPTokenEndpoint string `json:"idpTokenEndpoint,omitempty"`
```

`BanStatus` thêm giá trị logic mới: `"REAUTH_REQUIRED"` (không cần đổi kiểu, vẫn là string).

## Error Handling

| Tình huống | Hành vi |
|---|---|
| Tất cả 10 port bận | StartKiroSsoLogin trả lỗi, UI hiển thị |
| Leg-1 state mismatch | log debug, tiếp tục |
| Leg-1 discovery fail | single-shot KHÔNG tiêu thụ, error page |
| Leg-2 IdP state mismatch | 204, không tiêu thụ one-shot |
| Social code ở root | error page "không hỗ trợ", pushError |
| refresh invalid_grant | account → REAUTH_REQUIRED |
| refresh transient | giữ nguyên, retry lần sau |

## Correctness Properties

### Property 1: Port binding tất định
Với cùng trạng thái port hệ thống, `bindKiroLoopback` luôn chọn port trống đầu tiên theo thứ tự danh sách; redirect_uri của cả hai leg dùng đúng port đã bind.
**Validates: Requirements 1.1, 1.2, 1.4**

### Property 2: Single-shot bảo toàn
Nếu discovery/validate fail thì `leg2Processing` vẫn `false` → descriptor hợp lệ kế tiếp xử lý được; nếu thành công thì descriptor thứ hai luôn nhận 204.
**Validates: Requirements 2.1, 2.2, 2.3**

### Property 3: CSRF Leg-2 bất biến
Mọi callback `/oauth/callback` có state khác `s.IdPState` đều bị 204, không tiêu thụ one-shot.
**Validates: Requirements 3.3**

### Property 4: external_idp không bao giờ auto-ban do usage API
Với bất kỳ lỗi GetUsageLimits nào, account external_idp không bị set BanStatus=BANNED.
**Validates: Requirements 4.1, 4.2**

### Property 5: Phân loại lỗi refresh
invalid_grant → REAUTH_REQUIRED; lỗi transient (5xx/network) → không đổi trạng thái.
**Validates: Requirements 6.1, 6.3**

### Property 6: Không rò port
Sau timeout/cancel/error/success, loopback server của session được shutdown.
**Validates: Requirements 7.1, 7.2**

## Testing Strategy

Mở rộng `auth/kiro_sso_test.go`:
- `bindKiroLoopback` chọn đúng port đầu tiên trống; lỗi khi tất cả bận (dùng override).
- Leg-1 state mismatch không làm fail flow.
- Single-shot không bị tiêu thụ khi discovery fail (mock discovery trả lỗi → descriptor thứ 2
  vẫn xử lý được).
- exchange gồm scope khi non-empty.
- RefreshExternalIdpToken dùng cached endpoint, không gọi discovery khi endpoint có sẵn.
- isInvalidGrant phân loại đúng invalid_grant vs 5xx.
- social code ở root → error, không treo.
- LimitReader cắt body quá lớn.

`proxy` tests:
- RefreshAccountInfo external_idp không set BANNED dù GetUsageLimits/usage fail.
- apiPollKiroSso không còn màn ban-rồi-unban (account tạo ra Enabled=true, BanStatus rỗng/ACTIVE).

Chạy `go test ./...` và `go build ./...` để verify.

## Open Questions (cần verify với token thật, không chặn triển khai)
1. Entra coi `localhost` và `127.0.0.1` là khác host? → quyết định giữ `localhost` + bind kép.
2. `ListAvailableProfiles` có trả profileArn với token Entra v2 không? → cần test live.
3. Kiro portal có thực sự từ chối port ngoài tập 10 không? → dùng tập 10 cho an toàn.
