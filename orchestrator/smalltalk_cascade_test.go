package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/darkcode/core"
)

// TestSmalltalkCostsNoModelCall pins the cheapest case in the system actually
// being cheap. The answer cache was built to stop trivial repeats from costing
// an LLM call and a greeting is the example always given for it, but no rung
// could ever serve one: rung 1 requires 4 content tokens and rung 3 requires 2,
// so "hi" fell past every rung and reached the model every time.
func TestSmalltalkCostsNoModelCall(t *testing.T) {
	for _, msg := range []string{"hi", "hello", "hey there", "thanks", "ok", "good morning"} {
		t.Run(msg, func(t *testing.T) {
			client := &fakeLLMClient{name: "fake-primary"}
			deps := newTestKernel(t, client)

			out, err := deps.Kernel.Execute(context.Background(), msg)
			if err != nil {
				t.Fatalf("Execute(%q): %v", msg, err)
			}
			if n := client.callCount(); n != 0 {
				t.Errorf("Execute(%q) made %d model call(s), want 0", msg, n)
			}
			if strings.TrimSpace(out) == "" {
				t.Errorf("Execute(%q) returned an empty reply", msg)
			}
		})
	}
}

// TestGreetingCarryingARequestIsNotSmalltalk is the guard on the case above.
// Answering "hi, fix the build" with "Hey" would be far worse than spending a
// model call on it, so the whole message must be smalltalk to qualify.
func TestGreetingCarryingARequestIsNotSmalltalk(t *testing.T) {
	client := &fakeLLMClient{name: "fake-primary", responses: []string{"working on it"}}
	deps := newTestKernel(t, client)

	if _, err := deps.Kernel.Execute(context.Background(), "hi can you fix the failing build"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if client.callCount() == 0 {
		t.Error("a request wearing a greeting was answered as smalltalk, with no model call")
	}
}

// TestAbortedLoopIsRecordedAsFailure is the regression test for the reported
// "I asked it to do something and it answered with a previous error".
//
// The kernel recorded every agentic-loop run with success=true and a nil
// result set, so a run that gave up was written to episodic memory as a short,
// successful, TOOL-FREE answer — exactly the shape the answer cache most wants
// to replay. Nothing then stopped that abort text being served as the answer
// to a later request.
func TestAbortedLoopIsRecordedAsFailure(t *testing.T) {
	deps := newTestKernel(t, &fakeLLMClient{name: "fake-primary"})

	const abort = "The agent got stuck repeatedly calling write_file and stopped to avoid wasting iterations."
	deps.Kernel.recordOutcome("add a dark mode toggle", abort,
		[]*core.SubAgentResult{{Success: false, Output: abort}},
		false, "agentic-loop", 0, "")

	entries := deps.Memory.EpisodicGet()
	if len(entries) == 0 {
		t.Fatal("no episodic entry recorded")
	}
	got := entries[0]
	if got.Outcome != "failure" {
		t.Errorf("Outcome = %q, want %q — an abandoned run is not a success", got.Outcome, "failure")
	}
	if got.Replay != "never" {
		t.Errorf("Replay = %q, want %q — an abort message must never be replayed as an answer",
			got.Replay, "never")
	}
}
