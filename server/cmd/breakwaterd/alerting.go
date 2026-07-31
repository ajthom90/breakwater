package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/clock"
	"github.com/ajthom90/breakwater/server/internal/notify"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
)

// SMTP setting keys in catalog.settings (password never logged at the wiring layer).
const (
	settingSMTPHost     = "smtp.host"
	settingSMTPPort     = "smtp.port"
	settingSMTPUser     = "smtp.username"
	settingSMTPPassword = "smtp.password"
	settingSMTPFrom     = "smtp.from"
	settingSMTPTo       = "smtp.to" // comma-separated
	settingSMTPTLS      = "smtp.tls_mode"
	settingWatchSilence = "alert.watchdog_silence" // e.g. "36h"
	settingDigestHour   = "alert.digest_hour_utc"  // 0–23, default 13
)

// DefaultWatchdogSilence is how long past last success (or enrollment) before
// a missed-backup alert (PLAN: nightly + slack).
const DefaultWatchdogSilence = 36 * time.Hour

// DefaultDigestHourUTC is when the daily fleet digest is evaluated (UTC).
const DefaultDigestHourUTC = 13

// alertDeps are the production dependencies for alerting wiring.
// Tests inject Fake clock + FakeSender; production uses System + SMTP/LogSender.
type alertDeps struct {
	DB     *catalog.DB
	Engine *scheduler.Engine
	Clock  clock.Clock
	Log    *slog.Logger
	// Sender, when non-nil, overrides SMTP/LogSender construction (tests).
	Sender notify.Sender
	// SkipStart when true does not start the background worker/scheduler
	// (tests that call RunOnce themselves still need Start on the Notifier).
	WatchdogInterval time.Duration // wall tick; zero → 5m
	ExpectedSilence  time.Duration // zero → DefaultWatchdogSilence
	DigestHourUTC    int           // -1 → load from settings / default
}

// alertRuntime is the live alerting subsystem after wireAlerting.
type alertRuntime struct {
	Notifier  *notify.Notifier
	Scheduler *alertScheduler
	// Configured is true when SMTP host is set (email will be attempted).
	Configured bool
	closeOnce  sync.Once
}

// Close stops the notifier worker (best-effort drain).
func (a *alertRuntime) Close() {
	if a == nil {
		return
	}
	a.closeOnce.Do(func() {
		if a.Notifier != nil {
			a.Notifier.Close()
		}
	})
}

// wireAlerting constructs the notifier, registers the failure OnJobTerminal hook,
// and starts the watchdog/digest scheduler. This is the production construction
// path — tests must call wireAlerting (not notify.New alone) for CHAOS-F2.
func wireAlerting(ctx context.Context, d alertDeps) (*alertRuntime, error) {
	if d.Clock == nil {
		return nil, fmt.Errorf("wireAlerting: Clock required")
	}
	if d.DB == nil {
		return nil, fmt.Errorf("wireAlerting: DB required")
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}

	cfg, err := loadSMTPConfig(ctx, d.DB)
	if err != nil {
		return nil, err
	}

	var sender notify.Sender
	configured := cfg.Host != ""
	if d.Sender != nil {
		sender = d.Sender
		// Tests inject FakeSender; treat as configured for delivery assertions.
		configured = true
	} else if configured {
		// Log redacted config only — never password.
		red := cfg.Redacted()
		d.Log.Info("alerting: SMTP configured",
			"host", red.Host, "port", red.Port, "from", red.From,
			"to_count", len(red.To), "tls", red.TLSMode,
			"username_set", red.Username != "",
		)
		sender = &notify.SMTPSender{CFG: cfg}
	} else {
		// Visible at startup — not silent. Pipeline still runs via log.
		d.Log.Warn("alerting: SMTP not configured; alerts will be logged only (not emailed). " +
			"Set catalog settings smtp.host / smtp.to to enable email.")
		sender = &notify.LogSender{Log: d.Log}
	}

	n := notify.New(sender, d.Clock, d.Log)
	n.DefaultTo = cfg.To
	if len(n.DefaultTo) == 0 && d.Sender != nil {
		// Test path with FakeSender: ensure recipients so Enqueue does not drop.
		n.DefaultTo = []string{"ops@test.local"}
	}
	n.Start(ctx)

	// Failure alerts from job terminal transitions (reuse OnJobTerminal — M4-F2 path).
	if d.Engine != nil {
		engine := d.Engine
		db := d.DB
		log := d.Log
		engine.OnJobTerminal(func(jobID string) {
			// Non-blocking: only enqueue; never do SMTP on this goroutine.
			// Fresh context — terminal hooks must not be canceled with the request.
			alertJobTerminal(context.Background(), db, n, jobID, log)
		})
	}

	silence := d.ExpectedSilence
	if silence <= 0 {
		silence = loadWatchdogSilence(ctx, d.DB, d.Log)
	}
	digestHour := d.DigestHourUTC
	if digestHour < 0 {
		digestHour = loadDigestHour(ctx, d.DB)
	}
	tick := d.WatchdogInterval
	if tick <= 0 {
		tick = 5 * time.Minute
	}

	sched := &alertScheduler{
		Clock:            d.Clock,
		DB:               d.DB,
		Notifier:         n,
		Log:              d.Log,
		ExpectedSilence:  silence,
		DigestHourUTC:    digestHour,
		TickInterval:     tick,
		lastDigestDayKey: "",
	}
	sched.Start(ctx)

	return &alertRuntime{Notifier: n, Scheduler: sched, Configured: configured}, nil
}

