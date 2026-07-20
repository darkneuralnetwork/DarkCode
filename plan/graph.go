// Package plan defines the persistent, typed execution plan graph — the
// single source of truth for task decomposition. The markdown plan, mermaid
// architecture diagram, checkbox workflow, and chat preview are all rendered
// FROM a Graph (see render.go); the DAG executor runs a runtime view of it
// (ToDAG). This replaces the old split where the Blueprint text plan and the
// planner agent's in-memory DAG were two disjoint decompositions of the same
// goal that never synced.
package plan

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/dag"
)

// Node is one task in the plan graph. IDs are stable ("T1", "T2", ...) so
// revisions and status updates never renumber tasks the user has already
// seen or that have already completed.
type Node struct {
	ID       string   `json:"id"`   // "T1", "T2", ... stable across revisions
	Name     string   `json:"name"` // short human slug, e.g. "impl-parser"
	Goal     string   `json:"goal"`
	Agent    string   `json:"agent"`          // worker|critic|research|qa|security|ops|executive
	Deps     []string `json:"deps,omitempty"` // node IDs this task depends on
	Priority string   `json:"priority,omitempty"`

	// Planner-emitted execution hints.
	Acceptance    []string `json:"acceptance,omitempty"`     // verifiable completion criteria
	Artifacts     []string `json:"artifacts,omitempty"`      // expected deliverable paths
	EstComplexity int      `json:"est_complexity,omitempty"` // 1-10, drives model placement
	ParallelSafe  bool     `json:"parallel_safe,omitempty"`  // false = touches shared state

	// Execution state, synced back from the DAG after each run.
	Status core.TaskStatus `json:"status"`
	Output string          `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Graph is a full plan: the goal, its decomposition, and provenance.
type Graph struct {
	Version   int       `json:"version"`
	Goal      string    `json:"goal"`
	Summary   string    `json:"summary,omitempty"` // planner's one-paragraph rationale
	Nodes     []*Node   `json:"nodes"`
	Depth     string    `json:"depth,omitempty"`      // "light" | "deep" — planning effort used
	CreatedBy string    `json:"created_by,omitempty"` // model that produced it
	CreatedAt time.Time `json:"created_at"`
	Revisions int       `json:"revisions,omitempty"` // user-feedback revision count
}

// Node returns the node with the given ID, or nil.
func (g *Graph) Node(id string) *Node {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

// Validate checks structural invariants: non-empty unique IDs and goals,
// dependencies that exist, and acyclicity. It returns a single error
// aggregating every problem found so a repair pass can fix them all at once.
func (g *Graph) Validate() error {
	var problems []string
	if len(g.Nodes) == 0 {
		return fmt.Errorf("plan has no tasks")
	}
	seen := make(map[string]bool, len(g.Nodes))
	for _, n := range g.Nodes {
		if strings.TrimSpace(n.ID) == "" {
			problems = append(problems, "a task has an empty id")
			continue
		}
		if seen[n.ID] {
			problems = append(problems, fmt.Sprintf("duplicate task id %q", n.ID))
		}
		seen[n.ID] = true
		if strings.TrimSpace(n.Goal) == "" {
			problems = append(problems, fmt.Sprintf("task %s has an empty goal", n.ID))
		}
	}
	for _, n := range g.Nodes {
		for _, dep := range n.Deps {
			if !seen[dep] {
				problems = append(problems, fmt.Sprintf("task %s depends on unknown task %q", n.ID, dep))
			}
			if dep == n.ID {
				problems = append(problems, fmt.Sprintf("task %s depends on itself", n.ID))
			}
		}
	}
	if len(problems) == 0 && len(g.Waves()) == 0 {
		problems = append(problems, "dependency cycle detected")
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid plan: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Waves returns the nodes grouped into topological levels: every node in
// wave i has all dependencies in waves < i. Nodes within a wave can run in
// parallel. Returns nil when the graph has a cycle (callers use Validate for
// the actual error message).
func (g *Graph) Waves() [][]*Node {
	remaining := make(map[string]*Node, len(g.Nodes))
	for _, n := range g.Nodes {
		remaining[n.ID] = n
	}
	done := make(map[string]bool, len(g.Nodes))
	var waves [][]*Node
	for len(remaining) > 0 {
		var wave []*Node
		for _, n := range g.Nodes { // iterate g.Nodes for deterministic order
			if remaining[n.ID] == nil {
				continue
			}
			ready := true
			for _, dep := range n.Deps {
				if !done[dep] {
					ready = false
					break
				}
			}
			if ready {
				wave = append(wave, n)
			}
		}
		if len(wave) == 0 {
			return nil // cycle
		}
		for _, n := range wave {
			done[n.ID] = true
			delete(remaining, n.ID)
		}
		waves = append(waves, wave)
	}
	return waves
}

// ToDAG builds the runtime execution DAG from the graph. Node statuses carry
// over, so re-running a partially completed graph only executes what's left.
func (g *Graph) ToDAG() *dag.DAG {
	d := dag.NewDAG()
	// Waves() yields dependency-respecting order, which dag.AddNode requires
	// (it rejects nodes whose deps aren't registered yet).
	for _, wave := range g.Waves() {
		for _, n := range wave {
			status := n.Status
			if status == "" {
				status = core.TaskPending
			}
			role := roleFromString(n.Agent)
			_ = d.AddNode(&core.TaskNode{
				ID:           n.ID,
				Name:         n.Name,
				Goal:         n.Goal,
				Status:       status,
				Priority:     priorityFromString(n.Priority),
				Dependencies: append([]string(nil), n.Deps...),
				AgentRole:    role,
				ModelTier:    tierForRole(role, n.EstComplexity),
			})
		}
	}
	return d
}

// SyncFrom copies execution state (status/output/error) from a runtime DAG
// back into the graph, keyed by node ID. Unknown DAG nodes are ignored.
func (g *Graph) SyncFrom(d *dag.DAG) {
	for _, tn := range d.AllNodes() {
		if n := g.Node(tn.ID); n != nil {
			n.Status = tn.Status
			n.Output = tn.Output
			n.Error = tn.Error
		}
	}
}

// TasksJSON serializes the graph in the planner wire shape ({"summary",
// "tasks":[...]}) WITHOUT execution state — the form fed back to the model
// for self-review and feedback revision. Parse() round-trips it.
func (g *Graph) TasksJSON() string {
	wp := wirePlan{Summary: g.Summary, Tasks: make([]wireTask, 0, len(g.Nodes))}
	for _, n := range g.Nodes {
		ps := n.ParallelSafe
		wp.Tasks = append(wp.Tasks, wireTask{
			ID:            n.ID,
			Name:          n.Name,
			Goal:          n.Goal,
			Agent:         n.Agent,
			Deps:          n.Deps,
			Priority:      n.Priority,
			Acceptance:    n.Acceptance,
			Artifacts:     n.Artifacts,
			EstComplexity: n.EstComplexity,
			ParallelSafe:  &ps,
		})
	}
	b, err := json.MarshalIndent(wp, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Bytes serializes the graph as indented JSON (the persisted graph.json form).
func (g *Graph) Bytes() []byte {
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return nil
	}
	return b
}

// Load deserializes a graph persisted by Bytes.
func Load(b []byte) (*Graph, error) {
	var g Graph
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, fmt.Errorf("plan graph unmarshal: %w", err)
	}
	return &g, nil
}

// roleFromString mirrors agents.roleFromString (kept local so plan has no
// dependency on the agents package).
func roleFromString(s string) core.AgentRole {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critic":
		return core.RoleCritic
	case "planner":
		return core.RolePlanner
	case "executive":
		return core.RoleExecutive
	case "research":
		return core.RoleResearch
	case "qa":
		return core.RoleQA
	case "security":
		return core.RoleSecurity
	case "ops":
		return core.RoleOps
	default:
		return core.RoleWorker
	}
}

// priorityFromString mirrors agents.priorityFromString.
func priorityFromString(s string) core.TaskPriority {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return core.PriorityCritical
	case "high":
		return core.PriorityHigh
	case "low":
		return core.PriorityLow
	default:
		return core.PriorityNormal
	}
}

// tierForRole selects the model tier for a node. It follows the same role
// mapping as agents.tierForAgent, with one placement upgrade: a high
// estimated complexity promotes any role to the reasoning tier, and a low
// one demotes worker-class tasks to the fast tier — this is what lets the
// router send cheap nodes to a local/cheap model and hard nodes to the
// primary without a separate scheduler.
func tierForRole(role core.AgentRole, estComplexity int) core.ModelTier {
	if estComplexity >= 8 {
		return core.ModelTierReasoning
	}
	switch role {
	case core.RoleExecutive, core.RolePlanner, core.RoleSecurity:
		return core.ModelTierReasoning
	case core.RoleCritic, core.RoleQA:
		return core.ModelTierCritic
	default:
		if estComplexity > 0 && estComplexity <= 2 {
			return core.ModelTierFast
		}
		return core.ModelTierCoding
	}
}
