package security

// Policy defines the declarative security rules for a project.
type Policy struct {
	AllowedPaths []string `yaml:"allowed_paths"`
	DeniedPaths  []string `yaml:"denied_paths"`
	NetworkMode  string   `yaml:"network_mode"` // "none", "restricted", "full"
}

// LoadPolicy parses a security.yaml file.
func LoadPolicy(path string) (*Policy, error) {
	// Dummy implementation for now.
	return &Policy{
		AllowedPaths: []string{"*"}, // Default open
	}, nil
}
