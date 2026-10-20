-- cnb.lua
-- Route: /cnb -> https://cnb.cool
--
-- Two directions, matching the RECNB proxy exactly:
--   request : drop tool_choice, prefix tools[].function.name with params.prefix
--   response: strip params.prefix from tool/function call names
--
-- Both directions work on the decoded table. That re-orders keys, which is
-- acceptable here because the prefix rules operate on names, not on byte
-- layout. For byte-identical rewriting the RECNB scanner port in
-- recnb.lua can be used instead.

local PREFIX = params.prefix or "cnb_"

-- ── request ────────────────────────────────────────────────────────────────

function rewrite_request()
  local ok, d = pcall(json.decode, req.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local changed = false

  -- CNB rejects tool_choice outright.
  if d.tool_choice ~= nil then
    d.tool_choice = nil
    changed = true
  end

  -- Tools the model may call must live in CNB's namespace. tools[] is the
  -- only place touched: messages[].tool_calls holds the names the model
  -- already emitted, and rewriting those would rewrite history.
  for _, tool in ipairs(d.tools or {}) do
    local fn = tool["function"]
    if type(fn) == "table" and type(fn.name) == "string" then
      if fn.name:sub(1, #PREFIX) ~= PREFIX then
        fn.name = PREFIX .. fn.name
        changed = true
      end
    end
  end

  if not changed then
    return nil
  end
  return json.encode(d)
end

-- ── response ───────────────────────────────────────────────────────────────

local function strip(name)
  if type(name) == "string" and name:sub(1, #PREFIX) == PREFIX then
    return name:sub(#PREFIX + 1)
  end
end

local function strip_container(container, state)
  if type(container) ~= "table" then return end

  -- legacy singular shape
  local fc = container.function_call
  if type(fc) == "table" then
    local n = strip(fc.name)
    if n then fc.name = n; state.changed = true end
  end

  -- tool_calls array
  for _, tc in ipairs(container.tool_calls or {}) do
    local fn = tc["function"]
    if type(fn) == "table" then
      local n = strip(fn.name)
      if n then fn.name = n; state.changed = true end
    end
  end
end

function rewrite_response()
  local ok, d = pcall(json.decode, resp.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local state = { changed = false }
  for _, choice in ipairs(d.choices or {}) do
    strip_container(choice.delta, state)    -- streamed chunks
    strip_container(choice.message, state)  -- complete message
  end

  if not state.changed then
    return nil
  end
  return json.encode(d)
end

-- ── dispatch ───────────────────────────────────────────────────────────────
--
-- A single script serves both directions: the relay sets `req` for request
-- scripts and `resp` for response scripts, and exactly one global is present.

if type(resp) == "table" and type(resp.status) == "number" then
  return rewrite_response()
end
return rewrite_request()
