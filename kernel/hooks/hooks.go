// Package hooks runs user-configured commands at named points in a turn.
//
// # WHY THIS EXISTS
//
// Everything the agent learns today is something the kernel decided to write
// down. That is a closed loop: the agent's memory can only ever contain what
// its authors anticipated recording, and a user who wants the agent to notice
// something else has to change Go code and rebuild.
//
// A hook is the escape hatch. `gofmt` after every write, a lint gate that
// refuses a bad edit before it happens, a line appended to a project journal at
// the end of every turn — none of those belong in the binary, and all of them
// are two lines of config.
//
// # WHY SUBPROCESSES AND NOT A PLUGIN API
//
// The plugin host already exists for anything that needs to speak back. A hook
// is the other half: it is for the case where a shell one-liner is the whole
// answer, and where making the user write, build and install a plugin binary
// would be absurd. git made the same split and for the same reason.
//
// # WHY CONTEXT ARRIVES AS ENVIRONMENT AND NOT AS A FORMATTED COMMAND
//
// The obvious design is to build the command string by substituting the tool
// name and file path into a template. That is a command-injection hole with
// extra steps: a repository containing a file named `; rm -rf ~` would execute
// it. Passing context as environment variables and letting the user's own shell
// expand `$DARKCODE_FILE` moves the expansion after parsing, where a filename
// is a value rather than syntax. This is what git hooks do.
//
// # WHY ONLY pre_tool CAN BLOCK
//
// A hook that can fail the work it observes is a hook that turns a broken
// journal script into a broken agent. Only `pre_tool` is a gate, because
// refusing an action before it happens is the entire point of that one point;
// everywhere else a non-zero exit is logged and the turn continues.
package hooks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Point is a named moment in a turn where hooks run.
type Point string

const (
	// SessionStart fires when a fresh session begins — a new chat, a reset.
	SessionStart Point = "session_start"
	// PreTool fires before a tool runs. The only point that can refuse.
	PreTool Point = "pre_tool"
	// PostTool fires after a tool returns, successful or not.
	PostTool Point = "post_tool"
	// PreCompact fires before the context window is compressed, while the
	// messages about to be summarised are still whole.
	PreCompact Point = "pre_compact"
	// TurnEnd fires once per turn, after the answer is final.
	TurnEnd Point = "turn_end"
)

// Points lists every valid point, in the order they occur in a turn. Used to
// reject a misspelled point at load rather than silently never running it —
// a hook that never fires and never complains is the worst of both.
var Points = []Point{SessionStart, PreTool, PostTool, PreCompact, TurnEnd}

// defaultTimeout bounds one hook. A hook is meant to be a one-liner; anything
// that needs longer is a tool, and a hanging hook would stall the turn it is
// attached to.
const defaultTimeout = 30 * time.Second

// maxTimeout caps what a config may ask for, so a typo in a duration cannot
// wedge the agent for an hour.
const maxTimeout = 5 * time.Minute

// outputBudget bounds what a hook's output can contribute to an error message.
const outputBudget = 2000

// Hook is one configured command.
type Hook struct {
	// Match filters by tool name for the tool points. Empty matches every
	// tool. A trailing * globs: "write_*" matches write_file and write_patch.
	// Ignored at points that have no tool.
	Match string `json:"match,omitempty"`
	// Run is the command line, executed by the user's shell so that
	// $DARKCODE_* expansions and pipelines work as written.
	Run string `json:"run"`
	// Timeout overrides the default, e.g. "5s". Capped at maxTimeout.
	Timeout string `json:"timeout,omitempty"`
}

// Context is what a hook is told about the moment it fired. Every field is
// optional; only the ones meaningful at that point are set.
type Context struct {
	Tool    string
	File    string
	Goal    string
	Success bool
	// Detail carries a point-specific note — the compression ratio, the
	// session id — without giving each one its own field.
	Detail string
}

// Manager holds the configured hooks and runs them.
//
// A nil *Manager is valid and does nothing, so every call site can be one
// unconditional line rather than a nil check plus a call.
type Manager struct {
	byPoint map[Point][]Hook
	shell   []string
	log     func(string)
}

