package permission

// ============================================================================
// PERMISSION GATE
//
// A centralized approval layer that sits in front of tool execution. Before
// the registry dispatches a tool call, it asks the gate whether the action is
// permitted. The gate classifies the call (is it dangerous?), consults its
// session-scoped decision cache, and — if needed — prompts the user via an
// Approver callback.
//
// The user can respond with one of three decisions:
//   • AllowOnce    — permit this single call, ask again next time
//   • AllowSession — permit this tool for the rest of the session
//   • Deny         — refuse (and remember the refusal for the session)
//
// Classification is per-tool and inspects the arguments:
//   - terminal      → dangerous shell commands (rm, sudo, mkfs, git push, …)
//   - write_file    → always (creates/overwrites a file)
//   - patch         → always (modifies a file)
//   - git           → mutating actions (add, commit, stash, …)
//   - monitoring    → env (may expose secrets)
//   - memory        → mutating actions (add, replace, remove)
//
// The gate is safe for concurrent use: approval prompts are serialized so the
// user is never asked two questions at once.
// ============================================================================

import (
	"fmt"
	"github.com/darkcode/internal/strutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/security"
)

// Level controls how aggressive the gate is.
type Level int

const (
	LevelRelaxed Level = iota // auto-approve everything
	LevelNormal               // approve dangerous / mutating actions only
	LevelStrict               // approve every tool call
)

// LevelFromString parses a safety-level name.
func LevelFromString(s string) Level {
	switch strings.ToLower(s) {
	case "strict":
		return LevelStrict
	case "relaxed":
		return LevelRelaxed
	default:
		return LevelNormal
	}
}

// String returns the human-readable name.
func (l Level) String() string {
	switch l {
	case LevelStrict:
		return "strict"
	case LevelRelaxed:
		return "relaxed"
	default:
		return "normal"
	}
}

// Decision is the user's verdict on a permission request.
type Decision int

const (
	DecisionDeny         Decision = iota // not allowed
	DecisionAllowOnce                    // allow this one time
	DecisionAllowSession                 // allow for the rest of the session
)

// String returns a short label for the decision.
func (d Decision) String() string {
	switch d {
	case DecisionAllowOnce:
		return "allow-once"
	case DecisionAllowSession:
		return "allow-session"
	default:
		return "deny"
	}
}

// Verdict is the full result of an approval prompt: the decision plus optional
// free-form feedback the user attached. Feedback lets the user act as a
// mid-execution collaborator — e.g. denying a write_file with "use /tmp
// instead of /var" — and that steer is surfaced back to the agent through the
// tool-result channel so it adapts immediately (in both the ReAct loop and
// the DAG worker path). Empty feedback means "no extra instruction".
type Verdict struct {
	Decision Decision
	Feedback string
}

// AllowV is a convenience constructor for an allow verdict.
func AllowV(d Decision) Verdict { return Verdict{Decision: d} }

// DenyV is a convenience constructor for a deny verdict with optional feedback.
func DenyV(feedback string) Verdict { return Verdict{Decision: DecisionDeny, Feedback: feedback} }

// ApprovalRequest describes an action that needs user approval.
type ApprovalRequest struct {
	Tool      string                 `json:"tool"`
	Summary   string                 `json:"summary"`
	Preview   string                 `json:"preview"`
	Risk      core.RiskLevel         `json:"risk"`
	Args      map[string]interface{} `json:"args"`
	Timestamp time.Time              `json:"timestamp"`
}

// Approver is called when an action needs approval. It must return the
// user's verdict (decision + optional feedback). If no approver is set, the
// gate falls back to its default policy (see Check).
type Approver func(req ApprovalRequest) Verdict

// Gate classifies tool calls and enforces user approval for dangerous ones.
type Gate struct {
	mu       sync.Mutex
	promptMu sync.Mutex // serializes interactive prompts
	level    Level
	approver Approver

	// session-scoped decisions, keyed by tool name.
	allowed map[string]bool
	denied  map[string]bool

	// telemetry counters
	asked       int
	approved    int
	deniedCount int

	// onDecision is an optional hook (e.g. to emit a UI event).
	onDecision func(req ApprovalRequest, d Decision)

	scanner *security.SecretScanner
}