// alertJobTerminal fires a failure alert when a job ends in failed state.
// Success/cancelled are silent (digest covers fleet health).
func alertJobTerminal(ctx context.Context, db *catalog.DB, n *notify.Notifier, jobID string, log *slog.Logger) {
	if db == nil || n == nil || jobID == "" {
		return
	}
	j, err := db.JobByID(ctx, jobID)
	if err != nil || j == nil {
		return
	}
	if j.State != catalog.JobStateFailed {
		return
	}
	// Only operator-relevant job types (backup / restore / prune / verify).
	switch j.Type {
	case scheduler.TypeFileBackup, scheduler.TypeImageBackup, scheduler.TypeHyperVBackup,
		scheduler.TypeRestore, scheduler.TypePrune, scheduler.TypeVerify:
		// ok
	default:
		// inventory/noop noise — skip
		return
	}
	host := j.MachineID
	if j.MachineID != "" {
		if m, err := db.MachineByID(ctx, j.MachineID); err == nil && m != nil && m.Hostname != "" {
			host = m.Hostname
		}
	}
	errMsg := j.ErrorMessage
	if errMsg == "" {
		errMsg = "job failed"
	}
	n.AlertFailure(host, jobID, errMsg)
	if log != nil {
		log.Info("failure alert enqueued", "job_id", jobID, "machine", host, "type", j.Type)
	}
}

// alertScheduler runs missed-backup watchdog and daily digest on the injected clock.
type alertScheduler struct {
	Clock           clock.Clock
	DB              *catalog.DB
	Notifier        *notify.Notifier
	Log             *slog.Logger
	ExpectedSilence time.Duration
	DigestHourUTC   int
	TickInterval    time.Duration

	mu               sync.Mutex
	lastDigestDayKey string // "YYYY-MM-DD" of last digest send (clock day)
}

// Start launches the wall-ticker loop. Evaluation always uses Clock.Now().
func (s *alertScheduler) Start(ctx context.Context) {
	if s == nil {
		return
	}
	interval := s.TickInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		// Initial pass so a long-down server alerts soon after restart.
		s.RunOnce(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.RunOnce(ctx)
			}
		}
	}()
}

// RunOnce evaluates watchdog + digest at the current injected clock time.
// Tests call this after advancing a Fake clock (no wall sleep).
func (s *alertScheduler) RunOnce(ctx context.Context) {
	if s == nil || s.Clock == nil || s.Notifier == nil || s.DB == nil {
		return
	}
	s.runWatchdog(ctx)
	s.runDigest(ctx)
}

func (s *alertScheduler) runWatchdog(ctx context.Context) {
	machines, err := s.DB.ListMachines(ctx)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("watchdog: list machines", "err", err)
		}
		return
	}
	silence := s.ExpectedSilence
	if silence <= 0 {
		silence = DefaultWatchdogSilence
	}
	var wm []notify.WatchMachine
	for _, m := range machines {
		if m.Status == "removed" || m.Status == "disabled" {
			continue
		}
		last, _ := s.DB.LastSuccessfulJobTime(ctx, m.ID, scheduler.TypeFileBackup)
		host := m.Hostname
		if host == "" {
			host = m.ID
		}
		wm = append(wm, notify.WatchMachine{
			Hostname:    host,
			LastSuccess: last,
			EnrolledAt:  m.CreatedAt,
		})
	}
	s.Notifier.Watchdog(wm, silence)
}

