-- tool_prefix.lua — namespace tên tool hai chiều
-- params: prefix (mặc định "up_")
--
-- Request : thêm prefix vào tools[].function.name
-- Response: bỏ prefix khỏi mọi tên tool (cả function_call cũ và tool_calls[])
--
-- Idempotent: tên đã có prefix thì không thêm lần nữa, nên client gửi tên
-- đã prefix vẫn đúng.

local PREFIX = params.prefix or "up_"
local N = #PREFIX

local function rewrite_request()
  local ok, d = pcall(json.decode, req.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local changed = false
  for _, tool in ipairs(d.tools or {}) do
    -- "function" là từ khoá Lua: phải dùng ngoặc vuông
    local fn = tool["function"]
    if type(fn) == "table" and type(fn.name) == "string" then
      if fn.name:sub(1, N) ~= PREFIX then
        fn.name = PREFIX .. fn.name
        changed = true
      end
    end
  end

  if not changed then return nil end
  return json.encode(d)
end

local function strip(name)
  if type(name) == "string" and name:sub(1, N) == PREFIX then
    return name:sub(N + 1)
  end
  return nil
end

local function rewrite_response()
  local ok, d = pcall(json.decode, resp.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local changed = false
  for _, choice in ipairs(d.choices or {}) do
    for _, holder in pairs({ delta = choice.delta, message = choice.message }) do
      if type(holder) == "table" then
        local fc = holder.function_call
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
