package obs

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Logger emits one record per event. The zero value is unusable; call New.
//
// JSON mode is line-delimited objects so a collector (Loki, Vector, CloudWatch)
// can parse them without a regex. Text mode stays close to the original
// log.Printf output for interactive use.
type Logger struct {
	mu     sync.Mutex
	w      io.Writer
	json   bool
	fields map[string]any
}

// New builds a logger writing to w. json selects structured output.
func New(w io.Writer, json bool) *Logger {
	return &Logger{w: w, json: json}
}

// NewStdout is the production entry point: stdout, mode from config.
func NewStdout(json bool) *Logger { return New(os.Stdout, json) }

// With returns a logger that always includes the given key/value pairs.
func (l *Logger) With(kv ...any) *Logger {
	cp := make(map[string]any, len(l.fields)+len(kv)/2)
	for k, v := range l.fields {
		cp[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		cp[fmt.Sprint(kv[i])] = kv[i+1]
	}
	return &Logger{w: l.w, json: l.json, fields: cp}
}

// Info logs at info level.
func (l *Logger) Info(msg string, kv ...any) { l.emit("info", msg, kv) }

// Warn logs at warn level.
func (l *Logger) Warn(msg string, kv ...any) { l.emit("warn", msg, kv) }

// Error logs at error level.
func (l *Logger) Error(msg string, kv ...any) { l.emit("error", msg, kv) }

func (l *Logger) emit(level, msg string, kv []any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.json {
		rec := make(map[string]any, 4+len(l.fields)+len(kv)/2)
		for k, v := range l.fields {
			rec[k] = v
		}
		rec["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
		rec["level"] = level
		rec["msg"] = msg
		for i := 0; i+1 < len(kv); i += 2 {
			rec[fmt.Sprint(kv[i])] = kv[i+1]
		}
		// json.Encoder appends the newline and never fails for the value types
		// used here (strings, ints, durations, errors).
		_ = json.NewEncoder(l.w).Encode(rec)
		return
	}

	var b []byte
	b = append(b, time.Now().Format("2006/01/02 15:04:05")...)
	b = append(b, ' ')
	b = append(b, level...)
	b = append(b, ' ')
	b = append(b, msg...)
	for i := 0; i+1 < len(kv); i += 2 {
		b = append(b, ' ')
		b = append(b, fmt.Sprint(kv[i])...)
		b = append(b, '=')
		b = append(b, fmt.Sprint(kv[i+1])...)
	}
	b = append(b, '\n')
	_, _ = l.w.Write(b)
}
