-- finish_reason.lua — đổi một giá trị finish_reason trong response
-- params: from (mặc định "eos"), to (mặc định "stop")
--
-- Xử lý cả hai hình dạng stream: choices[].delta và choices[].message.

local FROM = params.from or "eos"
local TO   = params.to   or "stop"

local function rewrite_response()
  local ok, d = pcall(json.decode, resp.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local changed = false
  for _, choice in ipairs(d.choices or {}) do
    -- pairs chứ không ipairs: một trong delta/message luôn nil, và
    -- gopher-lua's ipairs dừng ở phần tử nil đầu tiên.
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
