package tools

// selfheal_tool.go — the agent's handle on proof-gated repair.
//
// Two actions, deliberately separate. `issues` reads what the graph says is
// wrong and is free. `stage` takes fixes the agent has already written and
// proven, and puts each on its own branch.
//
// Generating the fix is left to the agent rather than done here. It is the one
// step that needs a model, and routing it through the normal tool loop means
// the fix is written with the same context, the same file access and the same
// permission gate as any other edit — instead of a second, quieter path into
// the repository.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/darkcode/candidate"
	"github.com/darkcode/memory"
	"github.com/darkcode/selfheal"
)

// SelfHealTool surfaces structural repair to the agent.
type SelfHealTool struct {
	Workspace string
	KG        *memory.KnowledgeGraph
	Patterns  *memory.PatternLibrary
	Verify    string
}

func (t *SelfHealTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	fail := func(msg string) *ToolResult {
		return &ToolResult{Name: "self_heal", Success: false, Error: msg}
	}
	h := &selfheal.Healer{
		Workspace: t.Workspace, KG: t.KG, Patterns: t.Patterns, Verify: t.Verify,
	}

	switch action := str(args["action"]); action {
	case "", "issues":
		issues := h.FindIssues()
		limit := 10
		if n, ok := args["limit"].(float64); ok && n > 0 {
			limit = int(n)
		}
		if len(issues) > limit {
			issues = issues[:limit]
		}
		if len(issues) == 0 {
			return &ToolResult{Name: "self_heal", Success: true,
				Output: "nothing structurally repairable found"}
		}
		body, err := json.MarshalIndent(issues, "", "  ")
		if err != nil {
			return fail(err.Error())
		}
		return &ToolResult{Name: "self_heal", Success: true, Output: fmt.Sprintf(
			"%d repairable issue(s). Write a fix for one, prove it with rank_patches, "+
				"then stage it here.\n%s", len(issues), body)}

	case "stage":
		files, ok := args["files"].(map[string]interface{})
		if !ok || len(files) == 0 {
			return fail("stage needs files: {path: full new content}")
		}
		content := map[string]string{}
		for path, body := range files {
			s, ok := body.(string)
			if !ok {
				return fail("content for " + path + " must be a string")
			}
			content[path] = s
		}
		title := strings.TrimSpace(str(args["title"]))
		if title == "" {
			return fail("stage needs a title for the commit")
		}

		verify := strings.TrimSpace(str(args["verify"]))
		if verify == "" {
			verify = t.Verify
		}
		if verify == "" {
			verify = candidate.DefaultVerify(t.Workspace)
		}
		if verify == "" {
			return fail("no verify command, and nothing is staged unproven")
		}

		// The gate, applied here too rather than trusted from the caller: the
		// agent may believe it proved this, but staging is the point where
		// being wrong becomes a commit.
		patch := candidate.Patch{ID: "self-heal", Files: content}
		trial := candidate.FileTrial(t.Workspace, verify)(ctx, patch)
		if !trial.Applied {
			return fail("the fix does not apply: " + trial.Err)
		}
		if !trial.Verified {
			return fail("refusing to stage: `" + verify + "` failed with this change applied.\n" +
				trial.Output)
		}

		issue := selfheal.Issue{
			Kind:    strings.TrimSpace(nonBlankStr(str(args["kind"]), "fix")),
			Subject: strings.TrimSpace(nonBlankStr(str(args["subject"]), title)),
			Detail:  str(args["detail"]),
		}
		fix := selfheal.Fix{Issue: issue, Patch: patch, Title: title, Body: str(args["body"])}
		fix.Branch = str(args["branch"])
		if fix.Branch == "" {
			fix.Branch = selfheal.BranchFor(issue)
		}
		if fix.Body == "" {
			fix.Body = "Verified with `" + verify + "`, which exited zero with this change applied.\n\n" +
				"Proposed automatically from a structural finding. It has been run, not reviewed."
		}

		if err := h.Stage(ctx, fix); err != nil {
			return fail(err.Error())
		}
		return &ToolResult{Name: "self_heal", Success: true, Output: fmt.Sprintf(
			"verified and committed to %s. Nothing was pushed — review it, then push if you want it.",
			fix.Branch)}

	default:
		return fail("unknown action " + action + " (want issues, stage)")
	}
}

func nonBlankStr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// RegisterSelfHealTool adds the structural-repair tool to the registry.
func RegisterSelfHealTool(r *Registry, workspace string, kg *memory.KnowledgeGraph,
	patterns *memory.PatternLibrary, verify string) {
	t := &SelfHealTool{Workspace: workspace, KG: kg, Patterns: patterns, Verify: verify}
	r.Register(&ToolEntry{
		Name: "self_heal",
		Description: strings.TrimSpace(`
Repair structural problems the knowledge graph found, with proof. Two actions: "issues" lists what
is repairable (untested hotspots, files breaking a testing convention the rest of the package
keeps); "stage" takes a fix you have written and, only if the project's verifier passes with it
applied, commits it to its own branch. A fix that fails the verifier is refused, not staged with a
warning. Nothing is ever pushed and no pull request is opened — the branch is left for a person to
review. Architectural findings like import cycles are deliberately absent: those are design
decisions, not mechanical edits.`),
		Parameters: MustParseSchema(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["issues", "stage"], "description": "issues lists what is repairable; stage commits a proven fix"},
				"limit": {"type": "number", "description": "How many issues to list"},
				"files": {"type": "object", "description": "stage: path to full new file content"},
				"title": {"type": "string", "description": "stage: the commit subject"},
				"body": {"type": "string", "description": "stage: the commit body"},
				"branch": {"type": "string", "description": "stage: branch name (defaults to a namespaced one)"},
				"kind": {"type": "string", "description": "stage: what kind of issue this fixes"},
				"subject": {"type": "string", "description": "stage: what it concerns"},
				"detail": {"type": "string", "description": "stage: the finding being addressed"},
				"verify": {"type": "string", "description": "Command deciding pass/fail (defaults to the project's)"}
			}
		}`),
		Handler:  t.Execute,
		Category: "code",
		ReadOnly: false,
	})
}
