package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Registry manages the discovery of JSON-based plugins
type Registry struct {
	dir     string
	plugins []Manifest
}

func NewRegistry(dir string) *Registry {
	return &Registry{
		dir:     dir,
		plugins: []Manifest{},
	}
}

// DiscoverAll reads all .json files in the plugin directory as manifests
func (r *Registry) DiscoverAll() error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var manifests []Manifest
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}

		path := filepath.Join(r.dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var m Manifest
		if err := json.Unmarshal(data, &m); err == nil {
			manifests = append(manifests, m)
		}
	}

	r.plugins = manifests
	return nil
}

func (r *Registry) Plugins() []Manifest {
	return r.plugins
}
