package memory

// kg_forecast.go — reading the direction of travel (report #89).
//
// The health daemon accumulates a time series. A single score answers "how is
// this repository today"; the series answers the more useful question, "where
// is it heading, and when does it get somewhere I care about".
//
// The model is ordinary least squares on each metric against time. That is a
// deliberate choice, not a placeholder: a straight line is the most that a few
// dozen noisy samples of a human process can support, it extrapolates
// honestly, and — the part that matters — its confidence is checkable. Fitting
// something more elaborate would produce a more impressive-looking projection
// that no one could tell was wrong.
//
// So every projection carries R², and one below fitWeak is reported as "no
// discernible trend" rather than dressed up with a date. A forecast nobody can
// audit is worse than none, because it gets believed.

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// fitWeak is the R² below which a trend is not worth reporting. Structural
// metrics move in steps — a refactor lands, a package splits — so a genuine
// trend still fits loosely; this bar is set to exclude noise, not to demand a
// clean line.
const fitWeak = 0.3

// minSamplesToForecast is the shortest series that can support a projection.
// Two points always fit a line perfectly and predict nothing.
const minSamplesToForecast = 5

// Trend is the fitted direction of one metric.
type Trend struct {
	Metric string `json:"metric"`
	// PerDay is the fitted change per day: negative means falling.
	PerDay  float64 `json:"per_day"`
	Current float64 `json:"current"`
	// Fit is R² in [0,1]; below fitWeak the trend is not reported.
	Fit     float64 `json:"fit"`
	Samples int     `json:"samples"`
	// Span is how much wall-clock time the series covers.
	Span time.Duration `json:"span_ns"`
	// Verdict describes the movement in words, including when it has none.
	Verdict string `json:"verdict"`
	// CrossesAt is when the metric is projected to reach Threshold, zero when
	// it is not heading there.
	CrossesAt time.Time `json:"crosses_at,omitempty"`
	Threshold float64   `json:"threshold,omitempty"`
}

