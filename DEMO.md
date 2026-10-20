# DEMO.md — tham chiếu cú pháp để agent viết script mới

Tài liệu này là nguồn sự thật duy nhất về cú pháp `relay`. Mỗi khẳng định dưới
đây đều kiểm chứng được bằng code trong repo hoặc bằng test đang chạy. Khi sửa
engine, cập nhật file này cùng lúc.

Mục tiêu: một agent đọc xong file này có thể viết route TOML và script Lua đúng
ngay lần đầu, không cần đọc source Go.

---

## 1. Cấu trúc một route

```toml
[[route]]
name     = "myprovider"          # nhãn cho log và metrics
prefix   = "/myprovider"         # client gọi /myprovider/*
upstream = "https://api.example.com/v1"
script   = "myprovider.lua"      # trong script_dir
timeout  = "180s"                # mỗi upstream attempt
```

Client gọi `POST /myprovider/chat/completions` → upstream nhận
`POST https://api.example.com/v1/chat/completions`. Prefix bị strip, query
string giữ nguyên.

**Bắt buộc:** `prefix`, `upstream`, `script`. `name` mặc định = prefix bỏ dấu
`/`. `timeout` mặc định `120s`.

**Route match theo prefix dài nhất.** `/api/v1` thắng `/api` cho
`/api/v1/...`. Prefix không trùng nhau (validate lúc khởi động).

**Path tương đối resolve theo thư mục chứa `relay.toml`**, không theo cwd.

---

## 2. Header pipeline

Thứ tự cố định, không thể đổi:

```
client → drop → set → dynamic → credential (rotation)
```

```toml
  [route.headers]
  drop = ["accept-encoding", "cookie"]

  [route.headers.set]
  Referer    = "https://example.com/"
  User-Agent = "myapp/1.0"

  [route.headers.dynamic.x-session]
  kind   = "hex"          # hex | uuid | static | env | file
  prefix = "ses_"
  bytes  = 13
```

Credential từ rotation áp **sau** `set`, nên thắng header tĩnh trùng tên.

### Dynamic kinds

| kind | Field | Kết quả |
|---|---|---|
| `hex` | `prefix`, `bytes` | `prefix + 2*bytes ký tự hex` ngẫu nhiên mỗi request |
| `uuid` | `prefix` | `prefix + uuid v4` |
| `static` | `prefix`, `value` | `prefix + value` |
| `env` | `prefix`, `name` | `prefix + os.Getenv(name)` |
| `file` | `prefix`, `path` | `prefix + nội dung file (trim)` |

Sinh ngẫu nhiên **mỗi request**. Nếu upstream cần session cố định cho một
phiên, dùng `kind = "env"` hoặc `static`, không dùng `hex`.

### Header bị chặn luôn (hop-by-hop)

`connection`, `keep-alive`, `proxy-authenticate`, `proxy-authorization`, `te`,
`trailer`, `transfer-encoding`, `upgrade`, `host`, `content-length`,
`content-encoding`.

`X-Forwarded-*` **không** bị chặn, không bị ghi đè: giá trị từ gateway phía
trước đi thẳng tới upstream.

---

## 3. Contract của script

### Hai lần chạy mỗi request

```
RunRequest   globals: req, params          RESP: resp = nil
RunResponse  globals: resp, params         REQ:  req  = nil
```

`req`:

| Field | Kiểu | Nội dung |
|---|---|---|
| `body` | string | body thô |
| `method` | string | `POST` |
| `path` | string | path sau khi strip prefix |
| `headers` | table | header gửi tới upstream (đã qua pipeline) |

`resp`:

| Field | Kiểu |
|---|---|
| `status` | number |
| `body` | string |
| `headers` | table |

`params` = `[route.params]`, luôn là table (rỗng nếu route không khai báo).
**Không bao giờ nil.**

### Giá trị trả về

