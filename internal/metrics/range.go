package metrics

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Bounds of a chart or explorer request.
const (
	MaxSpan   = 30 * 24 * time.Hour // VictoriaMetrics keeps 30 days
	MinSpan   = 5 * time.Minute
	MaxPoints = 500
	// MinStep is a bit above vmagent's scrape interval (20s) and the
	// recording rules' evaluation interval (30s).
	MinStep = 30 * time.Second
	// targetPoints is what a chart gets without an explicit step.
	targetPoints = 240
)

// niceSteps are the steps a range rounds up to, so that charts line up with
// the clock and VictoriaMetrics' cache.
var niceSteps = []time.Duration{
	30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute,
	30 * time.Minute, time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

var spanRE = regexp.MustCompile(`^([1-9][0-9]{0,4})(s|m|h|d|w)$`)

// ParseDuration reads "90s", "15m", "6h", "7d" or "2w".
func ParseDuration(s string) (time.Duration, error) {
	m := spanRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not a duration like 15m, 6h or 7d", s)
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, nil
}

// NewRange is the window [now-span, now] with a step that keeps the number
// of points at or below MaxPoints. step 0 picks one for about 240 points;
// a smaller step than the bounds allow is raised, not refused.
func NewRange(now time.Time, span, step time.Duration) (Range, error) {
	if span < MinSpan || span > MaxSpan {
		return Range{}, fmt.Errorf("the time range must be between 5 minutes and 30 days")
	}
	if step < 0 {
		return Range{}, fmt.Errorf("the step must be positive")
	}
	if step == 0 {
		step = nice(span / targetPoints)
	}
	if minimum := nice(ceilDiv(span, MaxPoints)); step < minimum {
		step = minimum
	}
	step = max(step, MinStep).Truncate(time.Second)
	end := now.Truncate(time.Second)
	return Range{Start: end.Add(-span), End: end, Step: step}, nil
}

func ceilDiv(d time.Duration, n int) time.Duration {
	return (d + time.Duration(n) - 1) / time.Duration(n)
}

// nice rounds d up to the next of niceSteps (or whole days beyond them).
func nice(d time.Duration) time.Duration {
	for _, s := range niceSteps {
		if d <= s {
			return s
		}
	}
	day := 24 * time.Hour
	return (d + day - 1) / day * day
}

// Points is how many samples a range has per series.
func (r Range) Points() int { return int(r.End.Sub(r.Start)/r.Step) + 1 }
