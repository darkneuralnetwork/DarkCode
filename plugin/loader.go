package plugin

// loader.go — discovers and loads plugin binaries from a directory.
//
// Previously this tried to load every file (including non-executable ones)
// as a plugin binary. It now:
//   - Only loads files that are executable
//   - Follows a naming convention (plugin-* or *.plugin)
//   - Skips files that can't be stat'd or aren't regular files
//   - Continues on individual load failures (logs and moves on)

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Loader discovers and initializes plugins from a directory.
type Loader struct {
	host *Host
	dir  string
}

func NewLoader(host *Host, dir string) *Loader {
	return &Loader{
		host: host,
		dir:  dir,
	}
}

// DiscoverAll scans the plugin directory and loads all valid binaries.
func (l *Loader) DiscoverAll() error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no plugin dir = no plugins
		}
		return err
	}

	var loadErrors []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		name := e.Name()
		// Only load files that follow the plugin naming convention.
		if !isPluginBinary(name) {
			continue
		}

		binaryPath := filepath.Join(l.dir, name)
		info, err := e.Info()
		if err != nil {
			continue
		}

		// Check that the file is executable.
		if info.Mode()&0111 == 0 {
			continue
		}

		if err := l.host.Load(binaryPath); err != nil {
			loadErrors = append(loadErrors, fmt.Sprintf("%s: %v", name, err))
		}
	}

	if len(loadErrors) > 0 {
		return fmt.Errorf("some plugins failed to load: %s", strings.Join(loadErrors, "; "))
	}
	return nil
}

// isPluginBinary checks whether a filename follows the plugin naming convention.
func isPluginBinary(name string) bool {
	// Convention: plugin-* or *.plugin (or *.plugin.exe on Windows).
	if strings.HasPrefix(name, "plugin-") {
		return true
	}
	if strings.HasSuffix(name, ".plugin") {
		return true
	}
	if strings.HasSuffix(name, ".plugin.exe") {
		return true
	}
	return false
}