| Trả về | Ý nghĩa |
|---|---|
| `nil` | không đổi, bytes gốc đi tiếp (fast path) |
| `"chuỗi"` | body mới |
| `{ body = "..." }` | body mới |
| `{ headers = {...} }` | chỉ đổi header |
| `{ body = "...", headers = {...} }` | cả hai |

Trả về kiểu khác (number, boolean, function) → route trả 500.

**`{ body = false }` là lỗi** cố ý: chỉ string hoặc nil được làm body. Không
thể vô tình gửi chuỗi `"false"` lên upstream.

### Bảng hằng số

```lua
local ARRAY = "__json_array"   -- marker, không đặt tay
```

Không đặt tay `__json_array`. Engine dùng nó để phân biệt `[]` với `{}`.

---

## 4. Phát hiện chiều chạy

```lua
-- ĐÚNG
if type(resp) == "table" and type(resp.status) == "number" then
  return rewrite_response()
end
return rewrite_request()
```

```lua
-- SAI: request không có body sẽ bị nhận nhầm là response
if req.body then ... end
```

`req` và `resp` hơn kém nhau đúng một cái nil mỗi lần chạy.

---

## 5. API có sẵn

| Hàm | Chữ ký | Ví dụ |
|---|---|---|
| `json.decode` | `(string) -> any` | `json.decode('{"a":1}')` |
| `json.encode` | `(any) -> string` | `json.encode({a=1})` |
| `sha256` | `(string) -> hex` | `sha256("abc")` |
| `sha1` | `(string) -> hex` | `sha1("abc")` |
| `hmac_sha256` | `(key, msg) -> hex` | `hmac_sha256("k","m")` |
| `base64` | `(string) -> string` | `base64("hi")` |
| `unix_now` | `() -> number` | `unix_now()` |
| `time.utc_iso` | `(ts) -> string` | `time.utc_iso(ts)` → `19700101T000000Z` |
| `time.utc_rfc3339` | `(ts) -> string` | `time.utc_rfc3339(ts)` |

Cộng thêm các thư viện chuẩn: `base`, `table`, `string`, `math`.

**Không có:** `io`, `os`, `require`, `dofile`, `load`, `loadstring`, `print`,
`module`, `coroutine`, `debug`, `package`, `collectgarbage`.

Không có `require` → helper phải **copy vào cùng file**. `lib/splice.lua`
được thiết kế để copy, không phải require.

Error trong request script → 500. Error trong response script → log rồi trả
bytes gốc, request vẫn thành công.

Timeout: **100ms** mỗi lần chạy.

---

## 6. Bốn bẫy phải thuộc

### 6.1 `ipairs` dừng ở nil đầu tiên (gopher-lua)

```lua
-- SAI: chỉ thăm một trong hai
for _, h in ipairs({ choice.delta, choice.message }) do ... end

-- ĐÚNG
for _, h in pairs({ delta = choice.delta, message = choice.message }) do ... end
```

Mọi `choices[]` có `delta` (stream) hoặc `message` (đầy đủ), không bao giờ cả
hai, nên `ipairs` sẽ bỏ phần tử nil và dừng.

### 6.2 `function` là từ khoá

```lua
local fn = tool["function"]    -- đúng
-- local fn = tool.function    -- lỗi cú pháp
```

Áp dụng cho mọi key trùng từ khoá Lua: `end`, `then`, `for`, `repeat`,
`local`, `in`, `not`, `do`, `if`, `else`.

### 6.3 `json.decode` + `json.encode` thay đổi bytes

Round-trip qua table sẽ:
- sắp xếp key theo alphabet
- format lại số (`1e3` → `1000`, `1.0` → `1`)
- bỏ key trùng
- mất whitespace

Dùng được khi sửa field theo tên và upstream không quan tâm layout. **Không
dùng** khi cần byte-identical ngoài đoạn đã sửa.

### 6.4 Mảng rỗng vs object rỗng

`json.decode("[]")` rồi `json.encode` phải trả `[]`, không phải `{}`. Engine
gắn marker `__json_array` để làm được điều này. Không tự tạo table rỗng và
trông đợi nó thành `[]`:

