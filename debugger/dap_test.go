package debugger

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireDebugpy skips when debugpy is absent, which is the normal state of
// most machines and must never be a failure.
func requireDebugpy(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	if err := exec.Command("python3", "-c", "import debugpy").Run(); err != nil {
		t.Skip("debugpy not installed")
	}
}

func pythonFixture(t *testing.T) (dir, program string) {
	t.Helper()
	dir = t.TempDir()
	program = filepath.Join(dir, "calc.py")
	body := "def total(items):\n" +
		"    acc = 0\n" +
		"    for n in items:\n" +
		"        acc += n\n" +
		"    return acc\n" +
		"\n" +
		"print(total([1, 2, 3]))\n"
	if err := os.WriteFile(program, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, program
}

// The same Inspect call, the same Report shape, a different language.
func TestInspectPythonReadsRuntimeValues(t *testing.T) {
	requireDebugpy(t)
	dir, program := pythonFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	report, err := Inspect(ctx,
		Options{Dir: dir, Program: program},
		[]Breakpoint{{File: program, Line: 5}}, // return acc
		[]string{"acc * 10", "len(items)"})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(report.Observations) == 0 {
		t.Fatalf("no breakpoint hits; unbound=%v", report.Unbound)
	}

	obs := report.Observations[0]
	if obs.Function != "total" {
		t.Errorf("stopped in %q, want total", obs.Function)
	}
	locals := map[string]string{}
	for _, v := range obs.Locals {
		locals[v.Name] = v.Value
	}
	if locals["acc"] != "6" {
		t.Errorf("acc = %q, want 6 — the live value", locals["acc"])
	}

	exprs := map[string]string{}
	for _, v := range obs.Expressions {
		exprs[v.Name] = v.Value
	}
	if exprs["acc * 10"] != "60" {
		t.Errorf("acc*10 = %q, want 60", exprs["acc * 10"])
	}
	if exprs["len(items)"] != "3" {
		t.Errorf("len(items) = %q, want 3", exprs["len(items)"])
	}
	if len(obs.Stack) == 0 {
		t.Error("no stack captured")
	}
}

// A bad expression is data, not a failed run — same contract as the Go path.
func TestInspectPythonReportsBadExpression(t *testing.T) {
	requireDebugpy(t)
	dir, program := pythonFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	report, err := Inspect(ctx,
		Options{Dir: dir, Program: program},
		[]Breakpoint{{File: program, Line: 5}},
		[]string{"no_such_name"})
	if err != nil {
		t.Fatalf("a bad expression must not fail the run: %v", err)
	}
	if len(report.Observations) == 0 {
		t.Fatal("no observations")
	}
	got := report.Observations[0].Expressions
	if len(got) != 1 || !strings.HasPrefix(got[0].Value, "<") {
		t.Errorf("expected the failure recorded inline, got %+v", got)
	}
}

// Dispatch is what keeps one tool surface across languages.
func TestLanguageOfSelectsDebugger(t *testing.T) {
	cases := map[string]string{
		"main.go": "go", "app.py": "python", "s.pyw": "python",
		"index.js": "javascript", "index.mjs": "javascript",
		"app.ts":    "javascript", // parsed as TypeScript, debugged as JavaScript
		"README.md": "", "Makefile": "",
	}
	for file, want := range cases {
		if got := languageOf(file); got != want {
			t.Errorf("languageOf(%q) = %q, want %q", file, got, want)
		}
	}
}

func TestInspectRejectsUnsupportedLanguage(t *testing.T) {
	_, err := Inspect(context.Background(), Options{Dir: "."},
		[]Breakpoint{{File: "notes.md", Line: 1}}, nil)
	if err == nil || !strings.Contains(err.Error(), "no debugger is configured") {
		t.Errorf("expected a clear unsupported-language error, got %v", err)
	}
}

// A missing adapter must name what to install rather than fail obscurely.
//
// Asserting on one exact phrase made this test agree with whatever the message
// happened to say. On a machine with js-debug installed it skips, so a reworded
// error looked fine locally and broke every runner that did not have it. It now
// checks what the reader actually needs: which component, and how to point at
// an existing copy.
func TestMissingAdapterIsExplicit(t *testing.T) {
	_, err := launchDAP(context.Background(), "javascript", Options{Dir: t.TempDir()})
	if err == nil {
		t.Skip("a JavaScript debug adapter is installed here")
	}
	msg := err.Error()
	for _, want := range []string{"js-debug", "DARKCODE_JS_DEBUG"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error should mention %q so the reader knows what to do: %v", want, msg)
		}
	}
}
