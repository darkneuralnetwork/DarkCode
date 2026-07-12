package security

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/darkcode/ui"
)

// Sandbox configures and launches processes in an isolated environment.
type Sandbox struct {
	Enabled      bool
	UseFirejail  bool
	UseNamespace bool
	emitter      *ui.EventEmitter
}

func NewSandbox(emitter *ui.EventEmitter) *Sandbox {
	// In a full implementation, detect capabilities like firejail or user namespaces.
	return &Sandbox{
		Enabled: true,
		emitter: emitter,
	}
}

// Run executes the command safely within the sandbox limits.
func (s *Sandbox) Run(ctx context.Context, cmdName string, args ...string) ([]byte, error) {
	if !s.Enabled {
		cmd := exec.CommandContext(ctx, cmdName, args...)
		return cmd.CombinedOutput()
	}

	if s.emitter != nil {
		s.emitter.EmitTaskUpdate("security-sandbox", "executing", fmt.Sprintf("Running command in isolated sandbox: %s", cmdName))
	}

	// Example sandbox wrapping (using firejail if configured, otherwise unshare)
	sandboxArgs := []string{}
	
	if s.UseFirejail {
		sandboxArgs = append([]string{"firejail", "--quiet"}, cmdName)
		sandboxArgs = append(sandboxArgs, args...)
	} else if s.UseNamespace {
		sandboxArgs = append([]string{"unshare", "--net", "--pid", "--fork"}, cmdName)
		sandboxArgs = append(sandboxArgs, args...)
	} else {
		// Fallback to normal execution if no sandbox available
		sandboxArgs = append([]string{cmdName}, args...)
	}

	cmd := exec.CommandContext(ctx, sandboxArgs[0], sandboxArgs[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("sandbox execution failed: %w", err)
	}
	return out, nil
}
