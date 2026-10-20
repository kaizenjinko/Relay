# Relay — kế hoạch hoàn thiện

Mục tiêu: một binary chứa engine reverse-proxy đa scenario; script Lua tuỳ biến từng endpoint; tối ưu alloc/memory/speed/concurrency.

## Hiện trạng

```
main.go                         CLI, compile scripts, serve
internal/config/                TOML loader + validation (5 test)
internal/script/                Lua sandbox, JSON bridge, 6 API (10 test)
internal/server/server.go       route match, header pipeline, upstream call
internal/server/pipeline.go     headerSet, sseWriter (no test)
scripts/opencode_free.lua       gate thật, đã chạy live 200
scripts/cnb.lua                 hai chiều, đã chạy live 200
scripts/recnb.lua               port byte-identical RECNB (51/51 + 400 fuzz)
relay.toml                      2 route
```

Đã chứng minh live: OpenCode free (script add `read` + `stream:true`), CNB (cookie từ env, UA/Referer, prefix/strip tool).

## Thiếu

| Hạng mục | Chi tiết |
|---|---|
| Test server | 0 test. Cần httptest upstream + assert route/header/SSE/error path |
| Client pool | Tạo `HostClient` mỗi request → mất keep-alive. Phải compile 1 lần per route |
| Alloc | Body copy nhiều lần: `ctx.PostBody()` → `string(body)` → `[]byte(res.Body)`. Cần view-based |
| Concurrency | `Script` build VM mỗi Run (đúng cho isolation) nhưng chưa đo. Cần pool hoặc bench chứng minh |
| Rotation | Credential tĩnh qua env. Cần load-balance + health + retry |
| Benchmark | Chưa có baseline. Không có số thì mọi "tối ưu" là đoán |
| Docs | README, script API reference, migration từ cnb-proxy |

## Phase

### Phase 0 — Kế hoạch (xong)
Commit PLAN.md làm mốc.

### Phase 1 — Test server (agent A)
File mới: `internal/server/server_test.go`, `internal/server/testutil_test.go`.

Yêu cầu:
1. `newTestUpstream(t, handler)` — httptest server, trả URL.
2. Test route matching: prefix exact, prefix với trailing, không match → 404, longest-prefix ưu tiên.
3. Test header pipeline: `set` override, `drop` (exact + wildcard), `dynamic` hex/uuid/static/env/file, hop-by-hop bị strip.
4. Test body transform: script trả string → body đổi; trả nil → body giữ nguyên (so byte).
5. Test path rewrite: `/cnb/ai/chat` + upstream `https://x/base` → `/base/ai/chat`; query string giữ.
6. Test SSE: response script chạy per event, `[DONE]` nguyên vẹn, content không bị đụng.
7. Test non-stream: response script chạy 1 lần trên body.
8. Test error: upstream 500 → passthrough status; script error → 500 với message; upstream timeout → 502.
9. Test `stream_sse` explicit override.

Ràng buộc: không sửa file engine, chỉ thêm test. Nếu test lộ bug → ghi vào `FINDINGS.md`, không tự sửa.

### Phase 2 — Tối ưu (agent B)
Phụ thuộc Phase 1 (cần test để biết không phá behavior).

Yêu cầu:
1. **Client pool**: `compiledRoute.clients []*fasthttp.HostClient`, round-robin, key theo upstream host. Keep-alive mặc định 60s.
2. **Giảm alloc body**: tránh `string(body)` khi script không cần. Đo bằng `-benchmem` trước/sau.
3. **Buffer pool**: `sync.Pool` cho `bytes.Buffer` trong SSE writer.
4. **Benchmark**: `BenchmarkServe_OpenCode` (no-op script), `BenchmarkSSE_10Chunks`, `BenchmarkLuaRun_NoChange` (nil return fast path), `BenchmarkLuaRun_Transform`.
5. **Concurrency test**: 200 goroutine đồng thời, `-race`, không leak, không sai output.
6. Ghi baseline + kết quả vào `PERF.md`: allocs/op, ns/op, B/op trước và sau.

Ràng buộc: không được làm giảm functionality. Mọi thay đổi phải pass Phase 1 test.

### Phase 3 — Script library (agent C)
Phụ thuộc Phase 1 (test harness format).

Yêu cầu:
1. `scripts/lib/json_splice.lua` — helper dùng `recnb.lua`, wrap thành API thân thiện: `splice.tool_prefix(body, p)`, `splice.strip_tool_prefix(body, p)`.
2. `scripts/openai.lua` — pass-through + hook mở rộng (set default temperature, normalize `finish_reason`).
3. `scripts/anthropic.lua` — map `max_tokens` ↔ `max_completion_tokens`, strip `tools[].function.strict`.
4. `scripts/deepseek.lua` — rotate-friendly: không đụng gì, chỉ để minh hoạ.
5. `scripts/openrouter.lua` — thêm header `HTTP-Referer` + `X-Title` nếu thiếu.
6. `scripts/_template.lua` — khung có comment, hướng dẫn cho người viết mới.
7. Parity test: `scripts/test/parity_test.go` — chạy `cnb.lua` và `recnb.lua` trên corpus RECNB, assert output khớp Go reference (dùng binary trong `/tmp/kilo/luademo/parity/goref/ref`).
8. Perf test script: mỗi script < 10µs trên body 33KB.

Ràng buộc: script không được dùng `io`/`os`; tất cả phải chạy trong sandbox `internal/script`.

### Phase 4 — Rotation (agent D)
Độc lập, có thể chạy song song Phase 2/3.

Yêu cầu:
1. `internal/rotation/` package: `Rotator`, `Credential`, `Source` interface.
2. Source kinds: `static` (1), `list` (keys array), `file` (1 key/dòng), `env` (comma-separated), `cookie_jar` (JSON accounts).
3. Strategy: `round_robin`, `least_used`, `random`. Sticky để Phase sau.
4. Health: cooldown, exponential backoff, `fail_threshold` → dead, `reset_after`.
5. Retry loop: `max_attempts`, `on_status`, `Retry-After` respect.
6. Config: `[[route.rotation.source]]` + `[route.rotation]` + `[route.rotation.failure.rule]`.
7. Test: 1 credential, 3 credential round-robin, 429 cooldown, 401 → mark dead, all dead → 503, concurrent pick isolation, Retry-After parsing.
8. Integration: route dùng rotator thay `headers.dynamic`.

Ràng buộc: `rotation` optional; route không khai báo thì behavior y hệt hiện tại.

### Phase 5 — Integration review
Tôi review toàn bộ, chạy full test + race + bench, verify live OpenCode + CNB, viết README.

## Convention chung cho mọi agent

- Go 1.23, không thêm dependency ngoài `BurntSushi/toml`, `valyala/fasthttp`, `yuin/gopher-lua`.
- Test dùng `testing` chuẩn, không testify.
- Comment mô tả logic, không mô tả "cái gì đã làm".
- Không sửa `scripts/opencode_free.lua` và `scripts/cnb.lua` trừ khi được yêu cầu.
- Chạy `go test ./... && go vet ./... && go build ./...` trước khi báo xong.
- Nếu phát hiện bug ở engine: ghi `FINDINGS.md`, không sửa ngoài phạm vi phase.

## Tiêu chí hoàn thành

- [ ] `go test ./... -race` pass, coverage server > 80%
- [ ] Benchmark có baseline và kết quả sau tối ưu
- [ ] 6 script + template + parity test
- [ ] Rotation chạy được với ≥ 2 credential
- [ ] Live test OpenCode và CNB pass
- [ ] README + script API reference
