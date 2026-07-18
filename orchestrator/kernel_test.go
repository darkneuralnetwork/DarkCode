package orchestrator

import (
	"testing"

	"github.com/darkcode/core"
)

func TestClassifyGoalIntent(t *testing.T) {
	tests := []struct {
		name string
		goal string
		want goalIntent
	}{
		// Vague cold-start actions — the only class that gates.
		{name: "vague request", goal: "fix it", want: intentVagueAction},
		{name: "generic improvement", goal: "make it better", want: intentVagueAction},
		{name: "vague with filler", goal: "please fix it now", want: intentVagueAction},
		{name: "empty", goal: "", want: intentVagueAction},
		{name: "ultra short", goal: "hey", want: intentVagueAction},

		// Questions — always answerable, regardless of topic.
		{name: "factual question no question mark", goal: "what is the name of usa president", want: intentQuestion},
		{name: "question with mark", goal: "does this repo use modules?", want: intentQuestion},
		{name: "how question", goal: "how does the router pick a model", want: intentQuestion},
		{name: "non-coding topic ends with mark", goal: "tell me a joke, ok?", want: intentQuestion},

		// Concrete actions — default answerable, no keyword whitelist.
		{name: "specific implementation", goal: "implement a Go HTTP handler for user registration", want: intentAction},
		{name: "concrete debugging task", goal: "debug the nil pointer panic in the login flow", want: intentAction},
		{name: "non-coding statement", goal: "summarize the history of the roman empire", want: intentAction},
		{name: "vague phrase inside concrete request", goal: "help me fix the auth bug in login.go", want: intentAction},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Cold start: no STM, no active project.
			if got := classifyGoalIntent(tt.goal, nil, false); got != tt.want {
				t.Fatalf("classifyGoalIntent(%q) = %v, want %v", tt.goal, got, tt.want)
			}
		})
	}
}

func TestClassifyGoalIntent_ActiveConversationIsContinuation(t *testing.T) {
	stm := []core.Message{
		{Role: core.RoleUser, Content: "implement a Go HTTP handler for user registration"},
		{Role: core.RoleAssistant, Content: "done"},
		{Role: core.RoleUser, Content: "continue"},
	}
	// "continue" alone would be vague — a real prior assistant turn makes it
	// a continuation instead, so the gate never fires.
	if got := classifyGoalIntent("continue", stm, false); got != intentContinuation {
		t.Errorf("short follow-up after a real assistant turn = %v, want intentContinuation", got)
	}
}

func TestClassifyGoalIntent_ProjectGuidanceIsContinuation(t *testing.T) {
	if got := classifyGoalIntent("continue", nil, true); got != intentContinuation {
		t.Errorf("active project (plan/workflow available) = %v, want intentContinuation", got)
	}
}

func TestClassifyGoalIntent_ColdStartShortMessageStillVague(t *testing.T) {
	if got := classifyGoalIntent("continue", nil, false); got != intentVagueAction {
		t.Errorf("bare 'continue' with no history and no project = %v, want intentVagueAction", got)
	}
}
