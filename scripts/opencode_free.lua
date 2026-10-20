-- opencode_free.lua
-- Route: /opencode -> https://opencode.ai/inference/openai
--
-- The free tier is gated on the request shape, verified live:
--   * tools[] must contain a function literally named "read"
--   * stream must be true
--   * the client must look like OpenCode (User-Agent, x-opencode-session)
--
-- This script enforces the body half; the header half is declarative in
-- relay.toml. Returns nil when the body already satisfies the gate, so the
-- relay forwards the original bytes untouched.

-- This script is request-only. When it runs on the response path (scripts in
-- this engine may be invoked in both directions) `req` is nil, and there is
-- nothing to do.
if type(req) ~= "table" then
  return nil
end

local body = req.body
if body == nil or #body == 0 then
  return nil
end

local ok, decoded = pcall(json.decode, body)
if not ok or type(decoded) ~= "table" then
  return nil
end

local changed = false

if decoded.stream ~= true then
  decoded.stream = true
  changed = true
end

local tools = decoded.tools
if type(tools) ~= "table" then
  tools = {}
  decoded.tools = tools
  changed = true
end

local has_read = false
for _, tool in ipairs(tools) do
  local fn = tool["function"]
  if type(fn) == "table" and fn.name == "read" then
    has_read = true
    break
  end
end

if not has_read then
  table.insert(tools, { type = "function", ["function"] = { name = "read" } })
  changed = true
end

if not changed then
  return nil
end

return json.encode(decoded)
