package memory

import (
	"fmt"
	"testing"
	"time"

	"github.com/darkcode/core"
)

func addAged(t *testing.T, sys *System, id, goal string, age time.Duration, uses int, lastUse time.Duration) {
	t.Helper()
	e := core.EpisodicEntry{
		ID: id, TaskGoal: goal, Summary: goal, Outcome: "success",
		Timestamp: time.Now().Add(-age), UseCount: uses,
	}
	if uses > 0 {
		e.LastUsed = time.Now().Add(-lastUse)
	}
	if err := sys.EpisodicAdd(e); err != nil {
		t.Fatal(err)
	}
}

func entryIDs(entries []core.EpisodicEntry) map[string]bool {
	out := map[string]bool{}
	for _, e := range entries {
		out[e.ID] = true
	}
	return out
}

// TestUseBeatsAge is the whole reason this exists.
//
// EpisodicPrune already dropped everything past a cutoff, which deletes the fix
// for a bug that recurs every few months — retrieved constantly, older than
// almost everything — while keeping last Tuesday's run that nobody has needed
// since. Consolidation must make the opposite choice.
func TestUseBeatsAge(t *testing.T) {
	sys := newTestSystem(t)
	// The valuable old entry: written a year ago, retrieved a lot, recently.
	addAged(t, sys, "old-but-used", "the recurring certificate bug", 365*24*time.Hour, 20, time.Hour)
	// Filler, all older than the grace period and never retrieved.
	for i := 0; i < keepFloor+40; i++ {
		addAged(t, sys, fmt.Sprintf("filler-%03d", i), "routine run", 60*24*time.Hour, 0, 0)
	}

	if removed := sys.Consolidate(keepFloor + 10); removed == 0 {
		t.Fatal("consolidation removed nothing from an over-budget store")
	}
	if !entryIDs(sys.EpisodicGet())["old-but-used"] {
		t.Error("the oldest entry was evicted despite being the most used — this is exactly what an age cutoff gets wrong")
	}
}

// TestStrengthRewardsUse — the curve itself, independent of eviction.
func TestStrengthRewardsUse(t *testing.T) {
	now := time.Now()
	old := time.Now().Add(-30 * 24 * time.Hour)

	never := core.EpisodicEntry{Timestamp: old}
	used := core.EpisodicEntry{Timestamp: old, UseCount: 5, LastUsed: old}

	if Strength(used, now) <= Strength(never, now) {
		t.Errorf("use did not strengthen: used %.4f, never-used %.4f",
			Strength(used, now), Strength(never, now))
	}
	// A recent touch beats an old one at equal use count.
	fresh := core.EpisodicEntry{Timestamp: old, UseCount: 5, LastUsed: now.Add(-time.Hour)}
	if Strength(fresh, now) <= Strength(used, now) {
		t.Error("a recently retrieved entry did not outrank one retrieved long ago")
	}
	// Bounded, and a brand new entry is at full strength.
	if s := Strength(core.EpisodicEntry{Timestamp: now}, now); s != 1 {
		t.Errorf("a new entry scored %.4f, want 1", s)
	}
}

// TestConsolidateIsANoOpUnderBudget — it runs at every session boundary, so the
// common case has to cost nothing and change nothing.
func TestConsolidateIsANoOpUnderBudget(t *testing.T) {
	sys := newTestSystem(t)
	for i := 0; i < 10; i++ {
		addAged(t, sys, fmt.Sprintf("e%d", i), "run", 90*24*time.Hour, 0, 0)
	}
	if removed := sys.Consolidate(100); removed != 0 {
		t.Errorf("removed %d entries from a store under budget", removed)
	}
	if got := len(sys.EpisodicGet()); got != 10 {
		t.Errorf("store has %d entries, want 10 untouched", got)
	}
}

// TestTheGracePeriodIsAbsolute. A new entry has not had a chance to be
// retrieved yet, so evicting it would measure how recently the user worked
// rather than whether the memory was any good.
func TestTheGracePeriodIsAbsolute(t *testing.T) {
	sys := newTestSystem(t)
	for i := 0; i < keepFloor+60; i++ {
		addAged(t, sys, fmt.Sprintf("new-%03d", i), "just happened", time.Hour, 0, 0)
	}
	if removed := sys.Consolidate(keepFloor); removed != 0 {
		t.Errorf("evicted %d entries that were all inside the grace period", removed)
	}
}

