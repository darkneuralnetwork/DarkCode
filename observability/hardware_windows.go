//go:build windows

package observability

import (
	"sync"
	"syscall"
	"unsafe"
)

// hardware_windows.go — real Windows implementations of the RAM/CPU stats,
// replacing the old `return 0, 0` stub that made the GUI Resource Center show
// RAM 0/0 and CPU 0% on Windows. Pure stdlib (syscall + kernel32), no cgo and
// no external dependency.

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatus = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes     = kernel32.NewProc("GetSystemTimes")
)

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX structure. The layout is
// stable: two DWORDs followed by seven DWORDLONGs — the 64-bit fields all land
// on 8-byte offsets, so no manual padding is needed on 386.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// getWindowsMemory returns (totalMB, usedMB) of physical RAM via
// GlobalMemoryStatusEx. Returns 0,0 on failure (same as the old stub, so it's
// strictly no-worse if the syscall is unavailable).
func getWindowsMemory() (float64, float64) {
	var m memoryStatusEx
	m.length = uint32(unsafe.Sizeof(m))
	ret, _, _ := procGlobalMemoryStatus.Call(uintptr(unsafe.Pointer(&m)))
	if ret == 0 || m.totalPhys == 0 {
		return 0, 0
	}
	const mb = 1024 * 1024
	total := float64(m.totalPhys) / mb
	used := float64(m.totalPhys-m.availPhys) / mb
	return total, used
}

type filetime struct {
	low  uint32
	high uint32
}

func (f filetime) uint64() uint64 { return uint64(f.high)<<32 | uint64(f.low) }

var (
	cpuMu       sync.Mutex
	lastIdle    uint64
	lastKernel  uint64
	lastUser    uint64
	haveLastCPU bool
)

// getWindowsCPUUsage returns system-wide CPU utilization (%) computed from the
// delta of GetSystemTimes between successive calls. The first call has no
// prior sample and returns 0; subsequent calls (the dashboard polls every few
// seconds) return the real busy percentage over the interval. On Windows the
// kernel time already includes idle time, so busy = (kernel+user) - idle.
func getWindowsCPUUsage() float64 {
	var idle, kernel, user filetime
	ret, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ret == 0 {
		return 0
	}
	i, k, u := idle.uint64(), kernel.uint64(), user.uint64()

	cpuMu.Lock()
	defer cpuMu.Unlock()
	if !haveLastCPU {
		lastIdle, lastKernel, lastUser, haveLastCPU = i, k, u, true
		return 0
	}
	idleDelta := i - lastIdle
	totalDelta := (k - lastKernel) + (u - lastUser)
	lastIdle, lastKernel, lastUser = i, k, u
	if totalDelta == 0 {
		return 0
	}
	busy := float64(totalDelta-idleDelta) / float64(totalDelta) * 100
	if busy < 0 {
		busy = 0
	} else if busy > 100 {
		busy = 100
	}
	return busy
}