// New builds a manager from the configured map, rejecting unknown points and
// hooks with nothing to run.
//
// An empty or nil config yields a nil manager, not an empty one: the callers
// are all on hot paths, and nil is the cheapest possible no-op.
func New(cfg map[string][]Hook) (*Manager, error) {
	if len(cfg) == 0 {
		return nil, nil
	}
	valid := map[Point]bool{}
	for _, p := range Points {
		valid[p] = true
	}

	m := &Manager{byPoint: map[Point][]Hook{}, shell: shell()}
	// Sorted, so a config error is reported the same way every run rather
	// than depending on map order.
	names := make([]string, 0, len(cfg))
	for k := range cfg {
		names = append(names, k)
	}
	sort.Strings(names)

	for _, name := range names {
		p := Point(name)
		if !valid[p] {
			return nil, fmt.Errorf("hooks: unknown point %q (valid: %s)", name, joinPoints())
		}
		for i, h := range cfg[name] {
			if strings.TrimSpace(h.Run) == "" {
				return nil, fmt.Errorf("hooks: %s[%d] has no command to run", name, i)
			}
			if h.Timeout != "" {
				if _, err := time.ParseDuration(h.Timeout); err != nil {
					return nil, fmt.Errorf("hooks: %s[%d] timeout %q: %w", name, i, h.Timeout, err)
				}
			}
			m.byPoint[p] = append(m.byPoint[p], h)
		}
	}
	if len(m.byPoint) == 0 {
		return nil, nil
	}
	return m, nil
}

// SetLog installs a sink for hook failures at the non-blocking points. Without
// one they are silent, which is the wrong default for anything a user wrote.
func (m *Manager) SetLog(f func(string)) {
	if m != nil {
		m.log = f
	}
}

// Configured reports whether any hook is registered for a point. Call sites use
// it to skip building a Context they would only throw away.
func (m *Manager) Configured(p Point) bool {
	return m != nil && len(m.byPoint[p]) > 0
}

// Run executes every hook registered for p.
//
// The returned error is non-nil only when a PreTool hook refused, and the
// caller must treat that as a denial. At every other point the error is always
// nil and failures go to the log — see the package comment.
func (m *Manager) Run(ctx context.Context, p Point, hc Context) error {
	if !m.Configured(p) {
		return nil
	}
	env := m.environ(p, hc)
	for _, h := range m.byPoint[p] {
		if !h.matches(hc.Tool) {
			continue
		}
		out, err := m.exec(ctx, h, env)
		if err == nil {
			continue
		}
		if p == PreTool {
			return fmt.Errorf("blocked by a pre_tool hook (%s): %s", h.Run, firstLine(out, err))
		}
		m.logf("hook %s failed (%s): %s", p, h.Run, firstLine(out, err))
	}
	return nil
}

func (m *Manager) exec(ctx context.Context, h Hook, env []string) (string, error) {
	d := defaultTimeout
	if h.Timeout != "" {
		if parsed, err := time.ParseDuration(h.Timeout); err == nil && parsed > 0 {
			d = min(parsed, maxTimeout)
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	cmd := exec.CommandContext(runCtx, m.shell[0], append(m.shell[1:], h.Run)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// environ builds the hook's environment: the process environment plus the
// DARKCODE_* context. Values are passed as variables and never interpolated
// into the command, which is what keeps a hostile filename inert.
func (m *Manager) environ(p Point, hc Context) []string {
	env := append(os.Environ(),
		"DARKCODE_HOOK="+string(p),
		"DARKCODE_TOOL="+hc.Tool,
		"DARKCODE_FILE="+hc.File,
		"DARKCODE_GOAL="+hc.Goal,
		"DARKCODE_DETAIL="+hc.Detail,
	)
	if p == PostTool {
		env = append(env, "DARKCODE_SUCCESS="+boolEnv(hc.Success))
	}
	return env
}

func (m *Manager) logf(format string, args ...any) {
	if m != nil && m.log != nil {
		m.log(fmt.Sprintf(format, args...))
	}
}

// matches reports whether the hook applies to this tool. A hook with no Match
// applies to everything, including points where there is no tool at all.
func (h Hook) matches(tool string) bool {
	if h.Match == "" {
		return true
	}
	if tool == "" {
		return false // a tool filter cannot match a point that has no tool
	}
	if strings.HasSuffix(h.Match, "*") {
		return strings.HasPrefix(tool, strings.TrimSuffix(h.Match, "*"))
	}
	return h.Match == tool
}

// FileArg pulls the path a tool is acting on out of its arguments, so a hook
// can be told which file without every call site knowing the convention.
// Returns "" when the tool has no path, which is most of them.
func FileArg(args map[string]interface{}) string {
	for _, k := range []string{"path", "file_path", "file", "filename"} {
		if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// shell returns the command that runs a hook line. $SHELL is honoured so a
// user's own syntax works, falling back to sh, which is the one thing every
// POSIX system has.
func shell() []string {
	if s := os.Getenv("SHELL"); s != "" && filepath.IsAbs(s) {
		return []string{s, "-c"}
	}
	return []string{"/bin/sh", "-c"}
}

func joinPoints() string {
	names := make([]string, len(Points))
	for i, p := range Points {
		names[i] = string(p)
	}
	return strings.Join(names, ", ")
}

func boolEnv(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// firstLine renders a hook's failure for a human: the command's own message
// when it produced one, the exec error when it did not.
func firstLine(out string, err error) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return err.Error()
	}
	if len(out) > outputBudget {
		out = out[:outputBudget] + "…"
	}
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		return out[:i] + " (…)"
	}
	return out
}
