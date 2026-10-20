# relay

Một binary, nhiều reverse proxy. Mỗi route trong `relay.toml` trỏ tới một
upstream và một script Lua; script quyết định request/response được biến đổi
thế nào.

```bash
go build -o relay .
./relay -config relay.toml -print-routes
./relay -config relay.toml
```

## Thêm một provider

1. Tạo `scripts/<name>.lua`.
2. Thêm `[[route]]` vào `relay.toml`.

Không cần sửa Go, không cần build lại.

```toml
[[route]]
name     = "deepseek"
prefix   = "/deepseek"
upstream = "https://api.deepseek.com/v1"
script   = "openai.lua"
timeout  = "180s"

  [route.headers.set]
  Authorization = "Bearer sk-..."

  [route.params]
  default_temperature = 0.7
```

Client gọi `http://relay:8899/deepseek/chat/completions` → upstream nhận
`https://api.deepseek.com/v1/chat/completions`.

## Thứ tự áp dụng

```
client
  │  route match (longest prefix)
  ▼
  request headers   (client → drop → set → dynamic)
  │
  request script    (req.body / req.headers → body | nil | {body,headers})
  │
  ▼
upstream
  │
  ▼
  response headers
  │
  response script   (buffered: một lần; SSE: mỗi data: event)
  │
  ▼
client
```

Credential từ `[route.rotation]` được áp **sau** `headers.set`, nên credential
thắng header tĩnh trùng tên.

## Script API

Request script nhận `req` và `params`:

| Biến | Kiểu | Nội dung |
|---|---|---|
| `req.body` | string | body thô |
| `req.method` | string | `POST`… |
| `req.path` | string | path sau khi bỏ prefix |
| `req.headers` | table | header gửi tới upstream |
| `params` | table | `[route.params]` |

Response script nhận `resp` và `params`; `req` là `nil`:

| Biến | Kiểu |
|---|---|
| `resp.status` | number |
| `resp.body` | string |
| `resp.headers` | table |

Giá trị trả về:

| Trả về | Ý nghĩa |
|---|---|
| `nil` | không đổi, giữ nguyên bytes gốc (fast path) |
| `"..."` | body mới |
| `{ body = "..." }` | body mới |
| `{ headers = {...} }` | chỉ đổi header, body giữ nguyên |
| `{ body = "...", headers = {...} }` | cả hai |

Hàm có sẵn: `json.decode/encode`, `sha256`, `sha1`, `hmac_sha256`, `base64`,
`unix_now()`, `time.utc_iso(ts)`, `time.utc_rfc3339(ts)`.

Không có: `io`, `os`, `require`, `print`, `load`, `dofile`. Script chạy trong
VM riêng, timeout 100ms.

## Bốn cái bẫy

**1. `function` là từ khoá Lua.** Mọi tool schema OpenAI phải viết
`tool["function"]`, không phải `tool.function`.

**2. `ipairs` trong gopher-lua dừng ở phần tử nil đầu tiên.** Khác Lua chuẩn.
Với hai hình dạng `delta`/`message` (một cái luôn vắng), dùng
`pairs({delta=..., message=...})`.

**3. `json.encode` sắp xếp key.** Nếu upstream cần giữ thứ tự hoặc format,
dùng `recnb.lua` / `lib/splice.lua` (byte-splice) thay vì decode/encode.

**4. Script một chiều phải tự guard.** Một script có thể bị gọi ở cả hai
chiều. Bắt đầu response-side bằng `if type(req) ~= "table" then return nil end`
hoặc ngược lại.

## Rotation

Khi một provider cần nhiều key hoặc nhiều account:

```toml
[[route.rotation.source]]
kind     = "env"
name     = "DEEPSEEK_KEYS"          # "k1,k2,k3"
template = { Authorization = "Bearer {key}" }

[route.rotation]
strategy      = "round_robin"        # round_robin | least_used | random
max_attempts  = 3
cooldown      = "60s"
max_cooldown  = "30m"
backoff_mult  = 2.0
fail_threshold = 5
reset_after   = "15m"

[[route.rotation.failure.rule]]
name   = "auth"
status = [401, 403]
retry  = true

[[route.rotation.failure.rule]]
name   = "quota"
status = [429]
retry  = true
use_retry_after = true
```

