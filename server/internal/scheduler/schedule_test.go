package scheduler

import (
	"testing"
	"time"
)

func TestInWindow_Overnight(t *testing.T) {
	// 20:00–06:00
	cases := []struct {
		h, m int
		want bool
	}{
		{20, 0, true},
		{23, 30, true},
		{0, 0, true},
		{5, 59, true},
		{6, 0, false},
		{12, 0, false},
		{19, 59, false},
	}
	for _, c := range cases {
		now := time.Date(2026, 6, 1, c.h, c.m, 0, 0, time.UTC)
		got, err := InWindow(now, "20:00", "06:00")
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%02d:%02d: got %v want %v", c.h, c.m, got, c.want)
		}
	}
}

func TestInWindow_SameDay(t *testing.T) {
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	ok, _ := InWindow(now, "09:00", "17:00")
	if !ok {
		t.Fatal("expected in window")
	}
	ok, _ = InWindow(now, "11:00", "17:00")
	if ok {
		t.Fatal("expected out of window")
	}
}

func TestShouldDispatch_CronAndWindow(t *testing.T) {
	sched := Schedule{Cron: "0 20 * * *", WindowStart: "20:00", WindowEnd: "06:00"}
	// At 20:00, never fired → due
	now := time.Date(2026, 6, 1, 20, 0, 0, 0, time.UTC)
	due, err := ShouldDispatch(sched, now, time.Time{})
	if err != nil || !due {
		t.Fatalf("due=%v err=%v", due, err)
	}
	// Midday outside window → not due
	mid := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	due, err = ShouldDispatch(sched, mid, time.Time{})
	if err != nil || due {
		t.Fatalf("midday due=%v err=%v", due, err)
	}
	// After last fire yesterday 20:00, today 20:00 → due
	last := time.Date(2026, 5, 31, 20, 0, 0, 0, time.UTC)
	due, err = ShouldDispatch(sched, now, last)
	if err != nil || !due {
		t.Fatalf("next day due=%v err=%v", due, err)
	}
	// Same evening after fire → not due
	due, err = ShouldDispatch(sched, now.Add(time.Hour), now)
	if err != nil || due {
		t.Fatalf("already fired due=%v err=%v", due, err)
	}
}

func TestShouldDispatch_MissedWindowCatchUpOnce(t *testing.T) {
	// Server down across 20:00; recovers at 21:00 still in window → one catch-up.
	sched := Schedule{Cron: "0 20 * * *", WindowStart: "20:00", WindowEnd: "06:00"}
	last := time.Date(2026, 5, 30, 20, 0, 0, 0, time.UTC)
	now := time.Date(2026, 6, 1, 21, 0, 0, 0, time.UTC)
	due, err := ShouldDispatch(sched, now, last)
	if err != nil || !due {
		t.Fatalf("catch-up due=%v err=%v", due, err)
	}
	// After recording success at catch-up, not due again same window.
	due, _ = ShouldDispatch(sched, now.Add(time.Hour), now)
	if due {
		t.Fatal("must not re-fire after catch-up success")
	}
}

func TestRetryBackoffBounded(t *testing.T) {
	r := DefaultRetry()
	if !r.AllowRetry(0) {
		t.Fatal("first retry allowed")
	}
	if r.AllowRetry(100) {
		t.Fatal("unbounded retries")
	}
	d1 := r.Delay(1)
	d3 := r.Delay(3)
	if d3 <= d1 {
		t.Fatalf("backoff not increasing: %v %v", d1, d3)
	}
	if r.Delay(100) > r.MaxDelay {
		t.Fatal("exceeds max")
	}
}
