package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain redirects the config path for every test in this package.
//
// Save() writes to ~/.darkcode/config.json by default, so a test that
// exercises the save path — directly or through a helper — silently edits the
// developer's own configuration. That happened once during development and
// cost a registered provider and its key.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "darkcode-config-test")
	if err != nil {
		panic("cannot create a temp config dir: " + err.Error())
	}
	os.Setenv("DARKCODE_CONFIG", filepath.Join(dir, "config.json"))

	code := m.Run()

	os.RemoveAll(dir)
	os.Exit(code)
}
