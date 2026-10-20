-- strip_key.lua — xoá một key top-level khỏi request
-- params: drop_key
--
-- Ví dụ đời thực: upstream từ chối thẳng nếu body còn "tool_choice".

local function rewrite_request()
  local ok, d = pcall(json.decode, req.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local key = params.drop_key
  if not key or d[key] == nil then
    return nil
  end

  d[key] = nil
  return json.encode(d)
end

if type(resp) == "table" and type(resp.status) == "number" then
  return nil
end
return rewrite_request()
