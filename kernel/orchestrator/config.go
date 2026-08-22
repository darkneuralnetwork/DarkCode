package orchestrator

import "github.com/darkcode/infra/core"

// Config holds all configuration for the orchestrator kernel.
type Config struct {
	RoutingMode      core.RoutingMode
	UIMode           bool
	MaxConcurrent    int
	ExecutionProfile string
	MaxTurns         int
	SafetyLevel      SafetyLevel
	CompressContext  bool
	// UseCtxEngine turns on memory/ctxengine's Engine.Assemble as the builder
	// for pre-turn conversational history and injections (loop.go's Run,
	// executeChatReadOnly, executeDirectNoTools), replacing the flat
	// boundedChatContext fallback and letting a recall block compete for
	// budget with the conversation instead of always winning it. On by
	// default since Phase 5 of the context-management unification (see the
	// project's context-management plan); kept as a real field rather than
	// removed so it stays an opt-out escape hatch, not a one-way door — a
	// deliberate choice for a system that now touches every dispatch path.
	UseCtxEngine  bool
	ContextLength int

	// No AgenticLoop or MaxLoops here. Whether a request iterates is decided
	// per request by the /loop verb or the Loop chat mode, and the iteration
	// ceiling is loop.DefaultMaxLoops — a backstop against productive-looking
	// spinning, which is not a preference anyone should have to hold an
	// opinion about.

	// UseLocalForAux routes auxiliary calls to the local model when healthy +
	// fitting. (SkipAuxForReadOnly is applied server-side where the amend lives.)
	UseLocalForAux bool

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
		UseCtxEngine:    true,
	}
}
