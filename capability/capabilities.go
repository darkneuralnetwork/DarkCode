package capability

import (
	"context"
	"runtime"
)

// CPUInfo represents processor capabilities.
type CPUInfo struct {
	Cores      int
	Arch       string
	HasAVX2    bool
	HasAVX512  bool
	HasNEON    bool // For ARM
	ModelName  string
}

// MemoryInfo represents system RAM.
type MemoryInfo struct {
	TotalBytes     uint64
	AvailableBytes uint64
}

// GPUVendor identifies the GPU manufacturer.
type GPUVendor string

const (
	GPUVendorNvidia GPUVendor = "nvidia"
	GPUVendorAMD    GPUVendor = "amd"
	GPUVendorApple  GPUVendor = "apple"
	GPUVendorIntel  GPUVendor = "intel"
	GPUVendorNone   GPUVendor = "none"
)

// GPUInfo represents graphics capabilities.
type GPUInfo struct {
	Vendor       GPUVendor
	VRAMBytes    uint64
	HasCUDA      bool
	HasMetal     bool
	HasVulkan    bool
	ModelName    string
}

// StorageInfo represents disk capabilities.
type StorageInfo struct {
	TotalBytes     uint64
	AvailableBytes uint64
	IsSSD          bool // True if primary drive is solid state
}

// SystemCapabilities holds all detected hardware info.
type SystemCapabilities struct {
	CPU     CPUInfo
	Memory  MemoryInfo
	GPU     GPUInfo
	Storage StorageInfo
	OS      string // e.g., "linux", "darwin", "windows"
}

// Detector is the interface for OS-specific hardware detection.
type Detector interface {
	Detect(ctx context.Context) (*SystemCapabilities, error)
}

// Detect capabilities for the current system.
func Detect(ctx context.Context) (*SystemCapabilities, error) {
	detector := newDetector()
	caps, err := detector.Detect(ctx)
	if err != nil {
		return nil, err
	}

	// Always set the OS and Architecture natively
	caps.OS = runtime.GOOS
	caps.CPU.Arch = runtime.GOARCH

	return caps, nil
}
