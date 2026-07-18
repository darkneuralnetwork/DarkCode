package agents

import (
	"testing"

	"github.com/darkcode/core"
)

func TestParsePlannerJSON(t *testing.T) {
	out := `Here is the plan:
[
  {"name": "impl", "goal": "write the parser", "dependencies": [], "agent": "worker", "priority": "high"},
  {"name": "test", "goal": "add tests", "dependencies": ["impl"], "agent": "qa", "priority": "normal"}
]`
	tasks := ParsePlannerOutput(out)
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if tasks[0].Name != "impl" || tasks[0].Agent != core.RoleWorker || tasks[0].Priority != core.PriorityHigh {
		t.Errorf("task0 = %+v, want impl/worker/high", tasks[0])
	}
	if tasks[1].Agent != core.RoleQA {
		t.Errorf("task1 agent = %q, want qa", tasks[1].Agent)
	}
	if len(tasks[1].Deps) != 1 || tasks[1].Deps[0] != "impl" {
		t.Errorf("task1 deps = %v, want [impl]", tasks[1].Deps)
	}
}

func TestParsePlannerJSONInFence(t *testing.T) {
	out := "```json\n[{\"name\":\"a\",\"goal\":\"do a\",\"dependencies\":[],\"agent\":\"worker\",\"priority\":\"normal\"}]\n```"
	tasks := ParsePlannerOutput(out)
	if len(tasks) != 1 || tasks[0].Name != "a" {
		t.Fatalf("got %+v, want a single task named 'a' (fenced JSON must still parse)", tasks)
	}
}

func TestParsePlannerJSONDropsUnnamedAndNoneDeps(t *testing.T) {
	out := `[
	  {"name": "", "goal": "no name — dropped", "dependencies": [], "agent": "worker", "priority": "normal"},
	  {"name": "real", "goal": "kept", "dependencies": ["none", "  "], "agent": "worker", "priority": "normal"}
	]`
	tasks := ParsePlannerOutput(out)
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1 (unnamed dropped)", len(tasks))
	}
	if len(tasks[0].Deps) != 0 {
		t.Errorf("deps = %v, want empty ('none'/blank deps filtered out)", tasks[0].Deps)
	}
}

// TestParsePlannerPipeFallback verifies the legacy pipe-delimited format still
// parses when a model ignores the JSON instruction (backward compatibility).
func TestParsePlannerPipeFallback(t *testing.T) {
	out := `TASK: impl | GOAL: write it | DEPS: none | AGENT: worker | PRIORITY: high
TASK: verify | GOAL: check it | DEPS: impl | AGENT: qa | PRIORITY: normal
PLAN_END`
	tasks := ParsePlannerOutput(out)
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2 from the legacy pipe format", len(tasks))
	}
	if tasks[0].Priority != core.PriorityHigh || tasks[1].Agent != core.RoleQA {
		t.Errorf("pipe parse produced wrong values: %+v", tasks)
	}
	if len(tasks[1].Deps) != 1 || tasks[1].Deps[0] != "impl" {
		t.Errorf("task1 deps = %v, want [impl]", tasks[1].Deps)
	}
}

func TestParsePlannerEmptyAndGarbage(t *testing.T) {
	for _, in := range []string{"", "I couldn't make a plan.", "[not valid json", "{}"} {
		if tasks := ParsePlannerOutput(in); len(tasks) != 0 {
			t.Errorf("ParsePlannerOutput(%q) = %v, want no tasks", in, tasks)
		}
	}
}

func TestRoleAndPriorityFromString(t *testing.T) {
	if roleFromString("SECURITY") != core.RoleSecurity {
		t.Error("roleFromString should be case-insensitive")
	}
	if roleFromString("nonsense") != core.RoleWorker {
		t.Error("unknown role should default to worker")
	}
	if priorityFromString("Critical") != core.PriorityCritical {
		t.Error("priorityFromString should be case-insensitive")
	}
	if priorityFromString("") != core.PriorityNormal {
		t.Error("empty priority should default to normal")
	}
}
