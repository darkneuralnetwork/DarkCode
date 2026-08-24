package cli

import (
	"fmt"
	"strings"

	"github.com/darkcode/infra/metrics"
	"github.com/darkcode/infra/observability"
	"github.com/darkcode/internal/strutil"
)

// printUsageFull prints a detailed usage report with a sparkline.
func (c *Console) printUsageFull() {
	snap := metrics.Default.Snapshot()
	w := termWidth()
	if w > 80 {
		w = 80
	}
	fmt.Println()
	fmt.Println(paint(cAmber+clrBold, "  USAGE REPORT") + paint(cGray, "  since "+fmtTimeShort(snap.Since)))
	fmt.Println("  " + divider(w-4))

	// KPI line
	fmt.Printf("  %s  %s   %s  %s   %s  %s   %s  %s\n",
		paint(cGray, "tokens"), paint(cOrange+clrBold, fmtNum(snap.TotalTokens)),
		paint(cGray, "cost"), paint(cGreen+clrBold, fmtCost(snap.TotalCost)),
		paint(cGray, "requests"), paint(cBlue+clrBold, fmtNum(snap.TotalRequests)),
		paint(cGray, "avg lat"), paint(cYellow+clrBold, fmtDur(int64(snap.AvgLatencyMs))))

	// Sparkline of per-minute tokens
	var vals []float64
	for _, s := range snap.Series {
		vals = append(vals, float64(s.TotalTokens))
	}
	fmt.Println()
	fmt.Printf("  %s\n", paint(cGray, "token throughput /min"))
	fmt.Printf("  %s\n", sparkline(downsample(padZeros(vals, 30), w-6), cOrange))

	// Per-model bars
	if len(snap.PerModel) > 0 {
		fmt.Println()
		fmt.Printf("  %s\n", paint(cGray, "tokens by model"))
		maxTok := 0
		for _, m := range snap.PerModel {
			if m.TotalTokens > maxTok {
				maxTok = m.TotalTokens
			}
		}
		barW := w - 44
		if barW < 16 {
			barW = 16
		}
		for _, m := range snap.PerModel {
			label := m.Model
			if m.Provider != "" {
				label = m.Provider + "/" + m.Model
			}
			fmt.Println(barRow(label, m.TotalTokens, maxTok, barW, cAmber))
		}
	} else {
		fmt.Println()
		fmt.Println(paint(cGray, "  no model usage recorded yet."))
	}
	fmt.Println()
}

// padZeros left-pads a slice with zeros so the sparkline reads left-to-right.
func padZeros(v []float64, n int) []float64 {
	if len(v) >= n {
		return v
	}
	return append(make([]float64, n-len(v)), v...)
}

