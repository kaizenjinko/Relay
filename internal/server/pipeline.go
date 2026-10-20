package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/baongo/relay/internal/config"
	"github.com/baongo/relay/internal/script"
)

// route is a config.Route bound to its compiled script.
type route struct {
	cfg    *config.Route
	script *script.Script
}

// sseWriter rewrites an SSE body line by line, running the response script on
// each data: payload. Event framing (event:, id:, retry:, comments, blank
// lines) is copied verbatim; only what the script returns is substituted.
type sseWriter struct {
	r      *compiledRoute
	params map[string]any
}

// write consumes src and writes rewritten lines to dst, flushing after every
// line so the client's SSE parser sees tokens as they arrive.
func (w *sseWriter) write(src *bufio.Reader, dst interface {
	Write([]byte) (int, error)
}, flush func() error) error {
	// A single scratch buffer is reused for every line: the rewritten payload
	// replaces the original in place when it is no longer, so all but the
	// growing lines cost nothing.
	var scratch []byte
	for {
		line, err := src.ReadBytes('\n')
		if len(line) > 0 {
			out, rerr := w.rewriteLine(line, &scratch)
			if rerr != nil {
				return rerr
			}
			if _, werr := dst.Write(out); werr != nil {
				return werr
			}
			if flush != nil {
				if ferr := flush(); ferr != nil {
					return ferr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// rewriteLine returns the line unchanged when it carries no data: payload or
// when the script reports no change.
func (w *sseWriter) rewriteLine(line []byte, scratch *[]byte) ([]byte, error) {
	idx := bytes.Index(line, []byte("data:"))
	if idx < 0 {
		return line, nil
	}
	payload := bytes.TrimSpace(line[idx+len("data:"):])
	if len(payload) == 0 || string(payload) == "[DONE]" {
		return line, nil
	}
	res, err := w.r.script.RunResponse(&script.Response{
		Status: 200,
		Body:   string(payload),
	}, w.params)
	if err != nil {
		// A failing response script must not corrupt the stream.
		return line, nil
	}
	if !res.Changed || res.Body == "" {
		return line, nil
	}
	need := idx + len("data: ") + len(res.Body) + 2
	if cap(*scratch) < need {
		*scratch = make([]byte, need)
	}
	out := (*scratch)[:0]
	out = append(out, line[:idx]...)
	out = append(out, "data: "...)
	out = append(out, res.Body...)
	n := len(line)
	if n > 0 && line[n-1] == '\n' {
		if n > 1 && line[n-2] == '\r' {
			out = append(out, '\r', '\n')
		} else {
			out = append(out, '\n')
		}
	}
	return out, nil
}

// ── header pipeline ─────────────────────────────────────────────────────────

// headerSet is a compiled header transformation: static set, drop list and
// per-request dynamic generators.
type headerSet struct {
	set     map[string]string
	drop    []string
	dynamic []dynGen
}

type dynGen struct {
	name   string
	kind   string
	prefix string
	nbytes int
	value  string
	envVar string
	path   string
}

func compileHeaders(h config.Headers) *headerSet {
	hs := &headerSet{set: h.Set, drop: h.Drop}
	for name, d := range h.Dynamic {
		kind := d.Kind
		if kind == "" {
			kind = "hex"
		}
		nb := d.Bytes
		if kind == "hex" && nb == 0 {
			nb = 13
		}
		hs.dynamic = append(hs.dynamic, dynGen{
			name: name, kind: kind, prefix: d.Prefix, nbytes: nb,
			value: d.Value, envVar: d.Name, path: d.Path,
		})
	}
	return hs
}

// dropped reports whether the header must not be forwarded. Patterns support a
// trailing wildcard only, which is all the drop lists need in practice.
func (hs *headerSet) dropped(name string) bool {
	l := strings.ToLower(name)
	for _, p := range hs.drop {
		p = strings.ToLower(p)
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(l, strings.TrimSuffix(p, "*")) {
				return true
			}
		} else if l == p {
			return true
		}
	}
	return false
}

// generate renders the dynamic header values for one request.
func (hs *headerSet) generate() map[string]string {
	if len(hs.dynamic) == 0 {
		return nil
	}
	out := make(map[string]string, len(hs.dynamic))
	for _, d := range hs.dynamic {
		out[d.name] = d.render()
	}
	return out
}

func (d *dynGen) render() string {
	switch d.kind {
	case "static":
		return d.prefix + d.value
	case "env":
		return d.prefix + os.Getenv(d.envVar)
	case "file":
		b, err := os.ReadFile(d.path)
		if err != nil {
			return d.prefix
		}
		return d.prefix + strings.TrimSpace(string(b))
	case "uuid":
		var u [16]byte
		if _, err := rand.Read(u[:]); err != nil {
			return d.prefix
		}
		u[6] = (u[6] & 0x0f) | 0x40
		u[8] = (u[8] & 0x3f) | 0x80
		s := hex.EncodeToString(u[:])
		return d.prefix + s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
	default:
		b := make([]byte, d.nbytes)
		if _, err := rand.Read(b); err != nil {
			return d.prefix
		}
		return d.prefix + hex.EncodeToString(b)
	}
}

func (hs *headerSet) String() string {
	return fmt.Sprintf("set=%d drop=%d dynamic=%d", len(hs.set), len(hs.drop), len(hs.dynamic))
}
