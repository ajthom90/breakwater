package chaos_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/chaos"
	"github.com/ajthom90/breakwater/server/internal/clock"
	"github.com/ajthom90/breakwater/server/internal/notify"
)

// TestChaos09_MissedBackupWatchdog is PLAN chaos drill #9 / Trust Checklist #10:
// machine silent across its window → watchdog email fires.
//
// Fault: machine with last success older than expectedSilence; prove Watchdog
// enqueues kind=watchdog and FakeSender receives it (no network).
func TestChaos09_MissedBackupWatchdog(t *testing.T) {
	seed := chaos.Seed(t, time.Now().UnixNano())
	t.Logf("chaos#9 seed=%d", seed)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Server clock: now is 2026-06-15; last success 2026-06-10 → >36h silence.
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	lastOK := time.Date(2026, 6, 10, 2, 0, 0, 0, time.UTC)
	silence := 36 * time.Hour
	if now.Sub(lastOK) <= silence {
		t.Fatal("test setup: machine is not silent across window")
	}
	t.Logf("FAULT injected: machine silent last_success=%v now=%v silence=%v (gap=%v)",
		lastOK, now, silence, now.Sub(lastOK))

	fake := &notify.FakeSender{}
	clk := clock.NewFake(now)
	n := notify.New(fake, clk, nil)
	n.DefaultTo = []string{"ops@chaos.test"}
	n.Start(ctx)
	defer n.Close()

	// Healthy machine (recent success) must NOT alert.
	n.Watchdog([]notify.WatchMachine{
		{Hostname: "healthy", LastSuccess: now.Add(-1 * time.Hour)},
		{Hostname: "silent-host", LastSuccess: lastOK},
	}, silence)

	msg := waitNotify(t, fake, "watchdog", 3*time.Second)
	if !strings.Contains(msg.Subject, "silent-host") && !strings.Contains(msg.Body, "silent-host") {
		t.Fatalf("watchdog message should name silent machine: %+v", msg)
	}
	if strings.Contains(msg.Body, "healthy") && !strings.Contains(msg.Body, "silent-host") {
		t.Fatal("watchdog fired for wrong machine")
	}
	// Only one watchdog for the silent host (healthy skipped).
	count := 0
	for _, m := range fake.Messages() {
		if m.Kind == "watchdog" {
			count++
			if !strings.Contains(m.Body, "silent-host") {
				t.Fatalf("unexpected watchdog: %q", m.Body)
			}
		}
	}
	if count != 1 {
		t.Fatalf("want 1 watchdog email, got %d", count)
	}
	t.Logf("chaos#9 OK: watchdog email fired for silent-host subject=%q", msg.Subject)
}
