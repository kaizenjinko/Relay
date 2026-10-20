-- _template_response.lua
-- Annotated skeleton for a response-only transform. Copy, rename, edit.
--
-- ── the contract ────────────────────────────────────────────────────────────
--
-- The relay runs the script once per response, or once per SSE event when the
-- route sets [route.response] stream_sse = true (or the upstream answers with
-- Content-Type: text/event-stream).
--
-- Globals:
--   resp   = { status, body, headers }   frozen context
--   params = the route's [route.params] table
--   req    = an empty table              (nothing of the request survives)
--
-- In SSE mode `body` is the event payload only: the relay strips the leading
-- "data:" and trailing newline before the call and re-attaches them after. It
-- does not call the script for empty payloads or for "[DONE]".
--
-- Return one of:
--
--   nil                              no change; original bytes pass through.
--   "..."  (string)                  new body.
--   { body = "...", headers = {...} } body and/or headers.
--
-- In SSE mode only res.Body is used; res.Headers is ignored, because headers
-- are already on the wire by the time the first event is written.
--
-- A table without a body (`{ headers = {...} }`) sets Changed with an empty
-- body, which the server treats as "keep original" (FINDINGS.md F-3). Do not
-- rely on that: it is the same hazard described below for "".
--
-- ── the nil-return contract ─────────────────────────────────────────────────
--
-- Return nil, not an empty string, when nothing changed. An empty string is a
-- real answer ("make this body empty") and in buffered mode the server treats
-- it as "no replacement" (FINDINGS.md F-3) — so returning "" for "unchanged"
-- is both slower and ambiguous. Every branch that does not modify the body
-- must end in `return nil`.
--
-- Beware of json.encode(nil): it returns "null", a four-byte body. Guard the
-- decode, then return nil explicitly.
--
-- ── the ["function"] trap ───────────────────────────────────────────────────
--
-- `function` is reserved in Lua. A JSON key "function" needs brackets:
--   local fn = tc["function"]      -- correct
--   -- tc.function                -- syntax error
--
-- ── splice vs json.decode ───────────────────────────────────────────────────
--
-- json.decode + json.encode reorders keys, reformats numbers and drops
-- duplicate keys. Fine for reading finish_reason and rewriting a value; not
-- fine when the response must stay byte-identical outside the edited span.
-- For tool-name rewriting use splice.tool_strip from lib/splice.lua (copy the
-- helpers into this file; there is no require), which edits names in place.
--
-- ── sandbox ─────────────────────────────────────────────────────────────────
--
-- Available: json, sha256, sha1, hmac_sha256, base64, unix_now, time.utc_*,
--            base/table/string/math.
-- Unavailable: io, os, require, dofile, load, print, coroutine, debug,
--              package, collectgarbage.
--
-- A response script that errors does not fail the request: the server logs it
-- and passes the original bytes through. Keep the error path returning nil
-- anyway so the quiet path stays quiet.

local function rewrite()
  local ok, d = pcall(json.decode, resp.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local changed = false

  -- Example: clamp a vendor finish_reason the client does not know.
  for _, choice in ipairs(d.choices or {}) do
    -- pairs, not ipairs: one holder is always nil and gopher-lua's ipairs
    -- stops at the first nil (FINDINGS.md F-1).
    for _, holder in pairs({ delta = choice.delta, message = choice.message }) do
      if type(holder) == "table" and holder.finish_reason == "eos" then
        holder.finish_reason = "stop"
        changed = true
      end
    end
  end

  if not changed then
    return nil
  end
  return json.encode(d)
end

return rewrite()
