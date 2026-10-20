-- lib/splice.lua
-- Byte-level JSON helpers: edit a tool name or drop a top-level key without
-- re-serialising the document. json.encode reorders keys, reformats numbers
-- and collapses duplicate keys; splicing keeps every untouched byte where it
-- was.
--
-- HOW TO USE
--
-- The relay loads exactly ONE script per route and the sandbox has no
-- `require`, so this file cannot be imported. There are two ways to use it:
--
--   1. Point a route at lib/splice.lua directly. It defines the global table
--      `splice` and returns nil, so it is a valid passthrough script that
--      changes nothing.
--   2. Copy the functions below (everything between the "── helpers ──" markers,
--      plus the `splice` table and the three public functions) into the top of
--      your own script and call `splice.*` from there. That is the same trade
--      cnb.lua makes with recnb.lua: the code travels, not the file.
--
-- PUBLIC API (all return a new body string, or nil when nothing changed or the
-- body is not a single JSON object):
--
--   splice.tool_prefix(body, prefix)
--       Add `prefix` to every tools[].function.name that does not have it.
--       Only top-level tools[] is touched; history in messages[] is left alone.
--
--   splice.tool_strip(body, prefix)
--       Remove `prefix` from tool and function call names in
--       choices[].message.tool_calls, choices[].delta.tool_calls (stream
--       chunks), and the legacy choices[].{message,delta}.function_call.
--       Accepts a bare JSON object or an SSE-framed `... data:{...}` payload.
--
--   splice.drop_top_key(body, key)
--       Delete a top-level member, e.g. splice.drop_top_key(body, "tool_choice").
--
-- Attribution: the scanner (scan_string_end, scan_container_end,
-- container_depth, find_direct_member, escape/decode, the span helpers) is
-- lifted from scripts/recnb.lua, itself a byte-for-byte port of RECNB's
-- transform.go. recnb.lua is the reference implementation; this file is the
-- friendlier wrapper for scripts that do not want the whole recnb surface.

local splice = {}

-- ── helpers: byte scanning ───────────────────────────────────────────────────

local byte = string.byte

local function is_json_space(c)
  return c == 32 or c == 9 or c == 13 or c == 10
end

local function is_json_string_char(c)
  return c == 95 or c == 45
    or (c >= 97 and c <= 122)
    or (c >= 65 and c <= 90)
    or (c >= 48 and c <= 57)
end

-- End index (exclusive, 1-based) of the string literal opening at `start`.
local function scan_string_end(s, start)
  local i = start + 1
  local n = #s
  while i <= n do
    local c = byte(s, i)
    if c == 92 then
      i = i + 2
    elseif c == 34 then
      return i + 1
    else
      i = i + 1
    end
  end
  return nil
end

-- End index (exclusive) of the container opening at `start`.
local function scan_container_end(s, start, open, close)
  local depth = 0
  local i = start
  local n = #s
  while i <= n do
    local c = byte(s, i)
    if c == 34 then
      local e = scan_string_end(s, i)
      if not e then return nil end
      i = e
    elseif c == open then
      depth = depth + 1
      i = i + 1
    elseif c == close then
      depth = depth - 1
      if depth == 0 then return i + 1 end
      i = i + 1
    else
      i = i + 1
    end
  end
  return nil
end

-- Number of containers enclosing position `pos` (1-based); 1 means "top level".
local function container_depth_at(s, pos)
  local depth = 0
  local i = 1
  while i < pos do
    local c = byte(s, i)
    if c == 34 then
      local e = scan_string_end(s, i)
      if not e then return depth end
      i = e
    elseif c == 123 or c == 91 then
      depth = depth + 1
      i = i + 1
    elseif c == 125 or c == 93 then
      if depth > 0 then depth = depth - 1 end
      i = i + 1
    else
      i = i + 1
    end
  end
  return depth
end

-- Exclusive end of the value starting at `start`.
local function value_end(s, start)
  local n = #s
  if start > n then return nil end
  local c = byte(s, start)
  if c == 123 then
    return scan_container_end(s, start, 123, 125)
  elseif c == 91 then
    return scan_container_end(s, start, 91, 93)
  elseif c == 34 then
    return scan_string_end(s, start)
  else
    local i = start
    while i <= n do
      local ch = byte(s, i)
      if is_json_space(ch) or ch == 44 or ch == 125 or ch == 93 then break end
      i = i + 1
    end
    return i
  end
end

-- Start/end (end exclusive) of a document that is exactly one root object
-- surrounded by whitespace, or nil.
local function root_object(body)
  local i = 1
  local n = #body
  while i <= n and is_json_space(byte(body, i)) do i = i + 1 end
  if i > n or byte(body, i) ~= 123 then return nil end
  local e = scan_container_end(body, i, 123, 125)
  if not e then return nil end
  while e <= n and is_json_space(byte(body, e)) do e = e + 1 end
  if e ~= n + 1 then return nil end
  return i, e
end

