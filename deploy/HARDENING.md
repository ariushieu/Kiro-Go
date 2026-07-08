# Kiro-Go — Hardening chống DoS/DDoS

Tài liệu này gồm 2 phần:
1. **App-layer** (đã code trong `proxy/dos_guard.go`) — các ENV var để tinh chỉnh.
2. **Infra-layer** (cấu hình trên VPS) — Cloudflare + Nginx + fail2ban.

Kiến trúc mục tiêu:

```
[Internet] -> Cloudflare (chan L3/L4, an IP VPS) -> Nginx (VPS:443, limit_req/conn) -> Go (127.0.0.1:8090, dos_guard)
                                                       ^ fail2ban doc log, ban IP tai pham o firewall
```

Vì sao cần cả hạ tầng: code Go chỉ chặn **L7** (flood HTTP hợp lệ). DDoS thể tích **L3/L4**
(SYN/UDP flood hàng Gbps) làm nghẽn băng thông VPS trước khi gói tin tới Go — phải chặn
TRƯỚC khi tới VPS, đó là việc của Cloudflare.

---

## ⚠️ CHECKLIST & NHỮNG ĐIỀU DỄ QUÊN

Đọc kỹ phần này — đây là các điểm gây "tưởng đã bảo vệ nhưng thực ra không".

1. **`KIRO_TRUST_PROXY` phải khớp với việc có proxy hay không.**
   - Chưa có Nginx/Cloudflare (Go hứng thẳng): để **`false`** (mặc định). Nếu bật `true`
     lúc này, kẻ tấn công tự gửi header `X-Forwarded-For` giả để né giới hạn per-IP.
   - Đã đặt Nginx/Cloudflare phía trước: **bắt buộc `true`**. Nếu để `false`, Go thấy mọi
     request đến từ `127.0.0.1` (IP của Nginx) → giới hạn per-IP gộp chung tất cả người
     dùng làm một, vô dụng.

2. **Phải khóa firewall chỉ cho IP Cloudflare vào (ufw).** Nếu không, kẻ tấn công lấy được
   IP VPS sẽ đánh thẳng, bỏ qua toàn bộ Cloudflare. Đây là điểm hay quên nhất.

3. **DNS record phải ở chế độ Proxied (đám mây CAM), không phải "DNS only" (xám).** Xám =
   lộ IP VPS = Cloudflare vô tác dụng.

4. **Đổi cổng Go về `127.0.0.1:8090:8080`** trong docker-compose. Hiện đang `8090:8080`
   (mở ra mọi interface). Để vậy thì vẫn truy cập thẳng VPS:8090 được, bỏ qua Nginx.

5. **SSE streaming dễ bị Nginx làm hỏng.** Phải có `proxy_buffering off;` và
   `proxy_read_timeout 600s;`. Quên `proxy_buffering off` → client nhận token kiểu giật cục
   hoặc treo. WriteTimeout của Go đã cố ý để 0 cho SSE — đừng đặt lại.

6. **`client_max_body_size` của Nginx phải >= `KIRO_MAX_BODY_BYTES`.** Lệch nhau thì hoặc
   Nginx chặn nhầm request hợp lệ, hoặc tầng nào đó thành vô nghĩa. Cả hai đang để 10 MiB.

7. **fail2ban ban theo IP trong log Nginx.** Phải có `set_real_ip_from` + `real_ip_header
   CF-Connecting-IP` thì log mới ghi IP thật (không thì toàn ghi IP Cloudflare). Nhưng lớp
   chống bypass CHÍNH vẫn là ufw (mục 2) + Cloudflare WAF, không phải fail2ban.

8. **`certbot` cần cổng 80/443 mở lúc cấp cert.** Nếu khóa ufw trước khi chạy certbot, cấp
   cert có thể fail. Cấp cert xong rồi mới siết ufw, hoặc dùng DNS-01 challenge.

9. **Danh sách IP Cloudflare thay đổi.** Lệnh ufw ở dưới lấy từ cloudflare.com/ips lúc chạy.
   Cloudflare hiếm khi đổi nhưng nếu đổi mà chưa cập nhật, traffic hợp lệ bị chặn. Nên có
   cron cập nhật định kỳ nếu chạy lâu dài.

10. **SSL/TLS mode trên Cloudflare phải là Full (strict)**, cần cert hợp lệ trên VPS (certbot
    lo việc này). Để "Flexible" là lỗ hổng (Cloudflare->VPS không mã hóa).

---

## App-layer: các ENV var (proxy/dos_guard.go)

Đặt trong `docker-compose.yml` mục `environment`. Để giá trị `0` = tắt knob đó.

