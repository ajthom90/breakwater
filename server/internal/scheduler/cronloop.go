package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/clock"
)

// MachineSchedule is one machine's policy-driven schedule evaluation state.
type MachineSchedule struct {
	MachineID string
	Schedule  Schedule
	// LastSuccess is the last successful scheduled backup (zero if never).
	LastSuccess time.Time
	// FailedAttempts since last success (for retry/backoff).
	FailedAttempts int
	// NextRetryAfter is zero when not waiting on backoff.
	NextRetryAfter time.Time
}

// CronSource lists machines and their schedules + last success.
type CronSource interface {
	// ListScheduledMachines returns machines with file-backup schedules.
	ListScheduledMachines(ctx context.Context) ([]MachineSchedule, error)
}

// CronSubmit creates a scheduled file backup job.
type CronSubmit interface {
	Submit(ctx context.Context, req SubmitRequest) (jobID string, err error)
}

// CronLoop evaluates schedules on a tick using the injected clock.
// Production wires clock.System(); tests use clock.Fake for time-warp.
//
// Window close does NOT cancel running jobs (start gate only).
type CronLoop struct {
	Clock  clock.Clock
	Source CronSource
	Engine CronSubmit
	Log    *slog.Logger
	Retry  RetryPolicy

	// TickInterval is how often to evaluate (default 1m). Time-warp tests set low.
	TickInterval time.Duration

	mu      sync.Mutex
	lastDue map[string]time.Time // machineID → last time we submitted a catch-up/due job
}

// NewCronLoop constructs a schedule evaluator.
func NewCronLoop(clk clock.Clock, src CronSource, eng CronSubmit, log *slog.Logger) *CronLoop {
	if log == nil {
		log = slog.Default()
	}
	return &CronLoop{
		Clock: clk, Source: src, Engine: eng, Log: log,
		Retry: DefaultRetry(), TickInterval: time.Minute,
		lastDue: make(map[string]time.Time),
	}
}

// RunOnce evaluates all machine schedules at the current clock time and submits
// due jobs. Safe to call from a ticker or from the time-warp harness.
func (c *CronLoop) RunOnce(ctx context.Context) (submitted int, err error) {
	if c.Clock == nil {
		panic("scheduler.CronLoop: Clock is nil")
	}
	now := c.Clock.Now().UTC()
	machines, err := c.Source.ListScheduledMachines(ctx)
	if err != nil {
		return 0, err
	}
	for _, m := range machines {
		if m.FailedAttempts > 0 && !m.NextRetryAfter.IsZero() && now.Before(m.NextRetryAfter) {
			continue
		}
		if m.FailedAttempts > 0 && !c.Retry.AllowRetry(m.FailedAttempts) {
			c.Log.Warn("scheduled backup retries exhausted",
				"machine_id", m.MachineID, "failed_attempts", m.FailedAttempts)
			continue
		}
		due, err := ShouldDispatch(m.Schedule, now, m.LastSuccess)
		if err != nil {
			c.Log.Warn("schedule eval", "machine_id", m.MachineID, "err", err)
			continue
		}
		// Retry path: inside window after failure even if cron not "due" again.
		if !due && m.FailedAttempts > 0 {
			in, _ := InWindow(now, m.Schedule.WindowStart, m.Schedule.WindowEnd)
			due = in
		}
		if !due {
			continue
		}
		// Dedup: do not re-submit if we already submitted this tick window.
		c.mu.Lock()
		if last, ok := c.lastDue[m.MachineID]; ok && now.Sub(last) < time.Minute {
			c.mu.Unlock()
			continue
		}
		c.lastDue[m.MachineID] = now
		c.mu.Unlock()

		if c.Engine == nil {
			continue
		}
		jobID, err := c.Engine.Submit(ctx, SubmitRequest{
			MachineID:  m.MachineID,
			Type:       TypeFileBackup,
			Initiator:  "scheduler",
			ParamsJSON: `{"kind":"file","scheduled":true}`,
		})
		if err != nil {
			c.Log.Warn("schedule submit failed", "machine_id", m.MachineID, "err", err)
			continue
		}
		c.Log.Info("scheduled job submitted", "machine_id", m.MachineID, "job_id", jobID)
		submitted++
	}
	return submitted, nil
}

// Start runs RunOnce on TickInterval until ctx is done. Non-blocking; returns a
// done channel closed when the loop exits.
func (c *CronLoop) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	interval := c.TickInterval
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		defer close(done)
		// Use real timer for production ticks; time-warp harness calls RunOnce
		// directly rather than waiting on wall time.
		t := time.NewTicker(interval)
		defer t.Stop()
		if _, err := c.RunOnce(ctx); err != nil {
			c.Log.Warn("cron initial tick", "err", err)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := c.RunOnce(ctx); err != nil {
					c.Log.Warn("cron tick", "err", err)
				}
			}
		}
	}()
	return done
}

// CatalogCronSource implements CronSource from the catalog.
type CatalogCronSource struct {
	DB *catalog.DB
}

// ListScheduledMachines loads each machine's policy schedule and last successful file job.
func (s *CatalogCronSource) ListScheduledMachines(ctx context.Context) ([]MachineSchedule, error) {
	machines, err := s.DB.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	var out []MachineSchedule
	for _, m := range machines {
		if m.Status == "removed" || m.Status == "disabled" {
			continue
		}
		pol, err := s.DB.PolicyForMachine(ctx, m.ID)
		if err != nil {
			return nil, err
		}
		if pol == nil {
			continue
		}
		last, _ := s.DB.LastSuccessfulJobTime(ctx, m.ID, TypeFileBackup)
		out = append(out, MachineSchedule{
			MachineID:   m.ID,
			Schedule:    Schedule{Cron: pol.ScheduleCron, WindowStart: pol.WindowStart, WindowEnd: pol.WindowEnd},
			LastSuccess: last,
		})
	}
	return out, nil
}
