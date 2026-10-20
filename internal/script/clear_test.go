package script

import "testing"

func TestClearTableAndReuse(t *testing.T) {
	s, _ := Compile("x", `
		local seen = {}
		seen[#seen+1] = req.path
		seen[#seen+1] = tostring(req.headers["x-tag"])
		seen[#seen+1] = tostring(params.prefix)
		return table.concat(seen, "|")
	`)
	params := map[string]any{"prefix": "p"}
	for i := 0; i < 3; i++ {
		res, err := s.RunRequest(&Request{
			Body: "b", Method: "POST", Path: "/a",
			Headers: map[string]string{"x-tag": "v"},
		}, params)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if res.Body != "/a|v|p" {
			t.Fatalf("run %d: reuse leaked stale state: %q", i, res.Body)
		}
	}
}