func (s *alertScheduler) runDigest(ctx context.Context) {
	now := s.Clock.Now().UTC()
	hour := s.DigestHourUTC
	if hour < 0 || hour > 23 {
		hour = DefaultDigestHourUTC
	}
	// Fire once per UTC calendar day after DigestHourUTC.
	if now.Hour() < hour {
		return
	}
	dayKey := now.Format("2006-01-02")
	s.mu.Lock()
	if s.lastDigestDayKey == dayKey {
		s.mu.Unlock()
		return
	}
	s.lastDigestDayKey = dayKey
	s.mu.Unlock()

	rows, err := s.buildDigestRows(ctx, now)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("digest: build rows", "err", err)
		}
		return
	}
	s.Notifier.SendDigest(rows)
	if s.Log != nil {
		s.Log.Info("daily digest enqueued", "machines", len(rows), "day", dayKey)
	}
}

func (s *alertScheduler) buildDigestRows(ctx context.Context, now time.Time) ([]notify.DigestRow, error) {
	machines, err := s.DB.ListMachines(ctx)
	if err != nil {
		return nil, err
	}
	silence := s.ExpectedSilence
	if silence <= 0 {
		silence = DefaultWatchdogSilence
	}
	var rows []notify.DigestRow
	for _, m := range machines {
		if m.Status == "removed" || m.Status == "disabled" {
			continue
		}
		host := m.Hostname
		if host == "" {
			host = m.ID
		}
		last, _ := s.DB.LastSuccessfulJobTime(ctx, m.ID, scheduler.TypeFileBackup)
		row := notify.DigestRow{Machine: host, LastSuccess: last}
		if lj, _ := s.DB.LastJobOfType(ctx, m.ID, scheduler.TypeFileBackup); lj != nil {
			row.SizeBytes = lj.BytesStored
			if lj.StartedAt != nil && lj.FinishedAt != nil {
				row.Duration = lj.FinishedAt.Sub(*lj.StartedAt)
			}
			switch lj.State {
			case catalog.JobStateSuccess:
				row.Status = "success"
			case catalog.JobStateFailed:
				row.Status = "failed"
			default:
				row.Status = lj.State
			}
		} else {
			row.Status = "none"
		}
		// Missed overrides if silent past window.
		ref := last
		if ref.IsZero() {
			ref = m.CreatedAt
		}
		if !ref.IsZero() && now.Sub(ref) > silence {
			row.Status = "missed"
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func loadSMTPConfig(ctx context.Context, db *catalog.DB) (notify.SMTPConfig, error) {
	var cfg notify.SMTPConfig
	if db == nil {
		return cfg, nil
	}
	get := func(k string) string {
		v, _ := db.GetSetting(ctx, k)
		return strings.TrimSpace(v)
	}
	cfg.Host = get(settingSMTPHost)
	cfg.Username = get(settingSMTPUser)
	cfg.Password = get(settingSMTPPassword) // never log
	cfg.From = get(settingSMTPFrom)
	cfg.TLSMode = get(settingSMTPTLS)
	if p := get(settingSMTPPort); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			cfg.Port = n
		}
	}
	if to := get(settingSMTPTo); to != "" {
		for _, part := range strings.Split(to, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				cfg.To = append(cfg.To, part)
			}
		}
	}
	return cfg, nil
}

func loadWatchdogSilence(ctx context.Context, db *catalog.DB, log *slog.Logger) time.Duration {
	if db == nil {
		return DefaultWatchdogSilence
	}
	v, _ := db.GetSetting(ctx, settingWatchSilence)
	if v == "" {
		return DefaultWatchdogSilence
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		if log != nil {
			log.Warn("invalid alert.watchdog_silence; using default", "value", v, "default", DefaultWatchdogSilence.String())
		}
		return DefaultWatchdogSilence
	}
	return d
}

func loadDigestHour(ctx context.Context, db *catalog.DB) int {
	if db == nil {
		return DefaultDigestHourUTC
	}
	v, _ := db.GetSetting(ctx, settingDigestHour)
	if v == "" {
		return DefaultDigestHourUTC
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 23 {
		return DefaultDigestHourUTC
	}
	return n
}
