package cli

// ============================================================================
// LIVE MONITORING DASHBOARD
//
// A full-screen terminal dashboard that redraws in place using ANSI cursor
// control. It is the single, unified view for real-time observability:
//
//   • KPI strip         — tokens, cost, requests, errors, latency, RPM
//   • Token sparkline   — per-minute token throughput over time
//   • Per-model bars    — horizontal bar chart of token share by model
//   • Cost sparkline    — cumulative cost trend
//   • Event ticker      — the most recent orchestrator events
//
// Opened via the /monitor slash command. Exits on 'q', Esc, or Ctrl+C.
// ============================================================================

import (
	"context"
	"fmt"
	"github.com/darkcode/internal/strutil"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/darkcode/core"
	"github.com/darkcode/metrics"
	"github.com/darkcode/ui"
	"github.com/darkcode/capability"
)

// dashboardEvent is a trimmed event record kept in the ticker ring.
type dashboardEvent struct {
	time time.Time
	icon string
	kind string
	msg  string
}

// runDashboard opens the full-screen live monitoring view.
// It blocks until the user presses q/Esc/Ctrl+C.
func (c *Console) runDashboard() {
	// Buffer recent events from the emitter while the dashboard is open.
	var evRing []dashboardEvent
	const evCap = 40

	handler := func(e core.UIEvent) {
		ev := dashboardEvent{time: e.Timestamp, icon: eventIcon(string(e.Type)), kind: string(e.Type), msg: eventMessage(e)}
		evRing = append(evRing, ev)
		if len(evRing) > evCap {
			evRing = evRing[len(evRing)-evCap:]
		}
	}
	c.emitter.OnHandler(handler)
	defer c.removeHandler(handler)

	// Ctrl+C handling: first interrupt exits the dashboard (not the program).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	defer signal.Stop(sigCh)

	// Raw key reader on stdin so 'q'/Esc exits immediately.
	keyCh := make(chan byte, 4)
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				return
			}
			keyCh <- buf[0]
		}
	}()

	hideCursor()
	defer showCursor()
	restore := makeRaw()
	defer restore()
	clearScreen()

	ticker := time.NewTicker(800 * time.Millisecond)
	defer ticker.Stop()

	for {
		// Render one frame.
		frame := c.renderDashboardFrame(evRing)
		fmt.Print(ansiHome)
		fmt.Print(frame)

		select {
		case k := <-keyCh:
			if k == 'q' || k == 'Q' || k == 27 /*Esc*/ || k == 3 /*Ctrl+C*/ {
				clearScreen()
				return
			}
			if k == 'r' || k == 'R' {
				metrics.Default.Reset()
			}
		case <-sigCh:
			clearScreen()
			return
		case <-ticker.C:
		}
	}
}

// removeHandler removes a previously-registered event handler.
func (c *Console) removeHandler(target ui.EventHandler) {
	c.emitter.RemoveHandler(target)
}

