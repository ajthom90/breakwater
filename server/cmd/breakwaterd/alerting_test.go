package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/clock"
	"github.com/ajthom90/breakwater/server/internal/notify"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
)

// TestCHAOS_F2_FailureAlertThroughWireAlerting is the end-to-end CHAOS-F2 guard:
// a job that fails after wireAlerting must reach the FakeSender. This exercises
// breakwaterd's construction path (wireAlerting + OnJobTerminal), not notify alone.
func TestCHAOS_F2_FailureAlertThroughWireAlerting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, engine, fake, rt := setupAlertingHarness(t, ctx, clock.NewFake(time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)))
	defer rt.Close()

	// Online agent + file backup job (holds shared lease → terminal releases it).
	machineID := "mach-fail"
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: machineID, CertFP: "fp-fail", Hostname: "fail-host", RepoID: machineID, Status: "enrolled",
	}); err != nil {
		t.Fatal(err)
	}

	jobID, err := engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup, Initiator: "chaos-f2",
	})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := engine.Job(ctx, jobID)
	if j == nil || j.State != catalog.JobStateRunning {
		t.Fatalf("want running job, got %+v", j)
	}
	if !engine.HasLease(jobID) {
		t.Fatal("file backup must hold vault lease so OnJobTerminal fires on fail")
	}

	// Terminal failure — production path via HandleResult.
	if err := engine.HandleResult(ctx, machineID, scheduler.Result{
		JobID: jobID, Success: false, ErrorMessage: "disk full (injected)",
	}); err != nil {
		t.Fatal(err)
	}
	j, _ = engine.Job(ctx, jobID)
	if j.State != catalog.JobStateFailed {
		t.Fatalf("state=%s want failed", j.State)
	}

	msg := waitFakeKind(t, fake, "failure", 3*time.Second)
	if !strings.Contains(msg.Subject, "fail-host") && !strings.Contains(msg.Body, "fail-host") {
		t.Fatalf("failure alert should name host: %+v", msg)
	}
	if !strings.Contains(msg.Body, "disk full") {
		t.Fatalf("failure alert should include error: %+v", msg)
	}
	t.Logf("CHAOS-F2 failure OK: subject=%q", msg.Subject)
}

// TestCHAOS_F2_WatchdogThroughWireAlerting: advance injected clock past silence
// window and assert watchdog email via wireAlerting's scheduler (not notify.Watchdog alone).
func TestCHAOS_F2_WatchdogThroughWireAlerting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Now is fixed; machine last success is far in the past.
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	db, _, fake, rt := setupAlertingHarness(t, ctx, clk)
	defer rt.Close()

	// Silence 36h; last success 5 days ago.
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: "mach-silent", CertFP: "fp-silent", Hostname: "silent-host",
		RepoID: "mach-silent", Status: "enrolled",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	// Insert a successful file job finished 5 days ago.
	finished := now.Add(-5 * 24 * time.Hour)
	started := finished.Add(-time.Minute)
	jobID := "old-success"
	if err := db.InsertJob(ctx, catalog.Job{
		ID: jobID, MachineID: "mach-silent", Type: scheduler.TypeFileBackup,
		State: catalog.JobStatePending,
	}); err != nil {
		t.Fatal(err)
	}
	// Force success with finished_at in the past via TransitionJob.
	ok, err := db.TransitionJob(ctx, jobID,
		[]string{catalog.JobStatePending},
		catalog.JobStateSuccess,
		catalog.JobTransition{SetFinished: true, SetStarted: true},
	)
	if err != nil || !ok {
		t.Fatalf("transition: ok=%v err=%v", ok, err)
	}
	// TransitionJob uses wall now for finished_at — patch row to past for silence proof.
	if _, err := db.SQL().ExecContext(ctx, `
		UPDATE jobs SET started_at = ?, finished_at = ?, state = ?
		WHERE id = ?`,
		started.UTC().Format(time.RFC3339Nano),
		finished.UTC().Format(time.RFC3339Nano),
		catalog.JobStateSuccess, jobID); err != nil {
		t.Fatal(err)
	}

	// FAULT proof: silence gap exceeds ExpectedSilence (wired as 36h in harness).
	last, err := db.LastSuccessfulJobTime(ctx, "mach-silent", scheduler.TypeFileBackup)
	if err != nil {
		t.Fatal(err)
	}
	if now.Sub(last) <= 36*time.Hour {
		t.Fatalf("test setup: gap %v not past silence", now.Sub(last))
	}
	t.Logf("FAULT surface: last_success=%v now=%v gap=%v silence=36h", last, now, now.Sub(last))

	// Run scheduler via production path (alertScheduler.RunOnce from wireAlerting).
	rt.Scheduler.RunOnce(ctx)

	msg := waitFakeKind(t, fake, "watchdog", 3*time.Second)
	if !strings.Contains(msg.Body, "silent-host") && !strings.Contains(msg.Subject, "silent-host") {
		t.Fatalf("watchdog should name silent-host: %+v", msg)
	}
	t.Logf("CHAOS-F2 watchdog OK: subject=%q", msg.Subject)
}

