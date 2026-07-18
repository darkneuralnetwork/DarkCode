package router

import "testing"

func TestIsGeneralQuestion(t *testing.T) {
	general := []string{
		"what is a goroutine?",
		"why is the sky blue",
		"how does TLS work?",
		"explain the difference between TCP and UDP",
		"who is narendra modi?",
		"tell me about quantum computing",
		"compare REST and gRPC",
		"is Go garbage collected?",
	}
	for _, q := range general {
		if !IsGeneralQuestion(q) {
			t.Errorf("expected %q to be a general question", q)
		}
	}

	tooly := []string{
		"build a todo app",
		"fix the bug in main.go",
		"refactor the auth module",
		"write a function that sorts users",
		"add a login endpoint to the server",
		"run the test suite",
		"read the file config.yaml",
		"update the project README",
		"",
	}
	for _, q := range tooly {
		if IsGeneralQuestion(q) {
			t.Errorf("expected %q NOT to be a general question", q)
		}
	}
}
