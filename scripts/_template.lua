-- _template.lua
-- Annotated skeleton for a request+response transform. Copy, rename, edit.
--
-- ── the contract ────────────────────────────────────────────────────────────
--
-- The relay runs one script per route, twice per request:
--
--   RunRequest   globals: req = { body, method, path, headers }, params
--   RunResponse  globals: resp = { status, body, headers }, params, req = {}
--
-- The frozen context arrives as a global (req or resp). params is the route's
-- [route.params] table from relay.toml, converted from TOML literals.
--
-- The script returns exactly one of:
--
--   nil                              no change; the relay forwards the
--                                    ORIGINAL bytes untouched. Prefer this
--                                    whenever nothing was modified.
--   "..."  (string)                  new body
--   { body = "...", headers = {...} } body and/or headers. A table with only
--                                    `headers` sets Changed with an empty body;
--                                    the server then replaces the body with
--                                    "" (FINDINGS.md F-3). Always pass
--                                    body = req.body / resp.body alongside
--                                    headers so it is safe either way.
--
-- Anything else (number, boolean, function) is an error and the route 500s.
--
-- ── the dispatch trick ──────────────────────────────────────────────────────
--
-- One script can serve both directions. Only one of req/resp is populated per
-- run, and resp.status is only a number when the run is a response run, so:
--
--   if type(resp) == "table" and type(resp.status) == "number" then
--     return rewrite_response()
--   end
--   return rewrite_request()
--
-- That is what cnb.lua uses. Do not test `req.body` to detect the direction:
-- a request without a body would look like a response run.
--
-- ── splice vs json.decode ───────────────────────────────────────────────────
--
-- json.decode + json.encode round-trips through a table, which:
--   * reorders object keys (alphabetically),
--   * reformats numbers (1e3 -> 1000, 1.0 -> 1),
--   * drops duplicate keys (last one wins),
--   * loses insignificant whitespace.
-- Use it when you are transforming fields by name and the upstream does not
-- care about byte layout. It is the right tool for defaults and renames.
--
-- Use a byte-level splice when the output must stay byte-identical outside
-- the edited span: tool-name prefixing, stripping a key, or any path where a
-- parity test compares bytes. The helpers live in lib/splice.lua; copy them
-- in (there is no require) and call splice.tool_prefix / splice.tool_strip /
-- splice.drop_top_key. They return nil when they changed nothing.
--
-- ── the ipairs trap ────────────────────────────────────────────────────────
--
-- gopher-lua's ipairs stops at the first nil, unlike PUC Lua. An array used
-- only to carry optional siblings therefore skips entries:
--
--   ipairs({ choice.delta, choice.message })   -- visits one, never both
--   pairs({ delta = choice.delta, message = choice.message })  -- correct
--
-- See FINDINGS.md F-1.
--
-- ── the ["function"] trap ───────────────────────────────────────────────────
--
-- `function` is a Lua reserved word. A JSON key named "function" must be read
-- with brackets:
--
--   local fn = tool["function"]      -- correct
--   -- local fn = tool.function     -- syntax error
--
-- Same for any other key that collides with a keyword: "end", "then", "for",
-- "repeat", "local" and friends.
--
-- ── sandbox ─────────────────────────────────────────────────────────────────
--
-- Available: json.decode, json.encode, sha256, sha1, hmac_sha256, base64,
--            unix_now, time.utc_iso, time.utc_rfc3339, and the base, table,
--            string, math standard libraries.
-- Not available: io, os, require, dofile, load, loadstring, print, module,
--               coroutine, debug, package, collectgarbage.
--
-- Consequences:
--   * no file or environment access: read config through params,
--   * no timestamps except unix_now / time.utc_*,
--   * no logging: return nil or an error (error() is available),
--   * no shared state between runs: every run is a fresh VM, so a module-level
--     table is rebuilt per request and cannot be used as a cache.
--
-- Every run has a 100ms budget; exceeding it fails the request.

-- ── params ──────────────────────────────────────────────────────────────────

local PREFIX = params.prefix or ""

-- ── request ─────────────────────────────────────────────────────────────────

local function rewrite_request()
  local ok, d = pcall(json.decode, req.body)
  if not ok or type(d) ~= "table" then
    return nil  -- not JSON: pass it through rather than break the route
  end

  local changed = false

  -- Example: add a default only when the client omitted it.
  if params.default_value ~= nil and d.some_field == nil then
    d.some_field = params.default_value
    changed = true
  end

  if not changed then
    return nil
  end
  return json.encode(d)
end

-- ── response ────────────────────────────────────────────────────────────────

local function rewrite_response()
  local ok, d = pcall(json.decode, resp.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local changed = false

  -- Stream chunks carry choices[].delta, complete responses choices[].message.
  for _, choice in ipairs(d.choices or {}) do
    -- pairs, not ipairs: one holder is always nil and gopher-lua's ipairs
    -- stops at the first nil (FINDINGS.md F-1).
    for _, holder in pairs({ delta = choice.delta, message = choice.message }) do
      if type(holder) == "table" then
        -- ... inspect holder here, set changed = true when you modify it
      end
    end
  end

  if not changed then
    return nil
  end
  return json.encode(d)
end

-- ── dispatch ────────────────────────────────────────────────────────────────

if type(resp) == "table" and type(resp.status) == "number" then
  return rewrite_response()
end
return rewrite_request()
