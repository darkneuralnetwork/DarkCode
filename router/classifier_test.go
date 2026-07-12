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
