package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustNew(t *testing.T, cfg map[string][]Hook) *Manager {
	t.Helper()
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestNilManagerIsAValidNoOp — every call site is an unconditional line on a
// hot path. If nil were not safe, each would need a guard and one would be
// forgotten.
func TestNilManagerIsAValidNoOp(t *testing.T) {
	var m *Manager
	if m.Configured(PreTool) {
		t.Error("a nil manager reported hooks")
	}
	if err := m.Run(context.Background(), PreTool, Context{Tool: "x"}); err != nil {
		t.Errorf("a nil manager refused: %v", err)
	}
	m.SetLog(func(string) {}) // must not panic
}

// TestEmptyConfigYieldsNoManager — an empty map is "no hooks", not "a manager
// with nothing in it", so the no-op stays free.
func TestEmptyConfigYieldsNoManager(t *testing.T) {
	for name, cfg := range map[string]map[string][]Hook{
		"nil":            nil,
		"empty":          {},
		"point-no-hooks": {"pre_tool": {}},
	} {
		t.Run(name, func(t *testing.T) {
			if m := mustNew(t, cfg); m != nil {
				t.Errorf("got a manager for %s config", name)
			}
		})
	}
}

// TestUnknownPointIsRejectedAtLoad. A hook filed under a misspelled point would
// never fire and never complain — silent for the life of the install. This is
// the §3.4 precedent: a check that reports nothing is worse than no check.
func TestUnknownPointIsRejectedAtLoad(t *testing.T) {
	_, err := New(map[string][]Hook{"post_tools": {{Run: "true"}}})
	if err == nil {
		t.Fatal("a misspelled point loaded silently")
	}
	if !strings.Contains(err.Error(), "post_tools") || !strings.Contains(err.Error(), "post_tool") {
		t.Errorf("error names neither the mistake nor the valid set: %v", err)
	}
}

func TestEmptyCommandAndBadTimeoutAreRejected(t *testing.T) {
	if _, err := New(map[string][]Hook{"pre_tool": {{Match: "x"}}}); err == nil {
		t.Error("a hook with no command loaded")
	}
	if _, err := New(map[string][]Hook{"pre_tool": {{Run: "true", Timeout: "soon"}}}); err == nil {
		t.Error("an unparseable timeout loaded")
	}
}

// TestPreToolHookCanRefuse — the whole reason pre_tool is distinguished.
func TestPreToolHookCanRefuse(t *testing.T) {
	m := mustNew(t, map[string][]Hook{"pre_tool": {{Run: `echo "not on my watch" >&2; exit 1`}}})
	err := m.Run(context.Background(), PreTool, Context{Tool: "write_file"})
	if err == nil {
		t.Fatal("a failing pre_tool hook did not block")
	}
	if !strings.Contains(err.Error(), "not on my watch") {
		t.Errorf("the refusal lost the hook's own message: %v", err)
	}
}

// TestOtherPointsNeverBlock. A broken journal script must not break the agent.
func TestOtherPointsNeverBlock(t *testing.T) {
	for _, p := range []Point{SessionStart, PostTool, PreCompact, TurnEnd} {
		t.Run(string(p), func(t *testing.T) {
			var logged []string
			m := mustNew(t, map[string][]Hook{string(p): {{Run: "exit 3"}}})
			m.SetLog(func(s string) { logged = append(logged, s) })
			if err := m.Run(context.Background(), p, Context{}); err != nil {
				t.Errorf("%s blocked the turn: %v", p, err)
			}
			if len(logged) != 1 {
				t.Errorf("a failure at %s was silent (%d log lines)", p, len(logged))
			}
		})
	}
}

// TestContextArrivesAsEnvironment — and the values are readable.
func TestContextArrivesAsEnvironment(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env.txt")
	m := mustNew(t, map[string][]Hook{
		"post_tool": {{Run: `printf '%s|%s|%s|%s' "$DARKCODE_HOOK" "$DARKCODE_TOOL" "$DARKCODE_FILE" "$DARKCODE_SUCCESS" > ` + out}},
	})
	if err := m.Run(context.Background(), PostTool, Context{Tool: "write_file", File: "a.go", Success: true}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := "post_tool|write_file|a.go|1"; string(got) != want {
		t.Errorf("environment = %q, want %q", got, want)
	}
}

// TestAHostileFilenameIsAValueNotSyntax.
//
// This is why context is passed as environment rather than substituted into the
// command. Under the obvious design — building the command by formatting the
// path into it — a repository containing this filename would run the payload.
func TestAHostileFilenameIsAValueNotSyntax(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "canary")
	if err := os.WriteFile(canary, []byte("intact"), 0o600); err != nil {
		t.Fatal(err)
	}

	hostile := `x.go"; rm -f ` + canary + `; echo "`
	m := mustNew(t, map[string][]Hook{"post_tool": {{Run: `echo "$DARKCODE_FILE" > /dev/null`}}})
	if err := m.Run(context.Background(), PostTool, Context{Tool: "write_file", File: hostile}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("the filename executed: %v", err)
	}
}

// TestMatchFiltersByTool, including the glob and the point-with-no-tool case.
func TestMatchFiltersByTool(t *testing.T) {
	cases := []struct {
		match, tool string
		want        bool
	}{
		{"", "write_file", true},
		{"", "", true},
		{"write_file", "write_file", true},
		{"write_file", "read_file", false},
		{"write_*", "write_patch", true},
		{"write_*", "read_file", false},
		{"write_file", "", false}, // a tool filter cannot match turn_end
	}
	for _, c := range cases {
		if got := (Hook{Match: c.match}).matches(c.tool); got != c.want {
			t.Errorf("match=%q tool=%q = %v, want %v", c.match, c.tool, got, c.want)
		}
	}
}

// TestOnlyMatchingHooksRun — a filtered-out hook must not fire, and must not be
// able to block either.
func TestOnlyMatchingHooksRun(t *testing.T) {
	m := mustNew(t, map[string][]Hook{"pre_tool": {{Match: "write_file", Run: "exit 1"}}})
	if err := m.Run(context.Background(), PreTool, Context{Tool: "read_file"}); err != nil {
		t.Errorf("a hook matching another tool blocked this one: %v", err)
	}
	if err := m.Run(context.Background(), PreTool, Context{Tool: "write_file"}); err == nil {
		t.Error("the matching hook did not block")
	}
}

// TestATimeoutDoesNotHangTheTurn.
func TestATimeoutDoesNotHangTheTurn(t *testing.T) {
	m := mustNew(t, map[string][]Hook{"post_tool": {{Run: "sleep 30", Timeout: "100ms"}}})
	m.SetLog(func(string) {})
	done := make(chan struct{})
	go func() {
		_ = m.Run(context.Background(), PostTool, Context{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a slow hook stalled the turn past its own timeout")
	}
}

func TestFileArgReadsTheUsualKeys(t *testing.T) {
	for _, k := range []string{"path", "file_path", "file", "filename"} {
		if got := FileArg(map[string]interface{}{k: "x.go"}); got != "x.go" {
			t.Errorf("FileArg(%s) = %q", k, got)
		}
	}
	if got := FileArg(map[string]interface{}{"query": "x"}); got != "" {
		t.Errorf("FileArg on a tool with no path = %q, want empty", got)
	}
	if got := FileArg(map[string]interface{}{"path": "  "}); got != "" {
		t.Errorf("FileArg on a blank path = %q, want empty", got)
	}
}
