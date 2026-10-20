-- defaults.lua — điền default chỉ khi client không gửi
-- params: temperature, max_tokens
--
-- Một chiều (request). Chạy ở chiều response thì req là nil, nên vẫn phải
-- guard chiều: đọc req.body TRƯỚC khi vào pcall sẽ lỗi "index a nil value".
-- pcall chỉ bắt lỗi bên trong json.decode, không bắt lỗi đọc req.body.

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

if type(resp) == "table" and type(resp.status) == "number" then
  return nil
end
return rewrite_request()
