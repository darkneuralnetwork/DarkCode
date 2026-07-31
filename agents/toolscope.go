package agents

// toolscope.go — what a sub-agent is allowed to do, decided by its role.
//
// A role used to change two things: the wording of the system prompt and which
// model tier answered. It did not change what the agent could DO. Every
// sub-agent was handed the entire tool registry, so a research agent whose
// whole job is to fetch a page and summarise it held shell-execute and
// file-write the entire time.
//
// That is a prompt-injection path, and a short one. Untrusted text arrives from
// a web page, reaches a model that has a terminal, and the only thing standing
// between "summarise this page" and "the page told me to run something" is the
// model's judgement. A research agent cannot be tricked into deleting files it
// was never given the ability to delete.
//
// SubAgentConfig.Tools was declared for exactly this and never read by
// anything. This is that field being wired up, with a sensible default per role
// so callers get the protection without having to ask for it.
//
// Two layers, deliberately:
//
//   - The agent is only OFFERED the schemas its role permits, so a restricted
//     model is not tempted by a tool it cannot use.
//   - The dispatch context carries the read-only marker, so the registry
//     REFUSES a write even if the model invents a tool name it was never shown.
//
// The first is ergonomics. The second is the actual boundary — a model that
// hallucinates `terminal` into existence still gets turned away.

import (
	"github.com/darkcode/core"
	"github.com/darkcode/llm"
)

// ToolScope is how much of the registry a role may reach.
type ToolScope int

const (
	// ScopeReadOnly observes and never changes anything: read, list, search,
	// fetch. The registry already knows which tools qualify (ToolEntry.ReadOnly).
	ScopeReadOnly ToolScope = iota
	// ScopeFull is the whole registry, including execute and write.
	ScopeFull
)

// scopeForRole is the default authority per role. Roles whose output is an
// opinion, a report or a summary get read-only; roles whose job is to change
// the world get the full set.
//
// Unlisted roles default to ScopeFull, which keeps existing behaviour for
// anything not named here rather than silently disarming it.
var scopeForRole = map[core.AgentRole]ToolScope{
	core.RoleResearch: ScopeReadOnly, // reads pages and docs; must never execute
	core.RoleCritic:   ScopeReadOnly, // reviews work, does not perform it
	core.RoleQA:       ScopeReadOnly, // finds problems, reports them
	core.RoleSecurity: ScopeReadOnly, // analyses risk; the last role that should hold shell
	core.RolePlanner:  ScopeReadOnly, // decomposes a goal, does not execute it
}

// ScopeFor returns the tool authority for a role.
func ScopeFor(role core.AgentRole) ToolScope {
	if s, ok := scopeForRole[role]; ok {
		return s
	}
	return ScopeFull
}

// schemasFor returns the tool schemas an agent may be offered, honouring both
// its role's scope and any explicit allow-list on the config.
//
// An explicit Tools list narrows further but never widens: a caller may say
// "this worker only needs read_file and write_file", and may not say "give this
// research agent a terminal".
func schemasFor(reg core.ToolRegistry, cfg core.SubAgentConfig) []llm.ToolSchema {
	if reg == nil {
		return nil
	}
	var schemas []llm.ToolSchema
	if ScopeFor(cfg.Role) == ScopeReadOnly {
		schemas, _ = reg.LLMSchemasReadOnly().([]llm.ToolSchema)
	} else {
		schemas, _ = reg.LLMSchemas().([]llm.ToolSchema)
	}
	if len(cfg.Tools) == 0 {
		return schemas
	}
	allowed := make(map[string]bool, len(cfg.Tools))
	for _, t := range cfg.Tools {
		allowed[t] = true
	}
	out := schemas[:0:0]
	for _, s := range schemas {
		if allowed[s.Function.Name] {
			out = append(out, s)
		}
	}
	return out
}
