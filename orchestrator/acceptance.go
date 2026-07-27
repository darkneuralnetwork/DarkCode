package orchestrator

// acceptance.go — verifier-first execution.
//
// The planner already emits acceptance criteria per task. Until they are
// actually run, a task is "done" because a sub-agent said so. Running them
// turns each completed node into a claim with evidence attached: the command
// that was executed, whether it passed, and what it printed.
//
// Criteria that are not machine-checkable are recorded as unverified rather
// than quietly counted as passing — an honest gap is more useful than a green
// tick that means nothing.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/internal/strutil"
	"github.com/darkcode/plan"
	"github.com/darkcode/tools"
)

// acceptanceTimeout bounds one check. Verification that outlives the task it
// verifies is a hang, not a check.
const acceptanceTimeout = 3 * time.Minute

// backticked extracts a command written as `like this` inside a criterion.
var backticked = regexp.MustCompile("`([^`]+)`")

// runnableCommands are the leading words that mark a criterion as something
// the machine can execute rather than prose to be read.
var runnableCommands = []string{
	"go ", "npm ", "npx ", "yarn ", "pnpm ", "pytest", "python ", "python3 ",
	"cargo ", "mvn ", "gradle ", "make ", "bash ", "sh ", "./", "tsc", "eslint",
	"golangci-lint", "gofmt", "ruff", "mypy", "jest", "vitest", "dotnet ",
}

// acceptanceCommand returns the shell command a criterion implies, or "" when
// the criterion is prose.
func acceptanceCommand(criterion string) string {
	if m := backticked.FindStringSubmatch(criterion); m != nil {
		return strings.TrimSpace(m[1])
	}
	trimmed := strings.TrimSpace(criterion)
	lower := strings.ToLower(trimmed)
	for _, prefix := range runnableCommands {
		if strings.HasPrefix(lower, prefix) {
			return trimmed
		}
	}
	return ""
}

// defaultAcceptance is the fallback predicate for a task that shipped without
// one: build and test the project. This is the "test-first when no predicate
// exists" rule — a change with no stated success condition is still expected
// to leave the project compiling and its tests green.
func defaultAcceptance(dir string) string {
	for _, m := range []struct{ marker, cmd string }{
		{"go.mod", "go build ./... && go test ./..."},
		{"package.json", "npm test --silent"},
		{"Cargo.toml", "cargo test"},
		{"pyproject.toml", "pytest -q"},
		{"setup.py", "pytest -q"},
	} {
		if _, err := os.Stat(filepath.Join(dir, m.marker)); err == nil {
			return m.cmd
		}
	}
	return ""
}

// checkAcceptance runs a node's acceptance criteria and attaches the evidence.
// Returns false when a machine-checkable criterion failed.
//
// ran memoises commands already executed for this graph. Most nodes fall back
// to the same default predicate ("build and test the project"), and running a
// full test suite once per node would multiply the cost of verification by the
// plan's size for no extra information.
func (k *Kernel) checkAcceptance(ctx context.Context, node *plan.Node, ran map[string]bool) bool {
	dir := tools.CurrentWorkspace(ctx)
	if dir == "" {
		dir, _ = os.Getwd()
	}

	criteria := node.Acceptance
	if len(criteria) == 0 {
		if def := defaultAcceptance(dir); def != "" {
			criteria = []string{def}
		}
	}
	if len(criteria) == 0 || k.registry == nil {
		return true
	}

	allPassed := true
	for _, criterion := range criteria {
		p := plan.Proof{Criterion: criterion, CheckedAt: time.Now()}
		cmd := acceptanceCommand(criterion)
		if cmd == "" {
			p.Output = "not machine-checkable — recorded as unverified"
			node.Proof = append(node.Proof, p)
			continue
		}
		if ran[cmd] {
			continue // already proven for this graph
		}
		ran[cmd] = true

		runCtx, cancel := context.WithTimeout(ctx, acceptanceTimeout)
		res, err := k.registry.Execute(runCtx, "terminal", map[string]interface{}{
			"command": cmd, "workdir": dir,
		})
		cancel()

		p.Command = cmd
		switch {
		case err != nil:
			p.Output = err.Error()
		case res != nil:
			p.Passed = res.Success
			p.Output = strutil.Truncate(strings.TrimSpace(res.Output+" "+res.Error), 2000)
		}
		if !p.Passed {
			allPassed = false
		}
		node.Proof = append(node.Proof, p)
		k.log("verify", fmt.Sprintf("acceptance [%s] %s", passFail(p.Passed), cmd))
	}
	return allPassed
}

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

// verifyAcceptance runs the acceptance checks for every completed node and
// returns a summary of the evidence, or "" when nothing was checkable.
func (k *Kernel) verifyAcceptance(ctx context.Context, g *plan.Graph) string {
	var lines []string
	checked, failed := 0, 0
	ran := map[string]bool{}
	for _, n := range g.Nodes {
		if n.Status != core.TaskCompleted {
			continue
		}
		k.checkAcceptance(ctx, n, ran)
		for _, p := range n.Proof {
			if p.Command == "" {
				continue // prose criteria are not evidence
			}
			checked++
			mark := "✓"
			if !p.Passed {
				mark = "✗"
				failed++
			}
			lines = append(lines, fmt.Sprintf("- %s `%s` _(%s)_", mark, p.Command, n.ID))
		}
	}
	if checked == 0 {
		return ""
	}
	head := fmt.Sprintf("\n\n**Acceptance checks** — %d run", checked)
	if failed > 0 {
		head += fmt.Sprintf(", %d failing", failed)
	} else {
		head += ", all passing"
	}
	return head + ":\n" + strings.Join(lines, "\n")
}