func (c *Console) handleProjects(args []string) {
	if c.projects == nil {
		fmt.Println(paint(cRed, "✗ project store not initialized"))
		return
	}
	if len(args) == 0 {
		args = []string{"list"}
	}

	switch args[0] {
	case "list":
		projects := c.projects.List()
		if len(projects) == 0 {
			fmt.Println(paint(cGray, "  no projects created yet."))
			return
		}
		fmt.Printf("%s %s\n", paint(cAmber+clrBold, "PROJECTS"), paint(cGray, "("+fmtNum(len(projects))+")"))
		for _, p := range projects {
			active := "  "
			if c.activeProject == p.ID {
				active = paint(cGreen, "➜ ")
			}
			fmt.Printf("%s%s  %s  %s\n", active, paint(cWhite, padRight(p.Name, 20)), paint(cGray, padRight(p.ID, 30)), paint(cGray, strutil.Truncate(p.Description, 40)))
		}
	case "new":
		if len(args) < 3 {
			fmt.Printf("%s usage: /projects new <name> <path> [description...]\n", paint(cRed, "✗"))
			return
		}
		name := args[1]
		path := args[2]
		desc := ""
		if len(args) > 3 {
			desc = strings.Join(args[3:], " ")
		}
		p, err := c.projects.Create(name, desc, path, nil)
		if err != nil {
			fmt.Printf("%s failed to create project: %s\n", paint(cRed, "✗"), err)
			return
		}
		fmt.Printf("%s created project %s (%s)\n", paint(cGreen, "✓"), paint(cWhite, p.Name), paint(cGray, p.ID))
	case "open", "use", "set":
		if len(args) < 2 {
			fmt.Printf("%s usage: /projects %s <id>\n", paint(cRed, "✗"), args[0])
			return
		}
		id := args[1]
		p, err := c.projects.Get(id)
		if err != nil {
			fmt.Printf("%s project not found: %s\n", paint(cRed, "✗"), id)
			return
		}
		c.activeProject = p.ID
		_ = c.projects.Touch(p.ID)
		fmt.Printf("%s active project set to %s\n", paint(cGreen, "✓"), paint(cWhite, p.Name))
	case "clear":
		c.activeProject = ""
		fmt.Printf("%s active project cleared\n", paint(cGreen, "✓"))
	case "context":
		if len(args) < 2 {
			fmt.Printf("%s usage: /projects context <id>\n", paint(cRed, "✗"))
			return
		}
		id := args[1]
		ctxStr, err := c.projects.GetContext(id)
		if err != nil {
			fmt.Printf("%s project not found: %s\n", paint(cRed, "✗"), id)
			return
		}
		fmt.Println(paint(cAmber+clrBold, "PROJECT CONTEXT"))
		fmt.Println(ctxStr)
	case "plan":
		if len(args) < 2 {
			fmt.Printf("%s usage: /projects plan <id>\n", paint(cRed, "✗"))
			return
		}
		id := args[1]
		planStr, err := c.projects.GetPlan(id)
		if err != nil {
			fmt.Printf("%s project not found: %s\n", paint(cRed, "✗"), id)
			return
		}
		fmt.Println(paint(cAmber+clrBold, "PROJECT PLAN"))
		fmt.Println(planStr)
	case "workflow":
		if len(args) < 2 {
			fmt.Printf("%s usage: /projects workflow <id>\n", paint(cRed, "✗"))
			return
		}
		id := args[1]
		workflowStr, err := c.projects.GetWorkflow(id)
		if err != nil {
			fmt.Printf("%s project not found: %s\n", paint(cRed, "✗"), id)
			return
		}
		fmt.Println(paint(cAmber+clrBold, "PROJECT WORKFLOW"))
		fmt.Println(workflowStr)
	case "delete":
		if len(args) < 2 {
			fmt.Printf("%s usage: /projects delete <id>\n", paint(cRed, "✗"))
			return
		}
		id := args[1]
		if err := c.projects.Delete(id); err != nil {
			fmt.Printf("%s failed to delete project: %s\n", paint(cRed, "✗"), err)
			return
		}
		if c.activeProject == id {
			c.activeProject = ""
		}
		fmt.Printf("%s project deleted\n", paint(cGreen, "✓"))
	default:
		fmt.Printf("%s unknown /projects subcommand: %s\n", paint(cRed, "✗"), args[0])
		fmt.Printf("  %s /projects [list|new|open|clear|context|plan|workflow|delete]\n", paint(cGray, "usage:"))
	}
}

func (c *Console) printAudit() {
	if c.mem == nil || c.mem.Audit() == nil {
		fmt.Println(paint(cRed, "✗ audit log not initialized"))
		return
	}
	entries := c.mem.Audit().GetRecent(50)
	if len(entries) == 0 {
		fmt.Println(paint(cGray, "  no audit entries recorded yet."))
		return
	}
	fmt.Printf("%s %s\n", paint(cAmber+clrBold, "RECENT AUDIT LOG"), paint(cGray, "("+fmtNum(len(entries))+" entries)"))
	for _, e := range entries {
		statusStr := paint(cGreen, e.Outcome)
		if e.Outcome != "allowed" {
			statusStr = paint(cRed, e.Outcome)
		}
		fmt.Printf("  %s  %-12s  %-15s  %s  %s\n",
			paint(cGray, fmtTime(e.Timestamp)),
			paint(cWhite, string(e.Agent)),
			paint(cOrange, e.Action),
			statusStr,
			paint(cGray, strutil.Truncate(e.Detail, 40)))
	}
}

