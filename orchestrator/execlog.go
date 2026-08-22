package orchestrator

// execlog.go — an append-only journal of DAG execution.
//
// Every node start, completion and failure is written to a per-run JSONL file
// as it happens. That buys two things from one mechanism:
//
//   - Resumption. If the process dies mid-run, the completed nodes' outputs
//     are already on disk, so the next attempt at the same goal replays them
//     instead of paying for the same model calls again.
//   - Replay. The journal is a complete ordered record of what ran, in what
//     order, with what result — which is what a post-mortem needs and what a
//     timeline UI renders.
//
// Writes are best-effort: a journal failure degrades resumption, and must
// never fail the run it is describing.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ExecEvent is one entry in the journal.
type ExecEvent struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"` // run_started | node_completed | node_failed | run_finished
	Node   string    `json:"node,omitempty"`
	Name   string    `json:"name,omitempty"`
	Output string    `json:"output,omitempty"`
	Error  string    `json:"error,omitempty"`
}

// ExecJournal records one run's events and can replay a previous attempt.
type ExecJournal struct {
	mu   sync.Mutex
	path string
	// done maps node id → output for nodes a previous attempt completed.
	done map[string]string
}

// NewExecJournal opens the journal for a goal. Runs are keyed by a hash of the
// goal so a retry of the same request finds the previous attempt's progress.
// A nil journal is valid and inert, so callers need not branch.
func NewExecJournal(dir, goal string) *ExecJournal {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	j := &ExecJournal{
		path: journalPath(dir, goal),
		done: map[string]string{},
	}
	j.loadPrevious()
	return j
}

// loadPrevious reads any prior attempt's completed nodes. A run that finished
// is not resumable — its results were already delivered — so the journal is
// reset instead.
func (j *ExecJournal) loadPrevious() {
	f, err := os.Open(j.path)
	if err != nil {
		return
	}
	defer f.Close()

	finished := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var e ExecEvent
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			continue
		}
		switch e.Kind {
		case "node_completed":
			j.done[e.Node] = e.Output
		case "node_failed":
			// A failure is not progress: the node must be retried.
			delete(j.done, e.Node)
		case "run_finished":
			finished = true
		}
	}
	if finished {
		j.done = map[string]string{}
		_ = os.Remove(j.path)
	}
}

// Resumable reports how many nodes a previous attempt already completed.
func (j *ExecJournal) Resumable() int {
	if j == nil {
		return 0
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.done)
}

// Completed returns a previous attempt's output for a node.
func (j *ExecJournal) Completed(nodeID string) (string, bool) {
	if j == nil {
		return "", false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out, ok := j.done[nodeID]
	return out, ok
}

// Append writes one event.
func (j *ExecJournal) Append(e ExecEvent) {
	if j == nil {
		return
	}
	e.Time = time.Now()
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// Events reads the journal back in order, for replay and post-mortem.
func (j *ExecJournal) Events() []ExecEvent {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return readEvents(j.path)
}

// readEvents parses a journal file, skipping lines it cannot read: a run that
// was killed mid-write leaves a partial last line, and losing that one event
// is better than losing the whole history before it.
func readEvents(path string) []ExecEvent {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []ExecEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var e ExecEvent
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// Finish marks the run complete so the next attempt starts clean.
func (j *ExecJournal) Finish() {
	if j == nil {
		return
	}
	j.Append(ExecEvent{Kind: "run_finished"})
	j.mu.Lock()
	defer j.mu.Unlock()
	j.done = map[string]string{}
	_ = os.Remove(j.path)
}

// journalPath is where a goal's journal lives. Shared so a reader and the
// executor cannot disagree about it.
func journalPath(dir, goal string) string {
	sum := sha256.Sum256([]byte(goal))
	return filepath.Join(dir, hex.EncodeToString(sum[:])[:16]+".jsonl")
}

// ReadRunEvents returns a run's events without touching it.
//
// This exists because NewExecJournal is a *resumption* constructor: it reads
// the previous attempt to decide what can be skipped, and deletes the journal
// outright when it sees the run already finished, so the next attempt starts
// clean. That is right for executing and catastrophic for reading — routing a
// post-mortem through it deleted the history as a side effect of displaying
// it, and the record was gone by the time anyone scrolled.
func ReadRunEvents(dir, goal string) []ExecEvent {
	if dir == "" || goal == "" {
		return nil
	}
	return readEvents(journalPath(dir, goal))
}

// RunSummary describes one recorded run, for listing them without reading
// every event of each.
type RunSummary struct {
	// ID is the journal's filename stem: the goal hashed, which is how a run
	// is addressed. The goal itself is not recoverable from it, which is why
	// it is carried alongside.
	ID      string    `json:"id"`
	Goal    string    `json:"goal"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitempty"`
	Events  int       `json:"events"`
	// Status is "running", "finished" or "failed" — what a reader wants
	// before deciding which run to open.
	Status string `json:"status"`
}

// ListRuns summarises every journal in dir, most recent first.
//
// A replay view has to start somewhere, and journals are named by the hash of
// their goal, so the directory listing alone says nothing a person can choose
// from. Each file's first event carries the goal and its last carries the
// outcome, which is enough for an index without reading the whole log.
func ListRuns(dir string) []RunSummary {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var out []RunSummary
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		events := readEvents(filepath.Join(dir, e.Name()))
		if len(events) == 0 {
			continue
		}
		s := RunSummary{
			ID:      strings.TrimSuffix(e.Name(), ".jsonl"),
			Goal:    events[0].Name,
			Started: events[0].Time,
			Events:  len(events),
			Status:  "running",
		}
		last := events[len(events)-1]
		switch last.Kind {
		case "run_finished":
			s.Status, s.Ended = "finished", last.Time
		case "node_failed":
			s.Status, s.Ended = "failed", last.Time
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}