```lua
json.encode({})                    --> "{}"
json.encode(json.decode("[]"))     --> "[]"
```

---

## 7. Ví dụ chạy được

### 7.1 Thêm default, chỉ khi client không gửi

```lua
-- scripts/demo_defaults.lua
-- params: temperature, max_tokens

local function rewrite_request()
  local ok, d = pcall(json.decode, req.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local changed = false
  if params.temperature ~= nil and d.temperature == nil then
    d.temperature = params.temperature
    changed = true
  end
  if params.max_tokens ~= nil and d.max_tokens == nil then
    d.max_tokens = params.max_tokens
    changed = true
  end

  if not changed then return nil end
  return json.encode(d)
end

return rewrite_request()
```

```toml
[[route]]
name     = "demo-defaults"
prefix   = "/demo-defaults"
upstream = "https://api.example.com/v1"
script   = "demo_defaults.lua"

  [route.params]
  temperature = 0.7
  max_tokens  = 4096
```

Script một chiều **vẫn phải guard chiều còn lại**. Đọc `req.body` khi
`req == nil` là lỗi `attempt to index a non-table object(nil)` — và lỗi đó xảy
ra **trước** khi vào `pcall`, nên `pcall` không cứu được:

```lua
pcall(json.decode, req.body)   -- req.body được đánh giá TRƯỚC pcall
```

Vì vậy mọi script có hook một chiều cần:

```lua
if type(resp) == "table" and type(resp.status) == "number" then
  return nil
end
return rewrite_request()
```

(tương tự với script response: `if type(req) == "table" then return nil end`).

### 7.2 Strip một key khỏi request

```lua
-- scripts/demo_strip_key.lua
-- params: drop_key

local function rewrite_request()
  local ok, d = pcall(json.decode, req.body)
  if not ok or type(d) ~= "table" then return nil end
  local key = params.drop_key
  if not key or d[key] == nil then return nil end
  d[key] = nil
  return json.encode(d)
end

if type(resp) == "table" and type(resp.status) == "number" then
  return nil
end
return rewrite_request()
```

### 7.3 Đổi một field trong response (cả stream và buffered)

```lua
-- scripts/demo_finish_reason.lua
-- params: from, to

local FROM = params.from or "eos"
local TO   = params.to   or "stop"

local function rewrite_response()
  local ok, d = pcall(json.decode, resp.body)
  if not ok or type(d) ~= "table" then return nil end

  local changed = false
  for _, choice in ipairs(d.choices or {}) do
    -- pairs, không ipairs: một trong hai luôn nil (bẫy 6.1)
    for _, holder in pairs({ delta = choice.delta, message = choice.message }) do
      if type(holder) == "table" and holder.finish_reason == FROM then
        holder.finish_reason = TO
        changed = true
      end
    end
  end

  if not changed then return nil end
  return json.encode(d)
end

if type(resp) == "table" and type(resp.status) == "number" then
  return rewrite_response()
end
return nil
```

```toml
  [route.response]
  stream_sse = true
```

Bật `stream_sse` khi upstream trả SSE **kể cả lỗi** (như CNB), để script thấy
từng event thay vì một body gộp.

### 7.4 Ký request bằng HMAC

```lua
-- scripts/demo_signed.lua
-- Key không đọc được từ Lua: sandbox không có os.getenv.
-- Key đến từ pipeline: [route.headers.dynamic.X-Sign-Key] kind="env",
-- pipeline chạy trước script nên script đọc nó từ req.headers.

local function rewrite_request()
  local ts  = unix_now()
  local key = req.headers["X-Sign-Key"]
  if not key then return nil end

  local sig = hmac_sha256(key, ts .. ":" .. req.body)
  return {
    body = req.body,          -- luôn kèm body để không bị rỗng (F-3)
    headers = {
      ["X-Signature"] = sig,
      ["X-Timestamp"] = tostring(ts),
    },
  }
end

return rewrite_request()
```

