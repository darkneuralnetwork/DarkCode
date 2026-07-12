package capability

import (
	"context"
	"runtime"
)

type darwinDetector struct{}

func newDetector() Detector {
	return &darwinDetector{}
}

func (d *darwinDetector) Detect(ctx context.Context) (*SystemCapabilities, error) {
	caps := &SystemCapabilities{}
	caps.CPU.Cores = runtime.NumCPU()
	caps.CPU.Arch = runtime.GOARCH
	
	if caps.CPU.Arch == "arm64" {
		caps.CPU.HasNEON = true
		caps.GPU.Vendor = GPUVendorApple
		caps.GPU.HasMetal = true
	}

	// Simplistic fallback for darwin detection, in real life we'd use sysctl
	caps.Memory.TotalBytes = 16 * GB

	return caps, nil
}
