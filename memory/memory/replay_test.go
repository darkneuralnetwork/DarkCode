package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/darkcode/infra/core"
)

// TestLoopAbortMessageIsNeverReplayed is the regression test for the reported
// "I asked it to do something and it answered with a previous error".
//
// The agentic-loop path recorded every run with success=true and no tool list,
// so a loop that gave up ("got stuck repeatedly calling …") was written to
// episodic memory as a short, successful, tool-free answer — the single most
// replayable shape there is. The cascade then served that abort text, with no
// LLM call, to the next similar request.
func TestLoopAbortMessageIsNeverReplayed(t *testing.T) {
	sys := newTestSystem(t)
	const abort = "The agent got stuck repeatedly calling write_file and stopped to avoid wasting iterations."
	addEpisodic(t, sys, "add a dark mode toggle to the settings page", "success", abort, nil, time.Minute)

	r := NewHybridRetriever(sys, nil)

	if ra, ok := r.BestRecallAnswer("add a dark mode toggle to the settings page", 0); ok {
		t.Errorf("BestRecallAnswer replayed a loop-abort message as an answer:\n  %s", ra.Output)
	}
	if out, ok := r.ConfidentRecall("add a dark mode toggle to the settings page", 0); ok {
		t.Errorf("ConfidentRecall replayed a loop-abort message as an answer:\n  %s", out)
	}
}

// TestImperativeGoalIsNeverReplayed covers the broader rule the abort case is
// one instance of: a command asks for the world to change, so a past answer to
// it is a claim that work happened, not an answer that stays true. Only
// questions are replayable.
func TestImperativeGoalIsNeverReplayed(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "create the user login form component", "success",
		"Created src/LoginForm.tsx with email and password fields.", nil, time.Minute)

	r := NewHybridRetriever(sys, nil)

	if out, ok := r.ConfidentRecall("create the user login form component", 0); ok {
		t.Errorf("ConfidentRecall replayed a completed command as an answer:\n  %s", out)
	}
	if ra, ok := r.BestRecallAnswer("create the user login form component", 0); ok {
		t.Errorf("BestRecallAnswer replayed a completed command as an answer:\n  %s", ra.Output)
	}
}

// TestStableAnswerStaysReplayable is the other half of the contract: the
// cache exists to stop a repeated, settled explanation from costing an LLM
// call. Tightening admission must not cost that saving.
func TestStableAnswerStaysReplayable(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "what is a goroutine in go", "success",
		"A goroutine is a lightweight thread managed by the Go runtime.", nil, 30*24*time.Hour)

	r := NewHybridRetriever(sys, nil)

	ra, ok := r.BestRecallAnswer("what is a goroutine in go", 0)
	if !ok {
		t.Fatal("a settled definitional answer should still be replayable")
	}
	if !strings.Contains(ra.Output, "lightweight thread") {
		t.Errorf("unexpected replayed answer: %q", ra.Output)
	}
}

// TestWorkspaceDependentAnswerExpires covers answers that were true about the
// project when written and silently stop being true as the project changes.
func TestWorkspaceDependentAnswerExpires(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "how many handlers does the server package register", "success",
		"The server package registers 14 handlers.", nil, 30*24*time.Hour)

	r := NewHybridRetriever(sys, nil)

	if ra, ok := r.BestRecallAnswer("how many handlers does the server package register", 0); ok {
		t.Errorf("replayed a month-old claim about live project state:\n  %s", ra.Output)
	}
}

// TestClassifyReplay pins the classifier itself, since every gate above is
// downstream of it.
func TestClassifyReplay(t *testing.T) {
	tests := []struct {
		name  string
		entry core.EpisodicEntry
		want  string
	}{
		{"loop abort", core.EpisodicEntry{
			TaskGoal: "add dark mode",
			Output:   "The agent got stuck repeatedly calling write_file and stopped to avoid wasting iterations.",
		}, ReplayNever},
		{"max iterations", core.EpisodicEntry{
			TaskGoal: "build the site",
			Output:   "partial work\n\n_(agentic loop reached the max iteration limit)_",
		}, ReplayNever},
		{"imperative", core.EpisodicEntry{
			TaskGoal: "create the login form",
			Output:   "Created LoginForm.tsx.",
		}, ReplayNever},
		{"clarification", core.EpisodicEntry{
			TaskGoal: "what should I do about the parser",
			Output:   "I can help, but your request doesn't name anything to act on.",
		}, ReplayNever},
		{"mutating tool", core.EpisodicEntry{
			TaskGoal:  "what does the config say",
			Output:    "It sets port 8080.",
			ToolsUsed: []string{"write_file"},
		}, ReplayNever},
		{"workspace question", core.EpisodicEntry{
			TaskGoal: "how many handlers does the server package register",
			Output:   "Fourteen.",
		}, ReplayWorkspace},
		{"web-derived", core.EpisodicEntry{
			TaskGoal:  "who is the ceo of anthropic",
			Output:    "Dario Amodei.",
			ToolsUsed: []string{"web_search"},
		}, ReplayVolatile},
		{"definitional", core.EpisodicEntry{
			TaskGoal: "what is a goroutine in go",
			Output:   "A lightweight thread managed by the Go runtime.",
		}, ReplayStable},
		// Signals match whole words, not substrings. "concurrent" contains
		// "current" and "discussion" contains "cu"-nothing — the first of
		// those really did misclassify a permanent definition as a volatile
		// world fact, expiring it after a day.
		{"signal inside a longer word", core.EpisodicEntry{
			TaskGoal: "what is a mutex in concurrent programming",
			Output:   "A lock that lets one thread at a time enter a critical section.",
		}, ReplayStable},
		{"release as a whole word is volatile", core.EpisodicEntry{
			TaskGoal: "what is the latest release of postgres",
			Output:   "Postgres 17.",
		}, ReplayVolatile},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyReplay(tc.entry); got != tc.want {
				t.Errorf("ClassifyReplay = %q, want %q", got, tc.want)
			}
		})
	}
}
