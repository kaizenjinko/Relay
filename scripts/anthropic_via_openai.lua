-- anthropic_via_openai.lua
-- An OpenAI-shaped client in front of an Anthropic-shaped upstream.
-- Route: /anthropic -> https://api.anthropic.com/v1 (messages endpoint).
--
-- WHAT THIS SCRIPT CONVERTS
--
-- Request (OpenAI -> Anthropic body):
--   * "max_tokens" is passed through unchanged (both APIs share the name).
--   * "anthropic_version" is added as a body field when params.anthropic_version
--     is set and the client did not send one. The upstream normally wants this
--     as the anthropic-version HEADER; set the header in relay.toml and leave
--     this param unset unless your upstream reads it from the body.
--   * system messages (role == "system") are removed from messages[] and their
--     content is concatenated into a top-level "system" string, joined with
--     "\n\n". If the body already has a top-level system string it is kept and
--     the message-derived text is appended after it.
--   * tools[].function.strict is removed: Anthropic rejects unknown keys.
--   * tools[].function.parameters is renamed to tools[].input_schema, which is
--     the Anthropic tool shape.
--   * top-level "parallel_tool_calls" is removed (no Anthropic equivalent).
--   * top-level "response_format" is removed (no Anthropic equivalent).
--
-- Response (Anthropic shape -> client): ONLY finish_reason is normalised.
-- Both shapes are handled:
--   * complete response: choices[].message.finish_reason
--   * stream chunk:      choices[].delta.finish_reason
--   "end_turn" -> "stop", "max_tokens" -> "length", "tool_use" -> "tool_calls"
-- Nothing else in the response body is touched: no tool_use content block ->
-- tool_calls conversion, no usage rewrite, no stop_sequence mapping.
--
-- WHAT THIS SCRIPT DOES NOT CONVERT
--
--   * content: OpenAI strings stay strings, OpenAI content-part arrays stay
--     arrays. No image_url -> image block translation.
--   * tool_use / tool_result messages, Anthropic "content": [{type:"tool_use"}]
--     blocks, tool call ids and the "assistant" prefill trick.
--   * streaming event framing (message_start/message_delta/content_block_*
--     versus OpenAI chat.completion.chunk) and the [DONE] sentinel.
--   * usage fields (prompt_tokens/completion_tokens/total_tokens) and the
--     stop_sequences / top_p / metadata parameter renames.
--
-- Those need a real bidirectional translation layer, not a body shim. This
-- script is honest about being a shim: use it only where the client is happy
-- with Anthropic-shaped content and only finish_reason needs to look familiar.
-- Everything it does not understand it leaves byte-for-byte alone.

local function text_of(content)
  if type(content) == "string" then
    return content
  end
  if type(content) == "table" then
    local parts = {}
    for _, part in ipairs(content) do
      if type(part) == "string" then
        parts[#parts + 1] = part
      elseif type(part) == "table" and type(part.text) == "string" then
        parts[#parts + 1] = part.text
      end
    end
    return table.concat(parts)
  end
  return nil
end

local function rewrite_request()
  local ok, d = pcall(json.decode, req.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local changed = false

  if type(d.max_tokens) ~= "number" then
    -- Anthropic requires max_tokens; a client that omits it would be rejected
    -- with a schema error that points nowhere useful. Only fill it when the
    -- route supplies a default.
    if params.max_tokens_default ~= nil then
      d.max_tokens = params.max_tokens_default
      changed = true
    end
  end

  if params.anthropic_version ~= nil and d.anthropic_version == nil then
    d.anthropic_version = params.anthropic_version
    changed = true
  end

  if d.parallel_tool_calls ~= nil then
    d.parallel_tool_calls = nil
    changed = true
  end

  if d.response_format ~= nil then
    d.response_format = nil
    changed = true
  end

  for _, tool in ipairs(d.tools or {}) do
    local fn = tool["function"]
    if type(fn) == "table" then
      if fn.strict ~= nil then
        fn.strict = nil
        changed = true
      end
      if fn.parameters ~= nil then
        if fn.input_schema == nil then
          fn.input_schema = fn.parameters
        end
        fn.parameters = nil
        changed = true
      end
    end
  end

  if type(d.messages) == "table" then
    local kept = {}
    local collected = {}
    for _, msg in ipairs(d.messages) do
      if type(msg) == "table" and msg.role == "system" then
        local t = text_of(msg.content)
        if t and t ~= "" then
          collected[#collected + 1] = t
        end
      else
        kept[#kept + 1] = msg
      end
    end
    -- Only touch messages[] when a system message was actually there; an
    -- already-converted body must keep returning nil.
    if #kept ~= #d.messages then
      d.messages = kept
      if #collected > 0 then
        local joined = table.concat(collected, "\n\n")
        if type(d.system) == "string" and d.system ~= "" then
          d.system = d.system .. "\n\n" .. joined
        else
          d.system = joined
        end
      end
      changed = true
    end
  end

  if not changed then
    return nil
  end
  return json.encode(d)
end

-- The client speaks OpenAI names; finish_reason is what it sees. The request
-- half is not a downstream Anthropic message, so there is no reverse direction
-- to encode here.
local TO_OPENAI = {
  end_turn = "stop",
  max_tokens = "length",
  tool_use = "tool_calls",
}

local function rewrite_response()
  local ok, d = pcall(json.decode, resp.body)
  if not ok or type(d) ~= "table" then
    return nil
  end

  local changed = false
  local function map(holder)
    if type(holder) ~= "table" or type(holder.finish_reason) ~= "string" then
      return
    end
    local to = TO_OPENAI[holder.finish_reason]
    if to ~= nil then
      holder.finish_reason = to
      changed = true
    end
  end

  for _, choice in ipairs(d.choices or {}) do
    -- pairs over a keyed table: message and delta are mutually exclusive.
    for _, holder in pairs({ delta = choice.delta, message = choice.message }) do
      map(holder)
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
