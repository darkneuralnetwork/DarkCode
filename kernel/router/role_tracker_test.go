package router

import (
	"path/filepath"
	"testing"
)

// A model has to earn a bad reputation: one failure, or a handful of calls,
// must not sideline it.
func TestUnreliableNeedsEvidence(t *testing.T) {
	rt := NewRoleTracker()

	if rt.Unreliable("critic", "never-seen") {
		t.Error("an unknown model must not be treated as unreliable")
	}

	rt.RecordSuccess("critic", "flaky", false)
	if rt.Unreliable("critic", "flaky") {
		t.Error("a single failure is not enough evidence to demote a model")
	}

	// Sustained failure eventually is.
	for i := 0; i < 40; i++ {
		rt.RecordSuccess("critic", "flaky", false)
	}
	if !rt.Unreliable("critic", "flaky") {
		t.Error("a model that fails consistently should be demoted")
	}

	// A reliable model with the same call volume must not be.
	for i := 0; i < 40; i++ {
		rt.RecordSuccess("critic", "solid", true)
	}
	if rt.Unreliable("critic", "solid") {
		t.Error("a consistently successful model was demoted")
	}
}

// Reliability is per-role: failing as a critic says nothing about coding.
func TestReliabilityIsScopedToRole(t *testing.T) {
	rt := NewRoleTracker()
	for i := 0; i < 40; i++ {
		rt.RecordSuccess("critic", "m1", false)
	}
	if !rt.Unreliable("critic", "m1") {
		t.Fatal("setup failed")
	}
	if rt.Unreliable("worker", "m1") {
		t.Error("a bad record in one role leaked into another")
	}
}

func TestWeightTracksSuccessRate(t *testing.T) {
	rt := NewRoleTracker()
	for i := 0; i < 20; i++ {
		rt.RecordSuccess("qa", "good", true)
		rt.RecordSuccess("qa", "bad", false)
	}
	if rt.GetWeight("qa", "good") <= rt.GetWeight("qa", "bad") {
		t.Errorf("weights not ordered: good=%.3f bad=%.3f",
			rt.GetWeight("qa", "good"), rt.GetWeight("qa", "bad"))
	}
	if rt.GetWeight("qa", "unknown") != 1.0 {
		t.Error("an untracked model should carry the neutral weight")
	}
}

// The point of tracking is that it accumulates; a record that dies with the
// process teaches nothing.
func TestReliabilityPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reliability.json")

	first := NewRoleTracker()
	first.SetPersistPath(path)
	for i := 0; i < 40; i++ {
		first.RecordSuccess("critic", "flaky", false)
	}

	reopened := NewRoleTracker()
	reopened.SetPersistPath(path)
	if !reopened.Unreliable("critic", "flaky") {
		t.Error("the model's track record did not survive a restart")
	}
	if len(reopened.Stats()) != 1 {
		t.Errorf("Stats = %+v, want the one persisted pair", reopened.Stats())
	}
}

// Persistence being unavailable must not break recording.
func TestTrackerWorksWithoutPersistence(t *testing.T) {
	rt := NewRoleTracker()
	rt.RecordSuccess("worker", "m", true)
	if rt.GetWeight("worker", "m") == 0 {
		t.Error("recording failed with no persist path set")
	}
}