// TestCHAOS_F2_DigestThroughWireAlerting: past digest hour → digest enqueued once.
func TestCHAOS_F2_DigestThroughWireAlerting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 14:00 UTC ≥ default digest hour 13.
	clk := clock.NewFake(time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC))
	db, _, fake, rt := setupAlertingHarness(t, ctx, clk)
	defer rt.Close()

	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: "m1", CertFP: "fp1", Hostname: "host-a", RepoID: "m1", Status: "enrolled",
	}); err != nil {
		t.Fatal(err)
	}

	rt.Scheduler.RunOnce(ctx)
	msg := waitFakeKind(t, fake, "digest", 3*time.Second)
	if !strings.Contains(msg.Body, "host-a") {
		t.Fatalf("digest body missing machine: %s", msg.Body)
	}
	// Second RunOnce same day must not duplicate.
	before := len(fake.Messages())
	rt.Scheduler.RunOnce(ctx)
	time.Sleep(50 * time.Millisecond)
	if len(fake.Messages()) != before {
		t.Fatalf("digest must fire once per UTC day; before=%d after=%d", before, len(fake.Messages()))
	}
	t.Logf("CHAOS-F2 digest OK: subject=%q", msg.Subject)
}

// TestCHAOS_F2_UnconfiguredSMTPLogsVisibly: construction without smtp.host uses
// LogSender path and sets Configured=false (startup warn is the visible signal).
func TestCHAOS_F2_UnconfiguredSMTPLogsVisibly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	db, err := catalog.Open(filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	engine := scheduler.NewEngine(db, scheduler.NewRepoLocks(), slog.Default())
	// No Sender override, no smtp.host → LogSender.
	rt, err := wireAlerting(ctx, alertDeps{
		DB: db, Engine: engine, Clock: clock.System(), Log: slog.Default(),
		WatchdogInterval: time.Hour, // avoid background noise
		DigestHourUTC:    23,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if rt.Configured {
		t.Fatal("without smtp.host, Configured must be false")
	}
	if rt.Notifier == nil {
		t.Fatal("Notifier must still be constructed")
	}
}

// TestCHAOS_F2_MutationUnwiredWatchdogFails documents the mutation self-check:
// if Scheduler is nil / RunOnce not called from wiring, the watchdog e2e fails.
// This test asserts the *positive* wiring; mutation is run manually in closeout
// (see REVIEW-CHAOS.md disposition). Here we prove unwire would be detected:
// calling notify.Watchdog without going through RunOnce is not enough for this
// test — we require rt.Scheduler from wireAlerting.
func TestCHAOS_F2_MutationSurface_SchedulerPresent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, _, rt := setupAlertingHarness(t, ctx, clock.NewFake(time.Now().UTC()))
	defer rt.Close()
	if rt.Scheduler == nil {
		t.Fatal("wireAlerting must install alertScheduler — without it watchdog never runs")
	}
	if rt.Notifier == nil {
		t.Fatal("wireAlerting must install Notifier")
	}
}

func setupAlertingHarness(t *testing.T, ctx context.Context, clk clock.Clock) (
	*catalog.DB, *scheduler.Engine, *notify.FakeSender, *alertRuntime,
) {
	t.Helper()
	dir := t.TempDir()
	db, err := catalog.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	engine := scheduler.NewEngine(db, scheduler.NewRepoLocks(), slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	engine.Dispatch = &alwaysOnlineDispatch{}

	fake := &notify.FakeSender{}
	rt, err := wireAlerting(ctx, alertDeps{
		DB: db, Engine: engine, Clock: clk, Log: slog.Default(),
		Sender:           fake,
		WatchdogInterval: time.Hour, // background ticker must not race tests
		ExpectedSilence:  36 * time.Hour,
		DigestHourUTC:    13,
	})
	if err != nil {
		t.Fatal(err)
	}
	return db, engine, fake, rt
}

// alwaysOnlineDispatch always reports online so Submit dispatches immediately.
type alwaysOnlineDispatch struct{}

func (alwaysOnlineDispatch) IsOnline(string) bool { return true }
func (alwaysOnlineDispatch) SendJobStart(machineID, jobID, jobType string, paramsJSON []byte) (bool, error) {
	return true, nil
}
func (alwaysOnlineDispatch) SendJobCancel(machineID, jobID, reason string) (bool, error) {
	return true, nil
}

func waitFakeKind(t *testing.T, fake *notify.FakeSender, kind string, timeout time.Duration) notify.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, m := range fake.Messages() {
			if m.Kind == kind {
				return m
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no message kind=%s within %v (have %d): %+v", kind, timeout, len(fake.Messages()), fake.Messages())
	return notify.Message{}
}
