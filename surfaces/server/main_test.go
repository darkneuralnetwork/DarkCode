package server

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain redirects the config path for every test in this package.
//
// This is not a convenience. Handler tests build a Server around a
// config.Config and several of them reach updateConfig, which ends in
// cfg.Save() — and Save resolves to the developer's own
// ~/.darkcode/config.json unless told otherwise. One such test overwrote a
// real configuration during development: the model was replaced with the
// placeholder the test happened to use, and the registered provider and its
// key were lost.
//
// Setting it here rather than per-test means a test added later cannot
// reintroduce the problem by forgetting.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "darkcode-server-test-config")
	if err != nil {
		panic("cannot create a temp config dir: " + err.Error())
	}
	os.Setenv("DARKCODE_CONFIG", filepath.Join(dir, "config.json"))

	code := m.Run()

	os.RemoveAll(dir)
	os.Exit(code)
}
