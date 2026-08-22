// Package concurrency decides how many things the agent may do at once.
//
// # WHY THIS EXISTS
//
// The execution profile had three values — "parallel", "sequential", "auto" —
// and "auto" did nothing. SetExecutionProfile mapped parallel to 10 and
// sequential to 1 and left auto at whatever the config happened to say, so the
// setting most users leave alone was the one with no behaviour behind it.
//
// The two real settings were not much better, because a fixed number cannot be
// right: 10 parallel workers is correct against a paid cloud key with room to
// spare, and catastrophic against a free tier whose budget is a few dozen
// requests A DAY, where the fan-out converts one slow answer into ten
// rejections. It is equally wrong for a local model, where parallelism is
// bounded by cores and memory bandwidth rather than by anyone's quota — running
// four inferences on four cores does not finish sooner, it finishes later.
//
// So the limit is decided per request from what is actually true at that
// moment: how much independent work exists, whether the model is local or
// remote, whether the provider has recently said no, and what the machine has.
package concurrency

// Signals is what the decision is made from. Every field has a safe zero: an
// unknown signal must never argue for MORE parallelism than is known to be
// safe, so zero values collapse toward sequential.
type Signals struct {
	// ReadyTasks is how many independent units of work could start now. There
	// is never a reason to permit more than this.
	ReadyTasks int

	// ConfiguredMax is the user's explicit ceiling. Non-zero means they chose
	// a number and it wins — an automatic policy that overrides an explicit
	// setting is a bug, not a feature.
	ConfiguredMax int

	// LocalModel is true when the serving model runs on this machine, making
	// the constraint CPU and memory rather than anyone's quota.
	LocalModel bool

	// CPUCores is the usable core count. Zero means unknown.
	CPUCores int

	// Throttled is true when the provider has recently rejected a request for
	// rate reasons and the limiter is still backing off.
	Throttled bool

	// EffectiveRPM is the current requests-per-minute ceiling, 0 = unlimited.
	EffectiveRPM int
}

// Decision is the answer plus the reason, because a limit the user cannot
// explain is one they will override with a fixed number and be wrong again.
type Decision struct {
	Limit  int
	Reason string
}

// maxCloudParallel caps fan-out against a healthy remote provider. Beyond a
// handful the bottleneck stops being our concurrency and starts being theirs,
// and every extra in-flight request is one more to retry when something fails.
const maxCloudParallel = 4

// lowRPMThreshold is the requests-per-minute below which a provider is treated
// as scarce. One request every six seconds cannot usefully be fanned out.
const lowRPMThreshold = 10

// Decide returns how many units of work may run at once.
//
// The rules are ordered by how much they know. An explicit setting beats an
// inference; observed rejection beats a resource estimate; a resource estimate
// beats a default.
func Decide(s Signals) Decision {
	ready := s.ReadyTasks
	if ready <= 1 {
		return Decision{Limit: 1, Reason: "one unit of work"}
	}

	// 1. An explicit ceiling is a decision already made.
	if s.ConfiguredMax > 0 {
		return Decision{Limit: clamp(s.ConfiguredMax, 1, ready), Reason: "configured limit"}
	}

	// 2. The provider has actually said no. Nothing about this machine's
	// capacity is relevant while that is true.
	if s.Throttled {
		return Decision{Limit: 1, Reason: "provider is rate-limiting; running one at a time"}
	}

	// 3. A known-scarce budget. Fanning out into a small per-minute allowance
	// spends it on rejections.
	if s.EffectiveRPM > 0 && s.EffectiveRPM < lowRPMThreshold {
		return Decision{Limit: 1, Reason: "provider allows few requests per minute"}
	}

	// 4. A local model is CPU-bound. Half the cores leaves room for the tools
	// the agent is running alongside inference — a build, a test run, a search
	// — which are competing for the same machine.
	if s.LocalModel {
		if s.CPUCores <= 0 {
			return Decision{Limit: 1, Reason: "local model, unknown core count"}
		}
		limit := clamp(s.CPUCores/2, 1, ready)
		return Decision{Limit: limit, Reason: "local model, bounded by cores"}
	}

	// 5. A remote provider with no observed pressure.
	return Decision{Limit: clamp(maxCloudParallel, 1, ready), Reason: "remote provider with headroom"}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
