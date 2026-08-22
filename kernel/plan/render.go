package plan

import (
	"fmt"
	"strings"

	"github.com/darkcode/infra/core"
)

// RenderMermaid emits the architecture graph body (```mermaid fence included)
// in the same graph TD / T<n> node-ID dialect the Blueprint tab already
// renders. Node styling reflects live status so an executing graph reads as
// a progress view.
func RenderMermaid(g *Graph) string {
	var sb strings.Builder
	sb.WriteString("```mermaid\ngraph TD\n")
	for _, n := range g.Nodes {
		label := n.Name
		if label == "" {
			label = n.ID
		}
		sb.WriteString(fmt.Sprintf("    %s[%s]\n", n.ID, mermaidLabel(label)))
	}
	for _, n := range g.Nodes {
		for _, dep := range n.Deps {
			sb.WriteString(fmt.Sprintf("    %s --> %s\n", dep, n.ID))
		}
	}
	// Status classes (mermaid classDef) — completed green, running yellow,
	// failed red; pending stays default.
	var done, running, failed []string
	for _, n := range g.Nodes {
		switch n.Status {
		case core.TaskCompleted:
			done = append(done, n.ID)
		case core.TaskRunning:
			running = append(running, n.ID)
		case core.TaskFailed:
			failed = append(failed, n.ID)
		}
	}
	if len(done) > 0 {
		sb.WriteString("    classDef done fill:#9f9,stroke:#393\n")
		sb.WriteString("    class " + strings.Join(done, ",") + " done\n")
	}
	if len(running) > 0 {
		sb.WriteString("    classDef running fill:#ff9,stroke:#993\n")
		sb.WriteString("    class " + strings.Join(running, ",") + " running\n")
	}
	if len(failed) > 0 {
		sb.WriteString("    classDef failed fill:#f99,stroke:#933\n")
		sb.WriteString("    class " + strings.Join(failed, ",") + " failed\n")
	}
	sb.WriteString("```")
	return sb.String()
}

// RenderMarkdown emits the full Implementation Plan document in the same
// section layout generatePlanWorkflow produced, so the Blueprint tab and
// injectProjectContext consume it unchanged.
func RenderMarkdown(g *Graph) string {
	var sb strings.Builder
	sb.WriteString("# Implementation Plan\n\n")
	sb.WriteString("## Goal Description\n\n")
	sb.WriteString(strings.TrimSpace(g.Goal))
	sb.WriteString("\n\n")
	if g.Summary != "" {
		sb.WriteString("## Approach\n\n")
		sb.WriteString(g.Summary)
		sb.WriteString("\n\n")
	}
	sb.WriteString("## Proposed Changes\n\n")
	for _, n := range g.Nodes {
		sb.WriteString(fmt.Sprintf("- **%s — %s** (%s): %s\n", n.ID, n.Name, agentLabel(n.Agent), n.Goal))
		for _, a := range n.Artifacts {
			sb.WriteString(fmt.Sprintf("  - artifact: `%s`\n", a))
		}
	}
	sb.WriteString("\n## Verification Plan\n\n")
	anyAcceptance := false
	for _, n := range g.Nodes {
		for _, a := range n.Acceptance {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", n.ID, a))
			anyAcceptance = true
		}
	}
	if !anyAcceptance {
		sb.WriteString("- Verify each task's output before marking it done.\n")
	}
	sb.WriteString("\n## Architecture\n\n")
	sb.WriteString(RenderMermaid(g))
	sb.WriteString("\n")
	return sb.String()
}

// RenderWorkflow emits the checkbox Task Workflow in the exact dialect
// project.Store.MarkTaskStatus parses: "- [ ]" pending, "- [/]" running,
// "- [x]" done. Failed/cancelled tasks stay unchecked with an annotation.
func RenderWorkflow(g *Graph) string {
	var sb strings.Builder
	sb.WriteString("## Tasks\n\n")
	for _, n := range g.Nodes {
		box := "[ ]"
		note := ""
		switch n.Status {
		case core.TaskRunning:
			box = "[/]"
		case core.TaskCompleted:
			box = "[x]"
		case core.TaskFailed:
			note = " ⚠ failed"
			if n.Error != "" {
				note += ": " + firstLine(n.Error)
			}
		case core.TaskCancelled:
			note = " (blocked by failed dependency)"
		}
		line := n.Goal
		if len(n.Deps) > 0 {
			line += " (after " + strings.Join(n.Deps, ", ") + ")"
		}
		sb.WriteString(fmt.Sprintf("- %s %s: %s%s\n", box, n.ID, line, note))
	}
	return sb.String()
}

// Preview renders the chat-facing plan proposal: execution waves showing
// what runs in parallel, per-task detail, and the approve/revise/reject
// instructions. This is what the user sees when the approval gate holds a
// plan for review.
func Preview(g *Graph) string {
	waves := g.Waves()
	var sb strings.Builder
	depthNote := ""
	if g.Depth != "" {
		depthNote = " · " + g.Depth + " planning"
	}
	if g.Revisions > 0 {
		sb.WriteString(fmt.Sprintf("## Revised Plan (revision %d)\n\n", g.Revisions))
	} else {
		sb.WriteString("## Proposed Plan\n\n")
	}
	sb.WriteString(fmt.Sprintf("**%d task(s) in %d execution wave(s)%s**\n\n", len(g.Nodes), len(waves), depthNote))
	if g.Summary != "" {
		sb.WriteString(g.Summary + "\n\n")
	}
	for i, wave := range waves {
		if len(wave) > 1 {
			sb.WriteString(fmt.Sprintf("**Wave %d** — %d tasks in parallel:\n", i+1, len(wave)))
		} else {
			sb.WriteString(fmt.Sprintf("**Wave %d:**\n", i+1))
		}
		for _, n := range wave {
			deps := ""
			if len(n.Deps) > 0 {
				deps = " _(needs " + strings.Join(n.Deps, ", ") + ")_"
			}
			sb.WriteString(fmt.Sprintf("- `%s` **%s** [%s]: %s%s\n", n.ID, n.Name, agentLabel(n.Agent), n.Goal, deps))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("---\n")
	sb.WriteString("Reply **approve** to execute, **reject** to discard, or describe any changes and I'll revise the plan.")
	return sb.String()
}

// mermaidLabel returns a node label safe for a mermaid `T1[...]` node. The
// Blueprint frontend HTML-escapes the whole plan before mermaid runs, which
// turns double quotes into &quot; and breaks mermaid's parser — so quoted
// labels can't survive. We strip characters that need quoting and emit a bare
// label (mermaid accepts spaces in `[...]`), falling back to a quoted form
// only when nothing printable remains.
func mermaidLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"', '[', ']', '{', '}', '(', ')', '<', '>', '|', ';', '\n', '\r', '#':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "task"
	}
	return out
}

func agentLabel(agent string) string {
	if agent == "" {
		return "worker"
	}
	return agent
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
