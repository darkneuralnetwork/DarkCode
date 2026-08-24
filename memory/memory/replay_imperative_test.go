package memory

import "testing"

// QA audit Finding 3b: ComposeAnswer only declines to answer imperative
// commands (isImperative), never questions — but "read the file go.mod ...
// and tell me the module name" and "list the files ... how many .go files"
// both slipped through as "questions" because "read" and "list" weren't in
// imperativeVerbs, so the composer answered from loosely keyword-matching
// stored facts instead of declining and letting the request reach a model
// or the agentic loop that could actually read/list something.

func TestIsImperativeCatchesReadAndList(t *testing.T) {
	cases := []string{
		"read the file go.mod in this workspace and tell me the module name.",
		"list the files in this directory, then tell me how many .go files there are.",
		"read config.yaml",
		"list all open tasks",
	}
	for _, goal := range cases {
		if !isImperative(goal) {
			t.Errorf("isImperative(%q) = false, want true — this is a command to go do something, not a question", goal)
		}
	}
}

func TestIsImperativeStillLeavesQuestionsAlone(t *testing.T) {
	cases := []string{
		"what is the module name in go.mod?",
		"how many .go files are in this directory?",
		"explain how to read a file in Go",
	}
	for _, goal := range cases {
		if isImperative(goal) {
			t.Errorf("isImperative(%q) = true, want false — this is a genuine question", goal)
		}
	}
}
