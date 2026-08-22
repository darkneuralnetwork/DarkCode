package concurrency

import "testing"

// TestExplicitSettingAlwaysWins — an automatic policy that overrides a number
// the user typed is a bug. It may still be capped by the work available.
func TestExplicitSettingAlwaysWins(t *testing.T) {
	got := Decide(Signals{ReadyTasks: 8, ConfiguredMax: 2, Throttled: true, LocalModel: true, CPUCores: 64})
	if got.Limit != 2 {
		t.Errorf("limit = %d, want the configured 2 regardless of every other signal", got.Limit)
	}
}

// TestThrottledProviderGoesSequential is the free-tier case, and the reason
// this package exists. A budget of a few dozen requests per day does not go
// faster with fan-out; it turns one slow answer into several rejections.
func TestThrottledProviderGoesSequential(t *testing.T) {
	got := Decide(Signals{ReadyTasks: 10, Throttled: true, CPUCores: 32})
	if got.Limit != 1 {
		t.Errorf("limit = %d while the provider is rejecting requests, want 1", got.Limit)
	}
}

func TestScarceBudgetGoesSequential(t *testing.T) {
	got := Decide(Signals{ReadyTasks: 10, EffectiveRPM: 3, CPUCores: 32})
	if got.Limit != 1 {
		t.Errorf("limit = %d against 3 requests/minute, want 1", got.Limit)
	}
	// A generous cap is not scarcity.
	if got := Decide(Signals{ReadyTasks: 10, EffectiveRPM: 600}); got.Limit <= 1 {
		t.Errorf("limit = %d against 600 requests/minute, want fan-out", got.Limit)
	}
}

// TestLocalModelIsBoundedByCores — parallel inference on a machine that cannot
// run it finishes later, not sooner, and starves the tools running alongside.
func TestLocalModelIsBoundedByCores(t *testing.T) {
	cases := []struct{ cores, ready, want int }{
		{cores: 8, ready: 10, want: 4},
		{cores: 2, ready: 10, want: 1},
		{cores: 1, ready: 10, want: 1},
		{cores: 16, ready: 3, want: 3}, // never more than the work available
	}
	for _, c := range cases {
		got := Decide(Signals{ReadyTasks: c.ready, LocalModel: true, CPUCores: c.cores})
		if got.Limit != c.want {
			t.Errorf("%d cores, %d ready: limit = %d, want %d", c.cores, c.ready, got.Limit, c.want)
		}
	}
}

// TestUnknownSignalsCollapseToSequential — an unknown must never argue for
// more parallelism than is known to be safe.
func TestUnknownSignalsCollapseToSequential(t *testing.T) {
	if got := Decide(Signals{ReadyTasks: 10, LocalModel: true, CPUCores: 0}); got.Limit != 1 {
		t.Errorf("limit = %d with a local model and unknown cores, want 1", got.Limit)
	}
	if got := Decide(Signals{}); got.Limit != 1 {
		t.Errorf("limit = %d for a zero Signals, want 1", got.Limit)
	}
}

func TestRemoteProviderWithHeadroomFansOut(t *testing.T) {
	got := Decide(Signals{ReadyTasks: 10})
	if got.Limit != maxCloudParallel {
		t.Errorf("limit = %d against a healthy remote provider, want %d", got.Limit, maxCloudParallel)
	}
}

// TestNeverExceedsAvailableWork — permitting more workers than tasks spawns
// goroutines with nothing to do.
func TestNeverExceedsAvailableWork(t *testing.T) {
	for ready := 1; ready <= 6; ready++ {
		for _, s := range []Signals{
			{ReadyTasks: ready},
			{ReadyTasks: ready, LocalModel: true, CPUCores: 64},
			{ReadyTasks: ready, ConfiguredMax: 99},
		} {
			if got := Decide(s); got.Limit > ready {
				t.Errorf("%+v: limit %d exceeds the %d tasks available", s, got.Limit, ready)
			}
		}
	}
}

// TestLimitIsAlwaysAtLeastOne — a zero would stall the executor forever.
func TestLimitIsAlwaysAtLeastOne(t *testing.T) {
	for _, s := range []Signals{
		{ReadyTasks: 5, ConfiguredMax: -3},
		{ReadyTasks: -1},
		{ReadyTasks: 5, LocalModel: true, CPUCores: -8},
		{ReadyTasks: 5, EffectiveRPM: -1},
	} {
		if got := Decide(s); got.Limit < 1 {
			t.Errorf("%+v produced limit %d", s, got.Limit)
		}
	}
}

// TestEveryDecisionExplainsItself — a limit the user cannot explain is one
// they will override with a fixed number and be wrong again.
func TestEveryDecisionExplainsItself(t *testing.T) {
	for _, s := range []Signals{
		{ReadyTasks: 1},
		{ReadyTasks: 8, ConfiguredMax: 2},
		{ReadyTasks: 8, Throttled: true},
		{ReadyTasks: 8, EffectiveRPM: 2},
		{ReadyTasks: 8, LocalModel: true, CPUCores: 8},
		{ReadyTasks: 8, LocalModel: true},
		{ReadyTasks: 8},
	} {
		if got := Decide(s); got.Reason == "" {
			t.Errorf("%+v produced no reason", s)
		}
	}
}
