-- openai.lua
-- Generic OpenAI-compatible route: /openai -> https://api.openai.com/v1
--
-- Param-driven, no provider-specific constants. Everything is opt-in; with an
-- empty [route.params] table this script returns nil in both directions and
-- the relay forwards the original bytes.
--
-- params:
--   default_temperature = 0.2   request: set temperature only when absent
--   max_tokens_default  = 4096  request: set max_tokens only when absent
--   map_length          = true  response: finish_reason "length" -> "content_filter"
--
-- The response half reads both shapes: choices[].delta (stream chunks) and
-- choices[].message (complete response). Any other finish_reason value is left
-- untouched, because vendor-specific values ("eos", "safety") must survive.

local function rewrite_request()
  local ok, d = pcall(json.decode, req.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local changed = false

  if params.default_temperature ~= nil and d.temperature == nil then
    d.temperature = params.default_temperature
    changed = true
  end

  if params.max_tokens_default ~= nil and d.max_tokens == nil then
    d.max_tokens = params.max_tokens_default
    changed = true
  end

  if not changed then
    return nil
  end
  return json.encode(d)
end

local function rewrite_response()
  if params.map_length ~= true then
    return nil
  end

  local ok, d = pcall(json.decode, resp.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local changed = false
  for _, choice in ipairs(d.choices or {}) do
    -- Not ipairs: gopher-lua's ipairs stops at the first nil, and one of the
    -- two holders is always absent (a chunk has delta, a complete message has
    -- message). See FINDINGS.md F-1.
    for _, holder in pairs({ delta = choice.delta, message = choice.message }) do
      if type(holder) == "table" and holder.finish_reason == "length" then
        holder.finish_reason = "content_filter"
        changed = true
      end
    end
  end

  if not changed then
    return nil
  end
  return json.encode(d)
end

if type(resp) == "table" and type(resp.status) == "number" then
  return rewrite_response()
end
return rewrite_request()