// NewGate creates a permission gate at the given level.
func NewGate(level Level) *Gate {
	return &Gate{
		level:   level,
		allowed: make(map[string]bool),
		denied:  make(map[string]bool),
		scanner: security.NewSecretScanner(),
	}
}

// SetApprover installs the callback used to prompt the user.
func (g *Gate) SetApprover(a Approver) {
	g.mu.Lock()
	g.approver = a
	g.mu.Unlock()
}

// OnDecision installs a hook fired after every decision (approved or denied).
func (g *Gate) OnDecision(fn func(req ApprovalRequest, d Decision)) {
	g.mu.Lock()
	g.onDecision = fn
	g.mu.Unlock()
}

// Level returns the current gate level.
func (g *Gate) Level() Level {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.level
}

// SetLevel changes the gate level at runtime.
func (g *Gate) SetLevel(l Level) {
	g.mu.Lock()
	g.level = l
	g.mu.Unlock()
}

// ResetSession clears session-scoped decisions (e.g. on /new).
func (g *Gate) ResetSession() {
	g.mu.Lock()
	g.allowed = make(map[string]bool)
	g.denied = make(map[string]bool)
	g.mu.Unlock()
}

// Stats returns counters for observability.
type Stats struct {
	Level       Level
	Asked       int
	Approved    int
	Denied      int
	SessionAll  int
	SessionDeny int
}

// Stats returns a snapshot of the gate's counters.
func (g *Gate) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Stats{
		Level:       g.level,
		Asked:       g.asked,
		Approved:    g.approved,
		Denied:      g.deniedCount,
		SessionAll:  len(g.allowed),
		SessionDeny: len(g.denied),
	}
}

// Check decides whether a tool call may proceed. Returns (allowed, request,
// feedback). The feedback string is non-empty only when the user denied the
// call with an explanatory steer; the registry includes it in the tool-result
// so the agent sees the user's instruction and adapts. When the call is
// denied, the caller should skip execution and surface a "permission denied"
// result carrying the feedback.
func (g *Gate) Check(tool string, args map[string]interface{}) (bool, ApprovalRequest, string) {
	g.mu.Lock()
	level := g.level
	// Fast path: relaxed level approves everything.
	if level == LevelRelaxed {
		g.mu.Unlock()
		return true, ApprovalRequest{Tool: tool, Args: args, Timestamp: time.Now()}, ""
	}

	// Session-scoped decisions.
	if g.allowed[tool] {
		g.mu.Unlock()
		return true, ApprovalRequest{Tool: tool, Args: args, Timestamp: time.Now()}, ""
	}
	if g.denied[tool] {
		g.mu.Unlock()
		return false, ApprovalRequest{Tool: tool, Args: args, Timestamp: time.Now()}, ""
	}
	g.mu.Unlock()

	// Classify.
	req, dangerous := classify(tool, args)
	req.Timestamp = time.Now()

	// Secret guard: if the call's arguments carry something that looks like a
	// credential (API key, token, private key, …), force an approval prompt
	// even at Normal level and mark it at least Medium risk. This stops a
	// secret from being silently piped into a tool (e.g. exfiltrated via a
	// web_fetch URL or echoed into a world-readable file) without the user
	// seeing it first. Previously the scanner was instantiated but never used.
	if argsContainSecret(g.scanner, args) {
		dangerous = true
		if req.Risk < core.RiskMedium {
			req.Risk = core.RiskMedium
		}
		req.Summary = "⚠ possible secret — " + req.Summary
	}

	// Normal level: only dangerous actions need approval.
	if level == LevelNormal && !dangerous {
		return true, req, ""
	}
	// (strict level → always prompt; relaxed already returned)

	allowed, feedback := g.ask(req)
	return allowed, req, feedback
}

