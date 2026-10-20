package script

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Timeout bounds one script execution. A transform script that cannot finish
// in this budget is a bug in the script, not a slow upstream.
const Timeout = 100 * time.Millisecond

// Request is the read-only `req` global presented to a request script.
type Request struct {
	Body    string
	Method  string
	Path    string
	Headers map[string]string
}

// Response is the read-only `resp` global presented to a response script.
type Response struct {
	Status  int
	Headers map[string]string
	Body    string
}

// Result is what a script may return. When Body is empty and Headers is nil
// the script signalled "no change" (returned nil), which lets the caller skip
// re-serialisation entirely.
type Result struct {
	Body    string
	Headers map[string]string
	Changed bool
}

func jsonModule(L *lua.LState) *lua.LTable {
	m := L.NewTable()
	m.RawSetString("decode", L.NewFunction(func(vm *lua.LState) int {
		var v any
		if err := json.Unmarshal([]byte(vm.CheckString(1)), &v); err != nil {
			vm.RaiseError("json.decode: %v", err)
			return 0
		}
		vm.Push(goToLua(vm, v))
		return 1
	}))
	m.RawSetString("encode", L.NewFunction(func(vm *lua.LState) int {
		b, err := json.Marshal(luaToGo(vm.CheckAny(1)))
		if err != nil {
			vm.RaiseError("json.encode: %v", err)
			return 0
		}
		vm.Push(lua.LString(string(b)))
		return 1
	}))
	return m
}

// timeModule replaces os.date/os.time with UTC-only helpers. Signing code needs
// a timestamp, but local-timezone behaviour would make signatures
// non-reproducible across hosts.
func timeModule(L *lua.LState) *lua.LTable {
	m := L.NewTable()
	m.RawSetString("utc_iso", L.NewFunction(func(vm *lua.LState) int {
		ts := int64(vm.CheckNumber(1))
		vm.Push(lua.LString(time.Unix(ts, 0).UTC().Format("20060102T150405Z")))
		return 1
	}))
	m.RawSetString("utc_rfc3339", L.NewFunction(func(vm *lua.LState) int {
		ts := int64(vm.CheckNumber(1))
		vm.Push(lua.LString(time.Unix(ts, 0).UTC().Format(time.RFC3339)))
		return 1
	}))
	return m
}

func fnSHA256(vm *lua.LState) int {
	sum := sha256.Sum256([]byte(vm.CheckString(1)))
	vm.Push(lua.LString(hex.EncodeToString(sum[:])))
	return 1
}

func fnSHA1(vm *lua.LState) int {
	sum := sha1.Sum([]byte(vm.CheckString(1)))
	vm.Push(lua.LString(hex.EncodeToString(sum[:])))
	return 1
}

func fnHMACSHA256(vm *lua.LState) int {
	mac := hmac.New(sha256.New, []byte(vm.CheckString(1)))
	mac.Write([]byte(vm.CheckString(2)))
	vm.Push(lua.LString(hex.EncodeToString(mac.Sum(nil))))
	return 1
}

func fnBase64(vm *lua.LState) int {
	vm.Push(lua.LString(base64.StdEncoding.EncodeToString([]byte(vm.CheckString(1)))))
	return 1
}

// ── JSON bridge ─────────────────────────────────────────────────────────────

// arrayMarker is set on tables produced by json.decode for JSON arrays. A bare
// Lua table cannot express "empty array" vs "empty object", so without this
// marker `json.decode("[]")` followed by `json.encode` would emit `{}` and
// corrupt any body that carries an empty array (tools, messages, content).
// The marker also makes non-empty arrays unambiguous: a table whose keys are
// exactly 1..n but which came from an object must stay an object.
const arrayMarker = "__json_array"

func luaToGo(v lua.LValue) any {
	switch t := v.(type) {
	case *lua.LTable:
		count := 0
		t.ForEach(func(_, _ lua.LValue) { count++ })
		if t.RawGetString(arrayMarker) != lua.LNil {
			out := make([]any, 0, t.Len())
			for i := 1; i <= t.Len(); i++ {
				out = append(out, luaToGo(t.RawGetInt(i)))
			}
			return out
		}
		if count > 0 && count == t.Len() {
			out := make([]any, 0, t.Len())
			for i := 1; i <= t.Len(); i++ {
				out = append(out, luaToGo(t.RawGetInt(i)))
			}
			return out
		}
		out := make(map[string]any, count)
		t.ForEach(func(k, val lua.LValue) {
			if k.String() == arrayMarker {
				return
			}
			out[k.String()] = luaToGo(val)
		})
		return out
	case lua.LBool:
		return bool(t)
	case lua.LNumber:
		f := float64(t)
		if f == float64(int64(f)) {
			return int64(f)
		}
		return f
	case lua.LString:
		return string(t)
	default:
		return nil
	}
}

func goToLua(L *lua.LState, v any) lua.LValue {
	switch t := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(t)
	case string:
		return lua.LString(t)
	case int:
		return lua.LNumber(t)
	case int64:
		return lua.LNumber(t)
	case float64:
		return lua.LNumber(t)
	case []any:
		tbl := L.NewTable()
		for _, e := range t {
			tbl.Append(goToLua(L, e))
		}
		tbl.RawSetString(arrayMarker, lua.LBool(true))
		return tbl
	case map[string]any:
		tbl := L.NewTable()
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			tbl.RawSetString(k, goToLua(L, t[k]))
		}
		return tbl
	case map[string]string:
		tbl := L.NewTable()
		for k, e := range t {
			tbl.RawSetString(k, lua.LString(e))
		}
		return tbl
	default:
		return lua.LNil
	}
}

