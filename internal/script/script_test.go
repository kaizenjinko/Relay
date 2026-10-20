package script

import (
	"strings"
	"testing"
)

func mustCompile(t *testing.T, src string) *Script {
	t.Helper()
	s, err := Compile("test", src)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRunRequestReturnsBody(t *testing.T) {
	s := mustCompile(t, `
    return req.body .. "-touched"
  `)
	res, err := s.RunRequest(&Request{Body: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Body != "x-touched" || !res.Changed {
		t.Fatalf("got %+v", res)
	}
}

func TestRunRequestNilMeansNoChange(t *testing.T) {
	s := mustCompile(t, `return nil`)
	res, err := s.RunRequest(&Request{Body: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Fatalf("changed = true on nil return")
	}
}

func TestScriptCanReturnHeaders(t *testing.T) {
	s := mustCompile(t, `
    return { body = req.body, headers = { ["x-sig"] = sha256(req.body) } }
  `)
	res, err := s.RunRequest(&Request{Body: "abc"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Headers["x-sig"] != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("bad hash %q", res.Headers["x-sig"])
	}
}

func TestParamsReachScript(t *testing.T) {
	s := mustCompile(t, `return params.prefix .. req.body`)
	res, err := s.RunRequest(&Request{Body: "y"}, map[string]any{"prefix": "cnb_"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Body != "cnb_y" {
		t.Fatalf("body = %q", res.Body)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	s := mustCompile(t, `
    local d = json.decode(req.body)
    d.added = true
    return json.encode(d)
  `)
	res, err := s.RunRequest(&Request{Body: `{"a":1}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Body, `"added":true`) || !strings.Contains(res.Body, `"a":1`) {
		t.Fatalf("body = %q", res.Body)
	}
}

func TestResponseScriptSeesStatus(t *testing.T) {
	s := mustCompile(t, `
    if resp.status == 401 then return "retry" end
    return nil
  `)
	res, err := s.RunResponse(&Response{Status: 401, Body: ""}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Body != "retry" {
		t.Fatalf("body = %q", res.Body)
	}
}

func TestSandboxBlocksEscape(t *testing.T) {
	for _, probe := range []string{
		`io.open("/etc/passwd")`,
		`os.execute("id")`,
		`require("os")`,
		`dofile("/etc/passwd")`,
		`load("return 1")()`,
		`print("x")`,
	} {
		s := mustCompile(t, `local ok = pcall(function() `+probe+` end) return tostring(ok)`)
		res, err := s.RunRequest(&Request{}, nil)
		if err != nil {
			t.Fatalf("%s: %v", probe, err)
		}
		if res.Body != "false" {
			t.Errorf("%s was allowed", probe)
		}
	}
}

func TestTimeoutKillsInfiniteLoop(t *testing.T) {
	s := mustCompile(t, `while true do end`)
	if _, err := s.RunRequest(&Request{}, nil); err == nil {
		t.Fatal("expected timeout error")
		return
	}
}

func TestCompileRejectsSyntaxError(t *testing.T) {
	if _, err := Compile("bad", `function(`); err == nil {
		t.Fatal("expected syntax error")
	}
}

func TestConcurrentRunsAreIsolated(t *testing.T) {
	s := mustCompile(t, `
    local d = json.decode(req.body)
    d.n = d.n + 1
    return json.encode(d)
  `)
	const n = 50
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			res, err := s.RunRequest(&Request{Body: `{"n":0}`}, nil)
			if err != nil {
				errs <- err
				return
			}
			if !strings.Contains(res.Body, `"n":1`) {
				errs <- errString("state leaked between runs: " + res.Body)
				return
			}
			errs <- nil
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestJSONEmptyArraySurvivesRoundTrip guards F-4: json.decode of an empty
// array must re-encode as [] rather than {}, otherwise any body carrying
// tools/messages/content: [] is silently reshaped.
func TestJSONEmptyArraySurvivesRoundTrip(t *testing.T) {
	s := mustCompile(t, `
    local d = json.decode(req.body)
    return json.encode({ messages = d.messages, tools = d.tools, obj = {} })
  `)
	res, err := s.RunRequest(&Request{Body: `{"messages":[],"tools":[]}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Body, `"messages":[]`) {
		t.Fatalf("messages reshaped: %s", res.Body)
	}
	if !strings.Contains(res.Body, `"tools":[]`) {
		t.Fatalf("tools reshaped: %s", res.Body)
	}
	// A genuinely empty object literal must still encode as {}.
	if !strings.Contains(res.Body, `"obj":{}`) {
		t.Fatalf("empty object reshaped: %s", res.Body)
	}
}

// TestJSONArrayOfObjectsStaysArray guards that marking arrays does not leak
// into the encoded output as a stray key.
func TestJSONArrayOfObjectsStaysArray(t *testing.T) {
	s := mustCompile(t, `
    local d = json.decode(req.body)
    return json.encode(d)
  `)
	res, err := s.RunRequest(&Request{Body: `{"tools":[{"type":"function"}]}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Body, "__json_array") {
		t.Fatalf("marker leaked into output: %s", res.Body)
	}
	if !strings.Contains(res.Body, `"tools":[{"type":"function"}]`) {
		t.Fatalf("array shape lost: %s", res.Body)
	}
}

// TestNonStringBodyRejected guards F-2: { body = false } must be an error,
// not the literal string "false" sent upstream.
func TestNonStringBodyRejected(t *testing.T) {
	for _, src := range []string{
		`return { body = false }`,
		`return { body = 42 }`,
		`return { body = {} }`,
	} {
		s := mustCompile(t, src)
		if _, err := s.RunRequest(&Request{Body: `{}`}, nil); err == nil {
			t.Errorf("%s: expected an error", src)
		}
	}
}
