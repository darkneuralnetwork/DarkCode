//go:build !windows

package observability

// Non-Windows stubs for the Windows-specific hardware helpers. These are never
// called off Windows (GetHardwareStats only invokes them under
// runtime.GOOS == "windows"), but they must exist so the cross-platform build
// compiles. Linux uses getLinuxMemory/getLinuxCPUUsage instead.

func getWindowsMemory() (float64, float64) { return 0, 0 }
func getWindowsCPUUsage() float64          { return 0 }
