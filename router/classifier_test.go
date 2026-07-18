package router

import (
	"testing"
)

func TestClassifier_Classify(t *testing.T) {
	c := NewTaskClassifier()

	tests := []struct {
		name     string
		query    string
		expected TaskType
	}{
		{
			name:     "deterministic rename",
			query:    "rename variable x to y",
			expected: TaskTypeDeterministic,
		},
		{
			name:     "deterministic imports",
			query:    "list imports for file.go",
			expected: TaskTypeDeterministic,
		},
		{
			name:     "full consensus debug",
			query:    "investigate root cause of the memory leak",
			expected: TaskTypeFullConsensus,
		},
		{
			name:     "tiny local error",
			query:    "explain this error to me",
			expected: TaskTypeTinyLocal,
		},
		{
			name:     "medium local review",
			query:    "review this code",
			expected: TaskTypeMediumLocal,
		},
		{
			name:     "medium local coding",
			query:    "write code to sort an array",
			expected: TaskTypeMediumLocalCoding,
		},
		{
			name:     "medium local coding implement",
			query:    "implement a new function for adding users",
			expected: TaskTypeMediumLocalCoding,
		},
		{
			name:     "selective default short",
			query:    "format this file",
			expected: TaskTypeSelective,
		},
		{
			name:     "full consensus long query",
			query:    "This is a very long query. " +
				"It goes on and on and on and on and on and on and on and on. " +
				"It goes on and on and on and on and on and on and on and on. " +
				"It goes on and on and on and on and on and on and on and on. " +
				"It goes on and on and on and on and on and on and on and on. ",
			expected: TaskTypeFullConsensus,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.Classify(tt.query)
			if got != tt.expected {
				t.Errorf("Classify(%q) = %v; want %v", tt.query, got, tt.expected)
			}
		})
	}
}

func TestClassifier_EntryRung(t *testing.T) {
	c := NewTaskClassifier()

	tests := []struct {
		query    string
		expected int
	}{
		// Structural/code navigation → rung 0.
		{"where is HybridRetriever defined?", RungDeterministic},
		{"who calls Route?", RungDeterministic},
		{"which files import the router package", RungDeterministic},
		{"find references to Kernel", RungDeterministic},
		// Relational/history → rung 2.
		{"what tools did we use for auth tasks?", RungGraph},
		{"what is related to retry logic?", RungGraph},
		// Explicit memory intent → rung 1.
		{"have we solved a timeout issue like this?", RungCache},
		{"what did we decide last time about caching?", RungCache},
		// Action intent → rung 4 (never served from cache: side effects).
		{"create a REST endpoint for user signup", RungLLM},
		{"refactor the payment module for testability", RungLLM},
		{"fix the flaky integration test", RungLLM},
		// Explanation questions stay cache-eligible (repeated explains are
		// the answer cache's main win; ConfidentRecall is no-tool-only).
		{"explain the difference between mutex and channel", RungCache},
		// Default → rung 1 (cache check is ~free).
		{"capital of France", RungCache},
	}

	for _, tt := range tests {
		if got := c.EntryRung(tt.query); got != tt.expected {
			t.Errorf("EntryRung(%q) = %d; want %d", tt.query, got, tt.expected)
		}
	}
}
