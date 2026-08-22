package memory

// kg_daemon.go — repository health, watched rather than asked for.
//
// Health() already answers "what is structurally wrong here", but only when
// somebody asks. The interesting signal is not the score, it is the *change*:
// a cycle that appeared this week, coupling that has been climbing for a
// month, a hotspot that just lost its last test. Nobody runs a report often
// enough to notice those, which is what a daemon is for.
//
// The report frames this as something a local-first agent can do and a
// cloud-first one cannot, because the compute is free — but only if it stays
// out of the way. So the loop is built around a CPU budget rather than a
// schedule: it measures how long a scan took and sleeps proportionally, so it
// cannot exceed the configured share of one core no matter how large the
// repository grows. A daemon that makes the editor stutter gets switched off,
// and then it protects nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// DefaultCPUPercent is the share of a single core the daemon may use. The
// report's target is 5%; that is low enough to be unnoticeable on a laptop and
// still gets through a large repository many times an hour.
const DefaultCPUPercent = 5

// minScanInterval floors the sleep so a repository that scans instantly does
// not spin. Structure does not change fast enough for sub-minute polling to
// tell anyone anything.
const minScanInterval = time.Minute

// maxScanInterval caps the sleep so a very slow scan still reports daily
// rather than effectively never.
const maxScanInterval = time.Hour

// Alert is a threshold crossing worth telling someone about (report #90).
//
// Alerts are raised on transitions, not on states: a repository that has had
// the same three cycles for a year should be silent, because a warning that
// fires every scan is one nobody reads.
type Alert struct {
	Kind     string    `json:"kind"`     // cycle-appeared | score-dropped | hotspot-untested | dead-code-grew
	Subject  string    `json:"subject"`  // what it concerns
	Detail   string    `json:"detail"`   // human-readable change
	Severity string    `json:"severity"` // info | warning | critical
	At       time.Time `json:"at"`
}

// HealthSample is one scan's result, kept as the time series that trend
// detection and forecasting read.
type HealthSample struct {
	At       time.Time      `json:"at"`
	Score    float64        `json:"score"`
	Files    int            `json:"files"`
	Symbols  int            `json:"symbols"`
	Counts   map[string]int `json:"counts"`
	Cycles   []string       `json:"cycles,omitempty"` // subjects, for appearance detection
	ScanTime time.Duration  `json:"scan_ns"`
}

// HealthDaemon watches a repository's structure in the background.
type HealthDaemon struct {
	kg *KnowledgeGraph

	mu         sync.Mutex
	history    []HealthSample
	alerts     []Alert
	cpuPercent int
	path       string // where the series is persisted, "" for memory only
	onAlert    func(Alert)
	running    bool
	cancel     context.CancelFunc
}

// maxHistory bounds the retained series. At the daemon's cadence this is
// weeks of data, which is enough for a trend and small enough to keep in
// memory and rewrite atomically.
const maxHistory = 500

// NewHealthDaemon builds a daemon over kg. dir is where the series persists so
// a trend survives a restart; pass "" to keep it in memory.
func NewHealthDaemon(kg *KnowledgeGraph, dir string) *HealthDaemon {
	d := &HealthDaemon{kg: kg, cpuPercent: DefaultCPUPercent}
	if dir != "" {
		d.path = filepath.Join(dir, "health_history.json")
		d.load()
	}
	return d
}

// SetCPUPercent bounds the share of one core the daemon may consume. Values
// outside 1..50 are clamped: zero would stall the loop and anything above half
// a core stops being a background task.
func (d *HealthDaemon) SetCPUPercent(p int) {
	if p < 1 {
		p = 1
	}
	if p > 50 {
		p = 50
	}
	d.mu.Lock()
	d.cpuPercent = p
	d.mu.Unlock()
}

// OnAlert registers a callback invoked for each new alert, so a server can
// push it to a UI. It is called off the scan path and must not block.
func (d *HealthDaemon) OnAlert(fn func(Alert)) {
	d.mu.Lock()
	d.onAlert = fn
	d.mu.Unlock()
}

// Start runs the watch loop until ctx is cancelled or Stop is called. It
// returns immediately; a second call while running is a no-op.
func (d *HealthDaemon) Start(ctx context.Context) {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	d.running, d.cancel = true, cancel
	d.mu.Unlock()

	go d.loop(ctx)
}

// Stop ends the watch loop. Safe to call when not running.
func (d *HealthDaemon) Stop() {
	d.mu.Lock()
	cancel := d.cancel
	d.running, d.cancel = false, nil
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Running reports whether the watch loop is active.
func (d *HealthDaemon) Running() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running
}

// loop scans, then sleeps long enough to hold the CPU budget.
func (d *HealthDaemon) loop(ctx context.Context) {
	for {
		start := time.Now()
		d.Scan()
		elapsed := time.Since(start)

		select {
		case <-ctx.Done():
			return
		case <-time.After(d.sleepFor(elapsed)):
		}
	}
}

