package orchestrator

// concurrency.go — how many sub-agents run at once, decided per wave.
//
// The execution profile offered "parallel", "sequential" and "auto", and auto
// did nothing: SetExecutionProfile mapped the first two to 10 and 1 and left
// auto at whatever the config already said. Worse, the number never reached
// the thing that uses it — ConcurrentExecutor.SetMaxConcurrent had no caller
// outside tests, despite a comment saying "the kernel calls this from the
// resolved execution profile", so the limit was whatever was passed at
// construction and changing the profile at runtime did nothing at all.
//
// Both halves are fixed here: the limit is computed from live signals before
// every wave, and it is pushed into the executor.

import (
	"runtime"

	"github.com/darkcode/concurrency"
	"github.com/darkcode/core"
)

// pressureReporter is anything that knows how much budget its provider has
// left. Declared here rather than importing llm for the concrete type: the
// orchestrator must not depend on the model layer, and a one-method interface
// is all this needs. llm.RateLimitedClient satisfies it.
type pressureReporter interface {
	Pressure() (effectiveRPM int, throttled bool)
}

// resolveConcurrency decides the parallelism for a wave of readyTasks and
// applies it to the executor. Returns the decision for telemetry.
func (k *Kernel) resolveConcurrency(readyTasks int) concurrency.Decision {
	s := concurrency.Signals{
		ReadyTasks: readyTasks,
		CPUCores:   runtime.NumCPU(),
	}

	// An explicit profile is an explicit decision. "auto" (or empty) is the
	// only value that asks us to work it out.
	k.mu.Lock()
	profile := k.cfg.ExecutionProfile
	configured := k.cfg.MaxConcurrent
	k.mu.Unlock()
	switch profile {
	case "sequential":
		s.ConfiguredMax = 1
	case "parallel":
		s.ConfiguredMax = configured
		if s.ConfiguredMax <= 1 {
			s.ConfiguredMax = 10
		}
	}

	// Ask the model that would actually serve the work what it can take.
	//
	// ForceLocal is the only reliable "this runs here" signal available: a
	// client does not report its provider, and the tier a route lands on is
	// not visible from the returned client. When routing is not pinned local,
	// treating it as remote is the safe direction — it caps at a handful
	// rather than at half the cores.
	if k.router != nil {
		s.LocalModel = k.router.ForceLocal()
		if client, _, err := k.router.Route(core.ModelTierCoding, 0, ""); err == nil && client != nil {
			if rl, ok := client.(pressureReporter); ok {
				s.EffectiveRPM, s.Throttled = rl.Pressure()
			}
		}
	}

	d := concurrency.Decide(s)
	if k.executor != nil {
		k.executor.SetMaxConcurrent(d.Limit)
	}
	return d
}
