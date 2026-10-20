package rotation

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// staticSource is a single credential written out in TOML. Its headers are
// literal: templates are not applied, because there is no key.
type staticSource struct {
	headers map[string]string
	meta    map[string]string
}

func newStaticSource(headers, meta map[string]string) *staticSource {
	return &staticSource{headers: cloneMap(headers), meta: cloneMap(meta)}
}

func (s *staticSource) List() ([]*Credential, error) {
	return []*Credential{{
		ID:      "static#0",
		Headers: cloneMap(s.headers),
		Meta:    cloneMap(s.meta),
	}}, nil
}

// keyedSource turns a key list into credentials by rendering template once per
// key. It backs list, file and env, which differ only in where the keys come
// from.
type keyedSource struct {
	kind     string
	keys     []string
	template map[string]string
	meta     map[string]string
}

func (s *keyedSource) List() ([]*Credential, error) {
	keys := splitKeys(s.keys)
	out := make([]*Credential, 0, len(keys))
	for i, k := range keys {
		out = append(out, &Credential{
			ID:      fmt.Sprintf("%s#%d", s.kind, i),
			Headers: renderTemplate(s.template, k),
			Meta:    cloneMap(s.meta),
		})
	}
	return out, nil
}

// newListSource takes keys straight from TOML.
func newListSource(keys []string, template, meta map[string]string) *keyedSource {
	return &keyedSource{kind: "list", keys: keys, template: template, meta: meta}
}

// newEnvSource reads a comma-separated key list from the environment once, at
// build time.
func newEnvSource(name string, template, meta map[string]string) *keyedSource {
	raw := os.Getenv(name)
	var keys []string
	if raw != "" {
		keys = strings.Split(raw, ",")
	}
	return &keyedSource{kind: "env", keys: keys, template: template, meta: meta}
}

// fileSource reads one key per line. The file is read on every List call, so a
// reload sees edited keys; a missing file is an error rather than an empty
// pool.
type fileSource struct {
	path     string
	template map[string]string
	meta     map[string]string
}

func newFileSource(path string, template, meta map[string]string) *fileSource {
	return &fileSource{path: path, template: template, meta: meta}
}

func (s *fileSource) List() ([]*Credential, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var keys []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		// Full-line comments only; a key never starts with '#'.
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		keys = append(keys, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	ks := &keyedSource{kind: "file", keys: keys, template: s.template, meta: s.meta}
	return ks.List()
}

// cookieJarSource reads a JSON account file:
//
//	{"accounts":[{"id":"a1","headers":{"Cookie":"..."},"meta":{"plan":"pro"}}]}
//
// Header values are literal; account headers are the whole identity.
type cookieJarSource struct {
	path string
}

func newCookieJarSource(path string) (*cookieJarSource, error) {
	if path == "" {
		return nil, fmt.Errorf("cookie_jar needs path")
	}
	return &cookieJarSource{path: path}, nil
}

type cookieJarFile struct {
	Accounts []*jarAccount `json:"accounts"`
}

type jarAccount struct {
	ID      string            `json:"id"`
	Headers map[string]string `json:"headers"`
	Meta    map[string]string `json:"meta"`
}

func (s *cookieJarSource) List() ([]*Credential, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var jar cookieJarFile
	if err := json.Unmarshal(data, &jar); err != nil {
		return nil, fmt.Errorf("cookie_jar %s: %w", s.path, err)
	}
	out := make([]*Credential, 0, len(jar.Accounts))
	for i, a := range jar.Accounts {
		if a == nil {
			continue
		}
		id := a.ID
		if id == "" {
			id = fmt.Sprintf("cookie_jar#%d", i)
		}
		out = append(out, &Credential{ID: id, Headers: cloneMap(a.Headers), Meta: cloneMap(a.Meta)})
	}
	return out, nil
}

func cloneMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
