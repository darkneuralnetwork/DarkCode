package loop

// contract.go — what "finished" means, supplied by the caller.
//
// The loop's original stop condition was syntactic: the model emitted no tool
// calls, so it must be done. That is a statement about the model's output
// format, not about the world. The self-evaluation added on top of it asks the
// model whether it thinks the goal is met, which is better but is still an
// opinion — and an opinion from the same model that just decided it was
// finished.
//
// A Contract replaces the opinion with evidence. The planner already emits
// acceptance criteria per task ("go test ./... passes", "src/index.html
// exists"), and those are checkable. When a contract is supplied, the loop
// stops only once Verify reports the criteria actually hold; a failing check is
// fed back as concrete evidence and the loop keeps working.
//
// The contract is optional on purpose. A conversational goal ("explain what a
// mutex is") has nothing machine-checkable about it, and inventing a predicate
// for it would be worse than admitting there isn't one. With no contract the
// loop falls back to the self-evaluation path, which is the right tool for
// exactly that case.

import "context"

// Verdict is the outcome of one contract check.
type Verdict struct {
	// Passed reports whether every checkable criterion held.
	Passed bool

	// Checked is how many criteria were actually machine-checked. Zero means
	// the contract had nothing runnable in it, which is NOT the same as
	// passing — the caller distinguishes "proven" from "nothing to prove".
	Checked int

	// Evidence is what the checks printed: the failing command's output, the
	// missing artifact paths. It is fed back to the model verbatim, because a
	// compiler error is a far better correction signal than "please try again".
	Evidence string
}

// Proven reports whether this verdict is positive evidence of completion, as
// opposed to an absence of evidence.
func (v Verdict) Proven() bool { return v.Passed && v.Checked > 0 }

// Contract is the caller-supplied definition of done for one Run.
type Contract struct {
	// Criteria are the acceptance criteria in human-readable form. They are
	// shown to the model up front so it knows what it will be judged on —
	// telling an agent the test it must pass is not cheating, it is the
	// difference between working toward a target and guessing at one.
	Criteria []string

	// Artifacts are file paths the work is expected to produce. Listed to the
	// model alongside Criteria.
	Artifacts []string

	// Verify runs the criteria and returns what happened. Called at each stop
	// attempt. It must be safe to call repeatedly — the loop re-checks after
	// every correction round.
	//
	// Supplying Criteria without Verify is legal and means "tell the model the
	// target but do not enforce it"; the loop then treats the contract as
	// unverifiable and falls back to self-evaluation.
	Verify func(ctx context.Context) Verdict
}

// Empty reports whether the contract carries nothing worth telling the model
// or checking against.
func (c *Contract) Empty() bool {
	return c == nil || (len(c.Criteria) == 0 && len(c.Artifacts) == 0 && c.Verify == nil)
}

// enforceable reports whether this contract can actually decide completion.
func (c *Contract) enforceable() bool {
	return c != nil && c.Verify != nil
}

// brief renders the contract for the model's system prompt.
func (c *Contract) brief() string {
	if c.Empty() {
		return ""
	}
	var b []byte
	b = append(b, "\nDEFINITION OF DONE — you will be judged against exactly this:\n"...)
	for _, cr := range c.Criteria {
		b = append(b, "  - "...)
		b = append(b, cr...)
		b = append(b, '\n')
	}
	for _, a := range c.Artifacts {
		b = append(b, "  - the file "...)
		b = append(b, a...)
		b = append(b, " exists and has real content\n"...)
	}
	if c.enforceable() {
		b = append(b, "These are checked mechanically after you stop. Stopping before they hold\n"+
			"does not finish the task, it just costs another round — so verify your own\n"+
			"work before you give a final answer.\n"...)
	}
	return string(b)
}
