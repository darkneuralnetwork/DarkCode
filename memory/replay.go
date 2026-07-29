package memory

// replay.go — admission control for the answer cache.
//
// The cache exists to stop a settled question from costing an LLM call twice:
// "hi", "what is a goroutine", "explain closures". That is a real saving and
// worth keeping. What it must never do is hand back a past answer to a request
// whose whole point is that the world should change.
//
// The original design decided that at READ time, from textual similarity
// between the new question and the old one. That cannot work, for a reason
// worth stating plainly: similarity measures whether two requests are the same
// request, and says nothing about whether the stored answer is still true. By
// the time a bad entry is being matched, the damage is already in the store.
//
// The failure that motivated this file: the agentic loop recorded every run
// with success=true and an empty tool list, so a run that gave up ("got stuck
// repeatedly calling write_file …") landed in episodic memory as a SHORT,
// SUCCESSFUL, TOOL-FREE answer — which is precisely the shape the cache most
// wants to replay. Asking the agent to do something then returned the previous
// error, forever, because nothing ever expired it.
//
// So admission is decided at WRITE time instead, from three things the kernel
// already knows: what kind of request it was, whether it really succeeded, and
// what the answer depends on. An episode earns the right to be replayed when
// it is created, or it never gets it.

import (
	"strings"
	"time"

	"github.com/darkcode/core"
)

// Replay classes. Stored on the episode so the decision is made once, by the
// code that has the context to make it, rather than re-derived by every reader.
const (
	// ReplayStable is an answer that does not go stale: greetings, and
	// definitional or conceptual explanations that reference neither this
	// project nor the outside world. Served indefinitely — this is the case
	// the cache was built for.
	ReplayStable = "stable"

	// ReplayVolatile is a fact about the outside world. True when written,
	// not guaranteed true later, so it ages out.
	ReplayVolatile = "volatile"

	// ReplayWorkspace is a claim about the user's own code or files. It stops
	// being true the moment they edit anything, which is far more often than
	// any wall-clock TTL can track, so it ages out fastest.
	ReplayWorkspace = "workspace"

	// ReplayNever is everything that must not be served from memory at all:
	// commands, failures, partial work, clarification requests, plan
	// proposals, and anything produced by a run that changed the world.
	ReplayNever = "never"
)

// Replay TTLs. Zero means no expiry.
const (
	replayStableTTL    = 0
	replayVolatileTTL  = 24 * time.Hour
	replayWorkspaceTTL = 30 * time.Minute
)

// ReplayTTL returns how long an entry of the given class stays servable.
// Zero means it never expires.
func ReplayTTL(class string) time.Duration {
	switch class {
	case ReplayStable:
		return replayStableTTL
	case ReplayVolatile:
		return replayVolatileTTL
	case ReplayWorkspace:
		return replayWorkspaceTTL
	default:
		return -1 // never servable
	}
}

// Replayable reports whether an entry may be served as an answer right now.
// Entries written before this field existed carry an empty class; they are
// re-classified on read rather than trusted, because they were written by the
// code that had the bug.
func Replayable(e core.EpisodicEntry, now time.Time) bool {
	if e.Outcome != "success" || strings.TrimSpace(e.Output) == "" {
		return false
	}
	class := e.Replay
	if class == "" {
		class = ClassifyReplay(e)
	}
	ttl := ReplayTTL(class)
	if ttl < 0 {
		return false
	}
	if ttl == 0 {
		return true
	}
	return now.Sub(e.Timestamp) <= ttl
}