// ask prompts the user (via the approver) and caches the session decision.
// Returns (allowed, feedback).
func (g *Gate) ask(req ApprovalRequest) (bool, string) {
	g.mu.Lock()
	approver := g.approver
	onDec := g.onDecision
	g.mu.Unlock()

	// No approver available (e.g. server mode without one configured):
	// fall back to a safe default — deny in strict, allow otherwise.
	if approver == nil {
		allowed := g.Level() != LevelStrict
		if onDec != nil {
			d := DecisionDeny
			if allowed {
				d = DecisionAllowOnce
			}
			onDec(req, d)
		}
		return allowed, ""
	}

	// Serialize prompts so the user is never asked two questions at once.
	g.promptMu.Lock()
	defer g.promptMu.Unlock()

	// Re-check the session cache after acquiring the prompt lock: while we
	// were waiting, a concurrent ask() for the same tool may have already
	// recorded a session decision. If so, honour it without re-prompting
	// (this is what makes "allow for session" actually stick when the agent
	// fires several calls to the same tool at once).
	g.mu.Lock()
	if g.allowed[req.Tool] {
		g.mu.Unlock()
		return true, ""
	}
	if g.denied[req.Tool] {
		g.mu.Unlock()
		return false, ""
	}
	g.asked++
	g.mu.Unlock()

	v := approver(req)

	g.mu.Lock()
	switch v.Decision {
	case DecisionAllowSession:
		g.allowed[req.Tool] = true
		g.approved++
	case DecisionAllowOnce:
		g.approved++
	default:
		g.denied[req.Tool] = true
		g.deniedCount++
	}
	g.mu.Unlock()

	if onDec != nil {
		onDec(req, v.Decision)
	}

	return v.Decision != DecisionDeny, v.Feedback
}

// AutoApprover returns an approver that always allows (for the session),
// suitable for non-interactive contexts such as single-query mode.
func AutoApprover() Approver {
	return func(req ApprovalRequest) Verdict { return Verdict{Decision: DecisionAllowSession} }
}

// AutoDeny returns an approver that always denies.
func AutoDeny() Approver {
	return func(req ApprovalRequest) Verdict { return Verdict{Decision: DecisionDeny} }
}

// ============================================================================
// CLASSIFICATION
// ============================================================================

// ClassifyExported is the exported form of classify. It lets callers (such
// as the orchestrator kernel) ask "would this need approval?" without
// prompting or executing anything.
func ClassifyExported(tool string, args map[string]interface{}) (ApprovalRequest, bool) {
	return classify(tool, args)
}

