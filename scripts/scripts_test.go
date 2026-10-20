package scripts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/baongo/relay/internal/script"
)

// helperOnly scripts define functions and return nil; they are not routes.
var helperOnly = map[string]bool{
	"recnb.lua":      true,
	"lib/splice.lua": true,
}

type luaScript struct {
	name string
	sc   *script.Script
}

// compileScripts loads every scripts/**/*.lua that is a real route script.
func compileScripts(t *testing.T) []luaScript {
	t.Helper()
	root := "."
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".lua") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk scripts: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no scripts found")
	}
	sort.Strings(files)

	out := make([]luaScript, 0, len(files))
	for _, p := range files {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		sc, err := script.Compile(rel, string(src))
		if err != nil {
			t.Errorf("%s does not compile: %v", rel, err)
			continue
		}
		if helperOnly[rel] {
			continue
		}
		out = append(out, luaScript{name: rel, sc: sc})
	}
	return out
}

func runRequest(t *testing.T, sc *script.Script, body string, headers map[string]string, params map[string]any) *script.Result {
	t.Helper()
	if headers == nil {
		headers = map[string]string{}
	}
	res, err := sc.RunRequest(&script.Request{
		Body:    body,
		Method:  "POST",
		Path:    "/v1/chat/completions",
		Headers: headers,
	}, params)
	if err != nil {
		t.Fatalf("%s RunRequest: %v", sc.Name(), err)
	}
	return res
}

func runResponse(t *testing.T, sc *script.Script, body string, params map[string]any) *script.Result {
	t.Helper()
	res, err := sc.RunResponse(&script.Response{
		Status:  200,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    body,
	}, params)
	if err != nil {
		t.Fatalf("%s RunResponse: %v", sc.Name(), err)
	}
	return res
}

func decodeJSON(t *testing.T, label, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%s: invalid JSON output: %v\n%s", label, err, body)
	}
	return out
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	// simpleRequest has no temperature, no max_tokens, no system message, no
	// tools: every request script should leave it alone with empty params.
	simpleRequest = `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`

	// plainRequest is a request already in the shape every script accepts.
	plainRequest = `{"model":"gpt-4o","temperature":0.7,"max_tokens":256,
		"messages":[{"role":"user","content":"hi"}],"stream":false}`

	// toolRequest exercises tools[].function.strict, parameters,
	// parallel_tool_calls and response_format.
	toolRequest = `{"model":"gpt-4o","temperature":0.1,"max_tokens":1024,
		"parallel_tool_calls":true,
		"response_format":{"type":"json_object"},
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"read","description":"r",
			"strict":true,"parameters":{"type":"object","properties":{}}}}]}`

	// plainResponse is a complete non-streaming OpenAI response.
	plainResponse = `{"id":"cmpl-1","object":"chat.completion","model":"gpt-4o",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},
		"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`

	// chunkResponse is one SSE chunk in OpenAI shape.
	chunkResponse = `{"id":"cmpl-1","object":"chat.completion.chunk",
		"choices":[{"index":0,"delta":{"content":"he"},"finish_reason":null}]}`

	lengthResponse = `{"choices":[{"index":0,"message":{"role":"assistant","content":"cut","finish_reason":"length"}}]}`
)