// ClassifyReplay decides an episode's replay class. Deterministic and
// LLM-free: this runs on the write path of every task, so it may not cost a
// model call, and a cache whose admission rule is itself a guess is not an
// improvement on no cache.
//
// The order matters. Disqualifiers are checked before anything else, because
// a single one of them is enough regardless of how question-shaped the goal
// looks.
func ClassifyReplay(e core.EpisodicEntry) string {
	goal := strings.ToLower(strings.TrimSpace(e.TaskGoal))
	out := strings.TrimSpace(e.Output)

	if goal == "" || out == "" {
		return ReplayNever
	}

	// 1. The answer is not an answer. A failure report, an abandoned run, a
	// request for clarification or a plan awaiting approval are all things the
	// system says INSTEAD of answering, and replaying one is always wrong.
	if isNonAnswer(out) {
		return ReplayNever
	}

	// 2. The run changed something. Even if the goal reads like a question,
	// an episode that wrote a file or ran a command describes an action that
	// happened once; saying it again does not make it happen again.
	if !answerToolsEligible(e.ToolsUsed) {
		return ReplayNever
	}

	// 3. The request was a command. "create the login form" asks for work,
	// and the honest response to hearing it twice is to do the work twice —
	// never to report the first run's output as though it were fresh.
	if isImperative(goal) {
		return ReplayNever
	}

	// 4. Greetings are replayable forever and are the cheapest possible win.
	if IsSmalltalk(goal) {
		return ReplayStable
	}

	// 5. The answer describes the user's own code. True when written; stale
	// as soon as they touch the file, which no TTL can really track — so the
	// TTL is short and the honest position is that this barely caches at all.
	if referencesWorkspace(goal) {
		return ReplayWorkspace
	}

	// 6. The answer came from the web, or is about the changeable world.
	if len(e.ToolsUsed) > 0 || referencesWorld(goal) {
		return ReplayVolatile
	}

	// 7. What's left is a self-contained explanation — the case the cache was
	// built for.
	return ReplayStable
}

// nonAnswerMarkers are phrases the system emits when it is reporting that it
// could NOT answer. They are matched on the output rather than inferred from a
// status flag because the status flag was exactly what proved unreliable: five
// call sites recorded success=true unconditionally, so the text was the only
// honest signal left. The flag is fixed too, but this stays as the backstop.
var nonAnswerMarkers = []string{
	"got stuck repeatedly calling",
	"agentic loop aborted",
	"agentic loop reached the max iteration limit",
	"stopped to avoid wasting iterations",
	"i can help, but your request doesn't name anything to act on",
	"awaiting approval",
	"awaiting your approval",
	"cost limit reached",
	"no llm is available",
	"plan/workflow generation failed",
	"error:",
	"failed:",
	"traceback (most recent call last)",
}

func isNonAnswer(output string) bool {
	o := strings.ToLower(output)
	for _, m := range nonAnswerMarkers {
		if strings.Contains(o, m) {
			return true
		}
	}
	return false
}

// imperativeVerbs are the sentence-initial verbs that make a request a command
// rather than a question. Kept deliberately close to router.intentActionVerbs
// (the two packages can't share it — memory must not import router) and
// checked only at the START of the goal, so "explain how to build a parser"
// stays a question while "build a parser" does not.
var imperativeVerbs = map[string]bool{
	"add": true, "apply": true, "build": true, "change": true, "clone": true,
	"commit": true, "compile": true, "configure": true, "create": true,
	"debug": true, "delete": true, "deploy": true, "download": true,
	"edit": true, "execute": true, "fix": true, "generate": true,
	"implement": true, "init": true, "install": true, "make": true,
	"migrate": true, "modify": true, "move": true, "open": true,
	"patch": true, "publish": true, "pull": true, "push": true,
	"refactor": true, "remove": true, "rename": true, "replace": true,
	"reset": true, "restore": true, "revert": true, "run": true,
	"scaffold": true, "set": true, "setup": true, "start": true,
	"stop": true, "test": true, "update": true, "upgrade": true,
	"write": true,
}

// politeLeads are dropped before the imperative check so "please fix the
// build" and "can you fix the build" are recognised as the commands they are.
var politeLeads = []string{
	"please ", "pls ", "kindly ", "now ", "then ", "also ",
	"can you ", "could you ", "would you ", "will you ",
	"i want you to ", "i need you to ", "i'd like you to ",
	"lets ", "let's ", "go ahead and ", "help me ",
}

