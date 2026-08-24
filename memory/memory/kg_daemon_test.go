package memory

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/darkcode/infra/core"
)

// daemonGraph builds a graph with a known structure the tests can perturb.
func daemonGraph(t *testing.T) *KnowledgeGraph {
	t.Helper()
	kg, err := NewKnowledgeGraph(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kg.Shutdown)
	for _, f := range []string{"api/a.go", "core/c.go", "cli/m.go"} {
		if err := kg.AddNode(&core.KGNode{ID: "file:" + f, Label: f, Type: core.KGNodeFile,
			Confidence: 1, Properties: map[string]string{"origin": "code_index"}}); err != nil {
			t.Fatal(err)
		}
	}
	return kg
}

// addImport wires a package dependency, creating the package node on demand.
func addImport(t *testing.T, kg *KnowledgeGraph, fromFile, pkg string) {
	t.Helper()
	id := "package:" + pkg
	if err := kg.AddNode(&core.KGNode{ID: id, Label: pkg, Type: core.KGNodePackage, Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	if err := kg.AddEdge(&core.KGEdge{From: "file:" + fromFile, To: id, Relation: core.KGRelImports, Weight: 1}); err != nil {
		t.Fatal(err)
	}
}

// The daemon's reason to exist: noticing a change nobody ran a report for.
func TestDaemonAlertsWhenACycleAppears(t *testing.T) {
	kg := daemonGraph(t)
	d := NewHealthDaemon(kg, "")

	d.Scan() // baseline
	if got := d.Alerts(); len(got) != 0 {
		t.Fatalf("the first scan raised %d alerts; everything is new then, so it must raise none", len(got))
	}

	// Introduce api → core → api.
	addImport(t, kg, "api/a.go", "example.com/m/core")
	addImport(t, kg, "core/c.go", "example.com/m/api")
	d.Scan()

	var found bool
	for _, a := range d.Alerts() {
		if a.Kind == "cycle-appeared" {
			found = true
			if a.Severity != "critical" {
				t.Errorf("a new cycle is severity %q, want critical", a.Severity)
			}
		}
	}
	if !found {
		t.Errorf("no cycle-appeared alert: %+v", d.Alerts())
	}
}

// A cycle that was already there must stay quiet, or the alerts become noise.
func TestDaemonDoesNotRepeatAStandingAlert(t *testing.T) {
	kg := daemonGraph(t)
	addImport(t, kg, "api/a.go", "example.com/m/core")
	addImport(t, kg, "core/c.go", "example.com/m/api")

	d := NewHealthDaemon(kg, "")
	d.Scan()
	d.Scan()
	d.Scan()

	for _, a := range d.Alerts() {
		if a.Kind == "cycle-appeared" {
			t.Errorf("an unchanged cycle raised a repeat alert: %+v", a)
		}
	}
}

// The CPU budget is the property that makes a background daemon acceptable.
func TestSleepForHoldsTheCPUBudget(t *testing.T) {
	d := NewHealthDaemon(nil, "")
	scan := 30 * time.Second
	for _, pct := range []int{1, 5, 10, 25, 50} {
		d.SetCPUPercent(pct)
		// The floor and ceiling deliberately override the duty cycle — a fast
		// scan must not spin and a slow one must still report. Only the
		// unclamped range exercises the arithmetic under test.
		raw := time.Duration(float64(scan) * float64(100-pct) / float64(pct))
		if raw < minScanInterval || raw > maxScanInterval {
			continue
		}
		rest := d.sleepFor(scan)
		duty := float64(scan) / float64(scan+rest) * 100
		if math.Abs(duty-float64(pct)) > 1 {
			t.Errorf("at %d%%: scan %v + rest %v is a %.1f%% duty cycle", pct, scan, rest, duty)
		}
	}
}

func TestSleepForClampsAndRejectsSillyBudgets(t *testing.T) {
	d := NewHealthDaemon(nil, "")

	d.SetCPUPercent(0) // would stall the loop
	if got := d.sleepFor(time.Second); got < minScanInterval {
		t.Errorf("a zero budget produced a %v sleep, under the %v floor", got, minScanInterval)
	}
	d.SetCPUPercent(1000) // no longer a background task
	if got := d.sleepFor(time.Hour); got > maxScanInterval {
		t.Errorf("sleep %v exceeds the %v ceiling", got, maxScanInterval)
	}
	// An instant scan must not spin.
	d.SetCPUPercent(5)
	if got := d.sleepFor(0); got < minScanInterval {
		t.Errorf("an instant scan slept %v, under the %v floor", got, minScanInterval)
	}
}

// The series has to survive a restart or there is no trend to read.
func TestDaemonHistoryPersists(t *testing.T) {
	dir := t.TempDir()
	kg := daemonGraph(t)

	first := NewHealthDaemon(kg, dir)
	first.Scan()
	first.Scan()
	want := len(first.History())
	if want != 2 {
		t.Fatalf("recorded %d samples, want 2", want)
	}

	second := NewHealthDaemon(kg, dir)
	if got := len(second.History()); got != want {
		t.Errorf("after restart the series holds %d samples, want %d", got, want)
	}
}

func TestDaemonStartStopIsIdempotent(t *testing.T) {
	d := NewHealthDaemon(daemonGraph(t), "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d.Start(ctx)
	d.Start(ctx) // must not start a second loop
	if !d.Running() {
		t.Fatal("daemon reports not running after Start")
	}
	d.Stop()
	d.Stop() // must not panic
	if d.Running() {
		t.Error("daemon still reports running after Stop")
	}
}

func TestDaemonAlertCallbackFires(t *testing.T) {
	kg := daemonGraph(t)
	d := NewHealthDaemon(kg, "")

	got := make(chan Alert, 8)
	d.OnAlert(func(a Alert) { got <- a })

	d.Scan()
	addImport(t, kg, "api/a.go", "example.com/m/core")
	addImport(t, kg, "core/c.go", "example.com/m/api")
	d.Scan()

	select {
	case a := <-got:
		if a.Kind == "" {
			t.Error("callback received an empty alert")
		}
	case <-time.After(2 * time.Second):
		t.Error("the alert callback never fired")
	}
}

// --- forecasting (#89) ---

// seed builds a daemon whose series is fabricated, so the fit is checkable
// against a known answer rather than whatever the graph happens to do.
func seedHistory(samples ...HealthSample) *HealthDaemon {
	d := NewHealthDaemon(nil, "")
	d.history = samples
	return d
}

func sample(daysAgo int, score float64, counts map[string]int) HealthSample {
	return HealthSample{
		At:     time.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour),
		Score:  score,
		Counts: counts,
	}
}

func TestForecastNeedsEnoughSamples(t *testing.T) {
	d := seedHistory(sample(2, 90, nil), sample(1, 88, nil))
	f := d.Forecast()
	if f.Generated {
		t.Error("a two-sample series produced a projection")
	}
	if f.Note == "" {
		t.Error("refusing to forecast should say why")
	}
}

// A steady decline must be detected, and dated.
func TestForecastDetectsAFallingScore(t *testing.T) {
	var samples []HealthSample
	for i := 10; i >= 0; i-- {
		samples = append(samples, sample(i, 60+float64(i), nil)) // falls 1/day
	}
	f := seedHistory(samples...).Forecast()
	if !f.Generated {
		t.Fatalf("no forecast: %s", f.Note)
	}

	var score *Trend
	for i := range f.Trends {
		if f.Trends[i].Metric == "health-score" {
			score = &f.Trends[i]
		}
	}
	if score == nil {
		t.Fatal("no health-score trend")
	}
	if score.PerDay > -0.5 {
		t.Errorf("slope = %.3f/day, want about -1", score.PerDay)
	}
	if score.Fit < 0.9 {
		t.Errorf("R² = %.2f on a straight line, want near 1", score.Fit)
	}
	if score.CrossesAt.IsZero() {
		t.Error("a falling score heading for the floor should carry a date")
	}
	if len(f.Warnings) == 0 {
		t.Error("a projected floor crossing should warn")
	}
}

// Noise must not be dressed up as a trend — the property that makes the
// forecast worth believing.
func TestForecastReportsNoTrendForNoise(t *testing.T) {
	noisy := []float64{80, 62, 91, 55, 88, 60, 85, 58, 90, 63}
	var samples []HealthSample
	for i, v := range noisy {
		samples = append(samples, sample(len(noisy)-i, v, nil))
	}
	f := seedHistory(samples...).Forecast()
	for _, tr := range f.Trends {
		if tr.Metric != "health-score" {
			continue
		}
		if tr.Fit >= fitWeak {
			t.Fatalf("noise fitted at R²=%.2f, above the %.2f bar", tr.Fit, fitWeak)
		}
		if tr.Verdict != "no discernible trend" {
			t.Errorf("verdict = %q, want an explicit refusal", tr.Verdict)
		}
		if tr.PerDay != 0 {
			t.Errorf("a rejected trend still reported a slope of %v", tr.PerDay)
		}
	}
}

// A metric that never moves is not a trend, however perfectly a flat line fits.
func TestForecastTreatsAConstantAsNoTrend(t *testing.T) {
	var samples []HealthSample
	for i := 8; i >= 0; i-- {
		samples = append(samples, sample(i, 75, map[string]int{"dead-code": 3}))
	}
	for _, tr := range seedHistory(samples...).Forecast().Trends {
		if tr.Fit >= fitWeak {
			t.Errorf("%s: a constant fitted at R²=%.2f", tr.Metric, tr.Fit)
		}
	}
}

func TestFitLineRecoversAKnownSlope(t *testing.T) {
	var xs, ys []float64
	for i := 0; i < 10; i++ {
		xs = append(xs, float64(i))
		ys = append(ys, 3*float64(i)+7)
	}
	slope, intercept, r2 := fitLine(xs, ys)
	if math.Abs(slope-3) > 1e-9 || math.Abs(intercept-7) > 1e-9 {
		t.Errorf("fit = %.4fx + %.4f, want 3x + 7", slope, intercept)
	}
	if math.Abs(r2-1) > 1e-9 {
		t.Errorf("R² = %.6f on an exact line, want 1", r2)
	}
}

// A rising count of findings is worsening; a rising score is improving. The
// same slope must not read the same way for both.
func TestForecastDirectionDependsOnTheMetric(t *testing.T) {
	var samples []HealthSample
	for i := 8; i >= 0; i-- {
		// score climbs, dead code climbs too
		samples = append(samples, sample(i, 50+float64(8-i)*2, map[string]int{"dead-code": 8 - i}))
	}
	f := seedHistory(samples...).Forecast()

	for _, tr := range f.Trends {
		switch tr.Metric {
		case "health-score":
			if !strings.HasPrefix(tr.Verdict, "improving") {
				t.Errorf("a rising score reads as %q, want improving", tr.Verdict)
			}
		case "dead-code":
			if !strings.HasPrefix(tr.Verdict, "worsening") {
				t.Errorf("rising dead code reads as %q, want worsening", tr.Verdict)
			}
		}
	}
}