// ── execution ───────────────────────────────────────────────────────────────

// Script is a compiled Lua chunk. It is safe for concurrent use: every Run
// builds a fresh VM, so no Lua state is shared between goroutines.
type Script struct {
	name string
	src  string
	// proto is the compiled chunk. A *FunctionProto is immutable and can be
	// instantiated in any VM, so the source is parsed once at startup instead
	// of on every request.
	proto *lua.FunctionProto
	pool  vmPool
}

// Close releases VMs cached for this script.
func (s *Script) Close() { s.pool.closeAll() }

// Compile parses src once. Syntax errors surface here, at startup.
func Compile(name, src string) (*Script, error) {
	L := buildState()
	defer L.Close()
	installGlobals(L)
	fn, err := L.LoadString(src)
	if err != nil {
		return nil, err
	}
	if fn.Proto == nil {
		return nil, fmt.Errorf("%s: empty chunk", name)
	}
	return &Script{name: name, src: src, proto: fn.Proto}, nil
}

func (s *Script) Name() string { return s.name }

// RunRequest executes the script with a request context.
func (s *Script) RunRequest(req *Request, params map[string]any) (*Result, error) {
	v, cancel := s.pool.get(Timeout)
	defer cancel()
	L := v.L

	rt, _, _ := v.tables()
	L.SetGlobal("req", reqTableReuse(L, req, rt))
	// params is fixed for the lifetime of a route, so a pre-built table is
	// reused. Building it per request was the largest single allocator after
	// the globals fix.
	_, _, pt := v.tables()
	L.SetGlobal("params", paramsToLuaInto(L, params, pt))
	res, err := s.call(L)
	s.pool.put(v, err == nil)
	return res, err
}

// paramsToLuaInto converts the params map into a reusable table. Nested values
// still allocate; the common case (flat scalars) does not.
func paramsToLuaInto(L *lua.LState, params map[string]any, t *lua.LTable) lua.LValue {
	clearTable(t)
	if params == nil {
		return t
	}
	for k, v := range params {
		t.RawSetString(k, goToLua(L, v))
	}
	return t
}

// RunResponse executes the script with a response context.
func (s *Script) RunResponse(resp *Response, params map[string]any) (*Result, error) {
	v, cancel := s.pool.get(Timeout)
	defer cancel()
	L := v.L

	_, st, pt := v.tables()
	L.SetGlobal("resp", respTableReuse(L, resp, st))
	L.SetGlobal("params", paramsToLuaInto(L, params, pt))
	res, err := s.call(L)
	s.pool.put(v, err == nil)
	return res, err
}

// reqTableReuse fills t from r. Reusing a table across requests matters:
// gopher-lua's NewTable allocates its array and hash parts up front (~590ns,
// 3 allocs, 2.8KB for an empty table), so rebuilding req/params on every
// request was the dominant cost in the script layer.
func reqTableReuse(L *lua.LState, r *Request, t *lua.LTable) *lua.LTable {
	clearTable(t)
	t.RawSetString("body", lua.LString(r.Body))
	t.RawSetString("method", lua.LString(r.Method))
	t.RawSetString("path", lua.LString(r.Path))
	t.RawSetString("headers", goToLua(L, r.Headers))
	return t
}

func respTableReuse(L *lua.LState, r *Response, t *lua.LTable) *lua.LTable {
	clearTable(t)
	t.RawSetString("status", lua.LNumber(r.Status))
	t.RawSetString("body", lua.LString(r.Body))
	t.RawSetString("headers", goToLua(L, r.Headers))
	return t
}

// clearTable removes every key and array element in place, keeping the table's
// allocated storage. gopher-lua has no public Clear, but Len and RawSet cover
// both parts.
func clearTable(t *lua.LTable) {
	for i := t.Len(); i >= 1; i-- {
		t.RawSetInt(i, lua.LNil)
	}
	t.ForEach(func(k, _ lua.LValue) {
		t.RawSet(k, lua.LNil)
	})
}

func (s *Script) call(L *lua.LState) (*Result, error) {
	L.Push(L.NewFunctionFromProto(s.proto))
	if err := L.PCall(0, 1, nil); err != nil {
		return nil, fmt.Errorf("%s: %w", s.name, err)
	}

	ret := L.Get(-1)
	res := &Result{}
	switch v := ret.(type) {
	case *lua.LNilType:
		return res, nil
	case lua.LString:
		res.Body = string(v)
		res.Changed = true
		return res, nil
	case *lua.LTable:
		if b := v.RawGetString("body"); b != lua.LNil {
			// A script that returns { body = false } would otherwise stream the
			// literal string "false" upstream. Only a string is a body.
			str, ok := b.(lua.LString)
			if !ok {
				return nil, fmt.Errorf("%s: body must be a string or nil, got %s", s.name, b.Type())
			}
			res.Body = string(str)
			res.Changed = true
		}
		if h := v.RawGetString("headers"); h != lua.LNil {
			if tbl, ok := h.(*lua.LTable); ok {
				res.Headers = map[string]string{}
				tbl.ForEach(func(k, val lua.LValue) {
					res.Headers[k.String()] = val.String()
				})
				res.Changed = true
			}
		}
		return res, nil
	default:
		return nil, fmt.Errorf("%s: returned %s, want string or table", s.name, ret.Type())
	}
}