-- True when `quoted_key` at `pos` is a key (not part of a longer key and not
-- inside a string) and is followed by a colon.
local function key_at(s, pos, quoted_key)
  if pos > 1 then
    local prev = byte(s, pos - 1)
    if is_json_string_char(prev) or prev == 34 then return false end
  end
  local j = pos + #quoted_key
  local n = #s
  while j <= n and is_json_space(byte(s, j)) do j = j + 1 end
  return j <= n and byte(s, j) == 58
end

-- First direct member `quoted_key` at top level of `obj` (a JSON object) whose
-- value starts with `want_value`. Returns start, end (exclusive) of that value.
local function find_top_member(obj, quoted_key, want_value)
  local search_from = 1
  local n = #obj
  while search_from <= n do
    local pos = string.find(obj, quoted_key, search_from, true)
    if not pos then break end
    search_from = pos + #quoted_key
    if key_at(obj, pos, quoted_key) and container_depth_at(obj, pos) == 1 then
      local j = pos + #quoted_key
      while j <= n and is_json_space(byte(obj, j)) do j = j + 1 end
      j = j + 1  -- colon
      while j <= n and is_json_space(byte(obj, j)) do j = j + 1 end
      if j <= n and byte(obj, j) == want_value then
        local e
        if want_value == 123 or want_value == 91 then
          local close = want_value == 123 and 125 or 93
          e = scan_container_end(obj, j, want_value, close)
        else
          e = scan_string_end(obj, j)
        end
        if e then return j, e end
      end
    end
  end
  return nil
end

-- ── helpers: string literals ─────────────────────────────────────────────────

