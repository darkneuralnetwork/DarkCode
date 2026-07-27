package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/darkcode/core"
	"github.com/darkcode/memory"
)

// GraphTool exposes the code knowledge graph as a queryable agent tool.
//
// The graph already knows what every file defines, what it imports, and how
// confident it is in each of those facts. Without a query surface the agent
// can only reach it through generic recall, which means re-reading files to
// answer questions the graph could answer for free. This is the difference
// between owning an index and using one.
type GraphTool struct {
	KG        *memory.KnowledgeGraph
	Workspace string
	// Daemon supplies the health time series behind the trends and alerts
	// actions. Nil when the daemon is switched off, which those actions
	// report rather than treating as an error.
	Daemon *memory.HealthDaemon
	// Patterns carries conventions mined from other repositories, so a rule
	// kept elsewhere can be checked here. Nil limits checking to this one.
	Patterns *memory.PatternLibrary
}

// Execute dispatches one graph query.
func (t *GraphTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	if t.KG == nil {
		return &ToolResult{Name: "graph_query", Success: false, Error: "knowledge graph unavailable"}
	}
	action, _ := args["action"].(string)
	query, _ := args["query"].(string)
	limit := 25
	if n, ok := args["limit"].(float64); ok && n > 0 {
		limit = int(n)
	}

	var (
		result   interface{}
		headline string
	)
	switch action {
	case "search":
		matches := t.KG.Search(query, core.KGNodeType(str(args["type"])), limit)
		result, headline = matches, fmt.Sprintf("%d node(s) matching %q", len(matches), query)

	case "neighbors":
		if query == "" {
			return &ToolResult{Name: "graph_query", Success: false, Error: "query must be a node id for the neighbors action"}
		}
		matches := t.KG.Neighbors(query)
		result, headline = matches, fmt.Sprintf("%d neighbour(s) of %s", len(matches), query)

	case "subgraph":
		depth := 2
		if d, ok := args["depth"].(float64); ok && d > 0 {
			depth = int(d)
		}
		nodes := t.KG.GetSubgraph(query, depth)
		matches := make([]memory.Match, 0, len(nodes))
		for _, n := range nodes {
			matches = append(matches, memory.Match{
				ID: n.ID, Label: n.Label, Type: string(n.Type),
				Provenance: n.Provenance, Confidence: n.Confidence,
			})
		}
		result, headline = matches, fmt.Sprintf("%d node(s) within %d hop(s) of %s", len(matches), depth, query)

	case "low_confidence":
		threshold := 0.5
		if v, ok := args["threshold"].(float64); ok && v > 0 {
			threshold = v
		}
		matches := t.KG.LowConfidence(threshold, limit)
		result, headline = matches, fmt.Sprintf("%d belief(s) below confidence %.2f", len(matches), threshold)

	case "stale":
		matches := t.KG.StaleFiles(t.Workspace)
		result, headline = matches, fmt.Sprintf("%d file(s) indexed at an older commit than HEAD", len(matches))

	case "blast_radius":
		files := stringList(args["files"])
		if len(files) == 0 && query != "" {
			files = []string{query}
		}
		if len(files) == 0 {
			return &ToolResult{Name: "graph_query", Success: false, Error: "blast_radius needs files (or query as a single path)"}
		}
		depth := 2
		if d, ok := args["depth"].(float64); ok && d > 0 {
			depth = int(d)
		}
		imp := t.KG.BlastRadius(files, depth)
		result = imp
		headline = fmt.Sprintf("changing %d file(s) can reach %d other file(s) — severity %.0f%%",
			len(files), len(imp.Affected), imp.Severity*100)

	case "health":
		rep := t.KG.Health()
		result, headline = rep, fmt.Sprintf("health %.0f/100 across %d files, %d symbols, %d finding(s)",
			rep.Score, rep.Files, rep.Symbols, len(rep.Findings))

	case "dead_code":
		f := t.KG.DeadSymbols()
		result, headline = f, fmt.Sprintf("%d symbol(s) with no in-repo references", len(f))

	case "cycles":
		f := t.KG.Cycles()
		result, headline = f, fmt.Sprintf("%d import cycle(s)", len(f))

	case "untested":
		f := t.KG.UntestedHotspots(limit)
		result, headline = f, fmt.Sprintf("%d high-fan-in symbol(s) with no test references", len(f))

	case "defect_risk", "root_cause":
		days := 365
		if d, ok := args["days"].(float64); ok && d > 0 {
			days = int(d)
		}
		hist, err := memory.MineDefectHistory(t.Workspace, days)
		if err != nil {
			return &ToolResult{Name: "graph_query", Success: false, Error: "reading git history: " + err.Error()}
		}
		if action == "defect_risk" {
			risks := t.KG.DefectRisk(hist, limit)
			result = risks
			headline = fmt.Sprintf("%d defect-prone file(s) from %d fix commit(s) in the last %d days",
				len(risks), hist.TotalFixes, days)
			break
		}
		failing := stringList(args["files"])
		if len(failing) == 0 && query != "" {
			failing = []string{query}
		}
		if len(failing) == 0 {
			return &ToolResult{Name: "graph_query", Success: false,
				Error: "root_cause needs the failing file(s) in files (or query as a single path)"}
		}
		causes := t.KG.RankRootCauses(hist, failing, limit)
		result = causes
		headline = fmt.Sprintf("%d root-cause candidate(s) for a failure in %s", len(causes), strings.Join(failing, ", "))

	case "structure":
		if query == "" {
			return &ToolResult{Name: "graph_query", Success: false,
				Error: "structure needs query — describe what you are working on"}
		}
		budget := 800
		if b, ok := args["budget_tokens"].(float64); ok && b > 0 {
			budget = int(b)
		}
		view, sv := t.KG.StructuralView(query, budget)
		if view == "" {
			return &ToolResult{Name: "graph_query", Success: true,
				Output: "no indexed code matched that description — read the files directly"}
		}
		// The skeleton is prose for the model, not a JSON payload, so it is
		// returned as-is with the measurement appended.
		return &ToolResult{Name: "graph_query", Success: true, Output: fmt.Sprintf(
			"%s\n%d file(s) as %d tokens instead of ~%d (%.0fx smaller)",
			view, sv.Files, sv.ViewTokens, sv.SourceTokens, sv.Ratio())}

	case "simulate":
		change := memory.Change{
			Kind: str(args["change"]), Package: str(args["package"]),
			From: str(args["from"]), To: str(args["to"]),
			Into: stringList(args["into"]), Moves: stringList(args["moves"]),
		}
		sim, err := t.KG.Simulate(change)
		if err != nil {
			return &ToolResult{Name: "graph_query", Success: false, Error: err.Error()}
		}
		return &ToolResult{Name: "graph_query", Success: true, Output: sim.Format()}

	case "evolution":
		from, to := str(args["from"]), str(args["to"])
		if from == "" {
			from = "HEAD~1"
		}
		if to == "" {
			to = "HEAD"
		}
		events, err := memory.DiffCommits(t.Workspace, from, to)
		if err != nil {
			return &ToolResult{Name: "graph_query", Success: false, Error: err.Error()}
		}
		if limit > 0 && len(events) > limit {
			events = events[:limit]
		}
		result = events
		headline = fmt.Sprintf("%d structural change(s) between %s and %s", len(events), from, to)

	case "policy":
		// Mined patterns say what the repository does; a policy says what it
		// must. The difference matters the day somebody adds the import: a
		// pattern quietly stops holding, a policy fails.
		file := str(args["file"])
		if file == "" {
			file = filepath.Join(t.Workspace, "architecture.json")
		}
		pol, err := memory.LoadPolicy(file)
		if err != nil {
			return &ToolResult{Name: "graph_query", Success: false, Error: err.Error()}
		}
		if len(pol.Rules) == 0 {
			return &ToolResult{Name: "graph_query", Success: true, Output: "no policy at " + file +
				" — write one to turn an architectural decision into something that gets checked"}
		}
		breaches := t.KG.CheckPolicy(pol)
		return &ToolResult{Name: "graph_query", Success: true,
			Output: fmt.Sprintf("%d rule(s) checked\n%s", len(pol.Rules), memory.FormatBreaches(breaches))}

	case "patterns", "violations":
		// Conventions this repository actually follows, and where it breaks
		// them. Rules learned in other repositories are included when a library
		// is configured, which is what makes a convention outlive one codebase.
		mined := t.KG.MinePatterns(t.Workspace)
		if action == "patterns" {
			result = mined
			headline = fmt.Sprintf("%d convention(s) mined from this repository", len(mined))
			break
		}
		rules := mined
		if t.Patterns != nil {
			rules = append(rules, t.Patterns.Elsewhere(t.Workspace)...)
		}
		v := t.KG.CheckPatterns(rules)
		if limit > 0 && len(v) > limit {
			v = v[:limit]
		}
		result = v
		headline = fmt.Sprintf("%d place(s) break a convention held elsewhere", len(v))

	case "trends", "alerts":
		// Both read the health daemon's time series. Without a daemon the
		// answer is "nothing has been watched yet", which is information —
		// better than an error the model will retry.
		if t.Daemon == nil {
			result, headline = nil, "the health daemon is not running, so there is no history to read"
			break
		}
		if action == "alerts" {
			a := t.Daemon.Alerts()
			if limit > 0 && len(a) > limit {
				a = a[len(a)-limit:] // most recent
			}
			result, headline = a, fmt.Sprintf("%d structural alert(s)", len(a))
			break
		}
		f := t.Daemon.Forecast()
		result = f
		if f.Generated {
			headline = fmt.Sprintf("%d metric trend(s) over %d samples", len(f.Trends), len(t.Daemon.History()))
		} else {
			headline = "no trend yet: " + f.Note
		}

	default:
		return &ToolResult{Name: "graph_query", Success: false, Error: "unknown action " + action +
			" (want: search, neighbors, subgraph, low_confidence, stale, blast_radius, health, dead_code, cycles, untested, evolution, defect_risk, root_cause, structure, simulate, trends, alerts, patterns, violations, policy)"}
	}

	body, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return &ToolResult{Name: "graph_query", Success: false, Error: err.Error()}
	}
	return &ToolResult{Name: "graph_query", Success: true, Output: headline + "\n" + string(body)}
}

