package rotation

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/baongo/relay/internal/config"
)

// limiter is implemented by the Retry-After header type. The server wraps both
// fasthttp ResponseHeader and net/http Header in it.
type limiter interface {
	Peek(key string) []byte
}

// failureRule is a compiled config.Rule.
type failureRule struct {
	name          string
	status        map[int]bool
	headerMatch   []headerRe
	bodyRegex     *regexp.Regexp
	retry         bool
	abort         bool
	useRetryAfter bool
}

type headerRe struct {
	name string
	re   *regexp.Regexp
}

// compileRules compiles the ordered rules. A bad regex is a startup error:
// silently ignoring it would turn a policy into a no-op.
func compileRules(rules []*config.Rule) ([]*failureRule, error) {
	out := make([]*failureRule, 0, len(rules))
	for i, r := range rules {
		if r == nil {
			continue
		}
		fr := &failureRule{
			name:          r.Name,
			retry:         r.Retry,
			abort:         r.Abort,
			useRetryAfter: r.UseRetryAfter,
		}
		if fr.name == "" {
			fr.name = "rule#" + strconv.Itoa(i)
		}
		if len(r.Status) > 0 {
			fr.status = make(map[int]bool, len(r.Status))
			for _, s := range r.Status {
				fr.status[s] = true
			}
		}
		for name, expr := range r.HeaderMatch {
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, &ruleError{rule: fr.name, field: "header_match." + name, err: err}
			}
			fr.headerMatch = append(fr.headerMatch, headerRe{name: name, re: re})
		}
		if r.BodyRegex != "" {
			re, err := regexp.Compile(r.BodyRegex)
			if err != nil {
				return nil, &ruleError{rule: fr.name, field: "body_regex", err: err}
			}
			fr.bodyRegex = re
		}
		out = append(out, fr)
	}
	return out, nil
}

type ruleError struct {
	rule  string
	field string
	err   error
}

func (e *ruleError) Error() string {
	return "rotation: failure rule " + e.rule + ": " + e.field + ": " + e.err.Error()
}

func (e *ruleError) Unwrap() error { return e.err }

// matches applies the rule. body is only inspected when the status matched,
// because a streaming response body is large and a status is enough in the
// common case.
func (r *failureRule) matches(status int, h limiter, body []byte) bool {
	if len(r.status) > 0 && !r.status[status] {
		return false
	}
	for _, hm := range r.headerMatch {
		v := h.Peek(hm.name)
		if !hm.re.Match(v) {
			return false
		}
	}
	if r.bodyRegex != nil && !r.bodyRegex.Match(body) {
		return false
	}
	return true
}

// classify walks the rules in order; the first match wins. abort beats retry
// inside that rule: a rule that says both stops the loop without another
// attempt.
func classify(rules []*failureRule, status int, h limiter, body []byte) FailureClass {
	if len(body) > bodyCap {
		body = body[:bodyCap]
	}
	for _, r := range rules {
		if r.matches(status, h, body) {
			c := FailureClass{Rule: r.name, Matched: true, Retry: r.retry, Abort: r.abort}
			if c.Abort {
				c.Retry = false
			}
			return c
		}
	}
	return FailureClass{}
}

// parseRetryAfter reads a Retry-After value: delta-seconds, or an HTTP date.
// Anything else, or a value in the past, is zero.
func parseRetryAfter(v []byte, now time.Time) time.Duration {
	s := strings.TrimSpace(string(v))
	if s == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := parseHTTPDate(s); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

var httpDateLayouts = []string{
	"Mon, 02 Jan 2006 15:04:05 GMT",
	"Monday, 02-Jan-06 15:04:05 GMT",
	"Mon Jan _2 15:04:05 2006",
}

func parseHTTPDate(s string) (time.Time, error) {
	var err error
	for _, layout := range httpDateLayouts {
		var t time.Time
		if t, err = time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, err
}
