package orchestrator

import (
	"testing"
)

func TestJournalResumesCompletedNodesOnly(t *testing.T) {
	dir := t.TempDir()
	j := NewExecJournal(dir, "build the parser")
	j.Append(ExecEvent{Kind: "run_started", Name: "build the parser"})
	j.Append(ExecEvent{Kind: "node_completed", Node: "T1", Output: "lexer done"})
	j.Append(ExecEvent{Kind: "node_failed", Node: "T2", Error: "compile error"})

	// A fresh journal for the same goal is what a restarted process sees.
	resumed := NewExecJournal(dir, "build the parser")
	if got := resumed.Resumable(); got != 1 {
		t.Fatalf("Resumable = %d, want 1 (only T1 completed)", got)
	}
	if out, ok := resumed.Completed("T1"); !ok || out != "lexer done" {
		t.Errorf("T1 = %q, %v; want the recorded output", out, ok)
	}
	if _, ok := resumed.Completed("T2"); ok {
		t.Error("a failed node must be retried, not replayed")
	}
}

// A node that failed and later succeeded must replay as completed.
func TestJournalLatestOutcomeWins(t *testing.T) {
	dir := t.TempDir()
	j := NewExecJournal(dir, "goal")
	j.Append(ExecEvent{Kind: "node_failed", Node: "T1", Error: "flaky"})
	j.Append(ExecEvent{Kind: "node_completed", Node: "T1", Output: "ok on retry"})

	resumed := NewExecJournal(dir, "goal")
	if out, ok := resumed.Completed("T1"); !ok || out != "ok on retry" {
		t.Errorf("T1 = %q, %v; want the successful retry", out, ok)
	}
}

// Once a run finishes its results were delivered, so the next identical
// request must start clean rather than replaying stale output.
func TestFinishedRunIsNotResumable(t *testing.T) {
	dir := t.TempDir()
	j := NewExecJournal(dir, "goal")
	j.Append(ExecEvent{Kind: "node_completed", Node: "T1", Output: "done"})
	j.Finish()

	if n := NewExecJournal(dir, "goal").Resumable(); n != 0 {
		t.Errorf("Resumable after Finish = %d, want 0", n)
	}
}

func TestJournalsAreScopedPerGoal(t *testing.T) {
	dir := t.TempDir()
	NewExecJournal(dir, "goal A").Append(ExecEvent{Kind: "node_completed", Node: "T1", Output: "a"})

	if n := NewExecJournal(dir, "goal B").Resumable(); n != 0 {
		t.Errorf("a different goal saw %d resumable nodes, want 0", n)
	}
}

func TestEventsPreserveOrder(t *testing.T) {
	dir := t.TempDir()
	j := NewExecJournal(dir, "goal")
	for _, kind := range []string{"run_started", "node_completed", "node_failed"} {
		j.Append(ExecEvent{Kind: kind, Node: kind})
	}
	events := j.Events()
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for i, want := range []string{"run_started", "node_completed", "node_failed"} {
		if events[i].Kind != want {
			t.Errorf("event %d = %q, want %q", i, events[i].Kind, want)
		}
	}
	if events[0].Time.IsZero() {
		t.Error("events must be timestamped for replay")
	}
}

// A nil journal is the "journalling disabled" case and must be inert.
func TestNilJournalIsSafe(t *testing.T) {
	var j *ExecJournal
	j.Append(ExecEvent{Kind: "node_completed"})
	j.Finish()
	if j.Resumable() != 0 || j.Events() != nil {
		t.Error("nil journal should report nothing")
	}
	if _, ok := j.Completed("T1"); ok {
		t.Error("nil journal should have no completed nodes")
	}
	if NewExecJournal("", "goal") != nil {
		t.Error("an empty directory should disable journalling")
	}
}

// Reading a finished run must not destroy it. NewExecJournal deletes the
// journal when the previous run completed, so that the next attempt starts
// clean — correct for executing, and fatal for a replay view that routed
// through the same constructor.
func TestReadingAFinishedRunDoesNotDeleteIt(t *testing.T) {
	dir := t.TempDir()
	goal := "some goal"

	j := NewExecJournal(dir, goal)
	j.Append(ExecEvent{Kind: "run_started", Name: goal})
	j.Append(ExecEvent{Kind: "node_completed", Node: "t1"})
	j.Append(ExecEvent{Kind: "run_finished", Name: goal})

	for i := 0; i < 3; i++ {
		got := ReadRunEvents(dir, goal)
		if len(got) != 3 {
			t.Fatalf("read %d: got %d events, want 3 — the read destroyed the record", i+1, len(got))
		}
	}
	if runs := ListRuns(dir); len(runs) != 1 || runs[0].Status != "finished" {
		t.Errorf("ListRuns = %+v, want one finished run", runs)
	}
}