// classify inspects a tool call and returns an ApprovalRequest plus a flag
// indicating whether the call is dangerous (and thus needs approval in
// normal mode).
func classify(tool string, args map[string]interface{}) (ApprovalRequest, bool) {
	req := ApprovalRequest{
		Tool: tool,
		Args: args,
	}
	switch tool {
	case "terminal":
		cmd, _ := args["command"].(string)
		req.Summary = "Run shell command"
		req.Preview = cmd
		risk, dangerous := classifyCommand(cmd)
		req.Risk = risk
		return req, dangerous

	case "write_file":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		exists := fileExists(expand(path))
		if exists {
			req.Summary = "Overwrite existing file"
			req.Risk = core.RiskHigh
		} else {
			req.Summary = "Create new file"
			req.Risk = core.RiskMedium
		}
		req.Preview = fmt.Sprintf("path: %s (%s)\n--- new content (%d bytes) ---\n%s",
			path, existsLabel(exists), len(content), strutil.Truncate(content, 1000))
		return req, true // all writes need approval

	case "patch":
		path, _ := args["path"].(string)
		oldStr, _ := args["old_string"].(string)
		newStr, _ := args["new_string"].(string)
		req.Summary = "Edit file (find & replace)"
		req.Risk = core.RiskMedium
		req.Preview = fmt.Sprintf("path: %s\n--- replace ---\n%s\n--- with ---\n%s",
			path, strutil.Truncate(oldStr, 800), strutil.Truncate(newStr, 800))
		return req, true

	case "git":
		action, _ := args["action"].(string)
		extra, _ := args["args"].(string)
		req.Summary = "git " + action
		req.Risk = core.RiskMedium
		req.Preview = fmt.Sprintf("git %s %s", action, extra)
		return req, IsGitMutating(action)

	case "monitoring":
		action, _ := args["action"].(string)
		req.Summary = "monitoring " + action
		req.Risk = core.RiskLow
		req.Preview = action
		// env can expose secrets
		return req, action == "env"

	case "memory":
		action, _ := args["action"].(string)
		req.Summary = "memory " + action
		req.Risk = core.RiskLow
		req.Preview = action
		return req, isMemoryMutating(action)

	case "pdf":
		// PDF info/extract_text are read-only (not dangerous). merge/split/
		// rotate write a new file → medium risk (needs approval in normal/strict).
		op, _ := args["operation"].(string)
		switch op {
		case "merge", "split", "rotate":
			out, _ := args["output"].(string)
			req.Summary = "PDF " + op + " → " + out
			req.Risk = core.RiskMedium
			req.Preview = "operation: " + op + "\noutput: " + out
			return req, true
		default:
			req.Summary = "PDF " + op
			req.Risk = core.RiskLow
			return req, false
		}

	default:
		// Unknown / read-only tools (read_file, search_files, list_files,
		// web_fetch, web_search, todo) — not dangerous.
		req.Summary = tool
		req.Risk = core.RiskLow
		req.Preview = ""
		return req, false
	}
}

// argsContainSecret reports whether any string value in the tool arguments
// looks like a credential, walking nested maps/slices. A nil scanner (e.g. a
// zero-value Gate in a test) is treated as "no secret" so callers need not
// guard it.
func argsContainSecret(scanner *security.SecretScanner, args map[string]interface{}) bool {
	if scanner == nil {
		return false
	}
	var walk func(v interface{}) bool
	walk = func(v interface{}) bool {
		switch t := v.(type) {
		case string:
			return scanner.HasSecret(t)
		case map[string]interface{}:
			for _, vv := range t {
				if walk(vv) {
					return true
				}
			}
		case []interface{}:
			for _, vv := range t {
				if walk(vv) {
					return true
				}
			}
		}
		return false
	}
	for _, v := range args {
		if walk(v) {
			return true
		}
	}
	return false
}

func IsGitMutating(action string) bool {
	switch action {
	case "add", "commit", "stash", "reset", "rm", "mv", "push", "pull", "merge", "rebase", "cherry-pick":
		return true
	}
	return false
}

func isMemoryMutating(action string) bool {
	switch action {
	case "add", "replace", "remove":
		return true
	}
	return false
}

// commandMatchesAny reports whether cmd matches any of the patterns using
// the same word-boundary / multi-word rules as commandHas. It factors the
// repeated `for _, p := range X { if commandHas(c, p) ... }` scan that
// classifyCommand performs over each pattern list.
func commandMatchesAny(cmd string, patterns []string) bool {
	for _, p := range patterns {
		if commandHas(cmd, p) {
			return true
		}
	}
	return false
}

// classifyCommand scores a shell command in a single pass, returning both
// its risk level (for display/logging) and whether it requires approval. It
// supersedes the previous separate commandRisk + isDangerousCommand pair,
// which each normalized and scanned the command independently — double work
// on every terminal tool call.
//
// Behavior is identical to the old pair:
//   - critical/high/medium matches map to RiskCritical/High/Medium and are
//     approval-requiring;
//   - package-manager operations (pip/npm/apt/… install|uninstall) require
//     approval but stay RiskLow, since they don't belong to the severity
//     taxonomy — they mutate the system, which is an approval concern, not a
//     severity concern;
//   - everything else is RiskLow and auto-approved.
func classifyCommand(cmd string) (risk core.RiskLevel, dangerous bool) {
	c := strings.ToLower(strings.TrimSpace(cmd))
	if c == "" {
		return core.RiskLow, false
	}
	// Output redirection to a file (>, >>) is a file-writing operation. Without
	// this check, an agent whose write_file call was denied could simply use
	// `echo x > file` via the terminal tool to bypass the permission gate.
	// We treat any `>` / `>>` that writes to a file (i.e. not a `>&` FD
	// redirect like 2>&1) as dangerous so it prompts in normal/strict safety.
	if hasFileRedirection(c) {
		return core.RiskMedium, true
	}
	switch {
	case commandMatchesAny(c, criticalCommands):
		return core.RiskCritical, true
	case commandMatchesAny(c, highCommands):
		return core.RiskHigh, true
	case commandMatchesAny(c, mediumCommands):
		return core.RiskMedium, true
	case commandMatchesAny(c, packageInstalls):
		return core.RiskLow, true
	}
	return core.RiskLow, false
}