func (c *Console) printLearningStats() {
	if c.mem == nil || c.mem.Learning() == nil {
		fmt.Println(paint(cRed, "✗ learning engine not initialized"))
		return
	}
	stats := c.mem.Learning().GetStats()
	strategies := c.mem.Learning().GetAllStrategies()

	fmt.Printf("%s\n", paint(cAmber+clrBold, "LEARNING ENGINE STATS"))
	fmt.Printf("  %-25s %s\n", paint(cGray, "total tasks processed"), paint(cWhite, fmtNum(stats["total_tasks"].(int))))
	fmt.Printf("  %-25s %s%%\n", paint(cGray, "success rate"), paint(cGreen, fmt.Sprintf("%.0f", stats["success_rate"].(float64)*100)))
	fmt.Printf("  %-25s %s\n", paint(cGray, "strategies discovered"), paint(cBlue, fmtNum(stats["strategy_count"].(int))))

	if len(strategies) > 0 {
		fmt.Printf("\n%s %s\n", paint(cAmber+clrBold, "STRATEGIES"), paint(cGray, "("+fmtNum(len(strategies))+")"))
		for _, s := range strategies {
			fmt.Printf("  %s  %-20s  %s\n", paint(cOrange, "★"), paint(cWhite, s.Name), paint(cGray, strutil.Truncate(s.Description, 50)))
		}
	}
}

func (c *Console) printHardwareStats() {
	hw := observability.GetHardwareStats()
	fmt.Printf("\n%s\n", paint(cAmber+clrBold, "HARDWARE RESOURCE CENTER"))
	fmt.Printf("  %-25s %s\n", paint(cGray, "OS / Arch"), paint(cWhite, hw.OS+" / "+hw.Arch))
	fmt.Printf("  %-25s %s\n", paint(cGray, "CPU Usage"), paint(cYellow, fmt.Sprintf("%.1f%%", hw.CPUUsagePercent)))
	fmt.Printf("  %-25s %s\n", paint(cGray, "RAM Usage"), paint(cYellow, fmt.Sprintf("%.0f MB / %.0f MB", hw.RAMUsedMB, hw.RAMTotalMB)))
	fmt.Printf("  %-25s %s\n", paint(cGray, "Goroutines"), paint(cBlue, fmt.Sprintf("%d", hw.GoRoutines)))
	fmt.Printf("  %-25s %s\n", paint(cGray, "Heap Alloc"), paint(cPurple, fmt.Sprintf("%.0f MB", hw.GoHeapAllocMB)))
}

func (c *Console) printHistoryFull() {
	snap := metrics.Default.Snapshot()
	fmt.Printf("\n%s\n", paint(cAmber+clrBold, "RECENT REQUESTS HISTORY"))
	if len(snap.Recent) == 0 {
		fmt.Printf("  %s\n", paint(cGray, "no recent requests"))
		return
	}
	fmt.Printf("  %-25s %-20s %-12s %-12s %s\n", paint(cGray, "Timestamp"), paint(cGray, "Model"), paint(cGray, "Tokens"), paint(cGray, "Cost"), paint(cGray, "Latency"))
	for i, r := range snap.Recent {
		if i > 20 { // max 20
			break
		}
		fmt.Printf("  %-25s %-20s %-12s %-12s %s\n",
			paint(cWhite, r.Timestamp.Format("15:04:05.000")),
			paint(cYellow, strutil.Truncate(r.Model, 18)),
			paint(cBlue, fmtNum(r.TotalTokens)),
			paint(cGreen, fmtCost(r.Cost)),
			paint(cPurple, fmtDur(r.LatencyMs)))
	}
}