// TestTheNewestSurviveAMisconfiguredCap — a floor that cannot be argued below.
func TestTheNewestSurviveAMisconfiguredCap(t *testing.T) {
	sys := newTestSystem(t)
	for i := 0; i < keepFloor+100; i++ {
		addAged(t, sys, fmt.Sprintf("e%03d", i), "run", 200*24*time.Hour, 0, 0)
	}
	sys.Consolidate(1) // clamped to keepFloor
	if got := len(sys.EpisodicGet()); got < keepFloor {
		t.Errorf("store fell to %d entries, below the floor of %d", got, keepFloor)
	}
}

// TestConsolidationIsReproducible. Retrieval that is not reproducible is what
// the answer cache stops trusting, and eviction order feeds retrieval.
func TestConsolidationIsReproducible(t *testing.T) {
	var first []string
	for run := 0; run < 3; run++ {
		sys := newTestSystem(t)
		// Identical strengths on purpose, so only the tie-break decides.
		for i := 0; i < keepFloor+40; i++ {
			addAged(t, sys, fmt.Sprintf("e%03d", i), "run", 90*24*time.Hour, 0, 0)
		}
		sys.Consolidate(keepFloor + 10)
		var got []string
		for _, e := range sys.EpisodicGet() {
			got = append(got, e.ID)
		}
		if run == 0 {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("run %d kept %d entries, run 0 kept %d", run, len(got), len(first))
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("run %d differs at %d: %q vs %q", run, i, got[i], first[i])
			}
		}
	}
}

// TestRecallRecordsUse — the curve has to be fed by retrieval itself, or
// nothing is ever marked used and consolidation degenerates to an age cutoff.
func TestRecallRecordsUse(t *testing.T) {
	sys := newTestSystem(t)
	addEpisodic(t, sys, "implement user authentication with JWT", "success", "done", nil, time.Hour)
	addEpisodic(t, sys, "deploy the app to production", "success", "done", nil, time.Hour)

	r := NewHybridRetriever(sys, nil)
	hits := r.Recall("add JWT authentication", 5)
	if len(hits) == 0 {
		t.Fatal("no hits, so nothing could be credited")
	}

	var credited int
	for _, e := range sys.EpisodicGet() {
		if e.UseCount > 0 {
			credited++
			if e.LastUsed.IsZero() {
				t.Errorf("%s has a use count but no last-used time", e.ID)
			}
		}
	}
	if credited == 0 {
		t.Error("a recall credited nothing — retrieval is not feeding the curve")
	}
	if credited > len(hits) {
		t.Errorf("credited %d entries for %d hits", credited, len(hits))
	}
}

// TestNoteUseIgnoresUnknownIds — a recall can name a semantic key, and this
// tier only tracks its own.
func TestNoteUseIgnoresUnknownIds(t *testing.T) {
	sys := newTestSystem(t)
	addAged(t, sys, "real", "a run", time.Hour, 0, 0)
	sys.NoteUse([]string{"not-an-episodic-id", "real"})

	for _, e := range sys.EpisodicGet() {
		if e.ID == "real" && e.UseCount != 1 {
			t.Errorf("the known id was credited %d times, want 1", e.UseCount)
		}
	}
}

// TestEverySessionObserverRuns.
//
// The first version of OnNewSession replaced the callback instead of appending.
// Consolidation and the session_start hook register separately, so the second
// registration silently cancelled the first — and both call sites looked
// correct in isolation, which is why this needs a test rather than care.
func TestEverySessionObserverRuns(t *testing.T) {
	sys := newTestSystem(t)
	var ran []string
	sys.OnNewSession(func() { ran = append(ran, "first") })
	sys.OnNewSession(func() { ran = append(ran, "second") })
	sys.OnNewSession(nil) // must not panic or displace anything

	sys.StartNewSession()

	if len(ran) != 2 || ran[0] != "first" || ran[1] != "second" {
		t.Errorf("observers ran as %v, want both in registration order", ran)
	}
}