// hasFileRedirection reports whether the command redirects stdout/stderr to
// a file (>, >>) rather than to another file descriptor (>& such as 2>&1).
// Used to catch file writes performed via the terminal tool (e.g.
// `echo x > file`) that would otherwise bypass the write_file permission gate.
func hasFileRedirection(c string) bool {
	for i := 0; i < len(c); i++ {
		if c[i] != '>' {
			continue
		}
		j := i + 1
		if j < len(c) && c[j] == '>' { // append (>>)
			j++
		}
		for j < len(c) && (c[j] == ' ' || c[j] == '\t') { // skip spaces
			j++
		}
		if j >= len(c) { // trailing '>' with no target — ignore
			continue
		}
		if c[j] == '&' { // FD redirect (2>&1) — not a file write
			continue
		}
		return true
	}
	return false
}

// commandHas checks whether pattern p appears as a command word (or
// multi-word command) in c, rather than as a substring of another word.
// p may contain spaces (e.g. "git push").
func commandHas(c, p string) bool {
	if p == "" {
		return false
	}
	// Multi-word patterns: simple substring on the normalized command.
	if strings.Contains(p, " ") {
		return strings.Contains(c, p)
	}
	// Single-word patterns: match as a whole word.
	for _, tok := range strings.FieldsFunc(c, isDelim) {
		if tok == p {
			return true
		}
	}
	return false
}

func isDelim(r rune) bool {
	switch r {
	case ' ', '\t', '\n', ';', '|', '&', '(', ')', '`':
		return true
	}
	return false
}

var criticalCommands = []string{
	"rm", "rmdir", "sudo", "chmod", "chown", "mkfs", "dd",
	"shutdown", "reboot", "halt", "poweroff", "format", "truncate",
	// Note: a literal "rm-rf" token was previously listed here but never
	// matched — commands like `rm -rf /` tokenize to ["rm", "-rf", "/"], so
	// the whole-word matcher would never see "rm-rf" as a token. "rm" itself
	// already matches and classifies such commands as RiskCritical.
}

var highCommands = []string{
	"git push", "git commit", "git reset", "git clean", "git rm",
	"git checkout", "git rebase", "git merge", "git cherry-pick", "git stash",
	"kill", "pkill", "killall", "systemctl", "launchctl", "crontab",
	"mv", "scp", "rsync", "wget",
}

var mediumCommands = []string{
	"curl", "tar", "unzip", "zip", "gzip", "gunzip",
	"docker", "kubectl", "helm", "vagrant",
	"export", "source", "eval",
	"tee",
}

// packageInstalls are package-manager install/remove operations.
var packageInstalls = []string{
	"pip install", "pip uninstall", "pip3 install", "pip3 uninstall",
	"npm install", "npm uninstall", "npm rm",
	"yarn add", "yarn remove",
	"apt install", "apt remove", "apt-get install", "apt-get remove",
	"dnf install", "dnf remove", "yum install", "yum remove",
	"brew install", "brew uninstall",
	"cargo install",
	"go install",
}

// ---- small helpers ----

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func expand(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func existsLabel(exists bool) string {
	if exists {
		return "exists"
	}
	return "new"
}
