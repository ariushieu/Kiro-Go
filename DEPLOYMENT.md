# Kiro-Go — Hướng dẫn triển khai (Docker)

Tài liệu này mô tả cách triển khai Kiro-Go đúng cách bằng Docker Compose, giải
thích cơ chế loopback port của luồng đăng nhập SSO, và liệt kê các lỗi thường gặp
cùng cách khắc phục.

---

## 1. Tổng quan

Kiro-Go là reverse proxy dịch request Kiro API sang định dạng OpenAI/Anthropic,
kèm admin panel quản lý tài khoản. Service expose:

| Endpoint | Mô tả |
|----------|-------|
| `/admin` | Admin panel (web UI) |
| `/v1/messages` | Claude API compatible |
| `/v1/chat/completions` | OpenAI API compatible |

Cổng chính: **8080**. Mật khẩu admin mặc định: **`changeme`** (đổi qua
`ADMIN_PASSWORD`, xem mục 5).

---

## 2. Triển khai chuẩn (Docker Compose)

Đây là cách **được khuyến nghị** — cấu hình nằm trong `docker-compose.yml`,
version-controlled, tái lập được.

```bash
# Từ thư mục gốc repo
docker compose up -d --build
```

- `--build` ép build image từ source hiện tại. **Bắt buộc dùng khi code thay đổi**
  — nếu bỏ qua, compose sẽ tái sử dụng image cache cũ và chạy nhầm version cũ
  (xem Lỗi #3 ở mục 6).
- `-d` chạy nền (detached).

Kiểm tra:

```bash
docker compose ps                       # container Up?
curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/admin   # mong đợi 200
```

Mở admin panel: <http://localhost:8080/admin> → nhập mật khẩu (`changeme`).

Dừng / khởi động lại:

```bash
docker compose down        # dừng + xoá container (data giữ nguyên nhờ volume mount)
docker compose up -d       # chạy lại (không build lại)
docker compose up -d --build   # chạy lại CÓ build lại từ source
```

---

## 3. Cấu hình `docker-compose.yml`

```yaml
services:
  kiro-go:
    build: .
    ports:
      - "8080:8080"   # admin panel + API
      - "3128:3128"   # loopback SSO (port ưu tiên)
      - "4649:4649"   # loopback SSO fallback
      - "6588:6588"   # loopback SSO fallback
      - "8008:8008"   # loopback SSO fallback
      - "9091:9091"   # loopback SSO fallback
    volumes:
      - ./data:/app/data            # persist config + accounts
    environment:
      - CONFIG_PATH=/app/data/config.json
      - LOOPBACK_HOST=0.0.0.0       # BẮT BUỘC trong Docker — xem mục 4
    restart: unless-stopped
```

**Các điểm quan trọng:**

- **`LOOPBACK_HOST=0.0.0.0`** — bắt buộc khi chạy trong container. Mặc định app
  bind loopback server trên `127.0.0.1`, nhưng callback SSO đến từ browser **ngoài**
  container nên phải bind `0.0.0.0`. Giá trị phải đủ 4 octet — gõ thiếu thành
  `0.0.0` sẽ khiến bind fail toàn bộ (xem Lỗi #1).
- **`./data:/app/data`** — mount này giữ `config.json` (gồm accounts, stats, mật
  khẩu). Nhờ nó, xoá/tạo lại container **không mất dữ liệu**.
- **Danh sách port** chỉ gồm 5 port loopback thấp (3128–9091), **không** có dải
  `49153–53153`. Lý do ở mục 4 và Lỗi #2.

---

## 4. Cơ chế loopback port (luồng SSO)

Luồng "Add Account → Kiro Hosted SSO" (đăng nhập Microsoft Entra qua Kiro portal)
dùng OAuth loopback redirect. Khi bấm **Start Login**, backend:

1. Bind một HTTP loopback server trên **port trống đầu tiên** trong danh sách
   chính thức mà Kiro portal chấp nhận:

   ```
   3128, 4649, 6588, 8008, 9091, 49153, 50153, 51153, 52153, 53153
   ```

   (xem `auth/kiro_sso.go` → `kiroLoopbackPorts` / `bindKiroLoopback`)

2. Sinh Login URL trỏ về `redirect_uri=http://localhost:<port>` và trả cho UI.
3. Browser mở URL → đăng nhập Microsoft → Microsoft redirect về loopback server →
   app đổi code lấy token → tạo account.

**Vì sao chỉ cần publish 5 port thấp:** Trong container, chỉ có `:8080` đang
listen, nên port `3128` luôn trống và **luôn được chọn đầu tiên**. 5 port cao
(`49153+`) chỉ là fallback không bao giờ chạm tới trong môi trường container — và
trên macOS chúng còn nằm trong dải ephemeral nên gây lỗi bind (Lỗi #2). Do đó
compose chỉ map 5 port thấp.

> Nếu bạn vận hành ngoài Docker hoặc môi trường nơi 3128 đã bận, hãy đảm bảo ít
> nhất một port trong danh sách trên còn trống và được publish.

---

## 5. Biến môi trường

| Biến | Mặc định | Mô tả |
|------|----------|-------|
| `CONFIG_PATH` | `data/config.json` | Đường dẫn file config |
| `LOOPBACK_HOST` | `127.0.0.1` | Host bind loopback SSO. **Đặt `0.0.0.0` trong Docker.** |
| `ADMIN_PASSWORD` | (dùng giá trị trong config, mặc định `changeme`) | Ghi đè mật khẩu admin lúc khởi động |
| `ADMIN_PATH` | `/admin` | Prefix URL của panel & API admin (ví dụ `/panel-x7k9`). Khi đặt, `/admin` cũ trả 404 trơn — scanner không định vị được panel. Không ghi vào config. |
| `LOG_LEVEL` | `info` | Mức log (`debug`/`info`/`warn`/`error`) |
| `KIRO_TRUST_PROXY` | `false` | Tin `X-Forwarded-For`/`X-Forwarded-Proto` từ reverse proxy. **Bắt buộc `true` khi sau nginx/Cloudflare** — xem §5.2. |
| `KIRO_TRUSTED_PROXY_HOPS` | `1` | Số proxy hop trước app (nginx: 1; Cloudflare→nginx→app: 2). IP client lấy từ phải sang trong `X-Forwarded-For` theo số hop này. |

Ví dụ đổi mật khẩu admin: copy `.env.example` thành `.env` cạnh `docker-compose.yml`
rồi điền giá trị — compose tự đọc file này (`.env` đã nằm trong `.gitignore`):

```bash
cp .env.example .env
```

```dotenv
ADMIN_PASSWORD=my-strong-password
ADMIN_PATH=/panel-x7k9   # tùy chọn: giấu panel khỏi đường dẫn /admin mặc định
```

### 5.1. Trỏ subdomain riêng cho admin panel (nginx)

Muốn vào panel qua `admin.domain.com/` thay vì `domain.com/panel-x7k9/` thì
**chỉ cần nginx rewrite prefix — không cần đổi code**. (Đứng sau nginx thì nhớ
set `KIRO_TRUST_PROXY` — xem §5.2.) Template đầy đủ sẵn dùng (API + subdomain
admin + redirect HTTP→HTTPS, style certbot): **`deploy/nginx-kiro-admin.conf`**. Hai điều kiện đã có sẵn
trong codebase làm điều này hoạt động:

- Frontend gọi API bằng đường dẫn **tương đối** (`web/app.js` — `fetch('api' + path)`,
  tương đối với URL trang), nên panel chạy được dưới bất kỳ prefix nào mà không
  cần biết `ADMIN_PATH`.
- Cookie session admin set `Path: "/"` (`proxy/admin_session.go`), nên vẫn đi kèm
  bình thường khi mọi thứ nằm ở root của subdomain.

```nginx
server {
    server_name admin.domain.com;

    location / {
        # Dấu "/" cuối ở proxy_pass là QUAN TRỌNG: nginx rewrite
        # /xyz → /panel-x7k9/xyz trước khi đẩy vào backend.
        proxy_pass http://127.0.0.1:8080/panel-x7k9/;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;

        # SSE log stream của panel (EventSource) — tránh nginx buffer/cắt kết nối
        proxy_buffering off;
        proxy_read_timeout 1h;
    }
}
```

Lưu ý:

- **Trỏ thẳng vào `/panel-x7k9/` (có dấu `/` cuối)**, đừng trỏ bare path. Backend
  nhận bare `ADMIN_PATH` sẽ trả redirect `Location: /panel-x7k9/` — redirect này
  lộ secret path ra browser và đưa user về đường dẫn sai trên subdomain.
- Với config trên, mọi path của subdomain đều bị prefix admin path nên API `/v1/*`
  không lộ qua subdomain admin.
- **Chặn chiều ngược lại**: backend không phân biệt Host header nên
  `domain.com/panel-x7k9/` mặc định vẫn vào được panel. Đã có subdomain riêng thì
  chặn đường này ở server block API để thu hẹp bề mặt tấn công:

  ```nginx
  # trong server block của domain API công khai
  location ^~ /panel-x7k9 {
      access_log off;
      return 444;
  }
  ```
- `publicBaseURL` (callback SSO Microsoft qua domain) là cấu hình độc lập, không
  liên quan admin path.

### 5.2. Đặt sau Cloudflare

Thêm Cloudflare (proxy bật — đám mây cam) trước nginx/app chạy được **không cần
đổi code**, nhưng phải chỉnh cấu hình:

**Bắt buộc — trust proxy + số hop.** DoS guard giới hạn request theo IP
(`KIRO_IP_RPM`, mặc định 120/phút/IP). Mặc định app lấy IP từ `RemoteAddr` —
sau Cloudflare đó là IP edge của Cloudflare, mọi client dồn chung vài IP →
một client spam là tất cả bị chặn chung. Set trong `.env`:

```dotenv
KIRO_TRUST_PROXY=true
KIRO_TRUSTED_PROXY_HOPS=2   # client → Cloudflare → nginx → app = 2 hop
                            # Cloudflare trỏ thẳng app (không nginx) = 1
```

App lấy IP client bằng cách đếm **từ phải sang** trong `X-Forwarded-For` theo số
hop (chống spoof — xem `proxy/dos_guard.go`), nên số hop phải khớp thực tế.
Biến này cũng bật nhận diện HTTPS qua `X-Forwarded-Proto` để cookie session
admin mang flag `Secure`.

**Bắt buộc — chặn traffic không qua Cloudflare.** Khi trust proxy bật, client
gọi thẳng IP gốc VPS tự forge được `X-Forwarded-For` → bypass rate limit.
Firewall/nginx chỉ cho phép [dải IP Cloudflare](https://www.cloudflare.com/ips/)
vào port 80/443.

**SSL mode: Full (strict).** Đừng dùng "Flexible" (Cloudflare→origin chạy HTTP
trần, cookie Secure cũng hỏng).

**SSE / streaming.** Cloudflare cắt kết nối idle ~100 giây (lỗi 524). Chat
streaming hiếm khi idle lâu vậy, nhưng admin log stream lúc vắng log có thể
dính. Nếu gặp: cho subdomain đó về DNS-only (đám mây xám).

**Không ảnh hưởng:** giới hạn body 100MB của Cloudflare (payload app cap ~2MB);
flow SSO Microsoft (browser trên máy host, callback `localhost:3128` không đi
qua Cloudflare).

---

## 6. Lỗi thường gặp & cách khắc phục

### Lỗi #1 — Start Login trả HTTP 500, "tất cả port loopback đều bận"

**Triệu chứng:** Bấm Start Login → console báo `500` tại
`/admin/api/auth/kiro-sso/start`.

**Nguyên nhân:** `LOOPBACK_HOST` bị set sai (ví dụ thiếu octet: `0.0.0` thay vì
`0.0.0.0`). Khi đó `net.Listen("tcp", "0.0.0:<port>")` fail cho **mọi** port →
backend báo hết port → 500.

**Khắc phục:**

```bash
# Kiểm tra giá trị thật trong container
docker compose exec kiro-go printenv LOOPBACK_HOST   # phải in đúng: 0.0.0.0

# Nếu sai: sửa docker-compose.yml rồi recreate
docker compose up -d --force-recreate
```

> Env đã baked vào container lúc tạo — sửa file compose thôi chưa đủ, phải
> recreate container.

### Lỗi #2 — Compose fail: "ports are not available: ... 49153: address already in use"

**Triệu chứng:** `docker compose up` fail khi bind port `49153` (hoặc 5015x),
dù `lsof` báo port đó trống vài giây trước.

**Nguyên nhân:** Trên macOS, dải `49153–53153` nằm **trong vùng ephemeral port**
(`sysctl net.inet.ip.portrange.first` = 49152). OS liên tục cấp các port này làm
source port cho kết nối outbound, nên việc bind cố định bị race và fail bất chợt.

**Khắc phục:** Đã loại 5 port này khỏi `docker-compose.yml` (chúng không cần
thiết — xem mục 4). Nếu file của bạn vẫn còn, xoá các dòng `49153`–`53153`.

### Lỗi #3 — App chạy version cũ sau khi sửa code

**Triệu chứng:** Footer admin panel hiển thị version cũ dù `version.json` trên đĩa
đã mới hơn.

**Nguyên nhân:** `docker compose up` **không tự rebuild** khi image đã tồn tại
trong cache — nó tái dùng image cũ.

**Khắc phục:**

```bash
docker compose up -d --build   # ép rebuild từ source
```

### Lỗi #4 — Trộn lẫn `docker run` thủ công và `docker compose`

**Triệu chứng:** Có hai container Kiro song song, tên khác nhau
(`kiro-go` vs `kiro-go-kiro-go-1`), đụng port nhau.

**Nguyên nhân:** Container tạo bằng `docker run` thủ công độc lập với container do
compose quản lý. Compose không "thấy" container thủ công.

**Khắc phục — thống nhất một cách duy nhất (compose):**

```bash
# Xoá mọi container Kiro tạo thủ công
docker ps -a --filter ancestor=kiro-go --format '{{.Names}}' | xargs -r docker rm -f

# Từ nay chỉ dùng compose
docker compose up -d --build
```

---

## 7. Build thủ công (không khuyến khích)

Chỉ dùng khi cần chạy ngoài compose. Lưu ý phải tự đặt đúng `LOOPBACK_HOST` và
publish port:

```bash
docker build -t kiro-go .
docker run -d --name kiro-go \
  -p 8080:8080 -p 3128:3128 -p 4649:4649 -p 6588:6588 -p 8008:8008 -p 9091:9091 \
  -e CONFIG_PATH=/app/data/config.json \
  -e LOOPBACK_HOST=0.0.0.0 \
  -v "$(pwd)/data:/app/data" \
  kiro-go
```

> Đây chính là nguồn gốc các lỗi #1, #2, #4 ở trên (typo env, dư port, trộn lẫn
> với compose). Nếu không có lý do đặc biệt, hãy dùng `docker compose` ở mục 2.

---

## 8. Chạy không cần Docker (dev local)

```bash
go build -o kiro-go .
./kiro-go      # mặc định LOOPBACK_HOST=127.0.0.1, bind 0.0.0.0:8080
```

Mở <http://localhost:8080/admin>. Khi chạy local (không container), browser và
loopback server cùng máy nên **không cần** đặt `LOOPBACK_HOST=0.0.0.0`.