// stringList coerces a JSON array argument to []string, dropping non-strings.
func stringList(v interface{}) []string {
	items, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// RegisterGraphTool adds the graph query tool to the registry.
func RegisterGraphTool(r *Registry, kg *memory.KnowledgeGraph, workspace string, daemon *memory.HealthDaemon, patterns *memory.PatternLibrary) {
	t := &GraphTool{KG: kg, Workspace: workspace, Daemon: daemon, Patterns: patterns}
	r.Register(&ToolEntry{
		Name: "graph_query",
		Description: strings.TrimSpace(`
Query the persistent code knowledge graph: which files define which symbols, what imports what,
how confident each fact is, and what a change would break. Prefer this over re-reading files when
the question is structural ("where is X defined", "what depends on Y", "what will this break").
Actions: search (find nodes by label), neighbors (direct edges of a node id), subgraph (n-hop
neighbourhood), blast_radius (files a change can reach), health (repository health score and ranked
issues), dead_code (unreferenced symbols), cycles (import cycles), untested (high-fan-in symbols with
no test references), low_confidence (beliefs worth re-checking), stale (files indexed before HEAD),
evolution (what changed STRUCTURALLY between two commits — new dependencies, API breaks, cycles created —
rather than a line diff), defect_risk (files most likely to contain bugs, from fix history plus graph
centrality), root_cause (when a test fails, rank likely culprits by defect history AND graph distance from
the failure), structure (a compact skeleton of the code relevant to a goal — signatures, fan-in and
dependencies at roughly a thirtieth the tokens of the source; prefer it over reading many files, then read
full source only for what you are actually editing), simulate (measure a proposed architectural change —
splitting a package, removing or inverting a dependency — against the real graph before writing any code;
reports the delta in cycles, coupling and dependency depth). Every risk score carries the reasons behind it.`),
		Parameters: MustParseSchema(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["search", "neighbors", "subgraph", "blast_radius", "health", "dead_code", "cycles", "untested", "low_confidence", "stale", "evolution", "defect_risk", "root_cause", "structure", "simulate", "trends", "alerts", "patterns", "violations", "policy"], "description": "Which query to run"},
				"query": {"type": "string", "description": "Search term, or a node id for neighbors/subgraph, or a single path for blast_radius"},
				"files": {"type": "array", "description": "File paths for blast_radius, or the failing files for root_cause"},
				"days": {"type": "integer", "description": "History window for defect_risk/root_cause (default 365)"},
				"budget_tokens": {"type": "integer", "description": "Token budget for the structure action (default 800)"},
				"change": {"type": "string", "enum": ["split", "remove_dependency", "invert_dependency"], "description": "The architectural change to simulate"},
				"package": {"type": "string", "description": "Package to split"},
				"into": {"type": "array", "description": "Two names to split a package into"},
				"moves": {"type": "array", "description": "Which of the package's dependencies move to the first half"},
				"type": {"type": "string", "enum": ["file", "symbol", "package", "concept", "decision", "fix", "api"], "description": "Restrict a search to one node type"},
				"depth": {"type": "integer", "description": "Hops for subgraph or blast_radius (default 2)"},
				"threshold": {"type": "number", "description": "Confidence ceiling for low_confidence (default 0.5)"},
				"from": {"type": "string", "description": "Start commit for evolution (default HEAD~1)"},
				"to": {"type": "string", "description": "End commit for evolution (default HEAD)"},
				"limit": {"type": "integer", "description": "Maximum results (default 25)"}
			},
			"required": ["action"]
		}`),
		Handler:  t.Execute,
		Category: "knowledge",
		// Reading the graph mutates nothing, so Chat mode can use it too.
		ReadOnly:      true,
		Deterministic: true,
	})
}
