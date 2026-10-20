-- recnb.lua
-- Byte-level re-implementation of RECNB's transform.go + sse.go.
--
-- Every function mirrors its Go counterpart one-to-one, including the
-- structural JSON scanner (depth tracking, string skipping) and the "last
-- duplicate key wins" rule. It operates on raw bytes as Lua strings, so the
-- output is byte-identical to the Go implementation, not merely semantically
-- equivalent.
--
-- Exposed:
--   recnb.rewrite_request(body, prefix, strip_tool_choice) -> body|nil, changed
--   recnb.rewrite_sse_line(line, prefix)                   -> line|nil
--
-- Both return nil when the Go function returns its "no change" value.

local recnb = {}

-- ── byte helpers ────────────────────────────────────────────────────────────

local byte = string.byte

local function is_json_space(c)
  return c == 32 or c == 9 or c == 13 or c == 10
end

local function is_json_string_char(c)
  return c == 95 or c == 45                    -- _ -
    or (c >= 97 and c <= 122)                  -- a-z
    or (c >= 65 and c <= 90)                   -- A-Z
    or (c >= 48 and c <= 57)                   -- 0-9
end

-- Returns end index (exclusive) of the string literal starting at `start`
-- (1-based, pointing at the opening quote), or nil when unterminated.
local function scan_string_end(body, start)
  local i = start + 1
  local n = #body
  while i <= n do
    local c = byte(body, i)
    if c == 92 then          -- backslash: skip the escaped byte
      i = i + 2
    elseif c == 34 then      -- closing quote
      return i + 1
    else
      i = i + 1
    end
  end
  return nil
end

-- Returns end index (exclusive) of the container starting at `start`, or nil.
local function scan_container_end(body, start, open, close)
  local depth = 0
  local i = start
  local n = #body
  while i <= n do
    local c = byte(body, i)
    if c == 34 then
      local e = scan_string_end(body, i)
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

-- Trailing garbage check used by root_object: only whitespace may follow.
local function trim_space(s)
  local a = 1
  local b = #s
  while a <= b and is_json_space(byte(s, a)) do a = a + 1 end
  while b >= a and is_json_space(byte(s, b)) do b = b - 1 end
  return a, b
end

-- ── container depth ─────────────────────────────────────────────────────────

-- Mirrors containerDepth(prefix []byte) int.
local function container_depth(prefix)
  local depth = 0
  local i = 1
  local n = #prefix
  while i <= n do
    local c = byte(prefix, i)
    if c == 34 then
      local e = scan_string_end(prefix, i)
      if not e then return depth end
      i = e
    elseif c == 123 or c == 91 then       -- { [
      depth = depth + 1
      i = i + 1
    elseif c == 125 or c == 93 then       -- } ]
      if depth > 0 then depth = depth - 1 end
      i = i + 1
    else
      i = i + 1
    end
  end
  return depth
end

-- Mirrors isAfterColon(b []byte, pos int) bool — pos is 1-based here.
local function is_after_colon(b, pos)
  local i = pos - 1
  while i >= 1 do
    local c = byte(b, i)
    if not is_json_space(c) then
      return c == 58
    end
    i = i - 1
  end
  return false
end

-- Mirrors valueEnd(body, start) (int, bool) — returns 1-based exclusive end.
local function value_end(body, start)
  local n = #body
  if start > n then return nil end
  local c = byte(body, start)
  if c == 123 then
    return scan_container_end(body, start, 123, 125)
  elseif c == 91 then
    return scan_container_end(body, start, 91, 93)
  elseif c == 34 then
    return scan_string_end(body, start)
  elseif c == 116 or c == 102 or c == 110 then   -- t f n
    local i = start
    local limit = start + 5
    while i <= n and i < limit do
      local ch = byte(body, i)
      if is_json_space(ch) or ch == 44 or ch == 125 or ch == 93 then break end
      i = i + 1
    end
    return i
  else
    local i = start
    while i <= n do
      local ch = byte(body, i)
      if is_json_space(ch) or ch == 44 or ch == 125 or ch == 93 then break end
      i = i + 1
    end
    return i
  end
