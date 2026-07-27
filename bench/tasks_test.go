package bench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A "perfect agent" proves each shipped task is actually solvable and that its
// verify.sh accepts a correct solution — otherwise the suite scores 0 forever
// and nobody knows the fixtures are broken.
// taskTooling names the external tool each task needs, so a machine without
// it skips rather than reporting a false failure.
var taskTooling = map[string]string{
	"py-fix-failing-test":  "pytest",
	"go-fix-compile-error": "go",
	"go-add-test":          "go",
	"multi-file-refactor":  "go",
}

func TestShippedTasksAreSolvable(t *testing.T) {
	tasks, err := LoadTasks("tasks")
	if err != nil {
		t.Fatal(err)
	}
	solutions := map[string]func(ws string) error{
		"go-fix-compile-error": func(ws string) error {
			p := filepath.Join(ws, "main.go")
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(p, []byte(strings.Replace(string(b), "nam\n", "name\n", 1)), 0o644)
		},
		"go-add-test": func(ws string) error {
			return os.WriteFile(filepath.Join(ws, "calc_test.go"), []byte(
				"package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1,2) != 3 { t.Fatal(\"pos\") }\n\tif Add(-1,-2) != -3 { t.Fatal(\"neg\") }\n\tif Add(0,0) != 0 { t.Fatal(\"zero\") }\n}\n"), 0o644)
		},
		"multi-file-refactor": func(ws string) error {
			for _, f := range []string{"greet.go", "main.go", "other.go"} {
				p := filepath.Join(ws, f)
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				if err := os.WriteFile(p, []byte(strings.ReplaceAll(string(b), "Greet", "Welcome")), 0o644); err != nil {
					return err
				}
			}
			return nil
		},
		"py-fix-failing-test": func(ws string) error {
			return os.WriteFile(filepath.Join(ws, "fizzbuzz.py"), []byte(
				"def fizzbuzz(n):\n    if n % 15 == 0:\n        return \"FizzBuzz\"\n    if n % 3 == 0:\n        return \"Fizz\"\n    if n % 5 == 0:\n        return \"Buzz\"\n    return str(n)\n"), 0o644)
		},
	}

	for _, task := range tasks {
		fix, ok := solutions[task.Name]
		if !ok {
			t.Errorf("no reference solution for %s", task.Name)
			continue
		}
		// Skip tasks whose toolchain is absent on this machine rather than
		// reporting a false failure.
		if tool, ok := taskTooling[task.Name]; ok {
			if _, err := exec.LookPath(tool); err != nil {
				t.Logf("skipping %s: %s not installed", task.Name, tool)
				continue
			}
		}
		rep := Run(context.Background(), []Task{task}, agentFunc(func(_ context.Context, ws, _ string) error {
			return fix(ws)
		}))
		if rep.Solved != 1 {
			t.Errorf("task %s not solved by its reference solution: %s", task.Name, rep.Results[0].Reason)
		}
	}
}
