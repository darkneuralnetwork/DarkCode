package capability

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

type linuxDetector struct{}

func newDetector() Detector {
	return &linuxDetector{}
}

func (d *linuxDetector) Detect(ctx context.Context) (*SystemCapabilities, error) {
	caps := &SystemCapabilities{}
	caps.CPU.Cores = runtime.NumCPU()
	caps.CPU.Arch = runtime.GOARCH

	d.parseCPUInfo(caps)
	d.parseMemInfo(caps)
	d.detectGPU(ctx, caps)

	return caps, nil
}

func (d *linuxDetector) parseCPUInfo(caps *SystemCapabilities) {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "flags") || strings.HasPrefix(line, "Features") {
			if strings.Contains(line, " avx2 ") {
				caps.CPU.HasAVX2 = true
			}
			if strings.Contains(line, " avx512f ") {
				caps.CPU.HasAVX512 = true
			}
			if strings.Contains(line, " asimd ") || strings.Contains(line, " neon ") {
				caps.CPU.HasNEON = true
			}
		} else if strings.HasPrefix(line, "model name") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				caps.CPU.ModelName = strings.TrimSpace(parts[1])
			}
		}
	}
}

func (d *linuxDetector) parseMemInfo(caps *SystemCapabilities) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				kb, _ := strconv.ParseUint(parts[1], 10, 64)
				caps.Memory.TotalBytes = kb * 1024
			}
		} else if strings.HasPrefix(line, "MemAvailable:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				kb, _ := strconv.ParseUint(parts[1], 10, 64)
				caps.Memory.AvailableBytes = kb * 1024
			}
		}
	}
}

func (d *linuxDetector) detectGPU(ctx context.Context, caps *SystemCapabilities) {
	// Simple nvidia-smi check for nvidia GPUs
	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=memory.total,name", "--format=csv,noheader").Output()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 0 {
			parts := strings.Split(lines[0], ",")
			if len(parts) == 2 {
				memStr := strings.TrimSpace(parts[0])
				memStr = strings.TrimSuffix(memStr, " MiB")
				if mb, err := strconv.ParseUint(memStr, 10, 64); err == nil {
					caps.GPU.Vendor = GPUVendorNvidia
					caps.GPU.HasCUDA = true
					caps.GPU.VRAMBytes = mb * 1024 * 1024
					caps.GPU.ModelName = strings.TrimSpace(parts[1])
				}
			}
		}
	}
}
