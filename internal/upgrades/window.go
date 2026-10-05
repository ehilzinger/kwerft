package upgrades

import (
	"fmt"
	"strings"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// DefaultWindowDuration is a maintenance window's length when it names none.
const DefaultWindowDuration = 2 * time.Hour

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// Window is a parsed MaintenanceWindow.
type Window struct {
	days     map[time.Weekday]bool // empty: every day
	hour     int
	minute   int
	duration time.Duration
	loc      *time.Location
}

// ParseWindow reads a MaintenanceWindow. A nil window is never open.
func ParseWindow(w *kwerftv1.MaintenanceWindow) (*Window, error) {
	if w == nil {
		return nil, nil
	}
	out := &Window{days: map[time.Weekday]bool{}, duration: DefaultWindowDuration, loc: time.UTC}
	for _, d := range w.Days {
		key := strings.ToLower(d)
		if len(key) > 3 {
			key = key[:3]
		}
		wd, ok := weekdays[key]
		if !ok {
			return nil, fmt.Errorf("window: unknown day %q", d)
		}
		out.days[wd] = true
	}
	if _, err := fmt.Sscanf(w.Start, "%d:%d", &out.hour, &out.minute); err != nil || out.hour > 23 || out.minute > 59 {
		return nil, fmt.Errorf("window: start %q is not HH:MM", w.Start)
	}
	if w.Duration != nil {
		out.duration = w.Duration.Duration
	}
	if out.duration <= 0 || out.duration > 24*time.Hour {
		return nil, fmt.Errorf("window: duration must be between 1m and 24h")
	}
	if w.TimeZone != "" {
		loc, err := time.LoadLocation(w.TimeZone)
		if err != nil {
			return nil, fmt.Errorf("window: time zone %q: %w", w.TimeZone, err)
		}
		out.loc = loc
	}
	return out, nil
}

// start of the window on the day of t (in the window's zone).
func (w *Window) startOn(t time.Time) time.Time {
	t = t.In(w.loc)
	return time.Date(t.Year(), t.Month(), t.Day(), w.hour, w.minute, 0, 0, w.loc)
}

func (w *Window) dayAllowed(t time.Time) bool {
	return len(w.days) == 0 || w.days[t.In(w.loc).Weekday()]
}

// Open reports whether now is inside a window, and when that window ends.
// A window that starts late on an allowed day stays open past midnight.
func (w *Window) Open(now time.Time) (bool, time.Time) {
	if w == nil {
		return false, time.Time{}
	}
	for back := 0; back <= 1; back++ {
		day := now.In(w.loc).AddDate(0, 0, -back)
		if !w.dayAllowed(day) {
			continue
		}
		start := w.startOn(day)
		end := start.Add(w.duration)
		if !now.Before(start) && now.Before(end) {
			return true, end
		}
	}
	return false, time.Time{}
}

// Next is the next window start after now (zero for a nil window).
func (w *Window) Next(now time.Time) time.Time {
	if w == nil {
		return time.Time{}
	}
	for ahead := 0; ahead <= 8; ahead++ {
		day := now.In(w.loc).AddDate(0, 0, ahead)
		if !w.dayAllowed(day) {
			continue
		}
		if start := w.startOn(day); start.After(now) {
			return start
		}
	}
	return time.Time{}
}
