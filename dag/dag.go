package dag

import (
	"fmt"
	"sync"
	"time"

	"github.com/darkcode/core"
)

// DAG represents a directed acyclic graph of tasks.
// It is the backbone of task decomposition — the Planner agent creates a DAG,
// and the Orchestrator executes it respecting dependency ordering.
type DAG struct {
	mu    sync.RWMutex
	nodes map[string]*core.TaskNode
	edges map[string][]string // nodeID -> dependent node IDs
	order []string            // topological order (computed)
}

// NewDAG creates an empty task DAG.
func NewDAG() *DAG {
	return &DAG{
		nodes: make(map[string]*core.TaskNode),
		edges: make(map[string][]string),
	}
}

// AddNode adds a task node to the DAG.
func (d *DAG) AddNode(node *core.TaskNode) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.nodes[node.ID]; exists {
		return fmt.Errorf("node %s already exists", node.ID)
	}
	d.nodes[node.ID] = node

	// Register edges from dependencies
	for _, dep := range node.Dependencies {
		if _, exists := d.nodes[dep]; !exists {
			return fmt.Errorf("dependency %s not found for node %s", dep, node.ID)
		}
		d.edges[dep] = append(d.edges[dep], node.ID)
	}

	// Recompute topological order
	d.computeTopoOrder()
	return nil
}

// GetNode retrieves a node by ID.
func (d *DAG) GetNode(id string) (*core.TaskNode, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	node, ok := d.nodes[id]
	return node, ok
}

// AllNodes returns all nodes in topological order.
func (d *DAG) AllNodes() []*core.TaskNode {
	d.mu.RLock()
	defer d.mu.RUnlock()

	result := make([]*core.TaskNode, 0, len(d.order))
	for _, id := range d.order {
		if node, ok := d.nodes[id]; ok {
			result = append(result, node)
		}
	}
	return result
}

// Nodes is an alias for AllNodes.
func (d *DAG) Nodes() []*core.TaskNode { return d.AllNodes() }

// NodeCount returns the total number of nodes.
func (d *DAG) NodeCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.nodes)
}

// IsEmpty returns true if the DAG has no nodes.
func (d *DAG) IsEmpty() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.nodes) == 0
}

// MarkCompleted sets a node's status to completed.
func (d *DAG) MarkCompleted(id string) error {
	return d.UpdateStatus(id, core.TaskCompleted)
}

// GetReadyTasks returns nodes that are ready to execute (deps satisfied, pending).
// It filters out already-processed node IDs from the given map.
func (d *DAG) GetReadyTasks(processed map[string]bool) []*core.TaskNode {
	ready := d.ReadyNodes()
	var result []*core.TaskNode
	for _, node := range ready {
		if !processed[node.ID] {
			result = append(result, node)
		}
	}
	return result
}

// ReadyNodes returns nodes whose dependencies are all completed and
// whose status is pending. These can be executed now.
func (d *DAG) ReadyNodes() []*core.TaskNode {
	d.mu.RLock()
	defer d.mu.RUnlock()

	completed := make(map[string]bool)
	for _, node := range d.nodes {
		if node.Status == core.TaskCompleted {
			completed[node.ID] = true
		}
	}

	var ready []*core.TaskNode
	for _, node := range d.nodes {
		if node.Status != core.TaskPending {
			continue
		}
		if node.CanExecute(completed) {
			ready = append(ready, node)
		}
	}
	return ready
}

// UpdateStatus updates a node's status and timestamps.
func (d *DAG) UpdateStatus(id string, status core.TaskStatus) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	node, ok := d.nodes[id]
	if !ok {
		return fmt.Errorf("node %s not found", id)
	}

	node.Status = status
	switch status {
	case core.TaskRunning:
		now := time.Now()
		node.StartedAt = &now
	case core.TaskCompleted, core.TaskFailed, core.TaskCancelled:
		now := time.Now()
		node.CompletedAt = &now
	}
	return nil
}

// SetOutput stores the result of a completed task node.
func (d *DAG) SetOutput(id, output string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	node, ok := d.nodes[id]
	if !ok {
		return fmt.Errorf("node %s not found", id)
	}
	node.Output = output
	return nil
}

// SetError records an error on a failed task node.
func (d *DAG) SetError(id, errMsg string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	node, ok := d.nodes[id]
	if !ok {
		return fmt.Errorf("node %s not found", id)
	}
	node.Error = errMsg
	return nil
}

// IsComplete returns true if all nodes are completed, failed, or cancelled.
func (d *DAG) IsComplete() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()

	for _, node := range d.nodes {
		switch node.Status {
		case core.TaskCompleted, core.TaskFailed, core.TaskCancelled:
			continue
		default:
			return false
		}
	}
	return true
}

// Summary returns a human-readable summary of the DAG state.
func (d *DAG) Summary() string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var stats = map[core.TaskStatus]int{}
	for _, node := range d.nodes {
		stats[node.Status]++
	}
	total := len(d.nodes)
	return fmt.Sprintf("DAG: %d nodes — pending:%d running:%d completed:%d failed:%d",
		total,
		stats[core.TaskPending],
		stats[core.TaskRunning],
		stats[core.TaskCompleted],
		stats[core.TaskFailed],
	)
}

// CompletedCount returns how many nodes are done.
func (d *DAG) CompletedCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	count := 0
	for _, node := range d.nodes {
		if node.Status == core.TaskCompleted {
			count++
		}
	}
	return count
}

// FailedNodes returns all failed task nodes.
func (d *DAG) FailedNodes() []*core.TaskNode {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var failed []*core.TaskNode
	for _, node := range d.nodes {
		if node.Status == core.TaskFailed {
			failed = append(failed, node)
		}
	}
	return failed
}

// computeTopoOrder computes a topological sort using Kahn's algorithm.
// Must be called with write lock held.
func (d *DAG) computeTopoOrder() {
	inDegree := make(map[string]int)
	for id := range d.nodes {
		inDegree[id] = len(d.nodes[id].Dependencies)
	}

	// Start with nodes that have no dependencies
	var queue []string
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}

	d.order = d.order[:0] // clear
	for len(queue) > 0 {
		// Pop front
		curr := queue[0]
		queue = queue[1:]
		d.order = append(d.order, curr)

		// Reduce in-degree of dependents
		for _, dep := range d.edges[curr] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}
}

// HasCycle returns true if the DAG contains a cycle (invalid state).
func (d *DAG) HasCycle() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.order) != len(d.nodes)
}

// Dot returns a Graphviz DOT representation for visualization.
func (d *DAG) Dot() string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var sb string
	sb = "digraph task_dag {\n"
	sb += "  rankdir=TB;\n"
	sb += "  node [shape=box, style=filled];\n"

	for _, node := range d.nodes {
		color := "white"
		switch node.Status {
		case core.TaskCompleted:
			color = "lightgreen"
		case core.TaskRunning:
			color = "lightyellow"
		case core.TaskFailed:
			color = "lightcoral"
		case core.TaskPending:
			color = "lightblue"
		}
		sb += fmt.Sprintf("  \"%s\" [label=\"%s\\n(%s)\", fillcolor=%s];\n",
			node.ID, node.Name, node.Status, color)
	}

	for from, tos := range d.edges {
		for _, to := range tos {
			sb += fmt.Sprintf("  \"%s\" -> \"%s\";\n", from, to)
		}
	}

	sb += "}\n"
	return sb
}
