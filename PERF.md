# PERF

Máy: AMD Ryzen 7 7840HS, Go 1.23. Lệnh:

```
go test ./internal/server/ -bench . -benchmem -run '^$' -benchtime 1s
```

## Baseline (trước tối ưu)

Đo trên engine ban đầu: `HostClient` tạo mỗi request, VM Lua tạo mỗi lần chạy,
`LoadString` parse source mỗi request, SSE cấp phát slice mới mỗi dòng.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| Serve_NoOpScript | 278,888 | 411,637 | 1,099 |
| Serve_SSE_20Chunks | 2,254,516 | 4,252,412 | 10,901 |
| HeadersGenerate | 3,511 | 2,032 | 37 |
| LuaRun_NoChange | 79,748 | 190,093 | 519 |
| LuaRun_Transform | 128,954 | 227,142 | 792 |
| Serve_Parallel | 123,100 | 434,982 | 1,113 |

## After

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| Serve_NoOpScript | 81,115 | 50,888 | 224 |
| Serve_SSE_20Chunks | 475,662 | 534,031 | 1,686 |
| HeadersGenerate | 3,532 | 2,032 | 37 |
| LuaRun_NoChange | 11,265 | 20,401 | 90 |
| LuaRun_Transform | 48,267 | 54,465 | 273 |
| Serve_Parallel | 18,300 | 111,787 | 304 |

## So sánh

| Benchmark | speed | B/op | allocs |
|---|---:|---:|---:|
| Serve_NoOpScript | **3.4x** | -87% | -80% |
| Serve_SSE_20Chunks | **4.7x** | -87% | -85% |
| LuaRun_NoChange | **7.1x** | -89% | -83% |
| LuaRun_Transform | **2.7x** | -76% | -66% |
| Serve_Parallel | **6.7x** | -74% | -73% |
| HeadersGenerate | 1.0x | 0% | 0% |

`Serve_Parallel` đo trước tối ưu 123µs, sau 18.3µs. Con số này gồm cả ảnh
hưởng của pool kết nối (song song hưởng lợi nhiều nhất vì mỗi request không
còn phải bắt tay TCP/TLS mới).

## Thay đổi nào tạo ra cái gì

**1. Client pool (`internal/server/clients.go`).** `HostClient` được tạo mỗi
request trước đây, kéo theo việc vứt bỏ toàn bộ idle connection: mỗi request là
một TCP + TLS handshake mới. Giờ mỗi upstream host có một client dùng chung
(`MaxIdleConnDuration=60s`, `MaxConns` từ config). Test
`TestClientPool_ReusesConnections` chứng minh 3 request chỉ mở **1** connection.

Ảnh hưởng: phần lớn cải thiện của `Serve_*` và gần như toàn bộ của
`Serve_Parallel`.

**2. VM pool (`internal/script/pool.go`).** Tạo Lua state tốn 147KB / 423 alloc
(chủ yếu dựng closure stdlib). VM được tái sử dụng: `SetTop(0)` +
`installGlobals` + `SetContext` mới. VM bị **vứt bỏ thay vì tái sử dụng** khi
lần chạy trước không kết thúc sạch (timeout hoặc error), vì context đã hết hạn
hoặc stack ở trạng thái không rõ — tạo VM mới rẻ hơn là lý luận về mọi chế độ
lỗi.

Ảnh hưởng: `LuaRun_NoChange` 79.7µs → 21.6µs.

**3. Prototype cache.** `LoadString` parse source mỗi request. Một
`*lua.FunctionProto` là bất biến và dùng được trong bất kỳ VM nào
(`NewFunctionFromProto`), nên source được parse **một lần lúc khởi động**.

Ảnh hưởng: `LuaRun_NoChange` 21.6µs → 11.3µs, allocs 129 → 90.

**4. Scratch buffer cho SSE.** `rewriteLine` cấp phát slice mới cho mỗi dòng bị
sửa. Giờ dùng một buffer tái sử dụng cho cả stream.

Ảnh hưởng: SSE 722µs → 475µs, 1.16MB → 534KB.

