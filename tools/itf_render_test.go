package tools

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// shellQuote is the only thing standing between a model-supplied argument and
// a shell. Custom ITF tools declare a command template with {{name}} tokens;
// the values come from whatever the model decided to pass. Until now this had
// no tests at all.

// TestShellQuoteNeutralisesInjection runs each quoted value through a real
// shell and checks the payload arrives as one literal argument. Asserting on
// the quoted string alone would only test my idea of what bash does.
func TestShellQuoteNeutralisesInjection(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	payloads := []string{
		"; echo INJECTED",
		"&& echo INJECTED",
		"| echo INJECTED",
		"$(echo INJECTED)",
		"`echo INJECTED`",
		"' ; echo INJECTED ; '",
		"'\"'\"'",
		"$HOME",
		"${PATH}",
		"a\nb",
		"newline\necho INJECTED",
		">/tmp/should-not-exist",
		"--flag=value",
		"",
		"plain",
	}

	for _, p := range payloads {
		t.Run(strings.ReplaceAll(p, "\n", "\\n"), func(t *testing.T) {
			// printf %s writes the argument verbatim; anything the shell
			// expanded or executed shows up as a difference.
			cmd := exec.Command("bash", "-c", "printf %s "+shellQuote(p))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("shell rejected the quoted value: %v (%s)", err, out)
			}
			if string(out) != p {
				t.Errorf("value round-tripped as %q, want %q — the shell interpreted it", out, p)
			}
			if strings.Contains(string(out), "INJECTED") && !strings.Contains(p, "INJECTED") {
				t.Errorf("payload executed: %q", out)
			}
		})
	}
}

// TestShellQuoteAlwaysQuotes. An unquoted empty value would silently vanish
// from the command line, shifting every later positional argument.
func TestShellQuoteAlwaysQuotes(t *testing.T) {
	for _, s := range []string{"", "plain", "with space"} {
		got := shellQuote(s)
		if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
			t.Errorf("shellQuote(%q) = %q, want it wrapped in single quotes", s, got)
		}
	}
}

func TestRenderTemplate(t *testing.T) {
	args := map[string]interface{}{"name": "world", "n": float64(3)}

	cases := []struct{ tmpl, want string }{
		{"echo {{name}}", "echo world"},
		{"no placeholders", "no placeholders"},
		{"{{name}}{{name}}", "worldworld"},
		{"{{ name }}", "world"},                    // surrounding space is trimmed
		{"count {{n}}", "count 3"},                 // JSON numbers print as integers
		{"missing {{nope}} here", "missing  here"}, // unknown → empty, not left literal
		{"", ""},
		{"unclosed {{name", "unclosed {{name"}, // no terminator: emitted verbatim
		{"{{}}", ""},
	}
	for _, tc := range cases {
		if got := renderTemplate(tc.tmpl, args); got != tc.want {
			t.Errorf("renderTemplate(%q) = %q, want %q", tc.tmpl, got, tc.want)
		}
	}
}

// TestRenderTemplateDoesNotRescanSubstitutions. If a substituted value
// containing "{{" were rescanned, an argument could name another argument —
// which is a way to reach a value the template author never exposed.
func TestRenderTemplateDoesNotRescanSubstitutions(t *testing.T) {
	args := map[string]interface{}{
		"user":   "{{secret}}",
		"secret": "s3cr3t",
	}
	got := renderTemplate("echo {{user}}", args)
	if strings.Contains(got, "s3cr3t") {
		t.Errorf("a substituted value was re-expanded: %q", got)
	}
	if got != "echo {{secret}}" {
		t.Errorf("renderTemplate = %q, want the literal substitution", got)
	}
}

// TestToStrKeepsNumbersReadable. JSON decodes every number as float64, so
// without the integer check a count of 1000000 renders as "1e+06" and the
// command receives something the tool never meant.
func TestToStrKeepsNumbersReadable(t *testing.T) {
	cases := []struct {
		in   interface{}
		want string
	}{
		{nil, ""},
		{"text", "text"},
		{true, "true"},
		{false, "false"},
		{float64(0), "0"},
		{float64(42), "42"},
		{float64(1000000), "1000000"},
		{float64(-7), "-7"},
		{float64(1.5), "1.5"},
		{json.Number("12345678901234567890"), "12345678901234567890"},
	}
	for _, tc := range cases {
		if got := toStr(tc.in); got != tc.want {
			t.Errorf("toStr(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestToStrOnStructuredValues. A list or object argument must still produce
// something the command can consume rather than a Go-syntax dump.
func TestToStrOnStructuredValues(t *testing.T) {
	got := toStr([]interface{}{"a", "b"})
	if got != `["a","b"]` {
		t.Errorf("toStr(list) = %q, want JSON", got)
	}
	obj := toStr(map[string]interface{}{"k": "v"})
	if !json.Valid([]byte(obj)) {
		t.Errorf("toStr(map) = %q, which is not JSON", obj)
	}
}

// TestQuotedRenderIsInjectionSafeEndToEnd exercises the real order of
// operations: quote every value, then render. Quoting after rendering would
// let a value's contents become part of the command structure.
func TestQuotedRenderIsInjectionSafeEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	args := map[string]interface{}{"q": "; echo INJECTED ;"}
	quoted := make(map[string]interface{}, len(args))
	for k, v := range args {
		quoted[k] = shellQuote(toStr(v))
	}
	command := renderTemplate("printf %s {{q}}", quoted)

	out, err := exec.Command("bash", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %v (%s)", err, out)
	}
	if strings.Contains(string(out), "INJECTED\n") || strings.TrimSpace(string(out)) == "INJECTED" {
		t.Errorf("injection executed; output was %q", out)
	}
	if string(out) != "; echo INJECTED ;" {
		t.Errorf("output = %q, want the payload as one literal argument", out)
	}
}
