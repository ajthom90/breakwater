package retention

import "time"

// DefaultGrace is the ransomware undo window (PLAN: 7 days).
const DefaultGrace = 7 * 24 * time.Hour

// MassForgetThreshold: forgetting this many or more snapshots in one call
// is flagged mass_forget=true in the audit detail (admin + audit requirement).
const MassForgetThreshold = 10

// Policy holds GFS retention counts and schedule metadata.
// Matches catalog policies columns / PLAN "Standard Server" defaults.
type Policy struct {
	ID             string
	Name           string
	ScheduleCron   string // e.g. "0 20 * * *"
	WindowStart    string // "HH:MM" local-or-UTC wall; evaluated with injected clock
	WindowEnd      string // "HH:MM"; may wrap midnight (e.g. 20:00–06:00)
	KeepLast       int
	KeepHourly     int
	KeepDaily      int
	KeepWeekly     int
	KeepMonthly    int
	KeepYearly     int
	PruneGraceDays int // 0 → DefaultGrace (7d)
	IsDefault      bool
}

// StandardServer returns PLAN's suggested "Standard Server" defaults.
// keep-last 3, daily 14, weekly 8, monthly 12, yearly 2; nightly 20:00–06:00;
// prune grace 7 days.
func StandardServer() Policy {
	return Policy{
		ID:             "01DEFAULTPOLICY000000000000",
		Name:           "Standard Server",
		ScheduleCron:   "0 20 * * *",
		WindowStart:    "20:00",
		WindowEnd:      "06:00",
		KeepLast:       3,
		KeepHourly:     0,
		KeepDaily:      14,
		KeepWeekly:     8,
		KeepMonthly:    12,
		KeepYearly:     2,
		PruneGraceDays: 7,
		IsDefault:      true,
	}
}

// Grace returns the soft-delete window duration.
func (p Policy) Grace() time.Duration {
	if p.PruneGraceDays <= 0 {
		return DefaultGrace
	}
	return time.Duration(p.PruneGraceDays) * 24 * time.Hour
}

// Snapshot is the pure keep-set input (no I/O).
type Snapshot struct {
	ID        string
	Timestamp time.Time
}

// KeepDecision records why a snapshot was kept (for audit / debugging).
type KeepDecision struct {
	ID    string
	Rules []string // e.g. "keep-last", "daily", "newest-tip"
}

// KeepSetResult is the pure keep-set output.
type KeepSetResult struct {
	Keep    []KeepDecision
	Forget  []string // IDs not in keep set
	KeepIDs map[string]struct{}
}

// KeepIDSet returns a set of IDs that survive.
func (r KeepSetResult) KeepIDSet() map[string]struct{} {
	if r.KeepIDs != nil {
		return r.KeepIDs
	}
	m := make(map[string]struct{}, len(r.Keep))
	for _, k := range r.Keep {
		m[k.ID] = struct{}{}
	}
	return m
}