local function escape_json_string(s)
  local out = {}
  local n = #s
  for i = 1, n do
    local c = byte(s, i)
    if c == 34 then out[#out + 1] = '\\"'
    elseif c == 92 then out[#out + 1] = '\\\\'
    elseif c == 10 then out[#out + 1] = '\\n'
    elseif c == 13 then out[#out + 1] = '\\r'
    elseif c == 9 then out[#out + 1] = '\\t'
    elseif c == 8 then out[#out + 1] = '\\b'
    elseif c == 12 then out[#out + 1] = '\\f'
    elseif c < 32 then
      out[#out + 1] = string.format('\\u%04x', c)
    else
      out[#out + 1] = string.char(c)
    end
  end
  return table.concat(out)
end

local function decode_json_string(raw)
  if byte(raw, 1) ~= 34 or byte(raw, #raw) ~= 34 then return nil end
  local out = {}
  local i = 2
  local n = #raw - 1
  while i <= n do
    local c = byte(raw, i)
    if c == 92 then
      local e = byte(raw, i + 1)
      if e == 34 then out[#out + 1] = '"'; i = i + 2
      elseif e == 92 then out[#out + 1] = '\\'; i = i + 2
      elseif e == 47 then out[#out + 1] = '/'; i = i + 2
      elseif e == 98 then out[#out + 1] = '\b'; i = i + 2
      elseif e == 102 then out[#out + 1] = '\f'; i = i + 2
      elseif e == 110 then out[#out + 1] = '\n'; i = i + 2
      elseif e == 114 then out[#out + 1] = '\r'; i = i + 2
      elseif e == 116 then out[#out + 1] = '\t'; i = i + 2
      elseif e == 117 then
        local cp = tonumber(string.sub(raw, i + 2, i + 5), 16)
        if not cp then return nil end
        if cp < 128 then
          out[#out + 1] = string.char(cp)
        elseif cp < 2048 then
          out[#out + 1] = string.char(192 + math.floor(cp / 64), 128 + cp % 64)
        else
          out[#out + 1] = string.char(224 + math.floor(cp / 4096),
            128 + math.floor(cp / 64) % 64, 128 + cp % 64)
        end
        i = i + 6
      else
        return nil
      end
    else
      out[#out + 1] = string.char(c)
      i = i + 1
    end
  end
  return table.concat(out)
end

-- ── helpers: edits ───────────────────────────────────────────────────────────

-- apply_edits splices {start, finish, repl} spans (finish exclusive) into `s`.
-- Spans must be sorted and non-overlapping.
local function apply_edits(s, edits)
  if #edits == 0 then return nil end
  local out = {}
  local prev = 1
  for _, e in ipairs(edits) do
    out[#out + 1] = string.sub(s, prev, e.start - 1)
    out[#out + 1] = e.repl
    prev = e.finish
  end
  out[#out + 1] = string.sub(s, prev)
  return table.concat(out)
end

-- add_edit records the span of a direct "name" string inside `obj` (absolute
-- offsets via `base`) when it needs rewriting under `mode` ("prefix"|"strip").
local function add_edit(obj, base, quoted_key, mode, prefix, edits)
  local ns, ne = find_top_member(obj, quoted_key, 34)
  if not ns then return false end
  local name = decode_json_string(string.sub(obj, ns, ne - 1))
  if not name then return false end
  local has = string.sub(name, 1, #prefix) == prefix
  local repl
  if mode == "prefix" then
    if has then return false end
    repl = '"' .. prefix .. escape_json_string(name) .. '"'
  else
    if not has then return false end
    repl = '"' .. escape_json_string(string.sub(name, #prefix + 1)) .. '"'
  end
  edits[#edits + 1] = { start = base + ns, finish = base + ne, repl = repl }
  return true
end

-- For an element of an array starting at `arr_start`, visit each object.
local function each_array_object(buffer, arr_start, arr_end, fn)
  local i = arr_start + 1
  while i < arr_end do
    while i < arr_end and is_json_space(byte(buffer, i)) do i = i + 1 end
    local c = byte(buffer, i)
    if c == 44 then
      i = i + 1
    elseif c == 123 then
      local e = scan_container_end(buffer, i, 123, 125)
      if not e then return end
      fn(i, e)
      i = e
    else
      return
    end
  end
end

-- ── public API ───────────────────────────────────────────────────────────────

function splice.tool_prefix(body, prefix)
  if type(body) ~= "string" or body == "" then return nil end
  if type(prefix) ~= "string" or prefix == "" then return nil end
  if not root_object(body) then return nil end

  local ts, te = find_top_member(body, '"tools"', 91)
  if not ts then return nil end

  local edits = {}
  each_array_object(body, ts, te, function(ts_obj, te_obj)
    local obj = string.sub(body, ts_obj, te_obj - 1)
    local fn_start, fn_end = find_top_member(obj, '"function"', 123)
    if fn_start then
      local fn = string.sub(obj, fn_start, fn_end - 1)
      add_edit(fn, ts_obj - 1 + fn_start - 1, '"name"', "prefix", prefix, edits)
    end
  end)
  return apply_edits(body, edits)
end

function splice.tool_strip(body, prefix)
  if type(body) ~= "string" or body == "" then return nil end
  if type(prefix) ~= "string" or prefix == "" then return nil end

  local doc = body
  if not root_object(doc) then
    -- SSE-framed: "data: {...}" — isolate the first object and keep the rest.
    local first = string.find(doc, "{", 1, true)
    if not first then return nil end
    local e = scan_container_end(doc, first, 123, 125)
    if not e then return nil end
    doc = string.sub(doc, first, e - 1)
  end

  local cs, ce = find_top_member(doc, '"choices"', 91)
  if not cs then return nil end

  local edits = {}
  each_array_object(doc, cs, ce, function(obj_start, obj_end)
    local choice = string.sub(doc, obj_start, obj_end - 1)
    local base = obj_start - 1

    for _, holder_key in ipairs({ '"message"', '"delta"' }) do
      local hs, he = find_top_member(choice, holder_key, 123)
      if hs then
        local holder = string.sub(choice, hs, he - 1)
        local hbase = base + hs - 1

        -- tool_calls: [ {"function":{"name":...}}, ... ]
        local tcs, tce = find_top_member(holder, '"tool_calls"', 91)
        if tcs then
          each_array_object(holder, tcs, tce, function(tc_start, tc_end)
            local tc = string.sub(holder, tc_start, tc_end - 1)
            local fn_start, fn_end = find_top_member(tc, '"function"', 123)
            if fn_start then
              local fn = string.sub(tc, fn_start, fn_end - 1)
              add_edit(fn, hbase + tc_start - 1 + fn_start - 1, '"name"', "strip", prefix, edits)
            end
          end)
        end

        -- legacy function_call: {"name":...}
        local fcs, fce = find_top_member(holder, '"function_call"', 123)
        if fcs then
          local fc = string.sub(holder, fcs, fce - 1)
          add_edit(fc, hbase + fcs - 1, '"name"', "strip", prefix, edits)
        end
      end
    end
  end)

  table.sort(edits, function(x, y) return x.start < y.start end)
  return apply_edits(doc, edits)
end

function splice.drop_top_key(body, key)
  if type(body) ~= "string" or body == "" then return nil end
  if type(key) ~= "string" or key == "" then return nil end
  if not root_object(body) then return nil end

  local quoted_key = '"' .. key .. '"'
  local pos = string.find(body, quoted_key, 1, true)
  local n = #body
  while pos do
    if key_at(body, pos, quoted_key) and container_depth_at(body, pos) == 1 then
      local j = pos + #quoted_key
      while j <= n and is_json_space(byte(body, j)) do j = j + 1 end
      j = j + 1
      while j <= n and is_json_space(byte(body, j)) do j = j + 1 end
      local ve = value_end(body, j)
      if ve then
        local p = pos - 1
        while p >= 1 and is_json_space(byte(body, p)) do p = p - 1 end
        local comma_before = p >= 1 and byte(body, p) == 44
        local q = ve
        while q <= n and is_json_space(byte(body, q)) do q = q + 1 end
        local comma_after = q <= n and byte(body, q) == 44

        local cut_start, cut_end
        if comma_before then
          cut_start, cut_end = p, ve
        elseif comma_after then
          cut_start, cut_end = pos, q + 1
        else
          cut_start, cut_end = pos, ve
        end
        return string.sub(body, 1, cut_start - 1) .. string.sub(body, cut_end)
      end
    end
    pos = string.find(body, quoted_key, pos + #quoted_key, true)
  end
  return nil
end

-- The helpers are reachable as `splice.*` inside this file and, once copied
-- into another script, under the same name there. This file itself changes
-- nothing, so it is safe to use as a passthrough route.
return nil
