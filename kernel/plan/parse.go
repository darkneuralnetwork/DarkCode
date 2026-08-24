package plan

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/darkcode/infra/core"
)

// wireTask is the tolerant wire shape the planner emits. Both "deps" and
// "dependencies" are accepted, and dep references may be task names or IDs.
type wireTask struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Goal          string   `json:"goal"`
	Agent         string   `json:"agent"`
	Deps          []string `json:"deps"`
	Dependencies  []string `json:"dependencies"`
	Priority      string   `json:"priority"`
	Acceptance    []string `json:"acceptance"`
	Artifacts     []string `json:"artifacts"`
	EstComplexity int      `json:"est_complexity"`
	ParallelSafe  *bool    `json:"parallel_safe"`
}

// wirePlan is the preferred top-level object; a bare JSON array of tasks is
// also accepted.
type wirePlan struct {
	Summary string     `json:"summary"`
	Tasks   []wireTask `json:"tasks"`
}

var tIDRe = regexp.MustCompile(`^T\d+$`)

// Parse extracts a Graph from planner output. The planner is instructed to
// emit JSON, but models wrap it in prose, <analysis> blocks, or markdown
// fences — so extraction is deliberately tolerant. Returns an error when no
// parsable task list is found.
func Parse(text, goal string) (*Graph, error) {
	wp, err := extractWirePlan(text)
	if err != nil {
		return nil, err
	}
	if len(wp.Tasks) == 0 {
		return nil, fmt.Errorf("planner output contained no tasks")
	}

	g := &Graph{
		Version:   1,
		Goal:      goal,
		Summary:   strings.TrimSpace(wp.Summary),
		CreatedAt: time.Now(),
	}

	// Pass 1: assign stable IDs and build the name/id → ID lookup.
	byRef := make(map[string]string, len(wp.Tasks)*2)
	for i, t := range wp.Tasks {
		id := strings.TrimSpace(t.ID)
		if !tIDRe.MatchString(id) {
			id = fmt.Sprintf("T%d", i+1)
		}
		if name := strings.TrimSpace(t.Name); name != "" {
			byRef[strings.ToLower(name)] = id
		}
		byRef[strings.ToLower(id)] = id
		// Also key the emitted id even if we renamed it, so deps written
		// against the model's own ids still resolve.
		if raw := strings.ToLower(strings.TrimSpace(t.ID)); raw != "" {
			byRef[raw] = id
		}
		wp.Tasks[i].ID = id
	}

	// Pass 2: build nodes with resolved dependencies.
	for _, t := range wp.Tasks {
		goalText := strings.TrimSpace(t.Goal)
		if goalText == "" {
			continue // unnamed/empty tasks are dropped, matching legacy parser behavior
		}
		name := strings.TrimSpace(t.Name)
		if name == "" {
			name = t.ID
		}
		refs := t.Deps
		if len(refs) == 0 {
			refs = t.Dependencies
		}
		var deps []string
		for _, ref := range refs {
			ref = strings.ToLower(strings.TrimSpace(ref))
			if ref == "" || ref == "none" {
				continue
			}
			if id, ok := byRef[ref]; ok && id != t.ID {
				deps = append(deps, id)
			}
		}
		parallelSafe := len(deps) == 0
		if t.ParallelSafe != nil {
			parallelSafe = *t.ParallelSafe
		}
		g.Nodes = append(g.Nodes, &Node{
			ID:            t.ID,
			Name:          name,
			Goal:          goalText,
			Agent:         strings.ToLower(strings.TrimSpace(t.Agent)),
			Deps:          deps,
			Priority:      strings.ToLower(strings.TrimSpace(t.Priority)),
			Acceptance:    cleanList(t.Acceptance),
			Artifacts:     cleanList(t.Artifacts),
			EstComplexity: clampComplexity(t.EstComplexity),
			ParallelSafe:  parallelSafe,
			Status:        core.TaskPending,
		})
	}
	if len(g.Nodes) == 0 {
		return nil, fmt.Errorf("planner output contained no usable tasks")
	}
	return g, nil
}

// extractWirePlan tries progressively looser extractions: the whole text as
// a plan object, then as a task array, then the outermost {...} / [...]
// substring (which skips <analysis> prose and markdown fences).
func extractWirePlan(text string) (*wirePlan, error) {
	text = strings.TrimSpace(text)

	candidates := []string{text}
	if i, j := strings.Index(text, "{"), strings.LastIndex(text, "}"); i >= 0 && j > i {
		candidates = append(candidates, text[i:j+1])
	}
	if i, j := strings.Index(text, "["), strings.LastIndex(text, "]"); i >= 0 && j > i {
		candidates = append(candidates, text[i:j+1])
	}

	for _, c := range candidates {
		var wp wirePlan
		if err := json.Unmarshal([]byte(c), &wp); err == nil && len(wp.Tasks) > 0 {
			return &wp, nil
		}
		var tasks []wireTask
		if err := json.Unmarshal([]byte(c), &tasks); err == nil && len(tasks) > 0 {
			return &wirePlan{Tasks: tasks}, nil
		}
	}
	return nil, fmt.Errorf("no JSON plan found in planner output")
}

func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func clampComplexity(n int) int {
	if n < 0 {
		return 0
	}
	if n > 10 {
		return 10
	}
	return n
}
