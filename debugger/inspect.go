package debugger

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Observation is what was true at one breakpoint hit.
type Observation struct {
	File        string     `json:"file"`
	Line        int        `json:"line"`
	Function    string     `json:"function,omitempty"`
	Locals      []Variable `json:"locals,omitempty"`
	Expressions []Variable `json:"expressions,omitempty"`
	Stack       []Frame    `json:"stack,omitempty"`
}

// Report is the result of one inspection run.
type Report struct {
	Target       string        `json:"target"`
	Observations []Observation `json:"observations"`
	Exited       bool          `json:"exited"`
	ExitStatus   int           `json:"exit_status"`
	// Unbound records breakpoints delve refused, with the reason. A silently
	// missing breakpoint would read as "the code never ran", which is a very
	// different and much more alarming conclusion.
	Unbound []string `json:"unbound,omitempty"`
}

// maxHits bounds how many breakpoint hits are collected. A breakpoint inside a
// loop would otherwise run until the context deadline and return more data
// than anyone can read.
const maxHits = 10

// Inspect runs a program or test under the debugger, stops at each requested
// location, and reports what was actually true there.
//
// This is deliberately one call rather than a stateful session the model has
// to drive. The question an agent has is "what is the value of x when this
// runs", and making it issue launch/break/continue/eval/close in the right
// order is four extra chances to get it wrong for no benefit.
func Inspect(ctx context.Context, opts Options, breakpoints []Breakpoint, exprs []string) (*Report, error) {
	if len(breakpoints) == 0 {
		return nil, fmt.Errorf("at least one breakpoint is required")
	}
	// Go is debugged through delve's own API; everything else speaks DAP.
	// Dispatching here keeps one Report shape and one tool surface, so adding
	// a language never changes what the caller does.
	if lang := languageOf(breakpoints[0].File); lang != "go" {
		if lang == "" {
			return nil, fmt.Errorf("no debugger is configured for %s", breakpoints[0].File)
		}
		return inspectDAP(ctx, lang, opts, breakpoints, exprs)
	}
	session, err := Launch(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	target := opts.Dir
	if opts.Test {
		target += " (test)"
	}
	report := &Report{Target: target}

	// Bind every breakpoint before running. A refusal is reported and the run
	// continues — one bad line should not cost the observations from the rest.
	bound := map[string]string{} // "file:line" → function
	for _, bp := range breakpoints {
		set, err := session.Breakpoint(bp.File, bp.Line)
		if err != nil {
			report.Unbound = append(report.Unbound,
				fmt.Sprintf("%s:%d — %v", bp.File, bp.Line, err))
			continue
		}
		bound[fmt.Sprintf("%s:%d", set.File, set.Line)] = set.Function
	}
	if len(bound) == 0 {
		return report, fmt.Errorf("no breakpoint could be set: %s", strings.Join(report.Unbound, "; "))
	}

	for len(report.Observations) < maxHits {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		stop, err := session.Continue()
		if err != nil {
			// The process ending is the normal way a run finishes, not a fault.
			if strings.Contains(err.Error(), "has exited") {
				report.Exited = true
				break
			}
			return report, err
		}
		if stop.Exited {
			report.Exited, report.ExitStatus = true, stop.ExitStatus
			break
		}

		obs := Observation{
			File: stop.File, Line: stop.Line,
			Function: bound[fmt.Sprintf("%s:%d", stop.File, stop.Line)],
		}
		if locals, err := session.Locals(stop.GoroutineID); err == nil {
			obs.Locals = locals
		}
		for _, expr := range exprs {
			v, err := session.Eval(stop.GoroutineID, expr)
			if err != nil {
				// A failed expression is data too: it usually means the name is
				// not in scope here, which the caller needs to know.
				v = Variable{Name: expr, Value: "<" + err.Error() + ">"}
			}
			obs.Expressions = append(obs.Expressions, v)
		}
		if stack, err := session.Stack(stop.GoroutineID, 8); err == nil {
			obs.Stack = stack
		}
		report.Observations = append(report.Observations, obs)
	}
	return report, nil
}

// Format renders a report for a human or a model to read.
func (r *Report) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "debugged %s — %d breakpoint hit(s)", r.Target, len(r.Observations))
	if r.Exited {
		fmt.Fprintf(&b, ", process exited with status %d", r.ExitStatus)
	}
	b.WriteByte('\n')

	for _, u := range r.Unbound {
		fmt.Fprintf(&b, "  ⚠ breakpoint not set at %s\n", u)
	}
	for i, o := range r.Observations {
		fmt.Fprintf(&b, "\n#%d %s:%d", i+1, o.File, o.Line)
		if o.Function != "" {
			fmt.Fprintf(&b, " in %s", o.Function)
		}
		b.WriteByte('\n')
		for _, v := range o.Locals {
			fmt.Fprintf(&b, "    %s = %s  (%s)\n", v.Name, v.Value, v.Type)
		}
		for _, v := range o.Expressions {
			fmt.Fprintf(&b, "    %s → %s\n", v.Name, v.Value)
		}
		if len(o.Stack) > 0 {
			var names []string
			for _, f := range o.Stack {
				names = append(names, f.Function)
			}
			fmt.Fprintf(&b, "    stack: %s\n", strings.Join(names, " ← "))
		}
	}
	if len(r.Observations) == maxHits {
		fmt.Fprintf(&b, "\n(stopped after %d hits — narrow the breakpoint if you need more)\n", maxHits)
	}
	return b.String()
}

// languageOf maps a source file to the debugger family that handles it.
// Kept local rather than reusing the code index's table: that one exists to
// decide what to parse, and the two lists drift for good reasons — TypeScript
// is parsed but debugged as JavaScript.
func languageOf(file string) string {
	switch strings.ToLower(filepath.Ext(file)) {
	case ".go":
		return "go"
	case ".py", ".pyw":
		return "python"
	case ".js", ".mjs", ".cjs", ".ts", ".mts", ".cts":
		return "javascript"
	}
	return ""
}