// renderDashboardFrame builds the full dashboard string for one refresh.
func (c *Console) renderDashboardFrame(evRing []dashboardEvent) string {
	w := termWidth()
	if w < 80 {
		w = 80
	}
	if w > 110 {
		w = 110
	}
	snap := metrics.Default.Snapshot()
	now := time.Now()

	var b strings.Builder

	// ---- Header ----
	caps, err := capability.Detect(context.Background())
	var tier capability.ExecutionTier
	if err == nil {
		tier = capability.AssignTier(caps)
	}
	
	title := paint(cOrange+clrBold, " DARKCODE ") + paint(cAmber, "· LIVE MONITORING · ")
	if err == nil {
		title += paint(cPurple, tier.String())
	}
	hint := paint(cGray, "[q] quit   [r] reset   [Esc] back")
	clock := paint(cGray, now.Format("15:04:05"))
	headerPad := w - visibleLen(title) - visibleLen(hint) - visibleLen(clock) - 2
	if headerPad < 1 {
		headerPad = 1
	}
	b.WriteString(paint(cOrange, tl) + title + paint(cOrange, strings.Repeat(hz, headerPad)) + clock + " " + hint + paint(cOrange, tr))
	b.WriteString("\n")

	// ---- KPI strip ----
	rpm := 0
	if len(snap.Series) > 0 {
		rpm = snap.Series[len(snap.Series)-1].Requests
	}
	kpis := []struct {
		label string
		value string
		color string
	}{
		{"TOKENS", fmtNum(snap.TotalTokens), cOrange},
		{"PROMPT", fmtNum(snap.TotalPrompt), cBlue},
		{"COMPLETION", fmtNum(snap.TotalCompletion), cAmber},
		{"COST", fmtCost(snap.TotalCost), cGreen},
		{"REQUESTS", fmtNum(snap.TotalRequests), cWhite},
		{"ERRORS", fmtNum(snap.TotalErrors), ternaryColor(snap.TotalErrors == 0, cGreen, cRed)},
		{"AVG LAT", fmtDur(int64(snap.AvgLatencyMs)), cYellow},
		{"RPM", fmtNum(rpm), cPurple},
	}
	colW := (w - 2) / len(kpis)
	var kpiLine strings.Builder
	kpiLine.WriteString(paint(cOrange, vt))
	for _, k := range kpis {
		cell := fmt.Sprintf("%s\n%s", paint(cGray, k.label), paint(k.color+clrBold, center(k.value, colW-2)))
		// single-line cell instead:
		cell = paint(cGray, padRight(k.label, colW-4-len(k.value))) + paint(k.color+clrBold, k.value)
		kpiLine.WriteString(" " + cell + " ")
	}
	// Use a simpler single-line KPI layout
	kpiLine.Reset()
	kpiLine.WriteString(paint(cOrange, vt))
	for i, k := range kpis {
		cell := paint(cGray, k.label+" ") + paint(k.color+clrBold, k.value)
		kpiLine.WriteString(cell)
		if i < len(kpis)-1 {
			kpiLine.WriteString(paint(cGray, "  │  "))
		}
	}
	pad := w - 2 - visibleLen(kpiLine.String())
	if pad > 0 {
		kpiLine.WriteString(strings.Repeat(" ", pad))
	}
	b.WriteString(kpiLine.String() + paint(cOrange, vt) + "\n")
	b.WriteString(paint(cOrange, ml) + strings.Repeat(hz, w-2) + mr + "\n")

	// ---- Usage Stats ----
	series := snap.Series
	var peakTok, currentTok int
	var currentCost float64
	if len(series) > 0 {
		currentTok = series[len(series)-1].TotalTokens
		currentCost = series[len(series)-1].Cost
		for _, s := range series {
			if s.TotalTokens > peakTok {
				peakTok = s.TotalTokens
			}
		}
	}

	b.WriteString(renderRow(w, "TOKEN THROUGHPUT", 
		fmt.Sprintf("Current: %s/min  |  Peak: %s/min", paint(cOrange, fmtNum(currentTok)), paint(cOrange, fmtNum(peakTok)))))
	b.WriteString(renderRow(w, "COST TREND", 
		fmt.Sprintf("Current: %s/min  |  Cumulative: %s", paint(cGreen, fmtCost(currentCost)), paint(cGreen, fmtCost(snap.TotalCost)))))

	b.WriteString(paint(cOrange, ml) + strings.Repeat(hz, w-2) + mr + "\n")

	// ---- Per-model breakdown ----
	b.WriteString(renderSectionHeader(w, "TOKENS BY MODEL"))
	models := snap.PerModel
	if len(models) == 0 {
		b.WriteString(renderRow(w, "", paint(cGray, "  no LLM requests recorded yet — send a message to generate telemetry")))
	} else {
		for _, m := range models {
			label := m.Model
			if m.Provider != "" {
				label = m.Provider + "/" + m.Model
			}
			textRow := fmt.Sprintf("%-24s %12s tokens", paint(cAmber, label), paint(cWhite, fmtNum(m.TotalTokens)))
			b.WriteString(renderRow(w, "", textRow))
		}
	}

	b.WriteString(paint(cOrange, ml) + strings.Repeat(hz, w-2) + mr + "\n")

	// ---- Event ticker ----
	b.WriteString(renderSectionHeader(w, "RECENT EVENTS"))
	if len(evRing) == 0 {
		b.WriteString(renderRow(w, "", paint(cGray, "  awaiting orchestrator events…")))
	} else {
		// show newest first, up to ~8 rows
		n := len(evRing)
		if n > 8 {
			n = 8
		}
		for i := 0; i < n; i++ {
			ev := evRing[len(evRing)-1-i]
			line := fmt.Sprintf("%s  %s  %s  %s",
				paint(cGray, ev.time.Format("15:04:05")),
				paint(cOrange, ev.icon),
				paint(cBlue, padRight(ev.kind, 16)),
				strutil.Truncate(ev.msg, w-32))
			b.WriteString(renderRow(w, "", line))
		}
	}

	// ---- Footer ----
	b.WriteString(paint(cOrange, bl) + strings.Repeat(hz, w-2) + br + "\n")
	return b.String()
}

