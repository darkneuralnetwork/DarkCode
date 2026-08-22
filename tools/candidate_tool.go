package tools

// candidate_tool.go — exposing patch reranking to the agent.
//
// The agent can already write a file. What it cannot do on its own is try
// three different fixes and keep the one that actually works, because trying
// one means overwriting the tree and losing the chance to try the next. This
// tool does that: each candidate is applied in isolation, verified, and rolled
// back, and only the winner is reported.
//
// It deliberately does not apply the winner. Choosing and committing are
// different decisions, and the permission gate belongs on the second one — so
// the agent gets a verdict here and writes it through the normal, gated path.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/darkcode/candidate"
	"github.com/darkcode/memory"
)

// CandidateTool ranks competing patches for the agent.
type CandidateTool struct {
	Workspace string
	KG        *memory.KnowledgeGraph
	Patterns  *memory.PatternLibrary
	// Verify is the project's test command. Without one nothing can be proven
	// and the tool refuses rather than ranking on appearance.
	Verify string
}

// maxCandidates bounds one call. Each candidate costs a full verifier run, so
// this is a wall-clock limit as much as anything.
const maxCandidates = 5

func (t *CandidateTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	fail := func(msg string) *ToolResult {
		return &ToolResult{Name: "rank_patches", Success: false, Error: msg}
	}

	verify := strings.TrimSpace(str(args["verify"]))
	if verify == "" {
		verify = t.Verify
	}
	if verify == "" {
		return fail("no verify command: pass verify, or configure one — " +
			"ranking patches without running them would be guesswork")
	}

	raw, ok := args["candidates"].([]interface{})
	if !ok || len(raw) == 0 {
		return fail("candidates must be a non-empty array of {id, files:{path:content}}")
	}
	if len(raw) > maxCandidates {
		return fail(fmt.Sprintf("at most %d candidates per call — each one runs the verifier", maxCandidates))
	}

	patches := make([]candidate.Patch, 0, len(raw))
	for i, item := range raw {
		obj, ok := item.(map[string]interface{})
		if !ok {
			return fail(fmt.Sprintf("candidate %d is not an object", i+1))
		}
		files := map[string]string{}
		if fm, ok := obj["files"].(map[string]interface{}); ok {
			for path, body := range fm {
				content, ok := body.(string)
				if !ok {
					return fail(fmt.Sprintf("candidate %d: %s content must be a string", i+1, path))
				}
				files[path] = content
			}
		}
		if len(files) == 0 {
			return fail(fmt.Sprintf("candidate %d changes no files", i+1))
		}
		id := str(obj["id"])
		if id == "" {
			id = fmt.Sprintf("candidate-%d", i+1)
		}
		patches = append(patches, candidate.Patch{ID: id, Source: str(obj["source"]), Files: files})
	}

	r := &candidate.Ranker{
		KG:    t.KG,
		Trial: candidate.FileTrial(t.Workspace, verify),
	}
	if t.Patterns != nil {
		r.Patterns = t.Patterns.All()
	}

	scores, err := r.Rank(ctx, patches)
	if err != nil {
		return fail(err.Error())
	}

	body, err := json.MarshalIndent(scores, "", "  ")
	if err != nil {
		return fail(err.Error())
	}
	return &ToolResult{Name: "rank_patches", Success: true,
		Output: candidate.Format(scores) + "\n\n" + string(body)}
}

// RegisterCandidateTool adds the patch-ranking tool to the registry.
func RegisterCandidateTool(r *Registry, workspace string, kg *memory.KnowledgeGraph,
	patterns *memory.PatternLibrary, verify string) {
	t := &CandidateTool{Workspace: workspace, KG: kg, Patterns: patterns, Verify: verify}
	r.Register(&ToolEntry{
		Name: "rank_patches",
		Description: strings.TrimSpace(`
Try several competing fixes and report which one actually works. Each candidate is applied on its
own, the project's verifier is run, and the tree is restored — so nothing is kept and the
candidates cannot contaminate each other. Ranking is by evidence first: a candidate that passes
the verifier beats every candidate that does not, whatever they look like; structural cost (how
much of the repository the change reaches, conventions broken, size) only separates candidates
already equal on evidence. If nothing passes, that is reported as "keep none" rather than as a
winner. Use this when you have more than one plausible fix. It does not write the winner — apply
it yourself once you know which it is.`),
		Parameters: MustParseSchema(`{
			"type": "object",
			"properties": {
				"candidates": {
					"type": "array",
					"description": "The competing fixes, each {id, source, files:{path: full new content}}",
					"items": {
						"type": "object",
						"properties": {
							"id": {"type": "string"},
							"source": {"type": "string", "description": "What produced it, for the report"},
							"files": {"type": "object", "description": "Path to full new file content"}
						},
						"required": ["files"]
					}
				},
				"verify": {"type": "string", "description": "Command deciding pass/fail by exit status (defaults to the project's)"}
			},
			"required": ["candidates"]
		}`),
		Handler:  t.Execute,
		Category: "code",
		// Not read-only: it writes each candidate to the tree before restoring
		// it, and a crash mid-trial would leave those writes behind.
		ReadOnly: false,
	})
}
