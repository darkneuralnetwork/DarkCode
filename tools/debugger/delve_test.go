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

// requireDelve skips when delve is absent, which is the normal state of most
// machines and must never be a test failure.
func requireDelve(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("delve not installed")
	}
}

// fixture writes a small module with a function worth inspecting.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module dbgfixture\n\ngo 1.24\n",
		"calc.go": "package dbgfixture\n\n" +
			"func Total(items []int) int {\n" +
			"\ttotal := 0\n" +
			"\tfor _, n := range items {\n" +
			"\t\ttotal += n\n" +
			"\t}\n" +
			"\treturn total\n" +
			"}\n",
		"calc_test.go": "package dbgfixture\n\nimport \"testing\"\n\n" +
			"func TestTotal(t *testing.T) {\n" +
			"\tif Total([]int{1, 2, 3}) != 6 {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The whole point: real values from a real run, without editing the source.
func TestInspectReadsRuntimeValues(t *testing.T) {
	requireDelve(t)
	dir := fixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	report, err := Inspect(ctx,
		Options{Dir: dir, Test: true, Run: "TestTotal"},
		[]Breakpoint{{File: filepath.Join(dir, "calc.go"), Line: 8}}, // return total
		[]string{"total * 10", "len(items)"})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(report.Observations) == 0 {
		t.Fatalf("no breakpoint hits; unbound=%v", report.Unbound)
	}

	obs := report.Observations[0]
	if obs.Function != "dbgfixture.Total" {
		t.Errorf("stopped in %q, want dbgfixture.Total", obs.Function)
	}
	locals := map[string]string{}
	for _, v := range obs.Locals {
		locals[v.Name] = v.Value
	}
	if locals["total"] != "6" {
		t.Errorf("total = %q, want 6 — this is the live value, not a guess", locals["total"])
	}

	exprs := map[string]string{}
	for _, v := range obs.Expressions {
		exprs[v.Name] = v.Value
	}
	if exprs["total * 10"] != "60" {
		t.Errorf("total*10 = %q, want 60", exprs["total * 10"])
	}
	if exprs["len(items)"] != "3" {
		t.Errorf("len(items) = %q, want 3", exprs["len(items)"])
	}
	if len(obs.Stack) < 2 {
		t.Errorf("stack too shallow: %+v", obs.Stack)
	}
}

// An expression that does not resolve is information, not a failure — the
// caller needs to see which name was out of scope.
func TestInspectReportsBadExpressionInline(t *testing.T) {
	requireDelve(t)
	dir := fixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	report, err := Inspect(ctx,
		Options{Dir: dir, Test: true, Run: "TestTotal"},
		[]Breakpoint{{File: filepath.Join(dir, "calc.go"), Line: 8}},
		[]string{"noSuchVariable"})
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

// A line with no executable statement must say so rather than hang or report
// "the code never ran".
func TestInspectReportsUnbindableBreakpoint(t *testing.T) {
	requireDelve(t)
	dir := fixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	_, err := Inspect(ctx,
		Options{Dir: dir, Test: true, Run: "TestTotal"},
		[]Breakpoint{{File: filepath.Join(dir, "calc.go"), Line: 2}}, // blank line
		nil)
	if err == nil {
		t.Fatal("expected an error for a breakpoint that cannot bind")
	}
	if !strings.Contains(err.Error(), "no breakpoint could be set") {
		t.Errorf("unclear error: %v", err)
	}
}

func TestInspectRequiresBreakpoint(t *testing.T) {
	if _, err := Inspect(context.Background(), Options{Dir: "."}, nil, nil); err == nil {
		t.Error("expected an error when no breakpoint is given")
	}
}

// A build failure must surface the compiler's message, not a bare timeout.
func TestLaunchSurfacesBuildErrors(t *testing.T) {
	requireDelve(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module broken\n\ngo 1.24\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { undefinedCall() }\n"), 0o644)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, err := Launch(ctx, Options{Dir: dir})
	if err == nil {
		t.Fatal("expected a build failure")
	}
	if !strings.Contains(err.Error(), "undefinedCall") {
		t.Errorf("error should carry the compiler message, got: %v", err)
	}
}

func TestReportFormat(t *testing.T) {
	r := &Report{
		Target: "pkg (test)",
		Observations: []Observation{{
			File: "a.go", Line: 8, Function: "pkg.Total",
			Locals:      []Variable{{Name: "total", Value: "6", Type: "int"}},
			Expressions: []Variable{{Name: "len(items)", Value: "3"}},
			Stack:       []Frame{{Function: "pkg.Total"}, {Function: "pkg.TestTotal"}},
		}},
		Unbound: []string{"a.go:2 — no statement"},
	}
	out := r.Format()
	for _, want := range []string{"pkg (test)", "a.go:8", "pkg.Total", "total = 6", "len(items) → 3", "⚠", "←"} {
		if !strings.Contains(out, want) {
			t.Errorf("format missing %q:\n%s", want, out)
		}
	}
}