// Forecast is the projection over every tracked metric.
type Forecast struct {
	Trends    []Trend  `json:"trends"`
	Generated bool     `json:"generated"` // false when the series is too short
	Note      string   `json:"note,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// healthScoreFloor is the score at which a repository is worth warning about
// ahead of time, so a falling trend can be given a date.
const healthScoreFloor = 50

// Forecast projects where the repository's structure is heading.
func (d *HealthDaemon) Forecast() Forecast {
	history := d.History()
	if len(history) < minSamplesToForecast {
		return Forecast{Note: fmt.Sprintf(
			"only %d samples; a projection needs at least %d", len(history), minSamplesToForecast)}
	}

	// Time is measured in days from the first sample, so a slope reads as
	// "per day" regardless of how often the daemon happened to run.
	origin := history[0].At
	days := func(t time.Time) float64 { return t.Sub(origin).Hours() / 24 }

	span := history[len(history)-1].At.Sub(origin)
	if span <= 0 {
		return Forecast{Note: "every sample carries the same timestamp; no time has passed to project over"}
	}

	f := Forecast{Generated: true}

	series := map[string][]float64{"health-score": nil}
	var xs []float64
	for _, s := range history {
		xs = append(xs, days(s.At))
		series["health-score"] = append(series["health-score"], s.Score)
	}
	// Every issue kind the daemon has ever counted becomes its own series. A
	// kind absent from a sample counts as zero, which is what it means.
	for _, s := range history {
		for kind := range s.Counts {
			if _, seen := series[kind]; !seen {
				series[kind] = nil
			}
		}
	}
	for kind := range series {
		if kind == "health-score" {
			continue
		}
		vals := make([]float64, 0, len(history))
		for _, s := range history {
			vals = append(vals, float64(s.Counts[kind]))
		}
		series[kind] = vals
	}

	names := make([]string, 0, len(series))
	for k := range series {
		names = append(names, k)
	}
	sort.Strings(names) // deterministic output

	for _, name := range names {
		ys := series[name]
		slope, intercept, r2 := fitLine(xs, ys)
		t := Trend{
			Metric: name, PerDay: slope, Current: ys[len(ys)-1],
			Fit: r2, Samples: len(ys), Span: span,
		}

		switch {
		case r2 < fitWeak:
			t.Verdict = "no discernible trend"
			t.PerDay = 0 // do not report a slope the data does not support
		case name == "health-score":
			t.Verdict = describeSlope(slope, "points", true)
			if slope < 0 && t.Current > healthScoreFloor {
				t.Threshold = healthScoreFloor
				t.CrossesAt = crossing(origin, slope, intercept, healthScoreFloor)
			}
		default:
			t.Verdict = describeSlope(slope, "findings", false)
		}
		if !t.CrossesAt.IsZero() {
			f.Warnings = append(f.Warnings, fmt.Sprintf(
				"health score reaches %.0f around %s at the current rate",
				t.Threshold, t.CrossesAt.Format("2006-01-02")))
		}
		f.Trends = append(f.Trends, t)
	}
	return f
}

// describeSlope renders a rate in words. higherIsBetter flips which direction
// counts as improvement, since a rising score is good and rising findings are
// not.
func describeSlope(perDay float64, unit string, higherIsBetter bool) string {
	rate := math.Abs(perDay)
	if rate < 0.01 {
		return "stable"
	}
	rising := perDay > 0
	good := rising == higherIsBetter
	direction := "falling"
	if rising {
		direction = "rising"
	}
	quality := "worsening"
	if good {
		quality = "improving"
	}
	return fmt.Sprintf("%s (%s %.2f %s/day)", quality, direction, rate, unit)
}

// crossing returns when the fitted line reaches target.
func crossing(origin time.Time, slope, intercept, target float64) time.Time {
	if slope == 0 {
		return time.Time{}
	}
	atDay := (target - intercept) / slope
	if atDay <= 0 || math.IsInf(atDay, 0) || math.IsNaN(atDay) {
		return time.Time{}
	}
	// A projection further out than a year is arithmetic, not information.
	if atDay > 365 {
		return time.Time{}
	}
	return origin.Add(time.Duration(atDay * float64(24*time.Hour)))
}

// fitLine is ordinary least squares, returning slope, intercept and R².
//
// R² is 0 when the metric never moves: a flat line fits perfectly in the
// least-squares sense, but "perfectly predicts a constant" must not be
// reported as a strong trend, and callers threshold on this value.
func fitLine(xs, ys []float64) (slope, intercept, r2 float64) {
	n := float64(len(xs))
	if n < 2 {
		return 0, 0, 0
	}
	var sumX, sumY float64
	for i := range xs {
		sumX += xs[i]
		sumY += ys[i]
	}
	meanX, meanY := sumX/n, sumY/n

	var sxx, sxy, syy float64
	for i := range xs {
		dx, dy := xs[i]-meanX, ys[i]-meanY
		sxx += dx * dx
		sxy += dx * dy
		syy += dy * dy
	}
	if sxx == 0 || syy == 0 {
		return 0, meanY, 0 // no spread in time, or a metric that never moved
	}
	slope = sxy / sxx
	intercept = meanY - slope*meanX
	r := sxy / math.Sqrt(sxx*syy)
	return slope, intercept, r * r
}

// Format renders a forecast for a human or a model.
func (f Forecast) Format() string {
	if !f.Generated {
		return "no forecast: " + f.Note
	}
	out := "structural trends\n"
	for _, t := range f.Trends {
		out += fmt.Sprintf("  %-20s %-40s (R²=%.2f, n=%d)\n", t.Metric, t.Verdict, t.Fit, t.Samples)
	}
	for _, w := range f.Warnings {
		out += "\n⚠ " + w
	}
	return out
}
