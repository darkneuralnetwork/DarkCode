package orchestrator

import "testing"

func TestWorthPersistingSemantic(t *testing.T) {
	// Trivial conversational Q&A: no tools, prose answer, no fix/decision → drop.
	if worthPersistingSemantic("Narendra Modi is the Prime Minister of India.", nil, Reflection{Kind: ReflectionNone}) {
		t.Error("a plain trivia answer with no tools should NOT be persisted to semantic memory")
	}

	// Tools were used → durable work worth keeping.
	if !worthPersistingSemantic("done", []string{"web_search"}, Reflection{Kind: ReflectionNone}) {
		t.Error("an outcome that used tools should be persisted")
	}

	// A fix/decision → durable.
	if !worthPersistingSemantic("changed the retry logic", nil, Reflection{Kind: ReflectionFix}) {
		t.Error("a fix reflection should be persisted")
	}
	if !worthPersistingSemantic("we will use JSON", nil, Reflection{Kind: ReflectionDecision}) {
		t.Error("a decision reflection should be persisted")
	}

	// Output containing code → durable even with no tools.
	code := "Here is a solution:\n```go\nfunc add(a, b int) int { return a + b }\n```"
	if !worthPersistingSemantic(code, nil, Reflection{Kind: ReflectionNone}) {
		t.Error("output containing a code block should be persisted")
	}
}

func TestLooksLikeCode(t *testing.T) {
	yes := []string{
		"```python\nprint(1)\n```",
		"func main() {\n\treturn\n}",
		"const x = () => { return 1; };",
	}
	for _, s := range yes {
		if !looksLikeCode(s) {
			t.Errorf("expected code: %q", s)
		}
	}
	no := []string{
		"The Prime Minister of India is Narendra Modi.",
		"A channel in Go is a communication pipe.",
		"",
	}
	for _, s := range no {
		if looksLikeCode(s) {
			t.Errorf("expected NOT code: %q", s)
		}
	}
}