| ENV | Default | Ý nghĩa |
|-----|---------|---------|
| `KIRO_MAX_BODY_BYTES`   | `10485760` (10 MiB) | Body request tối đa (http.MaxBytesReader) |
| `KIRO_MAX_CONCURRENT`   | `256`  | Tổng số request đồng thời toàn server |
| `KIRO_IP_RPM`           | `120`  | Request/phút/IP — reject ngay khi vượt (token bucket) |
| `KIRO_PER_KEY_INFLIGHT` | `8`    | Số request 1 key được "ngủ" trong RPM-delay cùng lúc; vượt → 429 |
| `KIRO_TRUST_PROXY`      | `false`| Đọc IP thật từ X-Forwarded-For/X-Real-IP. Xem mục 1 checklist |

Chỉ áp dụng cho các endpoint API tốn kém (`/v1/messages`, `/v1/chat/completions`,
`/v1/responses`, `/v1/stats`, count_tokens...). `/admin`, `/health`, `/`, `/v1/models`
KHÔNG bị guard (admin có mật khẩu riêng; health phải luôn truy cập được).

Sau khi đặt Nginx, gợi ý chỉnh: `KIRO_TRUST_PROXY=true` và nới `KIRO_IP_RPM=600`
(Nginx đã chặn thô ở 10r/s, Go làm lớp dự phòng nên nới rộng hơn).

Đoạn docker-compose mẫu (production sau Nginx):

```yaml
    environment:
      - CONFIG_PATH=/app/data/config.json
      - KIRO_TRUST_PROXY=true
      - KIRO_IP_RPM=600
    ports:
      - "127.0.0.1:8090:8080"   # chi loopback; Nginx toi duoc, Internet thi khong
```

---

## Lớp 1 — Cloudflare (free)

Mục tiêu: ẩn IP thật của VPS + chặn L3/L4.

1. Thêm domain vào Cloudflare; đổi nameserver tại nhà cung cấp domain sang NS Cloudflare.
2. DNS: tạo record `A` trỏ IP VPS, **bật Proxied (đám mây cam)**. KHÔNG để "DNS only".
3. SSL/TLS → **Full (strict)**.
4. Security → bật **Bot Fight Mode**.
5. Rate Limiting Rule (free 1 rule): path `/v1/*`, ngưỡng `100 req / 1 phút / per IP`,
   action **Block** hoặc **Managed Challenge**.
6. Khóa firewall VPS chỉ cho IP Cloudflare (chống bypass — xem lệnh ufw bên dưới).

```bash
# Mo SSH truoc (doi 22 neu ban dung cong khac)
sudo ufw allow 22/tcp

# Chi cho dai IP Cloudflare vao 443
for ip in $(curl -s https://www.cloudflare.com/ips-v4); do sudo ufw allow from $ip to any port 443 proto tcp; done
for ip in $(curl -s https://www.cloudflare.com/ips-v6); do sudo ufw allow from $ip to any port 443 proto tcp; done

sudo ufw default deny incoming
sudo ufw enable
```

Sau bước này, đánh thẳng IP VPS bị firewall chặn — chỉ traffic qua Cloudflare lọt vào.

---

## Lớp 2 — Nginx

```bash
sudo apt install nginx certbot python3-certbot-nginx
# Cap cert TRUOC khi siet ufw chat (certbot can 80/443 mo)
sudo certbot --nginx -d api.your-domain.com
```

Copy `deploy/nginx-kiro.conf` vào `/etc/nginx/conf.d/kiro.conf`, đổi `api.your-domain.com`,
rồi:

```bash
sudo nginx -t && sudo systemctl reload nginx
```

Nhớ: bật `KIRO_TRUST_PROXY=true` + đổi cổng Go về loopback (mục 1 & 4 checklist).

---

## Lớp 3 — fail2ban

```bash
sudo apt install fail2ban
sudo cp deploy/fail2ban-filter-nginx-kiro.conf /etc/fail2ban/filter.d/nginx-kiro.conf
sudo cp deploy/fail2ban-jail-nginx-kiro.conf   /etc/fail2ban/jail.d/nginx-kiro.conf
sudo systemctl restart fail2ban
sudo fail2ban-client status nginx-kiro   # kiem tra
```

---

## Thứ tự triển khai khuyến nghị

1. Cloudflare proxied + khóa ufw chỉ cho IP Cloudflare (chặn được nhiều nhất, làm trước).
2. Nginx + bật `KIRO_TRUST_PROXY=true` + đổi cổng Go về loopback (đi cùng nhau).
3. fail2ban (tinh chỉnh sau khi chạy ổn).

## Cách kiểm tra nhanh sau khi xong

- `curl https://IP-VPS-truc-tiep` (không qua domain) → phải **timeout/từ chối** (ufw chặn).
- `curl https://api.your-domain.com/health` → trả `{"status":"ok"...}`.
- Spam nhanh `/v1/messages` quá ngưỡng → nhận **429** (Nginx hoặc Cloudflare).
- Xem log Go: IP trong log phải là IP thật của client, không phải `127.0.0.1`
  (chứng tỏ `KIRO_TRUST_PROXY` + header proxy hoạt động đúng).
