package tools

import (
	"strings"
	"testing"
)

// TestURLTemplateEscapesModelSuppliedValues is the regression guard for the
// ITF half of DC-06.
//
// itfHTTPHandler interpolates model-supplied arguments straight into the
// destination URL. Unencoded, a value containing a URL separator does not fill
// its slot — it restructures the request.
func TestURLTemplateEscapesModelSuppliedValues(t *testing.T) {
	cases := []struct {
		name     string
		tmpl     string
		arg      string
		mustNot  string
		whyItHur string
	}{
		{
			name:     "path traversal",
			tmpl:     "https://api.example.com/search/{{q}}",
			arg:      "../../admin",
			mustNot:  "/../../admin",
			whyItHur: "the value walked out of the path segment it was written for",
		},
		{
			name:     "query injection",
			tmpl:     "https://api.example.com/search/{{q}}",
			arg:      "x?admin=true",
			mustNot:  "?admin=true",
			whyItHur: "the value started a query string the template did not have",
		},
		{
			name:     "fragment injection",
			tmpl:     "https://api.example.com/x/{{q}}",
			arg:      "y#frag",
			mustNot:  "#frag",
			whyItHur: "the value truncated the URL with a fragment",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := renderURLTemplate(c.tmpl, map[string]interface{}{"q": c.arg})
			if strings.Contains(got, c.mustNot) {
				t.Errorf("renderURLTemplate = %q — %s", got, c.whyItHur)
			}
			if !strings.HasPrefix(got, "https://api.example.com/") {
				t.Errorf("renderURLTemplate = %q, the template's own structure must survive escaping", got)
			}
		})
	}
}

// TestURLTemplateKeepsTemplateSeparators — escaping must apply to substituted
// values only. Encoding the whole rendered string would break every URL.
func TestURLTemplateKeepsTemplateSeparators(t *testing.T) {
	got := renderURLTemplate("https://api.example.com/v1/{{kind}}/{{id}}", map[string]interface{}{
		"kind": "issues",
		"id":   "42",
	})
	if got != "https://api.example.com/v1/issues/42" {
		t.Errorf("renderURLTemplate = %q, want the plain URL — ordinary values must pass through unchanged", got)
	}
}

// TestBodyTemplateIsNotURLEscaped — the body is JSON, not a URL. Percent-
// encoding it would corrupt every request.
func TestBodyTemplateIsNotURLEscaped(t *testing.T) {
	got := renderTemplate(`{"path": "{{p}}"}`, map[string]interface{}{"p": "a/b"})
	if !strings.Contains(got, "a/b") {
		t.Errorf("renderTemplate = %q, a body value must not be percent-encoded", got)
	}
}
