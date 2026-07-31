package retention

import (
	"fmt"
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestComputeKeepSet_KeepLastAndNewestTip(t *testing.T) {
	now := ts("2026-06-15T12:00:00Z")
	snaps := []Snapshot{
		{ID: "s1", Timestamp: ts("2026-06-15T10:00:00Z")},
		{ID: "s2", Timestamp: ts("2026-06-14T10:00:00Z")},
		{ID: "s3", Timestamp: ts("2026-06-13T10:00:00Z")},
		{ID: "s4", Timestamp: ts("2026-06-12T10:00:00Z")},
	}
	p := Policy{KeepLast: 2}
	r := ComputeKeepSet(snaps, p, now)
	if len(r.KeepIDs) != 2 {
		t.Fatalf("keep %d want 2: %+v", len(r.KeepIDs), r.KeepIDs)
	}
	if _, ok := r.KeepIDs["s1"]; !ok {
		t.Fatal("newest must be kept")
	}
	if _, ok := r.KeepIDs["s2"]; !ok {
		t.Fatal("keep-last 2 must keep s2")
	}
	if _, ok := r.KeepIDs["s4"]; ok {
		t.Fatal("s4 must be forgotten")
	}
}

func TestComputeKeepSet_NewestAlwaysKeptEvenIfKeepLastZero(t *testing.T) {
	now := ts("2026-06-15T12:00:00Z")
	snaps := []Snapshot{
		{ID: "new", Timestamp: ts("2026-06-15T10:00:00Z")},
		{ID: "old", Timestamp: ts("2026-01-01T10:00:00Z")},
	}
	r := ComputeKeepSet(snaps, Policy{}, now)
	if _, ok := r.KeepIDs["new"]; !ok {
		t.Fatal("newest tip must always be kept")
	}
	if _, ok := r.KeepIDs["old"]; ok {
		t.Fatal("old must be forgotten with empty policy")
	}
}

func TestComputeKeepSet_DailyBucketsNewestWins(t *testing.T) {
	now := ts("2026-06-15T23:00:00Z")
	// Two on same day — only newest of that day for daily; both may be keep-last.
	snaps := []Snapshot{
		{ID: "d15a", Timestamp: ts("2026-06-15T20:00:00Z")},
		{ID: "d15b", Timestamp: ts("2026-06-15T08:00:00Z")},
		{ID: "d14", Timestamp: ts("2026-06-14T12:00:00Z")},
		{ID: "d13", Timestamp: ts("2026-06-13T12:00:00Z")},
	}
	p := Policy{KeepLast: 0, KeepDaily: 2}
	r := ComputeKeepSet(snaps, p, now)
	// newest-tip + daily buckets for 15 and 14 → d15a, d14 (d15b same day older)
	if _, ok := r.KeepIDs["d15a"]; !ok {
		t.Fatal("want d15a")
	}
	if _, ok := r.KeepIDs["d14"]; !ok {
		t.Fatal("want d14")
	}
	if _, ok := r.KeepIDs["d15b"]; ok {
		t.Fatal("d15b same-day older must not win daily alone (not keep-last)")
	}
	if _, ok := r.KeepIDs["d13"]; ok {
		t.Fatal("d13 beyond 2 daily buckets")
	}
}

func TestComputeKeepSet_Idempotent(t *testing.T) {
	now := ts("2026-06-15T12:00:00Z")
	snaps := []Snapshot{
		{ID: "a", Timestamp: ts("2026-06-15T10:00:00Z")},
		{ID: "b", Timestamp: ts("2026-06-14T10:00:00Z")},
		{ID: "c", Timestamp: ts("2026-06-01T10:00:00Z")},
	}
	p := StandardServer()
	r1 := ComputeKeepSet(snaps, p, now)
	// Second application on survivors only
	var survivors []Snapshot
	for _, s := range snaps {
		if _, ok := r1.KeepIDs[s.ID]; ok {
			survivors = append(survivors, s)
		}
	}
	r2 := ComputeKeepSet(survivors, p, now)
	if len(r2.Forget) != 0 {
		t.Fatalf("second pass must forget nothing, forgot %v", r2.Forget)
	}
	if len(r2.KeepIDs) != len(r1.KeepIDs) {
		t.Fatalf("keep size %d vs %d", len(r2.KeepIDs), len(r1.KeepIDs))
	}
}

func TestComputeKeepSet_NeverFewerThanKeepLast(t *testing.T) {
	now := ts("2026-06-15T12:00:00Z")
	var snaps []Snapshot
	for i := 0; i < 20; i++ {
		snaps = append(snaps, Snapshot{
			ID:        fmt.Sprintf("s%02d", i),
			Timestamp: now.Add(-time.Duration(i) * time.Hour),
		})
	}
	p := Policy{KeepLast: 5}
	r := ComputeKeepSet(snaps, p, now)
	if len(r.KeepIDs) < 5 {
		t.Fatalf("keep %d < keep-last 5", len(r.KeepIDs))
	}
}

func TestTruncateWeekMonday(t *testing.T) {
	// 2026-06-15 is a Monday
	mon := ts("2026-06-15T15:00:00Z")
	if !truncateWeek(mon).Equal(ts("2026-06-15T00:00:00Z")) {
		t.Fatalf("got %v", truncateWeek(mon))
	}
	// 2026-06-14 is Sunday → previous Monday 2026-06-08
	sun := ts("2026-06-14T12:00:00Z")
	if !truncateWeek(sun).Equal(ts("2026-06-08T00:00:00Z")) {
		t.Fatalf("got %v", truncateWeek(sun))
	}
}