**5. Globals cài một lần cho mỗi VM.** `installGlobals` (json/time/sha256/base64)
chạy mỗi lần thực thi và dựng lại toàn bộ table API. Một chunk trả về bình
thường không thể thay closure mà nó đã capture, nên chỉ cần cài lúc tạo VM.
Ảnh hưởng: `LuaRun_NoChange` 11.3µs → 7.3µs, allocs 90 → 57.

**6. Tái dùng `req`/`resp`/`params` table.** `L.NewTable()` của gopher-lua cấp
phát sẵn phần array + hash: ~590ns, 3 allocs, 2.9KB cho một table rỗng. Ba
table này bị dựng lại mỗi request. Nay chúng được gắn vào VM trong pool và
`clear` tại chỗ (xóa array qua `Len`, xóa hash qua `ForEach`) trước khi nạp lại.
Ảnh hưởng: `Overhead_NilReturn` 4.9µs → 3.3µs, allocs 41 → 24;
`Overhead_Parallel` 2.2µs → 1.4µs.

Hai hướng đã thử và **loại bỏ vì chậm hơn**:

- Pool `reqHeaders` bằng `sync.Pool` + `clear`: `clear` cộng tranh chấp pool
  đắt hơn alloc một map 20 slot (120k → 106k req/s).
- `req.headers` dạng lazy table (`__index` copy-toi-proxy): metatable tốn hơn
  dựng map trực tiếp (2.5µs → 3.1µs).

### Throughput end-to-end

AMD Ryzen 7 7840HS (16 CPU), upstream giả local qua loopback, keep-alive,
script `return nil`, body JSON ~1KB:

| | req/s | avg |
|---|---|---|
| upstream trực tiếp (c=128) | 288.000 | 443µs |
| relay, trước tối ưu (c=256) | 120.000 | 2123µs |
| relay, sau tối ưu (c=128) | 114.000 | 1119µs |
| relay, sau tối ưu (c=256) | 125.000 | 2033µs |
| relay, sau tối ưu (c=1024) | 127.000 | 7956µs |
| relay SSE, 20 chunk (c=64) | 54.000 | 1190µs |

Trần thực tế dừng ở ~127k req/s = **44% của direct**. Phần chênh gồm hai lần
parse HTTP (client + upstream), một lần ghi, và proxy hoàn toàn bằng Go
không có zero-copy splice. CPU profile lúc này: syscall 15%, GC 15%, Lua 7.5%
— nên tối ưu thêm Lua gần như không còn tác dụng.

## Điểm chưa tối ưu (đo được, chưa sửa)

| Chỗ | Chi phí | Lý do để yên |
|---|---|---|
| `string(ctx.PostBody())` trong `serve()` | 1 alloc + 1 copy mỗi request | API script nhận `string`; đổi sang `[]byte` sẽ lan ra toàn bộ script contract |
| `reqHeaders map[string]string` | 1 map + N string mỗi request | Script contract cần `req.headers`; pool `sync.Pool` đã thử và chậm hơn |
| `headerMap(&resp.Header)` | 1 map mỗi response buffered | Script cần `resp.headers`; không biết tĩnh script nào đọc |
| splice zero-copy client↔upstream | bỏ 1 copy body mỗi chiều | Mất khả năng đọc/sửa body trong script; `return nil` đã không copy thêm |
| `GOMAXPROCS`/`GOMEMLIMIT` tuning | vài % | Đo được nhưng phụ thuộc máy; nên đặt qua env khi triển khai |
| `json.encode` trong script | 1 alloc/field | Chỉ script dùng `json.decode` mới có; `splice`/`recnb` byte-level tránh được |

`HeadersGenerate` không đổi vì bản thân nó đã là 37 allocs cố định (map + rand),
không phải điểm nóng so với phần còn lại.

## Cách tái lập

```bash
cd ~/Projects/tmp/relay
go test ./internal/server/ -bench . -benchmem -run '^$' -benchtime 1s
go test ./internal/server/ -run TestClientPool_ReusesConnections -v
go test ./... -race -count=1
```
