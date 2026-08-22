package orchestrator

// finding1_regression_test.go — an in-process, no-network regression test for
// the QA audit's Finding 1: a trivial-classified task that wrote a file
// (destroying an existing, referenced function and breaking the build) used
// to report success anyway, because post-completion verification was gated
// on a complexity score the task never reached. A live model is not needed
// to prove the fix — a scripted fake client that calls write_file the same
// destructive way is deterministic and fast, unlike the real thing.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/infra/security"
	"github.com/darkcode/tools/tools"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestTrivialWriteThatBreaksTheBuildIsReportedAsFailure(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module finding1fixture\n\ngo 1.22\n")
	mustWrite(t, filepath.Join(dir, "main.go"), "package main\n\nimport (\n\t\"fmt\"\n\n\t\"finding1fixture/greet\"\n)\n\nfunc main() { fmt.Println(greet.Greeting(\"\")) }\n")
	if err := os.MkdirAll(filepath.Join(dir, "greet"), 0755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "greet", "greet.go"), "package greet\n\nfunc Greeting(name string) string {\n\tif name == \"\" {\n\t\tname = \"World\"\n\t}\n\treturn \"Hello, \" + name + \"!\"\n}\n")

	// The scripted "model": turn 0 calls write_file and — exactly like the
	// real failure this reproduces — overwrites greet.go with content that
	// drops Greeting entirely, which main.go still calls. Turn 1 (after
	// seeing the tool result) declares victory with no further tool calls.
	brokenContent := "package greet\n\nfunc SomethingElse() string { return \"x\" }\n"
	args, err := json.Marshal(map[string]string{
		"path":    filepath.Join(dir, "greet", "greet.go"),
		"content": brokenContent,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeLLMClient{
		name: "fake-primary",
		toolCallsFunc: func(idx int) []core.ToolCall {
			if idx == 0 {
				return []core.ToolCall{{
					ID:       "call1",
					Type:     "function",
					Function: core.FunctionCall{Name: "write_file", Arguments: string(args)},
				}}
			}
			return nil
		},
		respFunc: func(idx int, req *core.CompletionRequest) string {
			if idx == 0 {
				return ""
			}
			return "Added the requested change."
		},
	}

	deps := newTestKernel(t, client)
	tools.RegisterBuiltinTools(deps.Registry, nil, nil, security.NewSandbox(nil))

	ctx := context.WithValue(context.Background(), core.WorkspaceKey, dir)
	out, err := deps.Kernel.Execute(ctx, "Add a Double function to the greet package")
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	// The destructive write actually happened — confirms the scripted tool
	// call drove the real write_file handler, not a no-op.
	got, readErr := os.ReadFile(filepath.Join(dir, "greet", "greet.go"))
	if readErr != nil {
		t.Fatalf("read fixture: %v", readErr)
	}
	if strings.Contains(string(got), "func Greeting") {
		t.Fatal("test setup bug: greet.go should have been overwritten to drop Greeting")
	}

	if !strings.Contains(out, VerificationIssuesMarker) {
		t.Fatalf("expected the broken build to be caught and reported, got clean output:\n%s", out)
	}
	if !strings.Contains(out, "Greeting") {
		t.Fatalf("expected the build failure to name the missing symbol, got:\n%s", out)
	}
}
