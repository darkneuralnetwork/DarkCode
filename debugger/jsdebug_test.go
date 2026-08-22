package debugger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jsFixture writes a small Node program whose local values at a known line are
// unambiguous, so the assertion is about the debugger rather than about the
// program.
func jsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := `function compute(a, b) {
  const sum = a + b;
  const doubled = sum * 2;
  return doubled;
}
const answer = compute(3, 4);
console.log(answer);
`
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The JavaScript path was written against DAP and never verified, because
// js-debug has no stdio mode — dapDebugServer.js binds a port. This exercises
// the socket transport end to end against the real adapter.
func TestInspectJavaScriptReadsRuntimeValues(t *testing.T) {
	if _, ok := jsDebugAdapter(); !ok {
		t.Skip("js-debug not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	dir := jsFixture(t)
	program := filepath.Join(dir, "app.js")

	report, err := Inspect(ctx,
		Options{Dir: dir, Program: program},
		[]Breakpoint{{File: program, Line: 3}}, // after sum is assigned
		[]string{"sum * 10"})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(report.Observations) == 0 {
		t.Fatalf("no breakpoint hits; unbound=%v", report.Unbound)
	}

	obs := report.Observations[0]
	locals := map[string]string{}
	for _, v := range obs.Locals {
		locals[v.Name] = v.Value
	}
	if got := locals["sum"]; got != "7" {
		t.Errorf("sum = %q at the breakpoint, want 7 (locals: %v)", got, locals)
	}
	// An evaluated expression proves the session is live, not just stopped.
	if len(obs.Expressions) > 0 && obs.Expressions[0].Value != "70" {
		t.Errorf("sum * 10 = %q, want 70", obs.Expressions[0].Value)
	}
	t.Logf("\n%s", report.Format())
}

// A missing adapter must say what to install rather than failing obscurely.
func TestMissingJSAdapterExplainsItself(t *testing.T) {
	t.Setenv("DARKCODE_JS_DEBUG", filepath.Join(t.TempDir(), "absent.js"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())

	if _, ok := jsDebugAdapter(); ok {
		t.Skip("an adapter is still resolvable in this environment")
	}
	_, err := launchDAP(context.Background(), "javascript", Options{Dir: t.TempDir()})
	if err == nil {
		t.Fatal("launching with no adapter succeeded")
	}
	if !strings.Contains(err.Error(), "js-debug") {
		t.Errorf("the error should name what to install: %v", err)
	}
}

func TestListeningPortIsParsed(t *testing.T) {
	for line, want := range map[string]string{
		"Debug server listening at 127.0.0.1:8123": "8123",
		"Debug server listening at :0":             "0",
		"listening at 45321":                       "45321",
	} {
		m := listeningRe.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("no port found in %q", line)
			continue
		}
		if m[1] != want {
			t.Errorf("port from %q = %q, want %q", line, m[1], want)
		}
	}
	if listeningRe.MatchString("some unrelated log line") {
		t.Error("an unrelated line was read as a port announcement")
	}
}
