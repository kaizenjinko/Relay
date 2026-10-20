package script

import (
	"context"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// vmPool recycles Lua VMs between runs.
//
// Creating a VM costs ~147KB and ~420 allocations (it builds the standard
// library closures), which dominated the cost of a cache-hit transform. The
// state can be reset cheaply: dropping the stack and re-installing the globals
// is enough, because a chunk that returns normally cannot leave anything
// behind that the next chunk observes — globals are re-set on every run and no
// script-visible state is kept in the VM.
//
// A VM is discarded rather than reused when the previous run did not finish
// cleanly: a timeout or a script error can leave the VM's context expired or
// its stack in an unknown state, and a fresh VM costs less than reasoning
// about every failure mode.
type vmPool struct {
	mu   sync.Mutex
	free []*vm
}

// vm is a recycled LState plus the tables the bridge fills on every run.
// Reusing the tables avoids gopher-lua's per-NewTable allocation of the array
// and hash parts.
type vm struct {
	L      *lua.LState
	req    *lua.LTable
	resp   *lua.LTable
	params *lua.LTable
}

// tables returns the reusable tables for L, creating them on first use.
func (v *vm) tables() (*lua.LTable, *lua.LTable, *lua.LTable) {
	if v.req == nil {
		v.req = v.L.NewTable()
		v.resp = v.L.NewTable()
		v.params = v.L.NewTable()
	}
	return v.req, v.resp, v.params
}

func (p *vmPool) get(timeout time.Duration) (*vm, context.CancelFunc) {
	p.mu.Lock()
	var v *vm
	if n := len(p.free); n > 0 {
		v = p.free[n-1]
		p.free[n-1] = nil
		p.free = p.free[:n-1]
	}
	p.mu.Unlock()

	L := (*lua.LState)(nil)
	if v != nil {
		L = v.L
	}
	if L == nil {
		L = buildState()
		// The API surface is installed once per VM. A chunk that returns
		// normally cannot replace a closure it captured, and globals are only
		// read, so re-installing the tables on every run only allocated.
		installGlobals(L)
	} else {
		L.SetTop(0)
	}
	if v == nil {
		v = &vm{L: L}
	}

	// The context is per-run: SetContext replaces the previous one, so a
	// recycled VM is not permanently bound to an expired deadline.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	L.SetContext(ctx)
	// Reset per-run globals; the API tables (json, math helpers, time) stay.
	L.SetGlobal("req", lua.LNil)
	L.SetGlobal("resp", lua.LNil)
	L.SetGlobal("params", lua.LNil)
	return v, cancel
}

// put returns a VM to the pool, or drops it when reusable is false.
func (p *vmPool) put(v *vm, reusable bool) {
	if !reusable {
		v.L.Close()
		return
	}
	v.L.SetTop(0)
	p.mu.Lock()
	p.free = append(p.free, v)
	p.mu.Unlock()
}

// closeAll drops every pooled VM, releasing its memory.
func (p *vmPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, v := range p.free {
		v.L.Close()
	}
	p.free = nil
}

// buildState creates a VM with only the libraries a transform script may use.
func buildState() *lua.LState {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	for _, lib := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(lib.fn))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}
	return L
}

// installGlobals (re)creates the API surface. Called on every run so a
// recycled VM never carries a previous script's leftovers.
func installGlobals(L *lua.LState) {
	// The base library installs these; remove the ones that would let a script
	// escape the sandbox or print to the relay's stdout.
	for _, name := range []string{
		"dofile", "loadfile", "load", "loadstring",
		"collectgarbage", "print", "require", "module",
	} {
		L.SetGlobal(name, lua.LNil)
	}

	L.SetGlobal("json", jsonModule(L))
	L.SetGlobal("sha256", L.NewFunction(fnSHA256))
	L.SetGlobal("sha1", L.NewFunction(fnSHA1))
	L.SetGlobal("hmac_sha256", L.NewFunction(fnHMACSHA256))
	L.SetGlobal("base64", L.NewFunction(fnBase64))
	L.SetGlobal("unix_now", L.NewFunction(func(vm *lua.LState) int {
		vm.Push(lua.LNumber(time.Now().Unix()))
		return 1
	}))
	L.SetGlobal("time", timeModule(L))
	L.SetGlobal("req", lua.LNil)
	L.SetGlobal("resp", lua.LNil)
	L.SetGlobal("params", lua.LNil)
}
