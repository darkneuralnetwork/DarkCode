package permission

import "testing"

// Test-lock denies writes to test files and CI config while leaving
// everything else — including the user's own deny rules — untouched.
func TestLockTestsDeniesTestFileWrites(t *testing.T) {
	g := NewGate(LevelRelaxed)
	g.SetApprover(AutoApprover())
	g.SetTestLock(true)

	blocked := []struct {
		tool string
		path string
	}{
		{"write_file", "foo_test.go"},
		{"write_file", "pkg/bar_test.go"},
		{"patch", "pkg/bar_test.go"},
		{"replace_file_content", "pkg/bar_test.go"},
		{"write_file", ".github/workflows/ci.yml"},
		{"patch", ".gitlab-ci.yml"},
	}
	for _, tc := range blocked {
		args := map[string]interface{}{"path": tc.path}
		if allowed, _, feedback := g.Check(tc.tool, args); allowed {
			t.Errorf("%s %s: test lock did not block", tc.tool, tc.path)
		} else if feedback == "" {
			t.Errorf("%s %s: denial should explain which rule fired", tc.tool, tc.path)
		}
	}

	// Non-test, non-CI files still go through.
	if allowed, _, _ := g.Check("write_file", map[string]interface{}{"path": "main.go"}); !allowed {
		t.Error("test lock blocked a non-test file")
	}
	// Reading a test file is unaffected — only writes are locked.
	if allowed, _, _ := g.Check("read_file", map[string]interface{}{"path": "foo_test.go"}); !allowed {
		t.Error("test lock blocked reading a test file")
	}
}

func TestLockTestsOffRestoresWrites(t *testing.T) {
	g := NewGate(LevelRelaxed)
	g.SetApprover(AutoApprover())
	g.SetTestLock(true)
	g.SetTestLock(false)

	if allowed, _, _ := g.Check("write_file", map[string]interface{}{"path": "foo_test.go"}); !allowed {
		t.Error("toggling test lock off did not restore test-file writes")
	}
}

func TestLockTestsCoexistsWithUserDenyRules(t *testing.T) {
	g := NewGate(LevelRelaxed)
	g.SetApprover(AutoApprover())
	g.SetDenyRules([]string{"git:push"})
	g.SetTestLock(true)

	if allowed, _, _ := g.Check("git", map[string]interface{}{"args": "push"}); allowed {
		t.Error("user deny rule lost when test lock was enabled")
	}
	if allowed, _, _ := g.Check("write_file", map[string]interface{}{"path": "foo_test.go"}); allowed {
		t.Error("test lock did not apply alongside a pre-existing user deny rule")
	}

	g.SetTestLock(false)
	if allowed, _, _ := g.Check("git", map[string]interface{}{"args": "push"}); allowed {
		t.Error("turning test lock off should not clear the user's own deny rules")
	}
}