// buildLargeBody returns a >33KB request with one tool, a long message list and
// padding that must not be commented out of existence by a naive splice.
func buildLargeBody() string {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-4o","temperature":0.3,"max_tokens":2048,
		"messages":[{"role":"system","content":"you are helpful"}`)
	for i := 0; i < 150; i++ {
		b.WriteString(`,{"role":"user","content":"`)
		b.WriteString(strings.Repeat("padding text with \\\"quotes\\\" and {braces} ", 5))
		b.WriteString(`"}`)
	}
	b.WriteString(`],"tools":[{"type":"function","function":{"name":"grep","strict":true,`)
	b.WriteString(`"parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`)
	return b.String()
}

// oversizedParams is used by the generic pass-through check. It must not be
// able to provoke a change on a body the script considers "already fine".
var oversizedParams = map[string]any{}

// ---------------------------------------------------------------------------
// Per-script behaviour, keyed by file name. A script may appear only in the
// "no change" fixture expected to return nil, and optionally in one of the
// specialised tests below.
// ---------------------------------------------------------------------------

// responseOnly scripts have no request hook; the generic request legs skip them.
var responseOnly = map[string]bool{
	"_template_response.lua": true,
}

type scriptCase struct {
	// untouched responses on which the script must return nil.
	requestNoChange  []string
	responseNoChange []string
	paramsNoChange   map[string]any
}

var cases = map[string]scriptCase{
	"opencode_free.lua": {
		// Already has stream:true and a "read" tool.
		requestNoChange: []string{
			`{"stream":true,"tools":[{"type":"function","function":{"name":"read"}}]}`,
		},
		responseNoChange: []string{plainResponse, chunkResponse},
	},
	"cnb.lua": {
		requestNoChange: []string{
			`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"cnb_read"}}]}`,
		},
		// Already-stripped response.
		responseNoChange: []string{
			`{"choices":[{"message":{"tool_calls":[{"function":{"name":"read"}}]},"delta":{}}]}`,
		},
	},
	"openai.lua": {
		requestNoChange:  []string{plainRequest},
		responseNoChange: []string{plainResponse, chunkResponse},
	},
	"anthropic_via_openai.lua": {
		// Fully converted request: no strict, no parallel_tool_calls, no
		// response_format, no system message in messages.
		requestNoChange: []string{
			`{"model":"claude","max_tokens":100,"system":"be nice","messages":[{"role":"user","content":"hi"}],
			  "tools":[{"type":"function","function":{"name":"read","input_schema":{"type":"object"}}}]}`,
		},
		// Already mapped finish_reason.
		responseNoChange: []string{plainResponse, chunkResponse},
	},
	"openrouter.lua": {
		// No params: nothing is added.
		requestNoChange:  []string{simpleRequest},
		responseNoChange: []string{plainResponse},
	},
	"_template.lua": {
		requestNoChange:  []string{simpleRequest, plainRequest},
		responseNoChange: []string{plainResponse, chunkResponse},
	},
	"_template_response.lua": {
		// Response-only: there is no request hook to exercise.
		responseNoChange: []string{plainResponse, chunkResponse},
	},

	// ── scripts/demo: worked examples referenced by DEMO.md ──
	"demo/defaults.lua": {
		// Client already set both fields.
		requestNoChange:  []string{`{"messages":[],"temperature":0.2,"max_tokens":100}`},
		responseNoChange: []string{plainResponse, chunkResponse},
	},
	"demo/strip_key.lua": {
		// No tool_choice to drop.
		requestNoChange:  []string{simpleRequest},
		responseNoChange: []string{plainResponse},
	},
	"demo/finish_reason.lua": {
		requestNoChange: []string{simpleRequest},
		// Neither shape carries the "from" value.
		responseNoChange: []string{plainResponse, chunkResponse},
	},
	"demo/tool_prefix.lua": {
		// Already prefixed with up_; prefixing again would corrupt the name.
		requestNoChange: []string{
			`{"tools":[{"type":"function","function":{"name":"up_bash"}}]}`,
		},
		// No tool names to strip.
		responseNoChange: []string{plainResponse, chunkResponse},
	},
	"demo/headers_only.lua": {
		// Always adds a header, so there is no no-change request body.
		requestNoChange:  []string{},
		responseNoChange: []string{plainResponse},
	},
	"demo/signed.lua": {
		// Without X-Sign-Key the script must leave the body alone.
		requestNoChange:  []string{simpleRequest},
		responseNoChange: []string{plainResponse},
	},
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestScriptsCompile(t *testing.T) {
	scripts := compileScripts(t)
	if len(scripts) == 0 {
		t.Fatal("no script produced")
	}
	for _, s := range scripts {
		if _, ok := cases[s.name]; !ok {
			t.Errorf("%s has no fixture entry; add it to `cases`", s.name)
		}
	}
	for name := range cases {
		found := false
		for _, s := range scripts {
			if s.name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("fixture entry %q has no file", name)
		}
	}
}

func TestScriptsReturnNilWhenNothingToDo(t *testing.T) {
	for _, s := range compileScripts(t) {
		c := cases[s.name]
		for _, body := range c.requestNoChange {
			res := runRequest(t, s.sc, body, nil, c.paramsNoChange)
			if res.Changed {
				t.Errorf("%s: request unexpectedly changed (body=%q result=%q)", s.name, body, res.Body)
			}
		}
		for _, body := range c.responseNoChange {
			res := runResponse(t, s.sc, body, c.paramsNoChange)
			if res.Changed {
				t.Errorf("%s: response unexpectedly changed (body=%q result=%q)", s.name, body, res.Body)
			}
		}
	}
}

func TestScriptOutputIsValidJSON(t *testing.T) {
	type probe struct {
		name    string
		isResp  bool
		body    string
		headers map[string]string
		params  map[string]any
	}
	probes := []probe{
		{"opencode_free.lua", false, simpleRequest, nil, nil},
		{"opencode_free.lua", false, plainRequest, nil, nil},
		{"cnb.lua", false, toolRequest, nil, map[string]any{"prefix": "cnb_"}},
		{"cnb.lua", true, `{"choices":[{"delta":{"tool_calls":[{"function":{"name":"cnb_read"}}]}}]}`, nil, map[string]any{"prefix": "cnb_"}},
		{"openai.lua", false, simpleRequest, nil, map[string]any{"default_temperature": 0.1, "max_tokens_default": 128}},
		{"openai.lua", true, lengthResponse, nil, map[string]any{"map_length": true}},
		{"openai.lua", true, `{"choices":[{"delta":{"finish_reason":"length"}}]}`, nil, map[string]any{"map_length": true}},
		{"anthropic_via_openai.lua", false, toolRequest, nil, map[string]any{"anthropic_version": "2023-06-01"}},
		{"anthropic_via_openai.lua", false,
			`{"model":"c","system":"s1","messages":[{"role":"system","content":[{"type":"text","text":"s2"}]},{"role":"user","content":"u"}]}`,
			nil, nil},
		{"anthropic_via_openai.lua", true,
			`{"choices":[{"message":{"finish_reason":"end_turn"}},{"delta":{"finish_reason":"tool_use"}}]}`, nil, nil},
		{"openrouter.lua", false, simpleRequest, nil, map[string]any{"referer": "https://x.test", "title": "relay"}},
		{"_template.lua", false, plainRequest, nil, map[string]any{"default_value": 1}},
		{"_template_response.lua", true, `{"choices":[{"message":{"finish_reason":"eos"}}]}`, nil, nil},
	}
	byName := map[string]*script.Script{}
	for _, s := range compileScripts(t) {
		byName[s.name] = s.sc
	}

	for _, p := range probes {
		sc, ok := byName[p.name]
		if !ok {
			t.Fatalf("%s missing", p.name)
		}
		if !p.isResp && responseOnly[p.name] {
			continue
		}
		var res *script.Result
		if p.isResp {
			res = runResponse(t, sc, p.body, p.params)
		} else {
			res = runRequest(t, sc, p.body, p.headers, p.params)
		}
		if !res.Changed || res.Body == "" {
			t.Errorf("%s (resp=%v): expected a change on %q", p.name, p.isResp, p.body)
			continue
		}
		decodeJSON(t, p.name+" output", res.Body)
	}
}

func TestOpenAIDefaultsOnlyWhenAbsent(t *testing.T) {
	var sc *script.Script
	for _, s := range compileScripts(t) {
		if s.name == "openai.lua" {
			sc = s.sc
		}
	}
	if sc == nil {
		t.Fatal("openai.lua missing")
	}
	params := map[string]any{"default_temperature": 0.2, "max_tokens_default": 4096}

	if res := runRequest(t, sc, plainRequest, nil, params); res.Changed {
		t.Fatalf("temperature/max_tokens present but body changed: %q", res.Body)
	}

	res := runRequest(t, sc, simpleRequest, nil, params)
	if !res.Changed {
		t.Fatal("expected defaults to be applied")
	}
	d := decodeJSON(t, "openai defaults", res.Body)
	if got, _ := d["temperature"].(float64); got != 0.2 {
		t.Errorf("temperature = %v, want 0.2", d["temperature"])
	}
	if got, _ := d["max_tokens"].(float64); got != 4096 {
		t.Errorf("max_tokens = %v, want 4096", d["max_tokens"])
	}

	// No params: no change at all.
	if res := runRequest(t, sc, simpleRequest, nil, nil); res.Changed {
		t.Errorf("empty params changed body: %q", res.Body)
	}

	// map_length off by default.
	if res := runResponse(t, sc, lengthResponse, nil); res.Changed {
		t.Errorf("map_length unset but response changed: %q", res.Body)
	}

	// map_length on: both delta and message normalised.
	res = runResponse(t, sc, `{"choices":[{"message":{"finish_reason":"length"}},{"delta":{"finish_reason":"length"}}]}`, map[string]any{"map_length": true})
	if !res.Changed {
		t.Fatal("map_length=true produced no change")
	}
	d = decodeJSON(t, "openai map_length", res.Body)
	choices := d["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	delta := choices[1].(map[string]any)["delta"].(map[string]any)
	if msg["finish_reason"] != "content_filter" || delta["finish_reason"] != "content_filter" {
		t.Errorf("finish_reason not mapped: %v / %v", msg["finish_reason"], delta["finish_reason"])
	}

	// stop and tool_calls are untouched.
	res = runResponse(t, sc, `{"choices":[{"message":{"finish_reason":"stop"}},{"delta":{"finish_reason":"tool_calls"}}]}`, map[string]any{"map_length": true})
	if res.Changed {
		t.Errorf("non-length finish_reason changed: %q", res.Body)
	}
}

func TestAnthropicConvertsRequest(t *testing.T) {
	var sc *script.Script
	for _, s := range compileScripts(t) {
		if s.name == "anthropic_via_openai.lua" {
			sc = s.sc
		}
	}
	if sc == nil {
		t.Fatal("anthropic_via_openai.lua missing")
	}

	body := `{"model":"claude","max_tokens":512,"parallel_tool_calls":true,
		"response_format":{"type":"json_object"},
		"messages":[{"role":"system","content":"sys one"},{"role":"user","content":"u"}],
		"tools":[{"type":"function","function":{"name":"read","strict":true,
			"parameters":{"type":"object"}}}]}`
	res := runRequest(t, sc, body, nil, map[string]any{"anthropic_version": "2023-06-01"})
	if !res.Changed {
		t.Fatal("expected a change")
	}
	d := decodeJSON(t, "anthropic request", res.Body)

	if _, ok := d["parallel_tool_calls"]; ok {
		t.Error("parallel_tool_calls survived")
	}
	if _, ok := d["response_format"]; ok {
		t.Error("response_format survived")
	}
	if d["anthropic_version"] != "2023-06-01" {
		t.Errorf("anthropic_version = %v", d["anthropic_version"])
	}
	if got, _ := d["max_tokens"].(float64); got != 512 {
		t.Errorf("max_tokens = %v, want 512", d["max_tokens"])
	}
	if d["system"] != "sys one" {
		t.Errorf("system = %q, want %q", d["system"], "sys one")
	}
	msgs := d["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("messages = %v, want the user message only", msgs)
	}
	tool := d["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if _, ok := tool["strict"]; ok {
		t.Error("tools[].function.strict survived")
	}
	if _, ok := tool["parameters"]; ok {
		t.Error("tools[].function.parameters survived")
	}
	if _, ok := tool["input_schema"]; !ok {
		t.Error("tools[].function.input_schema missing")
	}

	// Without anthropic_version the field must not appear.
	res = runRequest(t, sc, `{"model":"c","max_tokens":1,"messages":[]}`, nil, nil)
	if res.Changed {
		t.Errorf("no params, minimal request changed: %q", res.Body)
	}

	// Response direction maps finish_reason both ways.
	res = runResponse(t, sc, `{"choices":[{"message":{"finish_reason":"end_turn"}},{"delta":{"finish_reason":"max_tokens"}},{"message":{"finish_reason":"tool_use"}}]}`, nil)
	if !res.Changed {
		t.Fatal("response not mapped")
	}
	d = decodeJSON(t, "anthropic response", res.Body)
	got := []any{}
	for _, c := range d["choices"].([]any) {
		m := c.(map[string]any)
		h := m["message"]
		if h == nil {
			h = m["delta"]
		}
		got = append(got, h.(map[string]any)["finish_reason"])
	}
	want := []any{"stop", "length", "tool_calls"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finish_reason[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestOpenRouterAddsHeadersKeepsBody(t *testing.T) {
	var sc *script.Script
	for _, s := range compileScripts(t) {
		if s.name == "openrouter.lua" {
			sc = s.sc
		}
	}
	if sc == nil {
		t.Fatal("openrouter.lua missing")
	}
	params := map[string]any{"referer": "https://app.test", "title": "relay"}

	res := runRequest(t, sc, plainRequest, nil, params)
	if !res.Changed {
		t.Fatal("expected headers")
	}
	if res.Body != plainRequest {
		t.Errorf("body altered:\n got %q\nwant %q", res.Body, plainRequest)
	}
	if got := res.Headers["HTTP-Referer"]; got != "https://app.test" {
		t.Errorf("HTTP-Referer = %q", got)
	}
	if got := res.Headers["X-Title"]; got != "relay" {
		t.Errorf("X-Title = %q", got)
	}
	if len(res.Headers) != 2 {
		t.Errorf("unexpected headers: %v", res.Headers)
	}

	// Client-supplied headers win, case-insensitively.
	res = runRequest(t, sc, plainRequest, map[string]string{"http-referer": "https://client.test"}, params)
	if _, ok := res.Headers["HTTP-Referer"]; ok {
		t.Error("HTTP-Referer overwritten despite client header")
	}
	if res.Headers["X-Title"] != "relay" {
		t.Error("X-Title should still be added")
	}

	// Both present: nothing to do.
	res = runRequest(t, sc, plainRequest, map[string]string{"HTTP-Referer": "a", "X-Title": "b"}, params)
	if res.Changed {
		t.Errorf("client sent both headers but result changed: %v", res.Headers)
	}

	// Empty params: nothing to do.
	if res := runRequest(t, sc, plainRequest, nil, nil); res.Changed {
		t.Error("empty params produced headers")
	}
}

// spliceSource returns lib/splice.lua with its final passthrough `return nil`
// replaced by `epilogue`, so the helpers can be exercised without `require`.
func spliceSource(t *testing.T, epilogue string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("lib", "splice.lua"))
	if err != nil {
		t.Fatal(err)
	}
	return spliceSourceFrom(t, string(src), epilogue)
}

func spliceSourceFrom(t *testing.T, src, epilogue string) string {
	t.Helper()
	// The last "return nil" in the file is the passthrough line.
	i := strings.LastIndex(src, "return nil")
	if i < 0 {
		t.Fatal("splice.lua has no trailing `return nil`")
	}
	return src[:i] + epilogue
}

// probeSplice runs `expr` against `body` with the splice helpers defined and
// returns "NIL" when it returns nil.
func probeSplice(t *testing.T, expr, body string) string {
	t.Helper()
	code := spliceSource(t, "local __b = "+expr+"\nreturn __b or 'NIL'\n")
	sc, err := script.Compile("splice-probe", code)
	if err != nil {
		t.Fatalf("compile probe: %v", err)
	}
	res, err := sc.RunRequest(&script.Request{Body: body}, nil)
	if err != nil {
		t.Fatalf("run probe %q: %v", expr, err)
	}
	return res.Body
}

func TestSpliceHelpers(t *testing.T) {
	// lib/splice.lua is a passthrough route: it must run clean and change
	// nothing.
	src, err := os.ReadFile(filepath.Join("lib", "splice.lua"))
	if err != nil {
		t.Fatal(err)
	}
	passthrough, err := script.Compile("lib/splice.lua", string(src))
	if err != nil {
		t.Fatalf("splice.lua does not compile: %v", err)
	}
	for _, body := range []string{toolRequest, plainResponse, ""} {
		if res := runRequest(t, passthrough, body, nil, nil); res.Changed {
			t.Errorf("splice.lua as a route changed %q -> %q", body, res.Body)
		}
	}

	// The helper table is reachable when the file is loaded as a script body.
	shim := spliceSource(t, "return type(splice) == 'table' and 'TABLE' or 'NIL'\n")
	sc, err := script.Compile("splice-probe", shim)
	if err != nil {
		t.Fatalf("shim does not compile: %v", err)
	}
	res := runRequest(t, sc, toolRequest, nil, nil)
	if res.Body != "TABLE" {
		t.Fatalf("splice global not defined: %q", res.Body)
	}

	// tool_prefix: rewrites only the name, bytes elsewhere identical.
	got := probeSplice(t, `splice.tool_prefix(req.body, "cnb_")`, toolRequest)
	if got == "NIL" {
		t.Fatal("tool_prefix returned nil")
	}
	decodeJSON(t, "tool_prefix", got)
	if want := strings.Replace(toolRequest, `"name":"read"`, `"name":"cnb_read"`, 1); got != want {
		t.Errorf("tool_prefix changed bytes outside the name:\n got %q\nwant %q", got, want)
	}

	// Idempotent.
	if got := probeSplice(t, `splice.tool_prefix(req.body, "cnb_")`, strings.Replace(toolRequest, `"name":"read"`, `"name":"cnb_read"`, 1)); got != "NIL" {
		t.Errorf("tool_prefix not idempotent: %q", got)
	}

	// tool_strip: complete response, stream chunk, legacy function_call.
	strip := `{"choices":[{"message":{"tool_calls":[{"function":{"name":"cnb_read"}}]}}]}`
	if got := probeSplice(t, `splice.tool_strip(req.body, "cnb_")`, strip); got == "NIL" || !strings.Contains(got, `"name":"read"`) {
		t.Errorf("tool_strip complete = %q", got)
	}

	chunk := `data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"cnb_grep"}}]}}]}`
	if got := probeSplice(t, `splice.tool_strip(req.body, "cnb_")`, chunk); got == "NIL" || !strings.Contains(got, `"name":"grep"`) {
		t.Errorf("tool_strip chunk = %q", got)
	}

	legacy := `{"choices":[{"message":{"function_call":{"name":"cnb_ls","arguments":"{}"}}}]}`
	if got := probeSplice(t, `splice.tool_strip(req.body, "cnb_")`, legacy); got == "NIL" || !strings.Contains(got, `"name":"ls"`) {
		t.Errorf("tool_strip legacy = %q", got)
	}

	// tool_strip no-op.
	if got := probeSplice(t, `splice.tool_strip(req.body, "cnb_")`,
		`{"choices":[{"message":{"tool_calls":[{"function":{"name":"read"}}]}}]}`); got != "NIL" {
		t.Errorf("tool_strip without prefix returned %q", got)
	}

	// drop_top_key: first, middle, last member.
	for _, in := range []string{
		`{"tool_choice":"auto","model":"m"}`,
		`{"model":"m","tool_choice":"auto","stream":true}`,
		`{"model":"m","tool_choice":"auto"}`,
		`{"model":"m","tool_choice":{"type":"function","function":{"name":"x"}}}`,
	} {
		got := probeSplice(t, `splice.drop_top_key(req.body, "tool_choice")`, in)
		if got == "NIL" {
			t.Errorf("drop_top_key(%s) returned nil", in)
			continue
		}
		d := decodeJSON(t, "drop_top_key", got)
		if _, ok := d["tool_choice"]; ok {
			t.Errorf("drop_top_key(%s) left key: %q", in, got)
		}
	}

	// Nested occurrences must not be dropped.
	nested := `{"messages":[{"role":"assistant","tool_choice":"auto"}]}`
	if got := probeSplice(t, `splice.drop_top_key(req.body, "tool_choice")`, nested); got != "NIL" {
		t.Errorf("drop_top_key touched a nested key: %q", got)
	}

	// Non-object bodies pass through.
	for _, in := range []string{"", "data: [DONE]", "not json at all", `[1,2,3]`} {
		if got := probeSplice(t, `splice.tool_prefix(req.body, "x_")`, in); got != "NIL" {
			t.Errorf("tool_prefix(%q) = %q, want nil", in, got)
		}
		if got := probeSplice(t, `splice.drop_top_key(req.body, "x")`, in); got != "NIL" {
			t.Errorf("drop_top_key(%q) = %q, want nil", in, got)
		}
	}
}

// TestSandboxIpairsStopsAtNil pins the gopher-lua behaviour the scripts must
// not rely on: unlike PUC Lua, ipairs here uses raw len(t), so a nil hole ends
// the loop. Scripts that need to visit optional siblings must use pairs.
// See FINDINGS.md F-1.
func TestSandboxIpairsStopsAtNil(t *testing.T) {
	sc, err := script.Compile("ipairs", `
		local out = {}
		for i, v in ipairs({ nil, "b" }) do out[#out + 1] = i .. ":" .. v end
		return table.concat(out, ",")`)
	if err != nil {
		t.Fatal(err)
	}
	res := runRequest(t, sc, "", nil, nil)
	if res.Body != "" {
		t.Fatalf("ipairs no longer stops at the nil hole (got %q); update the scripts and this test", res.Body)
	}
}

func TestScriptPerformance(t *testing.T) {
	const budget = 10 * time.Millisecond
	const runs = 5
	body := buildLargeBody()
	if len(body) < 33<<10 {
		t.Fatalf("large body is %d bytes, want >= 33KB", len(body))
	}

	for _, s := range compileScripts(t) {
		c := cases[s.name]
		params := c.paramsNoChange
		if params == nil {
			params = oversizedParams
		}

		if !responseOnly[s.name] {
			// Warm the code path (first run pays for the VM and parse).
			runRequest(t, s.sc, body, nil, params)

			var worst time.Duration
			for i := 0; i < runs; i++ {
				start := time.Now()
				runRequest(t, s.sc, body, nil, params)
				if d := time.Since(start); d > worst {
					worst = d
				}
			}
			if worst > budget {
				t.Errorf("%s: request run took %v on %d bytes, over budget %v", s.name, worst, len(body), budget)
			}
			t.Logf("%-26s request(33KB, %d runs) worst=%v", s.name, runs, worst)
		}

		start := time.Now()
		runResponse(t, s.sc, chunkResponse, params)
		respDur := time.Since(start)
		if respDur > budget {
			t.Errorf("%s: response run took %v, over budget %v", s.name, respDur, budget)
		}

		t.Logf("%-26s response(chunk)            =%v", s.name, respDur)
	}
}