Nguồn khác: `static`, `file` (mỗi dòng một key), `cookie_jar` (JSON nhiều
account với `headers`/`meta` riêng).

Rule khớp theo `status`, `header_match` (regex), `body_regex`. Rule đầu tiên
khớp thắng; `abort = true` dừng ngay không thử credential khác.

`sticky_by` được parse nhưng **chưa implement** — pick quyết định hoàn toàn bởi
`strategy`.

## Script có sẵn

| File | Dùng cho |
|---|---|
| `opencode_free.lua` | gate free tier: thêm tool `read` + `stream:true` |
| `cnb.lua` | CNB: bỏ `tool_choice`, prefix/strip tool `cnb_`, hai chiều |
| `openai.lua` | OpenAI-compatible: default temperature/max_tokens, map `finish_reason` |
| `anthropic_via_openai.lua` | client OpenAI → upstream Anthropic-shaped |
| `openrouter.lua` | thêm `HTTP-Referer` / `X-Title` |
| `recnb.lua` | port byte-identical của cnb-proxy (51/51 + 400 fuzz) |
| `lib/splice.lua` | helper byte-splice dùng chung |
| `_template.lua` | khung script request |
| `_template_response.lua` | khung script response |

## Kiểm thử

```bash
go test ./... -race -count=1     # 137 test, race clean
go test ./... -cover
go test ./internal/server/ -bench . -benchmem -run '^$'
```

Số hiện tại: `main` 60.5%, `internal/server` 87.9%, `script` 79.0%,
`rotation` 76.3%, `config` 44.8%.

Hiệu năng (xem `PERF.md`): so với engine ban đầu, `Serve_NoOpScript` nhanh
3.4x, SSE nhanh 4.7x, Lua no-change nhanh 7.1x.

## Chạy production

### systemd

```bash
sudo install -m 0755 relay /usr/local/bin/relay
sudo install -d -m 0755 -o relay -g relay /etc/relay /etc/relay/scripts
sudo install -m 0644 deploy/relay.toml.example /etc/relay/relay.toml
sudo install -m 0600 -o relay -g relay deploy/relay.env.example /etc/relay/relay.env
sudo cp scripts/*.lua /etc/relay/scripts/
sudo install -m 0644 deploy/relay.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now relay
```

`TimeoutStopSec=45s` trong unit phải lớn hơn `shutdown_timeout` trong
`relay.toml`, nếu không systemd sẽ giết tiến trình giữa lúc đang drain.

### Docker

```bash
docker build -t relay .
docker run -d --name relay -p 8787:8787 \
  -v "$PWD/deploy/relay.toml.example:/etc/relay/relay.toml:ro" \
  -v "$PWD/scripts:/etc/relay/scripts:ro" \
  --env-file /etc/relay/relay.env \
  relay -config /etc/relay/relay.toml -listen 0.0.0.0:8787
```

Image là distroless (16.9MB, không shell, chạy uid 65532). `HEALTHCHECK` gọi
`relay -healthcheck`, không cần curl trong image.

### Graceful shutdown

`SIGTERM`/`SIGINT` thực hiện theo thứ tự: ngừng nhận request mới (trả 503,
heartbeat vẫn đáp để load balancer thấy quá trình), chờ request đang chạy xong,
đóng. Một SSE stream đang mở **được chạy tới hết**; chỉ bị cắt nếu vượt
`shutdown_timeout`.

### Metrics

```toml
[server]
metrics_path = "/metrics"
admin_token  = "..."   # bắt buộc nếu listener không phải localhost
```

Expose `relay_requests_total`, `relay_request_duration_seconds_{sum,count}`,
`relay_inflight_requests`, `relay_upstream_requests_total`, `relay_uptime_seconds`.
Nhãn `stream` phản ánh response thực tế, không phải khai báo route.

### Log

`log_format = "json"` xuất một object mỗi dòng, có `ts`, `level`, `msg` và
field theo ngữ cảnh. Panic trong handler được recover, trả 500 và ghi stack
thay vì làm chết process.

## Tài liệu khác

- `PLAN.md` — kế hoạch và phạm vi
- `PERF.md` — baseline, sau tối ưu, giải thích từng thay đổi
- `FINDINGS.md` — bug phát hiện trong quá trình xây dựng và cách xử lý
