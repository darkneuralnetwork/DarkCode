package memory

// decay.go — letting memory forget what it never uses.
//
// # WHY AGE IS THE WRONG AXIS
//
// EpisodicPrune already existed and drops everything older than a cutoff. That
// is the intuitive rule and it deletes exactly the wrong entries: the fix for a
// bug that recurs every few months is retrieved constantly and is older than
// almost everything, while a run from Tuesday that nobody has needed since is
// young. An age cutoff keeps the second and deletes the first.
//
// What separates them is use. An entry that keeps being retrieved is
// load-bearing whatever its date; one that has never been retrieved has, by the
// only measure available, never helped. So retrieval strengthens an entry and
// disuse weakens it, which is the mechanism the memory literature describes and
// the one worth copying.
//
// # THE CURVE
//
// Retention follows exp(-t/S): strength falls off with time since the entry was
// last touched, over a stability S that grows with every use. One retrieval
// roughly doubles how long an entry lasts, the second triples it, and so on.
// That is the whole model — it is small on purpose, because a forgetting curve
// nobody can predict is a curve nobody will leave switched on.
//
// # WHY ONLY EPISODIC
//
// Episodic memory grows with every task and is the tier that actually runs
// away. Semantic holds durable facts and imported procedure, where "nobody
// asked for it recently" is not evidence it is wrong — a runbook for an annual
// migration would decay to nothing between uses. Procedural carries its own
// success rate already. Extending the curve to those tiers is a separate
// decision with a different argument, and it is not made here.
//
// # WHY THIS CANNOT DELETE SOMETHING IMPORTANT BY ACCIDENT
//
// Three floors, all of them hard: nothing below the cap is ever touched,
// nothing inside the grace period is ever touched however weak, and the newest
// keepFloor entries survive regardless. Consolidation can only ever remove
// entries that are simultaneously over budget, old, and unused.

import (
	"math"
	"sort"
	"time"

	"github.com/darkcode/core"
)

// baseStability is how long an entry that has never been retrieved keeps most
// of its strength. Two weeks: long enough that ordinary work is still there
// when a question comes back to it, short enough that a run nobody ever needed
// stops competing for space within a release cycle.
const baseStability = 14 * 24 * time.Hour

// gracePeriod is an absolute floor on age. Nothing inside it is ever evicted,
// whatever the arithmetic says — an entry written this week has not had a fair
// chance to be retrieved yet, and evicting it would measure how recently the
// user worked rather than whether the memory was useful.
const gracePeriod = 7 * 24 * time.Hour

// keepFloor is the number of most-recent entries kept unconditionally, so a
// misconfigured cap can never empty the store.
const keepFloor = 50

// DefaultEpisodicMax is the entry count consolidation aims for when the config
// names none.
const DefaultEpisodicMax = 2000

// Strength scores how much an episodic entry has earned its place, in (0, 1].
//
// Exported because it is the whole policy: a caller that wants to show the user
// why something was forgotten needs the same number the eviction used.
func Strength(e core.EpisodicEntry, now time.Time) float64 {
	touched := lastTouch(e)
	age := now.Sub(touched)
	if age <= 0 {
		return 1
	}
	// Each retrieval extends how long the entry survives disuse.
	stability := baseStability * time.Duration(1+e.UseCount)
	return math.Exp(-age.Seconds() / stability.Seconds())
}

// lastTouch is when the entry was last retrieved, or written if it never was.
// An entry loaded from a store written before use tracking existed has a zero
// LastUsed and falls back to its timestamp, which is the honest reading: as far
// as anything knows, it has not been used.
func lastTouch(e core.EpisodicEntry) time.Time {
	if e.LastUsed.After(e.Timestamp) {
		return e.LastUsed
	}
	return e.Timestamp
}

// NoteUse records that these entries were retrieved.
//
// Called from the retrieval path, so every recall feeds the curve without any
// caller having to remember to. Unknown ids are ignored: a recall can name a
// semantic key, and this tier only tracks its own.
func (s *System) NoteUse(ids []string) {
	if len(ids) == 0 {
		return
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	touched := 0
	for i := range s.episodic {
		if !want[s.episodic[i].ID] {
			continue
		}
		s.episodic[i].UseCount++
		s.episodic[i].LastUsed = now
		touched++
	}
	if touched > 0 {
		// The writer is debounced, so a read path marking dirty costs a flag
		// rather than a file write. Use counts are also the one thing here it
		// is fine to lose on a crash: they re-accumulate on the next recalls.
		s.episodicWriter.MarkDirty()
	}
}

// Consolidate evicts the weakest episodic entries until the store is under max,
// and reports how many it removed.
//
// max <= 0 uses DefaultEpisodicMax. Returns 0 without touching anything when
// the store is already under budget, which is the normal case and the reason
// this is cheap enough to run at every session boundary.
func (s *System) Consolidate(max int) int {
	if max <= 0 {
		max = DefaultEpisodicMax
	}
	if max < keepFloor {
		max = keepFloor
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.episodic) <= max {
		return 0
	}

	// Rank by strength, weakest first, breaking ties on id so two runs over the
	// same store evict the same entries — retrieval that is not reproducible is
	// what the answer cache stops trusting.
	idx := make([]int, len(s.episodic))
	for i := range idx {
		idx[i] = i
	}
	str := make([]float64, len(s.episodic))
	for i, e := range s.episodic {
		str[i] = Strength(e, now)
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ia, ib := idx[a], idx[b]
		if str[ia] != str[ib] {
			return str[ia] < str[ib]
		}
		return s.episodic[ia].ID < s.episodic[ib].ID
	})

	// The newest keepFloor entries are never candidates, whatever their score.
	protectedByAge := map[int]bool{}
	byAge := append([]int(nil), idx...)
	sort.SliceStable(byAge, func(a, b int) bool {
		return s.episodic[byAge[a]].Timestamp.After(s.episodic[byAge[b]].Timestamp)
	})
	for i := 0; i < keepFloor && i < len(byAge); i++ {
		protectedByAge[byAge[i]] = true
	}

	budget := len(s.episodic) - max
	drop := make(map[int]bool, budget)
	for _, i := range idx {
		if len(drop) >= budget {
			break
		}
		if protectedByAge[i] {
			continue
		}
		if now.Sub(s.episodic[i].Timestamp) < gracePeriod {
			continue // too new to have had a fair chance
		}
		drop[i] = true
	}
	if len(drop) == 0 {
		return 0
	}

	kept := s.episodic[:0]
	for i, e := range s.episodic {
		if !drop[i] {
			kept = append(kept, e)
		}
	}
	removed := len(s.episodic) - len(kept)
	s.episodic = kept
	s.episodicWriter.MarkDirty()
	return removed
}
