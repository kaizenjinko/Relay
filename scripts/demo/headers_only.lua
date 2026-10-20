-- headers_only.lua — thêm header mà không đụng body
-- params: header_name, header_value
--
-- Cạm bẫy: một table chỉ có `headers` khiến server coi body là rỗng.
-- Luôn truyền body = req.body cùng với headers.

local NAME  = params.header_name  or "X-Relay"
local VALUE = params.header_value or "1"

local function rewrite_request()
  local headers = req.headers
  headers[NAME] = VALUE

  -- Copy lại toàn bộ header để không mất gì, body giữ nguyên.
  local out = {}
  for k, v in pairs(headers) do out[k] = v end

  return { body = req.body, headers = out }
end

if type(resp) == "table" and type(resp.status) == "number" then
  return nil
end
return rewrite_request()