end

-- ── root object ─────────────────────────────────────────────────────────────

-- Returns start, end (1-based, end exclusive) of a single root JSON object,
-- or nil when the body is not exactly one object plus whitespace.
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

-- Mirrors findDirectMember(obj, quotedKey, wantValue, wantDepth).
-- `obj` is a slice of the body; positions returned are relative to obj (1-based).
local function find_direct_member(obj, quoted_key, want_value, want_depth)
  local search_from = 1
  local found = false
  local start, finish = 0, 0
  local n = #obj
  while search_from <= n do
    local pos = string.find(obj, quoted_key, search_from, true)
    if not pos then break end
    search_from = pos + #quoted_key
    local prev_ok = true
    if pos > 1 then
      local prev = byte(obj, pos - 1)
      if is_json_string_char(prev) or prev == 34 then prev_ok = false end
    end
    if prev_ok then
      local j = pos + #quoted_key
      while j <= n and is_json_space(byte(obj, j)) do j = j + 1 end
      if j <= n and byte(obj, j) == 58 then          -- ':'
        if container_depth(string.sub(obj, 1, pos - 1)) == want_depth then
          j = j + 1
          while j <= n and is_json_space(byte(obj, j)) do j = j + 1 end
          if j <= n and byte(obj, j) == want_value then
            local e
            local ok = true
            if want_value == 123 then
              e = scan_container_end(obj, j, 123, 125)
              if not e then ok = false end
            else
              e = scan_string_end(obj, j)
              if not e then ok = false end
            end
            if ok then
              start, finish, found = j, e, true
              search_from = e
            end
          end
        end
      end
    end
  end
  if not found then return nil end
  return start, finish
end

-- ── tool name rewriting ─────────────────────────────────────────────────────

-- Mirrors escapeJSONString (valid UTF-8 path). Lua strings are byte strings;
-- the Go version copies bytes >= 0x20 through unchanged and escapes the
-- control characters below that. Invalid UTF-8 is not repaired here because
-- the parity harness feeds valid UTF-8 only.
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

