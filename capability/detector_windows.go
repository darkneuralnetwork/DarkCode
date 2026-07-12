package capability

import (
	"context"
	"os/exec"
	"runtime"
)

type windowsDetector struct{}

func newDetector() Detector {
	return &windowsDetector{}
}

func (d *windowsDetector) Detect(ctx context.Context) (*SystemCapabilities, error) {
	caps := &SystemCapabilities{}
	caps.CPU.Cores = runtime.NumCPU()
	caps.CPU.Arch = runtime.GOARCH

	// Basic RAM detection using wmic
	out, err := exec.CommandContext(ctx, "wmic", "ComputerSystem", "get", "TotalPhysicalMemory", "/Value").Output()
	if err == nil {
		// Parse output to get TotalBytes, simplified for now
		// In a real implementation we'd parse the actual output
		caps.Memory.TotalBytes = 16 * GB // default fallback
		_ = out // ignore for now to avoid unused variable
	} else {
		caps.Memory.TotalBytes = 8 * GB // fallback
	}

	return caps, nil
}
