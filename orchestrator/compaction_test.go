package orchestrator

import "testing"

// TestShortConversationNeverCompacts is the regression.
//
// The old rule was `len(stm) >= 8 && grown >= 4`, a message count with no
// relationship to context pressure. Ten short turns are a few hundred tokens
// against a 200k window — under 1% full — and it would spend a model call
// summarising them, then again four messages later.
func TestShortConversationNeverCompacts(t *testing.T) {
	const window = 200_000

	// Ten messages, ~40 tokens each. The old count rule fired here.
	if shouldCompact(10, 400, window) {
		t.Error("compacted a 400-token conversation in a 200k window — " +
			"that is 0.2% full, and the call costs more than it saves")
	}
	// Even a hundred short messages are not context pressure.
	if shouldCompact(100, 4_000, window) {
		t.Error("compacted a 4k conversation in a 200k window")
	}
}

func TestCompactsOnlyNearTheLimit(t *testing.T) {
	const window = 200_000
	threshold := compactionThreshold(window) // 170_000 (85%)

	if shouldCompact(50, threshold-1, window) {
		t.Errorf("compacted at %d, below the %d threshold", threshold-1, threshold)
	}
	if !shouldCompact(50, threshold, window) {
		t.Errorf("did not compact at the %d threshold", threshold)
	}
	if !shouldCompact(50, window-100, window) {
		t.Error("did not compact with the window nearly full")
	}
}

// TestThresholdStaysInPublishedRange — 50–90% of the window is where shipping
// harnesses sit. Outside it in either direction is a bug: too low throws away
// information that fits, too high leaves no room to answer.
func TestThresholdStaysInPublishedRange(t *testing.T) {
	for _, window := range []int{4_000, 8_000, 16_000, 32_000, 64_000, 128_000, 200_000, 1_000_000} {
		got := compactionThreshold(window)
		pct := got * 100 / window
		if pct < 50 || pct > 90 {
			t.Errorf("window %d: threshold %d is %d%% of the window, want 50–90%%", window, got, pct)
		}
		if got >= window {
			t.Errorf("window %d: threshold %d leaves no headroom to answer", window, got)
		}
	}
}

// TestSmallWindowDoesNotCompactOnEveryTurn — the reserve exceeds a small local
// model's whole window, so without the floor the threshold goes negative and
// every turn compacts. That is the original defect wearing different clothes.
func TestSmallWindowDoesNotCompactOnEveryTurn(t *testing.T) {
	const window = 8_000
	got := compactionThreshold(window)
	if got <= 0 {
		t.Fatalf("threshold %d for an 8k window — every turn would compact", got)
	}
	if shouldCompact(4, 100, window) {
		t.Error("compacted a 100-token conversation on a small local model")
	}
	if !shouldCompact(4, 7_500, window) {
		t.Error("did not compact a nearly-full small window")
	}
}

// TestUnknownWindowDoesNotCompact — guessing the window and acting on the guess
// is what the message count amounted to. Not knowing means not compacting.
func TestUnknownWindowDoesNotCompact(t *testing.T) {
	if compactionThreshold(0) != 0 {
		t.Error("an unknown window produced a threshold")
	}
	if shouldCompact(50, 1_000_000, 0) {
		t.Error("compacted against an unknown window")
	}
	if shouldCompact(50, 1_000_000, -1) {
		t.Error("compacted against a negative window")
	}
}

func TestTrivialConversationNeverCompacts(t *testing.T) {
	if shouldCompact(1, 1_000_000, 1_000) {
		t.Error("compacted a single message — there is nothing to summarise but the question")
	}
	if shouldCompact(0, 1_000_000, 1_000) {
		t.Error("compacted an empty conversation")
	}
}
