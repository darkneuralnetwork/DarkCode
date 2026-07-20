package orchestrator

import "github.com/darkcode/core"

// Config holds all configuration for the orchestrator kernel.
type Config struct {
	RoutingMode      core.RoutingMode
	UIMode           bool
	MaxConcurrent    int
	ExecutionProfile string
	MaxTurns         int
	SafetyLevel      SafetyLevel
	CompressContext  bool
	UseCtxEngine     bool
	ContextLength    int

	// AgenticLoop enables the optional ReAct (Sense-Think-Act) execution loop
	// from the looping_tech design. When true, Execute delegates to the loop
	// package instead of the DAG decomposition.
	AgenticLoop bool
	MaxLoops    int

	// UseLocalForAux routes auxiliary calls to the local model when healthy +
	// fitting; PostLoopConsensus gates the redundant post-loop re-synthesis.
	// (SkipAuxForReadOnly is applied server-side where the amend lives.)
	UseLocalForAux    bool
	PostLoopConsensus bool

	// PlanApproval controls the interactive plan gate: "always" pauses every
	// planned (non-trivial) task for user approval, "auto" pauses only
	// deep-depth plans, "never"/"" executes immediately (the zero-value
	// default keeps tests/embedded uses on pre-gate behavior — config.Load
	// defaults real installs to "auto"). See plan_gate.go.
	PlanApproval string

	// PlanDepth overrides the adaptive planning-depth governor: "light"
	// forces single-call planning, "deep" forces decompose+self-review,
	// "auto"/"" lets complexity + project context decide. See deep_planner.go.
	PlanDepth string
}

// SafetyLevel controls how restrictive the safety checks are.
type SafetyLevel int

const (
	SafetyStrict  SafetyLevel = iota // require approval for all tool use
	SafetyNormal                     // require approval for destructive actions only
	SafetyRelaxed                    // auto-approve everything (sandboxed mode)
)

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		RoutingMode:     core.RouteSingle,
		UIMode:          false,
		MaxConcurrent:   3,
		MaxTurns:        10,
		SafetyLevel:     SafetyNormal,
		CompressContext: true,
	}
}