Key thật đến từ pipeline: `[route.headers.dynamic.X-Sign-Key]` với
`kind = "env"`. Script đọc nó từ `req.headers` vì pipeline chạy trước script.

### 7.5 Tool name namespace (prefix lên, strip xuống)

```lua
-- scripts/demo_tool_prefix.lua
-- params: prefix

local PREFIX = params.prefix or "up_"

local function rewrite_request()
  local ok, d = pcall(json.decode, req.body)
  if not ok or type(d) ~= "table" then return nil end

  local changed = false
  for _, tool in ipairs(d.tools or {}) do
    local fn = tool["function"]                     -- bẫy 6.2
    if type(fn) == "table" and type(fn.name) == "string" then
      if fn.name:sub(1, #PREFIX) ~= PREFIX then
        fn.name = PREFIX .. fn.name
        changed = true
      end
    end
  end

  if not changed then return nil end
  return json.encode(d)
end

local function strip(name)
  if type(name) == "string" and name:sub(1, #PREFIX) == PREFIX then
    return name:sub(#PREFIX + 1)
  end
end

local function rewrite_response()
  local ok, d = pcall(json.decode, resp.body)
  if not ok or type(d) ~= "table" then return nil end

  local changed = false
  for _, choice in ipairs(d.choices or {}) do
    for _, holder in pairs({ delta = choice.delta, message = choice.message }) do
      if type(holder) == "table" then
        local fc = holder.function_call          -- dạng cũ, một object
        if type(fc) == "table" then
          local n = strip(fc.name)
          if n then fc.name = n; changed = true end
        end
        for _, tc in ipairs(holder.tool_calls or {}) do
          local fn = tc["function"]
          if type(fn) == "table" then
            local n = strip(fn.name)
            if n then fn.name = n; changed = true end
          end
        end
      end
    end
  end

  if not changed then return nil end
  return json.encode(d)
end

if type(resp) == "table" and type(resp.status) == "number" then
  return rewrite_response()
end
return rewrite_request()
```

**Idempotent:** `tools[].function.name` đã có prefix thì không prefix lần nữa.
Điều này quan trọng khi client tự gửi tên đã prefix.

---

## 8. Byte-splice thay cho json round-trip

Khi output phải giữ nguyên ngoài đoạn đã sửa, dùng `lib/splice.lua`. Copy toàn
bộ nội dung file đó vào đầu script của bạn (không có `require`).

```lua
-- sau khi copy splice.lua vào
local out = splice.tool_prefix(req.body, "up_")
if out == nil then return nil end
return out
```

Ba hàm:

| Hàm | Việc |
|---|---|
| `splice.tool_prefix(body, prefix)` | thêm prefix vào mọi `tools[].function.name` |
| `splice.tool_strip(body, prefix)` | bỏ prefix khỏi mọi tên tool trong response |
| `splice.drop_top_key(body, key)` | xoá một key top-level khỏi object |

Cả ba trả `nil` khi không có gì đổi, để bạn `return nil` được ngay.

`recnb.lua` là ví dụ đầy đủ: 777 dòng, byte-identical với bản gốc, đã fuzz
51/51 + 400/400.

---

## 9. SSE

Route nhận SSE khi:
- `[route.response] stream_sse = true`, **hoặc**
- upstream trả `Content-Type: text/event-stream`

Chế độ SSE khác buffered ở ba điểm:

1. Script chạy **mỗi event**, không phải một lần cho cả body.
2. `resp.body` là payload của event (đã bỏ `data: ` đầu và newline cuối).
3. `resp.headers` bị bỏ qua: header đã lên dây trước event đầu tiên.

Engine **không** gọi script cho payload rỗng hoặc `[DONE]`.

Framing giữ nguyên từng byte: dòng trống, comment `: keepalive`, `id:`,
`event:`, multi-line `data:`.

Không retry sau khi byte SSE đầu tiên đã gửi đi — an toàn để rotation failover
trước điểm đó, không thể sau.

