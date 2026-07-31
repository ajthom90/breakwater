package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Schedule describes when a backup job may fire.
// Cron is parsed with robfig/cron (standard 5-field); evaluation uses the
// injected now so the time-warp harness can drive schedules.
//
// Window semantics (e.g. nightly 20:00–06:00):
//   - Jobs are only DISPATCHED when now is inside the window.
//   - A job still running at window close is NOT killed (document: window gates
//     start, not duration).
//   - Missed-window catch-up: if the server was down across a whole window and
//     no successful run occurred for that window, fire once on recovery rather
//     than replaying every missed cron tick.
type Schedule struct {
	// Cron is a standard 5-field cron expression (min hour dom mon dow).
	Cron string
	// WindowStart / WindowEnd are "HH:MM" in the same timezone as now (UTC for server).
	// Empty window means always open.
	WindowStart string
	WindowEnd   string
}

// ParseCron parses a standard cron expression. Used for validation and Next/Prev.
func ParseCron(expr string) (cron.Schedule, error) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return parser.Parse(expr)
}

// InWindow reports whether now falls inside [start, end) wall-clock window.
// Windows may wrap midnight (start > end), e.g. 20:00–06:00.
// Empty start and end → always in window.
func InWindow(now time.Time, start, end string) (bool, error) {
	if start == "" && end == "" {
		return true, nil
	}
	if start == "" || end == "" {
		return false, fmt.Errorf("window requires both start and end or neither")
	}
	sh, sm, err := parseHHMM(start)
	if err != nil {
		return false, err
	}
	eh, em, err := parseHHMM(end)
	if err != nil {
		return false, err
	}
	now = now.UTC()
	mins := now.Hour()*60 + now.Minute()
	s := sh*60 + sm
	e := eh*60 + em
	if s == e {
		// Degenerate full-day window.
		return true, nil
	}
	if s < e {
		// Same-day window [s, e).
		return mins >= s && mins < e, nil
	}
	// Wraps midnight: in window if mins >= s OR mins < e.
	return mins >= s || mins < e, nil
}

func parseHHMM(s string) (h, m int, err error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid HH:MM %q", s)
	}
	h, err = strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, 0, fmt.Errorf("invalid hour in %q", s)
	}
	m, err = strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("invalid minute in %q", s)
	}
	return h, m, nil
}

// ShouldDispatch reports whether a scheduled backup should fire at now given
// the last successful fire time. Uses cron Next() from lastFire (or a lookback
// if never fired). Requires now to be inside the backup window.
//
// Missed-window catch-up: if one or more cron occurrences were due between
// lastFire and now, and we are either inside the window now or just recovered,
// return true at most once until lastFire is updated (caller records success).
func ShouldDispatch(sched Schedule, now, lastFire time.Time) (bool, error) {
	in, err := InWindow(now, sched.WindowStart, sched.WindowEnd)
	if err != nil {
		return false, err
	}
	if !in {
		return false, nil
	}
	cs, err := ParseCron(sched.Cron)
	if err != nil {
		return false, fmt.Errorf("cron: %w", err)
	}
	now = now.UTC()
	if lastFire.IsZero() {
		// Never fired: due if the previous cron tick is within a bound lookback
		// (avoid firing on every restart from epoch). Lookback = 48h.
		prev := previousCron(cs, now)
		if prev.IsZero() {
			return true, nil
		}
		if now.Sub(prev) <= 48*time.Hour {
			return true, nil
		}
		// Far past — still catch up once inside window.
		return true, nil
	}
	lastFire = lastFire.UTC()
	// Next occurrence after lastFire.
	next := cs.Next(lastFire)
	if next.IsZero() {
		return false, nil
	}
	// Due when next <= now (we missed or hit the schedule).
	return !next.After(now), nil
}

// previousCron finds the most recent schedule time strictly before now by
// probing Next from now-48h (sufficient for daily/nightly schedules).
func previousCron(cs cron.Schedule, now time.Time) time.Time {
	start := now.Add(-48 * time.Hour)
	var last time.Time
	t := start
	for i := 0; i < 100; i++ {
		n := cs.Next(t)
		if n.IsZero() || !n.Before(now) {
			break
		}
		last = n
		t = n
	}
	return last
}

// RetryPolicy bounds retries for failed scheduled jobs.
type RetryPolicy struct {
	// MaxAttempts including the first try (default 5).
	MaxAttempts int
	// BaseDelay for exponential backoff (default 1m).
	BaseDelay time.Duration
	// MaxDelay caps backoff (default 1h).
	MaxDelay time.Duration
}

// DefaultRetry is the production retry/backoff bound.
func DefaultRetry() RetryPolicy {
	return RetryPolicy{MaxAttempts: 5, BaseDelay: time.Minute, MaxDelay: time.Hour}
}

// Delay returns the backoff before attempt n (1-based attempt after failure).
// attempt 1 → BaseDelay, then doubles until MaxDelay.
func (r RetryPolicy) Delay(attempt int) time.Duration {
	if r.MaxAttempts <= 0 {
		r = DefaultRetry()
	}
	if r.BaseDelay <= 0 {
		r.BaseDelay = time.Minute
	}
	if r.MaxDelay <= 0 {
		r.MaxDelay = time.Hour
	}
	if attempt < 1 {
		attempt = 1
	}
	d := r.BaseDelay
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= r.MaxDelay {
			return r.MaxDelay
		}
	}
	if d > r.MaxDelay {
		return r.MaxDelay
	}
	return d
}

// AllowRetry reports whether another attempt is permitted after failedAttempts
// previous failures (0 = first retry after initial failure).
func (r RetryPolicy) AllowRetry(failedAttempts int) bool {
	if r.MaxAttempts <= 0 {
		r = DefaultRetry()
	}
	// MaxAttempts includes the original try: after (MaxAttempts-1) failures, stop.
	return failedAttempts < r.MaxAttempts-1
}
