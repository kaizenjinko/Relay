-- signed.lua — ký request bằng HMAC-SHA256
--
-- Cần một key. Không có os.getenv trong sandbox, nên key phải đến từ
-- pipeline: [route.headers.dynamic.X-Sign-Key] với kind = "env".
-- Pipeline chạy trước script, nên script đọc được nó từ req.headers.

local function rewrite_request()
  local key = req.headers["X-Sign-Key"]
  if not key or key == "" then
    return nil
  end

  local ts = unix_now()
  local sig = hmac_sha256(key, tostring(ts) .. ":" .. req.body)

  local out = {}
  for k, v in pairs(req.headers) do out[k] = v end
  out["X-Signature"] = sig
  out["X-Timestamp"] = tostring(ts)

  -- body luôn kèm theo, nếu không server coi response là rỗng.
  return { body = req.body, headers = out }
end

if type(resp) == "table" and type(resp.status) == "number" then
  return nil
end
return rewrite_request()
