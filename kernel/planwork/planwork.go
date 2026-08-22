// Package planwork rewrites a project's implementation plan and task workflow
// to reflect new instructions.
//
// # WHY THIS EXISTS
//
// This feature was implemented twice, once per surface, and the two copies did
// not agree about anything that mattered:
//
//	                    web (server)              console (cli)
//	LLM calls per turn   1, delimiter-split       2, one per document
//	model selection      RouteAux, local-first    a hand-built cloud client
//	metering             through the router       none — bypassed it entirely
//	task IDs             stable, never renumber   unconstrained
//	mermaid graph        preserved, status-styled  not mentioned in the prompt
//
// So the same project, edited from the console instead of the browser, cost
// twice as many calls on a metered tier, spent them off the cost governor's
// books, and came back with renumbered tasks and no architecture graph.
//
// The web implementation was the better one. This package is that logic, with
// the *Server receiver removed so both surfaces can reach it.
package planwork

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/darkcode/infra/core"
	"github.com/darkcode/kernel/modelport"
)

// amendSystemPrompt asks for both documents in ONE response, split by a stable
// delimiter. Two calls to rewrite two halves of one decision produced halves
// that disagreed — the workflow gained a task the plan's graph did not have.
const amendSystemPrompt = "You are an AI software architect. Rewrite a project's plan and workflow to reflect a new instruction. " +
	"Output ONLY raw markdown in TWO sections separated by a line containing exactly ===WORKFLOW===\n" +
	"Section 1 = the updated Implementation Plan: keep the ```mermaid\ngraph TD``` architecture; node IDs T1,T2,... MUST match workflow task IDs; " +
	"reuse existing IDs, add new ones only for new tasks; add an 'Open Questions' section if underspecified.\n" +
	"Section 2 = the updated Task Workflow: \"- [ ] T<n>: ...\" (pending) / \"- [/] T<n>: ...\" (running — mark the task worked on next) / " +
	"\"- [x] T<n>: ...\" (done); keep task IDs stable, never renumber."

// Amend rewrites plan and workflow together to reflect query.
//
// It never fails the caller: on any error, or an empty/unparseable response,
// the documents are returned unchanged. A plan refresh is a display refinement
// on top of work that already succeeded, and losing a plan because a model
// call timed out would be a worse outcome than a stale one.
//
// client and model come from the caller's router — this package deliberately
// does not construct an LLM client, because the copy that did is the reason
// two surfaces disagreed.
func Amend(ctx context.Context, client core.LLMClient, model, query, oldPlan, oldWorkflow string) (plan, workflow string) {
	plan, workflow = oldPlan, oldWorkflow
	if client == nil || strings.TrimSpace(query) == "" {
		return plan, workflow
	}

	temp := 0.2
	// Bound the reply — this plan and workflow rewrite ran with no ceiling. The
	// number comes from the one policy table.
	_, maxTok, _ := modelport.PolicyFor(modelport.PurposePlan)
	user := fmt.Sprintf("Current Implementation Plan:\n%s\n\nCurrent Task Workflow:\n%s\n\nThe user just requested: %s",
		oldPlan, oldWorkflow, query)

	resp, err := client.ChatCompletion(ctx, &core.CompletionRequest{
		Model: model,
		Messages: []core.Message{
			{Role: core.RoleSystem, Content: amendSystemPrompt},
			{Role: core.RoleUser, Content: user},
		},
		Temperature: &temp,
		MaxTokens:   &maxTok,
	})
	if err != nil || resp == nil || len(resp.Choices) == 0 {
		return plan, workflow
	}

	np, nw := Split(strings.TrimSpace(resp.Choices[0].Message.Content))
	if np != "" {
		plan = np
	}
	if nw != "" {
		workflow = nw
	}
	return InjectNodeStatus(plan, workflow), workflow
}

// Split tolerantly separates a combined plan+workflow response, preferring the
// explicit delimiter and falling back to the first checkbox section when the
// model omits it.
func Split(text string) (plan, workflow string) {
	for _, delim := range []string{"===WORKFLOW===", "=== WORKFLOW ===", "==WORKFLOW=="} {
		if i := strings.Index(text, delim); i >= 0 {
			return strings.TrimSpace(text[:i]), strings.TrimSpace(text[i+len(delim):])
		}
	}
	if idx := strings.Index(text, "- [ ]"); idx >= 0 {
		if head := strings.LastIndex(text[:idx], "\n## "); head >= 0 {
			return strings.TrimSpace(text[:head]), strings.TrimSpace(text[head:])
		}
		return strings.TrimSpace(text[:idx]), strings.TrimSpace(text[idx:])
	}
	return strings.TrimSpace(text), ""
}

var (
	workflowTaskLineRe = regexp.MustCompile(`^\s*[-*]\s*\[([ xX/])\]\s*(T\d+)\b`)
	mermaidFenceRe     = regexp.MustCompile("(?s)```mermaid\\n(.*?)```")
)

// TaskStatuses maps each workflow task ID to pending/running/done.
func TaskStatuses(workflow string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(workflow, "\n") {
		m := workflowTaskLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		status := "pending"
		switch m[1] {
		case "x", "X":
			status = "done"
		case "/":
			status = "running"
		}
		out[m[2]] = status
	}
	return out
}

// InjectNodeStatus styles the plan's mermaid fence so the architecture graph
// reflects the workflow's task status. No-op without a fence or ID'd tasks.
func InjectNodeStatus(plan, workflow string) string {
	statuses := TaskStatuses(workflow)
	if len(statuses) == 0 {
		return plan
	}
	return mermaidFenceRe.ReplaceAllStringFunc(plan, func(block string) string {
		m := mermaidFenceRe.FindStringSubmatch(block)
		if m == nil {
			return block
		}
		body := strings.TrimRight(m[1], "\n")
		ids := make([]string, 0, len(statuses))
		for id := range statuses {
			ids = append(ids, id)
		}
		// Sorted so the same plan and workflow always render identically —
		// map order would otherwise churn the document on every rewrite.
		sort.Strings(ids)

		var sb strings.Builder
		sb.WriteString("```mermaid\n")
		sb.WriteString(body)
		sb.WriteString("\n")
		sb.WriteString("classDef done fill:#2ea043,stroke:#1a7f37,color:#fff\n")
		sb.WriteString("classDef running fill:#d29922,stroke:#9e6a03,color:#fff\n")
		sb.WriteString("classDef pending fill:#30363d,stroke:#8b949e,color:#c9d1d9\n")
		for _, id := range ids {
			fmt.Fprintf(&sb, "class %s %s\n", id, statuses[id])
		}
		sb.WriteString("```")
		return sb.String()
	})
}
