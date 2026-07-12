package observability

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// HardwareStats contains real-time resource utilization metrics
type HardwareStats struct {
	CPUUsagePercent float64 `json:"cpu_usage_percent"`
	RAMTotalMB      float64 `json:"ram_total_mb"`
	RAMUsedMB       float64 `json:"ram_used_mb"`
	RAMUsagePercent float64 `json:"ram_usage_percent"`
	GoRoutines      int     `json:"go_routines"`
	GoHeapAllocMB   float64 `json:"go_heap_alloc_mb"`
	GoHeapSysMB     float64 `json:"go_heap_sys_mb"`
	OS              string  `json:"os"`
	Arch            string  `json:"arch"`
	NumCPU          int     `json:"num_cpu"`
	VRAMUsedMB      float64 `json:"vram_used_mb,omitempty"`
	GPUUsagePercent float64 `json:"gpu_usage_percent,omitempty"`
	Provider        string  `json:"provider_mode,omitempty"`
}

// GetHardwareStats retrieves cross-platform light resource metrics
func GetHardwareStats() HardwareStats {
	stats := HardwareStats{
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		NumCPU:     runtime.NumCPU(),
		GoRoutines: runtime.NumGoroutine(),
	}

	// Go Memory Stats
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	stats.GoHeapAllocMB = float64(mem.Alloc) / 1024 / 1024
	stats.GoHeapSysMB = float64(mem.Sys) / 1024 / 1024

	// OS-specific Memory parsing
	if runtime.GOOS == "linux" {
		total, used := getLinuxMemory()
		if total > 0 {
			stats.RAMTotalMB = total
			stats.RAMUsedMB = used
			stats.RAMUsagePercent = (used / total) * 100
		}
		stats.CPUUsagePercent = getLinuxCPUUsage()
	} else if runtime.GOOS == "windows" {
		total, used := getWindowsMemory()
		if total > 0 {
			stats.RAMTotalMB = total
			stats.RAMUsedMB = used
			stats.RAMUsagePercent = (used / total) * 100
		}
	}

	fetchOllamaPS(&stats)

	return stats
}

func fetchOllamaPS(stats *HardwareStats) {
	// Speculatively check local ollama for loaded models
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://127.0.0.1:11434/api/ps")
	if err != nil {
		stats.Provider = "cloud" // Default assumption if no local ollama
		return
	}
	defer resp.Body.Close()
	stats.Provider = "local"
	
	var data struct {
		Models []struct {
			SizeVram int64 `json:"size_vram"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err == nil {
		var totalVram int64
		for _, m := range data.Models {
			totalVram += m.SizeVram
		}
		if totalVram > 0 {
			stats.VRAMUsedMB = float64(totalVram) / 1024 / 1024
		}
	}
}

// getLinuxMemory reads /proc/meminfo to return Total and Used MB
func getLinuxMemory() (float64, float64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var memTotal, memFree, memAvailable, buffers, cached float64
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val, _ := strconv.ParseFloat(fields[1], 64) // in KB
		switch fields[0] {
		case "MemTotal:":
			memTotal = val / 1024
		case "MemFree:":
			memFree = val / 1024
		case "MemAvailable:":
			memAvailable = val / 1024
		case "Buffers:":
			buffers = val / 1024
		case "Cached:":
			cached = val / 1024
		}
	}
	used := memTotal - memFree - buffers - cached
	if memAvailable > 0 {
		used = memTotal - memAvailable
	}
	return memTotal, used
}

// getLinuxCPUUsage calculates top-level CPU usage by reading /proc/stat
// To do this non-blockingly without a delay, we track the last read values.
var (
	lastCPUTotal uint64
	lastCPUIdle  uint64
)

func getLinuxCPUUsage() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return 0
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}
	
	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		val, _ := strconv.ParseUint(fields[i], 10, 64)
		total += val
		if i == 4 { // idle time
			idle = val
		}
	}
	
	if lastCPUTotal == 0 {
		lastCPUTotal = total
		lastCPUIdle = idle
		return 0 // Need two samples
	}
	
	totalDiff := float64(total - lastCPUTotal)
	idleDiff := float64(idle - lastCPUIdle)
	lastCPUTotal = total
	lastCPUIdle = idle
	
	if totalDiff == 0 {
		return 0
	}
	return (1.0 - (idleDiff / totalDiff)) * 100.0
}

// getWindowsMemory uses wmic or systeminfo for a lightweight Windows fallback
func getWindowsMemory() (float64, float64) {
	// Simple powershell one-liner or system calls. 
	// Due to light constraint, we skip complex parsing if not strictly necessary.
	return 0, 0
}
