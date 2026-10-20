# FINDINGS

Issues found while building the Phase 3 script library. Engine bugs are
recorded here, not fixed: the constraint for this phase is scripts + one test
file.

## F-1 — gopher-lua `ipairs` stops at the first nil (PUC Lua does not)

`github.com/yuin/gopher-lua@v1.1.2/baselib.go`:

```go
func ipairsaux(L *LState) int {
	tb := L.CheckTable(1)
	i := L.CheckInt(2)
	i++
	v := tb.RawGetInt(i)
	if v == LNil {
		return 0        // <- ends the loop on a hole
	}
	...
}
```

`baseIpairs` seeds the iterator with `raw len(t)`, and `ipairsaux` terminates on
the first nil read. In PUC Lua 5.1, `ipairs` returns `nil` for the hole and
continues to later indices.

**Impact.** The natural idiom for the two OpenAI response shapes is

```lua
for _, holder in ipairs({ choice.delta, choice.message }) do ... end
```

Exactly one of those two is present, so the array always has a hole at index 1
or 2. Under gopher-lua that loop visits **one iteration and silently skips the
other** (or zero iterations if `delta` is the missing one, because the seed
length is the trailing-run length). A response script written this way maps
`finish_reason` on chunks but not on complete responses — an invisible,
data-dependent bug.

**Workaround used in the scripts.** Iterate a keyed table instead:

```lua
for _, holder in pairs({ delta = choice.delta, message = choice.message }) do
```

`pairs` visits both keys in arbitrary order, and the body is order-independent.

**Regression guard.** `scripts_test.go` → `TestSandboxIpairsStopsAtNil` pins the
behaviour, so if a future gopher-lua fixes it the test fails loudly and the
comments in the scripts can be revisited.

## F-2 — no test for the `_template.lua` return contract edge cases

`script.call()` sets `Changed = true` for a table whose `body` key is present
even when the value is `false`/`0` (`b.String()` → `"false"`/`"0"`). Returning
`{ body = false }` therefore sends a body of `"false"`. Scripts must return a
string or nil; this is documented in both templates but is not enforced by the
engine.

## F-3 — a header-only script result blanks the request body

`internal/server/server.go:199-202`:

```go
outBody := body
if res.Changed {
	outBody = []byte(res.Body)
}
```

`script.call()` sets `Changed = true` whenever the returned table has a
`headers` key, even with no `body`. `res.Body` is then `""`, so a script that
returns `{ headers = {...} }` replaces the request body with a zero-length
buffer. The buffered **response** path is guarded correctly
(`server.go:306`: `res.Changed && res.Body != ""`); the **request** path is not,
and the SSE path (`pipeline.go:80`) is also guarded.

The natural fix is the same guard on the request path:

```go
if res.Changed && res.Body != "" {
	outBody = []byte(res.Body)
}
```

**Not fixed here** (engine file). `openrouter.lua` therefore always returns
`{ body = req.body, headers = {...} }`, which is correct under the current rule
and under the guarded rule. Any future header-only script must do the same
until the request path is guarded.

## F-4 — `json.decode` turns an empty JSON array into `{}` (Phase 1)

Test: `internal/server/server_test.go` → `TestRequestScript_StringReturnRewritesBody`

- **Expected:** `json.decode('{"messages":[]}')` yields a table that
  `json.encode` renders as `[]`.
- **Actual:** it renders as `{}`. Reproduced directly against the package:

  ```
  script.Compile(...); RunRequest(&Request{Body: `{"messages":[]}`})
  script: local d = json.decode(req.body); return json.encode({arr = d.messages, n = #d.messages, t = type(d.messages)})
  result: {"arr":{},"n":0,"t":"table"}
  ```

- **Suspect:** `internal/script/script.go:157-173` (`luaToGo`). An empty Lua
  table has `count == 0`, so the `count > 0 && count == t.Len()` test fails and
  the table falls through to the map branch, producing `map[string]any{}`.
  There is no way to distinguish an empty array from an empty object after
  `goToLua`, so the round-trip is lossy the other way too. Any script that
  decodes and re-encodes a body containing `[]` (e.g. an empty `tools`,
  `messages` or `content` array) silently changes the JSON shape; upstreams
  that schema-check will reject it.
- Phase 1 tests avoid relying on this (the body-transform test uses a non-empty
  array) so the suite stays green, and the bug is not masked.

## Trạng thái xử lý (sau Phase 2–4)