---

## 10. Rotation

```toml
  [[route.rotation.source]]
  kind     = "list"
  keys     = ["k1", "k2"]
  template = { Authorization = "Bearer {key}" }

  [route.rotation]
  strategy       = "round_robin"   # round_robin | least_used | weighted | random | priority
  max_attempts   = 2
  cooldown       = "60s"
  max_cooldown   = "30m"
  backoff_mult   = 2.0
  fail_threshold = 1
  reset_after    = "15m"

  [[route.rotation.failure.rule]]
  name   = "auth"
  status = [401, 403]
  retry  = true
```

### Nguồn

| kind | Field | Dùng khi |
|---|---|---|
| `static` | `headers`, `meta` | một credential cố định |
| `list` | `keys`, `template` | vài key ngắn, viết trong TOML |
| `file` | `path`, `template` | mỗi dòng một key |
| `env` | `name`, `template` | `"k1,k2,k3"` trong biến môi trường |
| `http` | `url`, `template` | nguồn động |
| `cookie_jar` | `path` | nhiều account, mỗi account nhiều header |

`cookie_jar` JSON:

```json
{
  "accounts": [
    {"id": "a", "headers": {"Cookie": "...", "Csrftoken": "..."}},
    {"id": "b", "headers": {"Cookie": "...", "Csrftoken": "..."}}
  ]
}
```

### Rule khớp lỗi

| Field | Kiểu | Ghi chú |
|---|---|---|
| `status` | `[int]` | danh sách mã |
| `header_match` | `{TênHeader = "regex"}` | regex Go |
| `body_regex` | string | regex Go, chỉ áp trên N bytes đầu |
| `retry` | bool | thử credential khác |
| `abort` | bool | dừng ngay, thắng `retry` |
| `use_retry_after` | bool | đọc header `Retry-After` |

Rule đầu tiên khớp **thắng**. Không rule nào khớp = thành công, dừng luôn.

**Lỗi nằm trong SSE body của response 200** (CNB): phải dùng `body_regex` +
`status = [200]`, `status` một mình sẽ không bắt được.

---

## 11. Những gì engine tự xử lý, script không cần

- Header hop-by-hop (bảng ở mục 2)
- `Content-Length` (tính lại)
- Redirect từ upstream **được chuyển tiếp**, không tự follow
- `Server` header không được thêm
- Keep-alive pool tới upstream (60s idle), key `tls|host:port`
- Buffering body để retry an toàn (trước khi byte đầu tiên ra client)

---

## 12. Kiểm tra script trước khi chạy

```bash
# Cú pháp Lua và route resolve đúng
./relay -config relay.toml -print-routes

# Chạy thật
./relay -config relay.toml -verbose

# Test end-to-end qua relay
curl -sS -N -X POST http://127.0.0.1:8787/myprovider/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"hi"}]}'
```

Script được compile **một lần lúc khởi động**. Lỗi cú pháp làm process không
start, không phải 500 ở request đầu. Đây là chủ ý: fail sớm.

Sửa script cần restart relay.

---

## 13. Checklist viết script mới

1. Xác định chiều: request only, response only, hay hai chiều.
2. Hai chiều → dùng dispatch ở mục 4. Một chiều → guard chiều còn lại.
3. Cần giữ bytes ngoài đoạn sửa? → splice (mục 8). Ngược lại → `json`.
4. `pcall(json.decode, ...)` mọi lần decode, trả `nil` khi không phải JSON.
5. Đọc `choices[]` → `pairs` cho `delta`/`message` (bẫy 6.1).
6. Key `"function"` → `tool["function"]` (bẫy 6.2).
7. Mọi nhánh không sửa gì → `return nil`, không `return ""`.
8. Trả `{headers=...}` → luôn kèm `body = req.body` / `resp.body`.
9. Cần credential → pipeline `dynamic`/`rotation`, không tìm cách đọc env.
10. Chạy `-print-routes` rồi test một request thật.