-- Mirrors findNameInTool: returns absolute span (relative to tool) or nil.
local function find_name_in_tool(tool, prefix)
  local fn_start, fn_end = find_direct_member(tool, '"function"', 123, 1)
  if not fn_start then return nil end
  local fn_body = string.sub(tool, fn_start, fn_end - 1)
  local n_start, n_end = find_direct_member(fn_body, '"name"', 34, 1)
  if not n_start then return nil end

  local raw = string.sub(fn_body, n_start, n_end - 1)
  -- Decode the JSON string literal (only the escapes escapeJSONString emits).
  local name = decode_json_string(raw)
  if not name then return nil end
  if string.sub(name, 1, #prefix) == prefix then return nil end

  local repl = '"' .. prefix .. escape_json_string(name) .. '"'
  return fn_start + n_start - 1, fn_start + n_end - 1, repl
end

-- Minimal JSON string literal decoder covering the escapes produced by
-- escapeJSONString and by Python's json.dumps with default settings.
function decode_json_string(raw)
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
        local hex = string.sub(raw, i + 2, i + 5)
        local cp = tonumber(hex, 16)
        if not cp then return nil end
        if cp < 128 then
          out[#out + 1] = string.char(cp); i = i + 6
        elseif cp < 2048 then
          out[#out + 1] = string.char(192 + math.floor(cp / 64), 128 + cp % 64); i = i + 6
        else
          out[#out + 1] = string.char(224 + math.floor(cp / 4096),
            128 + math.floor(cp / 64) % 64, 128 + cp % 64); i = i + 6
        end
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

-- Mirrors rewriteToolsEdits: returns a list of {start, finish, repl} spans
-- relative to `body` (1-based, finish exclusive), or nil.
local function rewrite_tools_edits(body, prefix)
  local tools_start = nil
  local tools_is_array = false
  local search_from = 1
  local n = #body
  local key = '"tools"'
  while search_from <= n do
    local pos = string.find(body, key, search_from, true)
    if not pos then break end
    search_from = pos + #key
    local prev_ok = true
    if pos > 1 then
      local prev = byte(body, pos - 1)
      if is_json_string_char(prev) or prev == 34 then prev_ok = false end
    end
    if prev_ok then
      local j = pos + #key
      while j <= n and is_json_space(byte(body, j)) do j = j + 1 end
      if j <= n and byte(body, j) == 58 then
        if container_depth(string.sub(body, 1, pos - 1)) == 1 then
          j = j + 1
          while j <= n and is_json_space(byte(body, j)) do j = j + 1 end
          if j <= n then
            tools_start = j
            tools_is_array = byte(body, j) == 91
          end
        end
      end
    end
  end
  if not tools_start then return nil end
  if not tools_is_array then return nil end

  local edits = {}
  local i = tools_start + 1
  while i <= n do
    while i <= n and is_json_space(byte(body, i)) do i = i + 1 end
    if i > n then break end
    local c = byte(body, i)
    if c == 93 then
      break
    elseif c == 44 then
      i = i + 1
    elseif c == 123 then
      local obj_end = scan_container_end(body, i, 123, 125)
      if not obj_end then return nil end
      local tool = string.sub(body, i, obj_end - 1)
      local s, e, repl = find_name_in_tool(tool, prefix)
      if s then
        edits[#edits + 1] = { start = i + s - 1, finish = i + e - 1, repl = repl }
      end
      i = obj_end
    else
      return nil
    end
  end
  if #edits == 0 then return nil end
  return edits
end

-- Mirrors stripTopLevelKey (single occurrence, then the caller loops).
local function strip_top_level_key(body, quoted_key)
  local search_from = 1
  local n = #body
  while search_from <= n do
    local pos = string.find(body, quoted_key, search_from, true)
    if not pos then return nil end
    search_from = pos + #quoted_key
    local prev_ok = true
    if pos > 1 then
      local prev = byte(body, pos - 1)
      if is_json_string_char(prev) or prev == 34 then prev_ok = false end
    end
    if prev_ok then
      local j = pos + #quoted_key
      while j <= n and is_json_space(byte(body, j)) do j = j + 1 end
      if j <= n and byte(body, j) == 58 then
        if container_depth(string.sub(body, 1, pos - 1)) == 1 then
          j = j + 1
          while j <= n and is_json_space(byte(body, j)) do j = j + 1 end
          local val_end_pos = value_end(body, j)
          if not val_end_pos then return nil end

          local cut_start = pos
          local p = pos - 1
          while p >= 1 and is_json_space(byte(body, p)) do p = p - 1 end
          local comma_before = p >= 1 and byte(body, p) == 44

          local q = val_end_pos
          while q <= n and is_json_space(byte(body, q)) do q = q + 1 end
          local comma_after = q <= n and byte(body, q) == 44

          local cut_end = val_end_pos
          if comma_before then
            cut_start = p
            cut_end = val_end_pos
          elseif comma_after then
            cut_start = pos
            cut_end = q + 1
          else
            cut_start = pos
            cut_end = val_end_pos
          end
          return string.sub(body, 1, cut_start - 1) .. string.sub(body, cut_end)
        end
      end
    end
  end
  return nil
end

-- ── request entry point ─────────────────────────────────────────────────────

-- Mirrors rewriteRequestJSON. Returns body, changed.
-- Returns the rewritten body plus changed, or (nil, false) when the Go
-- function would return false. Callers keep the ORIGINAL bytes on false, which
-- matters when the body has leading whitespace: the Go code slices from the
-- first '{' but the caller only uses that slice when a change occurred.
function recnb.rewrite_request(body, prefix, strip_tool_choice)
  local first = string.find(body, '{', 1, true)
  if first then
    body = string.sub(body, first)
  else
    return nil, false
  end
  if byte(body, 1) ~= 123 then return nil, false end
  if not is_valid_json(body) then return nil, false end

  local changed = false
  if strip_tool_choice then
    while true do
      local stripped = strip_top_level_key(body, '"tool_choice"')
      if not stripped then break end
      body = stripped
      changed = true
    end
  end

  local edits = rewrite_tools_edits(body, prefix)
  if edits then
    local out = {}
    local prev = 1
    for _, e in ipairs(edits) do
      out[#out + 1] = string.sub(body, prev, e.start - 1)
      out[#out + 1] = e.repl
      prev = e.finish
    end
    out[#out + 1] = string.sub(body, prev)
    return table.concat(out), true
  end
  if changed then
    return body, true
  end
  return nil, false
end

-- ── SSE entry point ─────────────────────────────────────────────────────────

-- Mirrors rewriteNameInObject: strips prefix from a "name" member.
local function span_name_in_object(obj, prefix)
  local n_start, n_end = find_direct_member(obj, '"name"', 34, 1)
  if not n_start then return nil end
  local raw = string.sub(obj, n_start, n_end - 1)
  local name = decode_json_string(raw)
  if not name then return nil end
  if string.sub(name, 1, #prefix) ~= prefix then return nil end
  local rest = string.sub(name, #prefix + 1)
  return n_start, n_end, '"' .. escape_json_string(rest) .. '"'
end

-- Mirrors rewriteFunctionNameInObject: finds "function":{"name":...}.
local function span_function_name_in_object(obj, prefix)
  local search_from = 1
  local n = #obj
  local fn_pos = nil
  while search_from <= n do
    local pos = string.find(obj, '"function"', search_from, true)
    if not pos then break end
    search_from = pos + #'"function"'
    if not is_after_colon(obj, pos) then
      fn_pos = pos
      break
    end
  end
  if not fn_pos then return nil end

  local j = fn_pos + #'"function"'
  while j <= n and is_json_space(byte(obj, j)) do j = j + 1 end
  if j > n or byte(obj, j) ~= 58 then return nil end
  j = j + 1
  while j <= n and is_json_space(byte(obj, j)) do j = j + 1 end
  if j > n or byte(obj, j) ~= 123 then return nil end
  local fn_end = scan_container_end(obj, j, 123, 125)
  if not fn_end then return nil end

  local body = string.sub(obj, j, fn_end - 1)
  local s, e, repl = span_name_in_object(body, prefix)
  if not s then return nil end
  return j + s - 1, j + e - 1, repl
end

-- Convention: every span `finish` is an EXCLUSIVE 1-based offset into
-- raw_payload. `string.sub(s, prev, e.start - 1)` yields the untouched chunk
-- before an edit and the loop advances prev to e.finish.

-- Mirrors rewriteToolCallArray. `arr` includes its brackets; `base` is the
-- offset to subtract so that arr[1] maps to raw_payload[base + 1].
local function collect_tool_call_array(arr, prefix, base, edits)
  local i = 2
  local n = #arr
  while i <= n do
    while i <= n and is_json_space(byte(arr, i)) do i = i + 1 end
    if i > n then break end
    local c = byte(arr, i)
    if c == 93 then
      break
    elseif c == 44 then
      i = i + 1
    elseif c ~= 123 then
      while i <= n and byte(arr, i) ~= 44 and byte(arr, i) ~= 93 do i = i + 1 end
    else
      local obj_end = scan_container_end(arr, i, 123, 125)
      if not obj_end then return end
      local obj = string.sub(arr, i, obj_end - 1)
      local s, e, repl = span_function_name_in_object(obj, prefix)
      if s then
        edits[#edits + 1] = {
          start = base + i + s - 1,
          finish = base + i + e - 1,
          repl = repl,
        }
      end
      i = obj_end
    end
  end
end

-- Mirrors the collect() closure in rewriteSSELine over one container.
local function collect_in_container(container, base, prefix, edits)
  local n = #container
  for _, key in ipairs({ '"tool_calls"', '"function_call"' }) do
    local search_from = 1
    while search_from <= n do
      local pos = string.find(container, key, search_from, true)
      if not pos then break end
      search_from = pos + #key
      local prev_ok = true
      if pos > 1 then
        local prev = byte(container, pos - 1)
        if is_json_string_char(prev) or prev == 34 then prev_ok = false end
      end
      if prev_ok then
        local j = pos + #key
        while j <= n and is_json_space(byte(container, j)) do j = j + 1 end
        if j <= n and byte(container, j) == 58 then
          j = j + 1
          while j <= n and is_json_space(byte(container, j)) do j = j + 1 end
          if j <= n then
            local c = byte(container, j)
            if c == 91 then
              local arr_end = scan_container_end(container, j, 91, 93)
              if arr_end then
                local arr = string.sub(container, j, arr_end - 1)
                collect_tool_call_array(arr, prefix, base + j - 1, edits)
                search_from = arr_end
              end
            elseif c == 123 and key == '"function_call"' then
              local obj_end = scan_container_end(container, j, 123, 125)
              if obj_end then
                local obj = string.sub(container, j, obj_end - 1)
                local s, e, repl = span_name_in_object(obj, prefix)
                if s then
                  edits[#edits + 1] = {
                    start = base + j + s - 1,
                    finish = base + j + e - 1,
                    repl = repl,
                  }
                end
                search_from = obj_end
              end
            end
          end
        end
      end
    end
  end
end

-- Mirrors rewriteSSELine. Returns the rewritten line or nil.
function recnb.rewrite_sse_line(line, prefix)
  local idx = string.find(line, "data:", 1, true)
  if not idx then return nil end

  local raw_payload = string.sub(line, idx + 5)
  local a, b = trim_space(raw_payload)
  if b < a or string.sub(raw_payload, a, b) == "[DONE]" then return nil end

  local obj_start = string.find(raw_payload, '{', 1, true)
  if not obj_start then return nil end
  local obj_end = scan_container_end(raw_payload, obj_start, 123, 125)
  if not obj_end then return nil end

  local obj = string.sub(raw_payload, obj_start, obj_end - 1)
  if not is_valid_json(obj) then return nil end

  local after = string.sub(raw_payload, obj_end)
  local aa, bb = trim_space(after)
  if bb >= aa then return nil end

  local edits = {}
  collect_in_container(obj, obj_start - 1, prefix, edits)
  if #edits == 0 then return nil end

  table.sort(edits, function(x, y) return x.start < y.start end)

  local out = {}
  out[#out + 1] = string.sub(line, 1, idx + 4)
  out[#out + 1] = string.sub(raw_payload, 1, obj_start - 1)
  local prev = obj_start
  for _, e in ipairs(edits) do
    out[#out + 1] = string.sub(raw_payload, prev, e.start - 1)
    out[#out + 1] = e.repl
    prev = e.finish
  end
  out[#out + 1] = string.sub(raw_payload, prev)
  return table.concat(out)
end

-- ── JSON validity ───────────────────────────────────────────────────────────

-- Full JSON validator (RFC 8259). Mirrors encoding/json's acceptance for the
-- documents RECNB sees: strict syntax, no trailing garbage, no duplicate-key
-- rejection (Go keeps the last one, and so do the scanners above).
local function skip_ws(s, i, n)
  while i <= n and is_json_space(byte(s, i)) do i = i + 1 end
  return i
end

local parse_value

local function parse_string(s, i, n)
  if i > n or byte(s, i) ~= 34 then return nil end
  i = i + 1
  while i <= n do
    local c = byte(s, i)
    if c == 34 then
      return i + 1
    elseif c == 92 then
      local e = byte(s, i + 1)
      if e == nil then return nil end
      if e == 117 then                     -- \uXXXX
        local hex = string.sub(s, i + 2, i + 5)
        if #hex < 4 or not hex:match('^%x%x%x%x$') then return nil end
        i = i + 6
      elseif e == 34 or e == 92 or e == 47 or e == 98 or e == 102
          or e == 110 or e == 114 or e == 116 then
        i = i + 2
      else
        return nil
      end
    elseif c < 32 then
      return nil
    else
      i = i + 1
    end
  end
  return nil
end

local function parse_number(s, i, n)
  local start = i
  if i <= n and byte(s, i) == 45 then i = i + 1 end
  if i > n then return nil end
  local c = byte(s, i)
  if c == 48 then
    i = i + 1
  elseif c >= 49 and c <= 57 then
    while i <= n and byte(s, i) >= 48 and byte(s, i) <= 57 do i = i + 1 end
  else
    return nil
  end
  if i <= n and byte(s, i) == 46 then
    i = i + 1
    if i > n or byte(s, i) < 48 or byte(s, i) > 57 then return nil end
    while i <= n and byte(s, i) >= 48 and byte(s, i) <= 57 do i = i + 1 end
  end
  if i <= n then
    local e = byte(s, i)
    if e == 101 or e == 69 then
      i = i + 1
      if i <= n and (byte(s, i) == 43 or byte(s, i) == 45) then i = i + 1 end
      if i > n or byte(s, i) < 48 or byte(s, i) > 57 then return nil end
      while i <= n and byte(s, i) >= 48 and byte(s, i) <= 57 do i = i + 1 end
    end
  end
  if i == start then return nil end
  return i
end

local function parse_literal(s, i, n, lit)
  if string.sub(s, i, i + #lit - 1) ~= lit then return nil end
  return i + #lit
end

local function parse_object(s, i, n)
  i = i + 1
  i = skip_ws(s, i, n)
  if i <= n and byte(s, i) == 125 then return i + 1 end
  while true do
    i = skip_ws(s, i, n)
    local after_key = parse_string(s, i, n)
    if not after_key then return nil end
    i = skip_ws(s, after_key, n)
    if i > n or byte(s, i) ~= 58 then return nil end
    i = parse_value(s, i + 1, n)
    if not i then return nil end
    i = skip_ws(s, i, n)
    if i > n then return nil end
    local c = byte(s, i)
    if c == 44 then
      i = i + 1
    elseif c == 125 then
      return i + 1
    else
      return nil
    end
  end
end

local function parse_array(s, i, n)
  i = i + 1
  i = skip_ws(s, i, n)
  if i <= n and byte(s, i) == 93 then return i + 1 end
  while true do
    i = parse_value(s, i, n)
    if not i then return nil end
    i = skip_ws(s, i, n)
    if i > n then return nil end
    local c = byte(s, i)
    if c == 44 then
      i = i + 1
    elseif c == 93 then
      return i + 1
    else
      return nil
    end
  end
end

parse_value = function(s, i, n)
  i = skip_ws(s, i, n)
  if i > n then return nil end
  local c = byte(s, i)
  if c == 123 then return parse_object(s, i, n)
  elseif c == 91 then return parse_array(s, i, n)
  elseif c == 34 then return parse_string(s, i, n)
  elseif c == 116 then return parse_literal(s, i, n, "true")
  elseif c == 102 then return parse_literal(s, i, n, "false")
  elseif c == 110 then return parse_literal(s, i, n, "null")
  else return parse_number(s, i, n)
  end
end

function is_valid_json(s)
  local n = #s
  local i = parse_value(s, 1, n)
  if not i then return false end
  i = skip_ws(s, i, n)
  return i == n + 1
end

return recnb