// renderRow wraps a content line in vertical borders for the given width.
func renderRow(w int, label, content string) string {
	rem := w - 2 - visibleLen(content)
	if rem < 0 {
		rem = 0
	}
	labelPart := ""
	if label != "" {
		labelPart = paint(cGray, " ") + paint(cGrayLt, label) + paint(cGray, " · ")
	}
	return paint(cOrange, vt) + " " + labelPart + content + strings.Repeat(" ", rem) + " " + paint(cOrange, vt) + "\n"
}

// renderSectionHeader renders a divider row with an embedded title.
func renderSectionHeader(w int, title string) string {
	inner := " " + paint(cAmber+clrBold, title) + " "
	rem := w - 2 - visibleLen(inner)
	if rem < 0 {
		rem = 0
	}
	return paint(cOrange, ml) + inner + paint(cOrange, strings.Repeat(hz, rem)) + mr + "\n"
}

// ---- event helpers ----

func eventIcon(t string) string {
	switch t {
	case "task_update":
		return "►"
	case "agent_spawn":
		return "✦"
	case "agent_complete":
		return "✓"
	case "tool_execution":
		return "⚡"
	case "model_route":
		return "↓"
	case "compression":
		return "⊕"
	case "memory_store":
		return "⟐"
	case "final_output":
		return "▣"
	case "error":
		return "✗"
	case "dag_update":
		return "⬡"
	case "skill_extract":
		return "★"
	case "consensus":
		return "⚖"
	case "token_usage":
		return "🪙"
	default:
		return "•"
	}
}

func eventMessage(e core.UIEvent) string {
	switch e.Type {
	case core.EventTaskUpdate:
		return fmt.Sprintf("%s — %v", e.Status, e.Content)
	case core.EventAgentSpawn:
		return fmt.Sprintf("%s spawned: %v", e.Agent, e.Content)
	case core.EventAgentComplete:
		return fmt.Sprintf("%s %s", e.Agent, e.Status)
	case core.EventToolExecution:
		return fmt.Sprintf("%s %s", e.Tool, e.Status)
	case core.EventModelRoute:
		return fmt.Sprintf("%s mode — %v", e.Status, e.Content)
	case core.EventCompression:
		return fmt.Sprintf("%v", e.Content)
	case core.EventMemoryStore:
		return fmt.Sprintf("%s: %v", e.Status, e.Content)
	case core.EventFinalOutput:
		return "final answer ready"
	case core.EventError:
		return fmt.Sprintf("%v", e.Content)
	case core.EventDAGUpdate:
		return fmt.Sprintf("dag — %v", e.Content)
	case core.EventSkillExtract:
		return fmt.Sprintf("skill: %v", e.Content)
	case core.EventConsensus:
		return fmt.Sprintf("%v", e.Content)
	case core.EventTokenUsage:
		return fmt.Sprintf("%v", e.Content)
	default:
		return fmt.Sprintf("%v", e.Content)
	}
}

// ---- small numeric helpers ----

func maxFloat(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

// downsample reduces a series to at most n points by averaging windows.
func downsample(v []float64, n int) []float64 {
	if len(v) <= n || n <= 0 {
		return v
	}
	out := make([]float64, 0, n)
	window := float64(len(v)) / float64(n)
	for i := 0; i < n; i++ {
		start := int(float64(i) * window)
		end := int(float64(i+1) * window)
		if end > len(v) {
			end = len(v)
		}
		if start >= end {
			out = append(out, v[len(v)-1])
			continue
		}
		sum := 0.0
		cnt := 0
		for j := start; j < end; j++ {
			sum += v[j]
			cnt++
		}
		out = append(out, sum/float64(cnt))
	}
	return out
}

func ternaryColor(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
