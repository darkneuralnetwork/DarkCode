package orchestrator

import "testing"

func TestNeedsClarification(t *testing.T) {
	tests := []struct {
		name     string
		goal     string
		complex  int
		want     bool
	}{
		{name: "vague request", goal: "fix it", complex: 2, want: true},
		{name: "generic improvement", goal: "make it better", complex: 2, want: true},
		{name: "specific implementation", goal: "implement a Go HTTP handler for user registration", complex: 6, want: false},
		{name: "concrete debugging task", goal: "debug the nil pointer panic in the login flow", complex: 7, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsClarification(tt.goal, tt.complex); got != tt.want {
				t.Fatalf("needsClarification(%q, %d) = %v, want %v", tt.goal, tt.complex, got, tt.want)
			}
		})
	}
}
