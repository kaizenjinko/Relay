-- openrouter.lua
-- OpenRouter attribution headers. OpenRouter uses HTTP-Referer and X-Title for
-- its app leaderboard; neither affects routing, so this is a header-only shim.
--
-- params:
--   referer = "https://example.com"   -> header HTTP-Referer
--   title   = "My App"                -> header X-Title
--
-- Headers are set only when the param is non-empty AND the client did not
-- already send that header. Client-supplied values win.
--
-- SAFETY: the server applies a script's headers whenever Result.Changed is set,
-- and it replaces the body whenever Changed is set and res.Body is non-empty.
-- A result with only `headers` therefore sets Changed with an empty body, and
-- the current server would forward a zero-length body (see FINDINGS.md, F-3).
-- Returning body = req.body explicitly makes the result correct under both the
-- current rule and a rule that keeps the original body when res.Body == "".
--
-- req.headers keys come from fasthttp verbatim, so the lookup is
-- case-insensitive over a small set of spellings rather than exact.

local function has_header(name)
  local want = string.lower(name)
  local headers = req.headers
  if type(headers) ~= "table" then
    return false
  end
  for k, v in pairs(headers) do
    if string.lower(k) == want and v ~= nil and v ~= "" then
      return true
    end
  end
  return false
end

-- Request-only script: on the response path `req` is nil and there is nothing
-- to add.
if type(req) ~= "table" then
  return nil
end

local headers = {}
local changed = false

if type(params.referer) == "string" and params.referer ~= "" and not has_header("HTTP-Referer") then
  headers["HTTP-Referer"] = params.referer
  changed = true
end

if type(params.title) == "string" and params.title ~= "" and not has_header("X-Title") then
  headers["X-Title"] = params.title
  changed = true
end

if not changed then
  return nil
end

return { body = req.body, headers = headers }