// sleepFor converts a scan duration into the pause that keeps the daemon
// within its budget. Working for `elapsed` at a p% duty cycle means resting
// for elapsed*(100-p)/p — so a scan costing one second at 5% buys nineteen
// seconds of quiet, and a repository ten times larger simply scans ten times
// less often instead of using ten times the CPU.
func (d *HealthDaemon) sleepFor(elapsed time.Duration) time.Duration {
	d.mu.Lock()
	p := d.cpuPercent
	d.mu.Unlock()

	rest := time.Duration(float64(elapsed) * float64(100-p) / float64(p))
	if rest < minScanInterval {
		return minScanInterval
	}
	if rest > maxScanInterval {
		return maxScanInterval
	}
	return rest
}

// Scan takes one health sample, records it, and raises alerts for what
// changed since the previous one. Exported so a caller can force a scan
// without waiting for the loop, and so tests need no timing.
func (d *HealthDaemon) Scan() HealthSample {
	start := time.Now()
	rep := d.kg.Health()

	sample := HealthSample{
		At: start, Score: rep.Score, Files: rep.Files, Symbols: rep.Symbols,
		Counts: rep.Counts, ScanTime: time.Since(start),
	}
	for _, f := range rep.Findings {
		if f.Kind == "import-cycle" {
			sample.Cycles = append(sample.Cycles, f.Detail)
		}
	}
	sort.Strings(sample.Cycles)

	d.mu.Lock()
	var previous *HealthSample
	if len(d.history) > 0 {
		previous = &d.history[len(d.history)-1]
	}
	fresh := diffSamples(previous, sample)
	d.history = append(d.history, sample)
	if len(d.history) > maxHistory {
		d.history = d.history[len(d.history)-maxHistory:]
	}
	d.alerts = append(d.alerts, fresh...)
	if len(d.alerts) > maxHistory {
		d.alerts = d.alerts[len(d.alerts)-maxHistory:]
	}
	notify := d.onAlert
	d.mu.Unlock()

	d.save()
	if notify != nil {
		for _, a := range fresh {
			notify(a)
		}
	}
	return sample
}

// scoreDropAlert is the fall in health score that is worth interrupting
// someone for. Small movements are noise from re-indexing.
const scoreDropAlert = 5.0

// diffSamples raises alerts for what changed between two scans. The first
// scan of a repository raises nothing: everything is "new" then, and a wall of
// alerts on first run teaches people to ignore them.
func diffSamples(prev *HealthSample, cur HealthSample) []Alert {
	if prev == nil {
		return nil
	}
	var out []Alert

	for _, c := range cur.Cycles {
		if !containsString(prev.Cycles, c) {
			out = append(out, Alert{
				Kind: "cycle-appeared", Subject: c, At: cur.At, Severity: "critical",
				Detail: "a new import cycle appeared: " + c,
			})
		}
	}

	if drop := prev.Score - cur.Score; drop >= scoreDropAlert {
		out = append(out, Alert{
			Kind: "score-dropped", Subject: "repository", At: cur.At, Severity: "warning",
			Detail: fmt.Sprintf("health score fell %.1f points, %.1f → %.1f", drop, prev.Score, cur.Score),
		})
	}

	for kind, now := range cur.Counts {
		was := prev.Counts[kind]
		if now <= was {
			continue
		}
		severity := "info"
		if kind == "untested-hotspot" {
			severity = "warning"
		}
		out = append(out, Alert{
			Kind: kind + "-grew", Subject: kind, At: cur.At, Severity: severity,
			Detail: fmt.Sprintf("%s findings rose from %d to %d", kind, was, now),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return severityRank(out[i].Severity) < severityRank(out[j].Severity)
	})
	return out
}

func severityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "warning":
		return 1
	default:
		return 2
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// History returns the recorded samples, oldest first.
func (d *HealthDaemon) History() []HealthSample {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]HealthSample(nil), d.history...)
}

// Alerts returns the alerts raised so far, oldest first.
func (d *HealthDaemon) Alerts() []Alert {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Alert(nil), d.alerts...)
}

// persisted is the on-disk shape of the daemon's memory.
type persisted struct {
	History []HealthSample `json:"history"`
	Alerts  []Alert        `json:"alerts"`
}

func (d *HealthDaemon) save() {
	if d.path == "" {
		return
	}
	d.mu.Lock()
	blob, err := json.Marshal(persisted{History: d.history, Alerts: d.alerts})
	d.mu.Unlock()
	if err != nil {
		return
	}
	// Write-then-rename: a crash mid-write must not leave a truncated series
	// that fails to parse on the next start.
	tmp := d.path + ".tmp"
	if os.WriteFile(tmp, blob, 0o600) == nil {
		_ = os.Rename(tmp, d.path)
	}
}

func (d *HealthDaemon) load() {
	blob, err := os.ReadFile(d.path)
	if err != nil {
		return // no history yet is the normal first run
	}
	var p persisted
	if json.Unmarshal(blob, &p) != nil {
		return // corrupt series: start over rather than refuse to run
	}
	d.history, d.alerts = p.History, p.Alerts
}
