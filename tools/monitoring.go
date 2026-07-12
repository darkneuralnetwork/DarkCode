package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// MonitoringTool provides system monitoring and health checks.
type MonitoringTool struct{}

// NewMonitoringTool creates a new monitoring tool.
func NewMonitoringTool() *MonitoringTool {
	return &MonitoringTool{}
}

// Schema returns the JSON Schema for the monitoring tool.
func (m *MonitoringTool) Schema() string {
	return `{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"description": "Monitoring action: system_info, processes, disk, health_check, env",
				"enum": ["system_info", "processes", "disk", "health_check", "env"]
			},
			"target": {
				"type": "string",
				"description": "Target for health_check (URL to check), or filter for processes/env"
			}
		},
		"required": ["action"]
	}`
}

// Execute runs the monitoring action.
func (m *MonitoringTool) Execute(ctx context.Context, args map[string]interface{}) *ToolResult {
	action, _ := args["action"].(string)
	target, _ := args["target"].(string)

	switch action {
	case "system_info":
		return m.systemInfo()
	case "processes":
		return m.processList(ctx, target)
	case "disk":
		return m.diskUsage(ctx)
	case "health_check":
		return m.healthCheck(ctx, target)
	case "env":
		return m.envVars(target)
	default:
		return &ToolResult{Name: "monitoring", Success: false, Error: fmt.Sprintf("unknown action: %s", action)}
	}
}

func (m *MonitoringTool) systemInfo() *ToolResult {
	hostname, _ := os.Hostname()
	var sb strings.Builder
	sb.WriteString("System Information:\n")
	sb.WriteString(fmt.Sprintf("  OS:        %s\n", runtime.GOOS))
	sb.WriteString(fmt.Sprintf("  Arch:      %s\n", runtime.GOARCH))
	sb.WriteString(fmt.Sprintf("  CPUs:      %d\n", runtime.NumCPU()))
	sb.WriteString(fmt.Sprintf("  Hostname:  %s\n", hostname))
	sb.WriteString(fmt.Sprintf("  Go:        %s\n", runtime.Version()))
	sb.WriteString(fmt.Sprintf("  Goroutines: %d\n", runtime.NumGoroutine()))

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	sb.WriteString(fmt.Sprintf("  Heap Alloc: %.1f MB\n", float64(memStats.HeapAlloc)/1024/1024))
	sb.WriteString(fmt.Sprintf("  Sys:        %.1f MB\n", float64(memStats.Sys)/1024/1024))

	return &ToolResult{Name: "monitoring", Success: true, Output: sb.String()}
}

func (m *MonitoringTool) processList(ctx context.Context, filter string) *ToolResult {
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(timeoutCtx, "tasklist", "/FO", "TABLE")
	} else {
		cmd = exec.CommandContext(timeoutCtx, "ps", "aux", "--sort=-pcpu")
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return &ToolResult{Name: "monitoring", Success: false, Error: err.Error(), Output: string(out)}
	}

	output := string(out)
	if filter != "" {
		var filtered []string
		for _, line := range strings.Split(output, "\n") {
			if strings.Contains(strings.ToLower(line), strings.ToLower(filter)) {
				filtered = append(filtered, line)
			}
		}
		output = strings.Join(filtered, "\n")
	}

	if len(output) > 30000 {
		output = output[:30000] + "\n... (truncated)"
	}

	return &ToolResult{Name: "monitoring", Success: true, Output: output}
}

func (m *MonitoringTool) diskUsage(ctx context.Context) *ToolResult {
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(timeoutCtx, "wmic", "logicaldisk", "get", "size,freespace,caption")
	} else {
		cmd = exec.CommandContext(timeoutCtx, "df", "-h")
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return &ToolResult{Name: "monitoring", Success: false, Error: err.Error(), Output: string(out)}
	}

	return &ToolResult{Name: "monitoring", Success: true, Output: string(out)}
}

func (m *MonitoringTool) healthCheck(ctx context.Context, target string) *ToolResult {
	if target == "" {
		return &ToolResult{Name: "monitoring", Success: false, Error: "target URL required for health_check"}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(timeoutCtx, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", target)
	} else {
		cmd = exec.CommandContext(timeoutCtx, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", target)
	}

	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))

	if err != nil {
		return &ToolResult{
			Name:    "monitoring",
			Success: false,
			Output:  fmt.Sprintf("Health check failed for %s: %s", target, output),
			Error:   err.Error(),
		}
	}

	return &ToolResult{
		Name:    "monitoring",
		Success: true,
		Output:  fmt.Sprintf("Health check %s: HTTP %s", target, output),
	}
}

func (m *MonitoringTool) envVars(filter string) *ToolResult {
	envs := os.Environ()
	var sb strings.Builder
	sb.WriteString("Environment Variables:\n")

	for _, env := range envs {
		if filter == "" || strings.Contains(strings.ToLower(env), strings.ToLower(filter)) {
			// Mask potential secrets
			parts := strings.SplitN(env, "=", 2)
			if len(parts) == 2 {
				key := parts[0]
				value := parts[1]
				keyLower := strings.ToLower(key)
				if strings.Contains(keyLower, "key") || strings.Contains(keyLower, "secret") ||
					strings.Contains(keyLower, "password") || strings.Contains(keyLower, "token") {
					value = "***masked***"
				}
				sb.WriteString(fmt.Sprintf("  %s=%s\n", key, value))
			}
		}
	}

	return &ToolResult{Name: "monitoring", Success: true, Output: sb.String()}
}
