package permission

// LockTestsDenyRules is the preset that keeps test suites and CI config
// read-only. It exists so an automated fix loop cannot satisfy its own
// verifier by weakening the assertions it is being checked against.
func LockTestsDenyRules() []string {
	tools := []string{"write_file", "patch", "replace_file_content"}
	patterns := []string{"*_test.go", ".github/workflows/*", ".gitlab-ci.yml"}
	var rules []string
	for _, tool := range tools {
		for _, pattern := range patterns {
			rules = append(rules, tool+":"+pattern)
		}
	}
	return rules
}

// SetTestLock enables or disables the test/CI write lock, independent of the
// user's own deny rules — toggling it off must never clear rules the user set
// separately via SetDenyRules.
func (g *Gate) SetTestLock(on bool) {
	g.mu.Lock()
	if on {
		g.testLockRules = ParseDenyRules(LockTestsDenyRules())
	} else {
		g.testLockRules = nil
	}
	g.mu.Unlock()
}