| Mã | Trạng thái | Ghi chú |
|---|---|---|
| F-1 | **Giữ nguyên, đã document** | Hành vi của `gopher-lua`, không phải bug của relay. `ipairs` dừng ở nil đầu tiên. Test `TestSandboxIpairsStopsAtNil` pin lại. Tất cả script dùng `pairs` cho `delta`/`message`. |
| F-2 | **Đã sửa** | `{ body = false }` giờ là lỗi `body must be a string or nil`. Test `TestNonStringBodyRejected`. |
| F-3 | **Đã sửa** | Request path guard `res.Changed && res.Body != ""`. Test `TestRequestScript_HeadersOnlyReturn` giờ assert body giữ nguyên `"payload"`. |
| F-4 | **Đã sửa** | `json.decode("[]")` đánh dấu bảng bằng key `__json_array`; `luaToGo` đọc key này để encode thành `[]`, và bỏ marker khỏi output. Test `TestJSONEmptyArraySurvivesRoundTrip`, `TestJSONArrayOfObjectsStaysArray`. |

## Bug mới phát hiện trong Phase 2 (do chính phần tối ưu gây ra, đã sửa)

**F-5 — client pool bỏ mất `IsTLS`.** Bản đầu của `clientPool.new` không set
`IsTLS`, nên mọi upstream `https://` bị nói chuyện bằng HTTP thô. Upstream trả
redirect cross-protocol và `HostClient` từ chối đi theo. Phát hiện khi test
live OpenCode. Sửa: `For(hostPort, isTLS)` truyền `IsTLS` vào `HostClient`, key
pool phân biệt `tls|host:port`.

**F-6 — `MaxConnWaitTimeout` gây lỗi redirect.** Đặt giá trị khác 0 làm
`HostClient` chuyển sang đường lấy connection khác, đường này từ chối redirect
khác protocol. Bỏ hẳn; chờ đợi được giới hạn bởi `DoTimeout` mỗi request.

**F-7 — `Host` header rỗng khi forward.** `SetHost` gọi *trước*
`SetRequestURI` bị URI parser ghi đè. Origin nhận `Host: ""` và trả redirect.
Sửa: `Header.SetHost` sau `SetRequestURI`.

**F-8 — script một chiều crash ở chiều kia.** Sau khi VM pool thay `req` rỗng
bằng `nil` trên response path, `opencode_free.lua` và `openrouter.lua` crash
(`attempt to index nil`). Contract đã đúng; script thiếu guard. Sửa cả hai
bằng `if type(req) ~= "table" then return nil end`, và ghi bẫy này vào README.

## F-9: `Rotator.Do` retried after a successful response

A response that matched no failure rule was **not** the end of the loop. The
loop fell through to the next iteration, so with two credentials every healthy
request was sent twice, and the second response overwrote the first in `last`.

Cause: `last, lastErr = res, nil` was assigned before the failure class was
known, and the success branch called `markSuccess` without breaking.

Impact: 2x upstream traffic per request on any multi-credential route, plus the
wrong response body when the second attempt differed. Latent until a live test
with a rule whose `status` matched a success code (the CNB SSE shape) exposed
it.

Fix: return/break on the first unmatched response; only matched rules continue.

Covered by `TestSSEBodySuccessNotRetried`, `TestSSEBodyErrorTriggersFailover`,
`TestDeadAccountSkippedOnNextRequest`.

## F-10: OpenCode upstream moved under `/v1`

`https://opencode.ai/inference/openai` now 404s; the working base is
`https://opencode.ai/inference/openai/v1`. Also, models are addressed without
the `opencode/` prefix: `deepseek-v4.1-flash-free` works, `opencode/...` returns
`Model is unavailable`. `relay.toml` updated accordingly.

## F-11: OpenCode thinking models require `reasoning_content` echo-back

A multi-turn request that replays an assistant turn with `tool_calls` but no
`reasoning_content` is now rejected with:

```
400 invalid_request_error: The `reasoning_content` in the thinking mode must
be passed back to the API.
```

This is an upstream contract change, not a relay bug: sending the same body
straight to `opencode.ai` fails identically. The relay passes the field through
untouched, so a client that stores and replays `reasoning_content` works
unmodified. Clients that drop it (many OpenAI-compatible SDKs strip unknown
delta fields) will break on tool-call round-trips.

Verified: adding `reasoning_content` to the assistant turn turns the same
request into 200 with the expected answer.

## F-12: upstream redirects are forwarded, not followed

A 307/308 from an upstream reaches the client intact (`Location` preserved).
This is deliberate: behind a gateway the redirect is a routing decision the
gateway owns, and following it server-side would hide that. Covered by
`TestUpstreamRedirectIsForwardedNotFollowed`.

The relay also adds no `Server` header, does not rewrite `X-Forwarded-For` or
`X-Forwarded-Proto`, and returns JSON and SSE bodies byte-identical when no
script touches them — all verified in `internal/server/standard_test.go`.
