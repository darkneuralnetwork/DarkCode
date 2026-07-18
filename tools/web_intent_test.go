package tools

import "testing"

// classifySearchIntent replaces a per-search LLM classification call with a
// deterministic keyword heuristic — verify it maps the four buckets and
// defaults to web.
func TestClassifySearchIntent(t *testing.T) {
	cases := map[string]string{
		"who is the usa president":                 "web",
		"latest news on ai regulation":             "web",
		"where is Handler defined in this repo":    "local",
		"search the codebase for the auth module":  "local",
		"find a github repository for a go lru":     "github",
		"open source examples of rate limiters":     "github",
		"gin framework documentation":              "docs",
		"how to use the stripe sdk":                "docs",
	}
	for q, want := range cases {
		if got := classifySearchIntent(q); got != want {
			t.Errorf("classifySearchIntent(%q) = %q, want %q", q, got, want)
		}
	}
}