func isImperative(goal string) bool {
	g := goal
	// Strip politeness repeatedly: "please can you fix it".
	for changed := true; changed; {
		changed = false
		for _, lead := range politeLeads {
			if strings.HasPrefix(g, lead) {
				g = strings.TrimSpace(strings.TrimPrefix(g, lead))
				changed = true
			}
		}
	}
	if g == "" {
		return false
	}
	// A trailing question mark makes it a question regardless of the verb:
	// "should I run the tests?" asks, "run the tests" tells.
	if strings.HasSuffix(g, "?") {
		return false
	}
	first := g
	if i := strings.IndexAny(g, " \t"); i > 0 {
		first = g[:i]
	}
	first = strings.Trim(first, ".,:;!")
	return imperativeVerbs[first]
}

// workspaceSignals mark an answer as a claim about the user's own project.
// Deliberately broad: over-classifying here costs one avoidable LLM call,
// while under-classifying serves a confidently stale claim about their code,
// and those two errors are not the same size.
var workspaceSignals = []string{
	"this repo", "the repo", "this project", "the project", "this codebase",
	"the codebase", "this file", "the file", "this function", "the function",
	"this package", "the package", "this module", "the module",
	"this directory", "the directory", "my code", "our code", "the code",
	"the test", "the tests", "the build", "the config", "the server",
	"how many", "list the", "which files", "what files", "where is",
	"currently", "right now",
}

func referencesWorkspace(goal string) bool {
	if strings.Contains(goal, "/") || strings.Contains(goal, "\\") {
		return true
	}
	if hasCodeExtension(goal) {
		return true
	}
	words := wordPad(goal)
	for _, s := range workspaceSignals {
		if phraseIn(words, s) {
			return true
		}
	}
	return false
}

// wordPad normalises a goal for whole-word phrase matching: every non-word
// rune becomes a space, and the result is wrapped in spaces so a phrase can be
// searched as " phrase " without special-casing the string's ends.
//
// Substring matching was not good enough here, and the way it failed is worth
// recording: the world signal "current" matched inside "con(current)
// programming", so "what is a mutex in concurrent programming" — a definition
// that never goes stale — was classified volatile and expired after a day.
func wordPad(s string) string {
	var b strings.Builder
	b.WriteByte(' ')
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '\'':
			b.WriteRune(r) // keep "what's", "how's" intact
		default:
			b.WriteByte(' ')
		}
	}
	b.WriteByte(' ')
	return b.String()
}

// phraseIn reports whether padded (from wordPad) contains phrase as whole
// words. phrase may itself be multi-word.
func phraseIn(padded, phrase string) bool {
	return strings.Contains(padded, " "+strings.TrimSpace(phrase)+" ")
}

var codeExtensions = []string{
	".go", ".js", ".ts", ".tsx", ".jsx", ".py", ".rs", ".java", ".rb", ".php",
	".c", ".h", ".cpp", ".hpp", ".cs", ".swift", ".kt", ".sh", ".sql",
	".json", ".yaml", ".yml", ".toml", ".md", ".html", ".css",
}

func hasCodeExtension(s string) bool {
	for _, ext := range codeExtensions {
		if strings.Contains(s, ext) {
			return true
		}
	}
	return false
}

// worldSignals mark a question whose answer is a fact about the changeable
// outside world rather than a settled concept.
var worldSignals = []string{
	"who is", "who are", "who's", "latest", "newest", "current", "today",
	"this year", "this month", "recent", "news", "price", "version of",
	"release", "released", "weather", "stock", "ceo", "president",
	"prime minister",
}

func referencesWorld(goal string) bool {
	words := wordPad(goal)
	for _, s := range worldSignals {
		if phraseIn(words, s) {
			return true
		}
	}
	return false
}
