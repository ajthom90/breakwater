package retention

import (
	"sort"
	"time"
)

// ComputeKeepSet is the pure, deterministic GFS keep-set function.
// No I/O, no clock reads — now is an explicit parameter.
// See package doc for bucketing rules.
func ComputeKeepSet(snapshots []Snapshot, policy Policy, now time.Time) KeepSetResult {
	now = now.UTC()
	if len(snapshots) == 0 {
		return KeepSetResult{KeepIDs: map[string]struct{}{}}
	}

	// Defensive copy + sort newest-first; ties by ID desc.
	snaps := make([]Snapshot, len(snapshots))
	copy(snaps, snapshots)
	for i := range snaps {
		snaps[i].Timestamp = snaps[i].Timestamp.UTC()
	}
	sort.SliceStable(snaps, func(i, j int) bool {
		if !snaps[i].Timestamp.Equal(snaps[j].Timestamp) {
			return snaps[i].Timestamp.After(snaps[j].Timestamp)
		}
		return snaps[i].ID > snaps[j].ID
	})

	// id → rules that selected it
	selected := make(map[string][]string, len(snaps))
	add := func(id, rule string) {
		for _, r := range selected[id] {
			if r == rule {
				return
			}
		}
		selected[id] = append(selected[id], rule)
	}

	// Safety: always keep the absolute newest tip.
	add(snaps[0].ID, "newest-tip")

	// keep-last
	n := policy.KeepLast
	if n > len(snaps) {
		n = len(snaps)
	}
	for i := 0; i < n; i++ {
		add(snaps[i].ID, "keep-last")
	}

	// GFS tiers: walk newest-first, fill distinct buckets up to count.
	selectBuckets := func(count int, bucketFn func(time.Time) time.Time, rule string) {
		if count <= 0 {
			return
		}
		seen := make(map[int64]struct{}, count)
		for _, s := range snaps {
			key := bucketFn(s.Timestamp).Unix()
			if _, ok := seen[key]; ok {
				continue // older in same bucket — skip
			}
			// Only count buckets that exist (non-empty). Cap at count.
			if len(seen) >= count {
				// Already have count distinct buckets from newer snaps.
				// Do not add older buckets.
				break
			}
			seen[key] = struct{}{}
			add(s.ID, rule)
		}
	}

	selectBuckets(policy.KeepHourly, truncateHour, "hourly")
	selectBuckets(policy.KeepDaily, truncateDay, "daily")
	selectBuckets(policy.KeepWeekly, truncateWeek, "weekly")
	selectBuckets(policy.KeepMonthly, truncateMonth, "monthly")
	selectBuckets(policy.KeepYearly, truncateYear, "yearly")

	keep := make([]KeepDecision, 0, len(selected))
	keepIDs := make(map[string]struct{}, len(selected))
	for _, s := range snaps {
		if rules, ok := selected[s.ID]; ok {
			keep = append(keep, KeepDecision{ID: s.ID, Rules: rules})
			keepIDs[s.ID] = struct{}{}
		}
	}

	forget := make([]string, 0)
	for _, s := range snaps {
		if _, ok := keepIDs[s.ID]; !ok {
			forget = append(forget, s.ID)
		}
	}

	return KeepSetResult{Keep: keep, Forget: forget, KeepIDs: keepIDs}
}

func truncateHour(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
}

func truncateDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// truncateWeek: Monday 00:00 UTC of the week containing t (ISO-style Monday start).
func truncateWeek(t time.Time) time.Time {
	t = truncateDay(t)
	// Go Weekday: Sunday=0 … Saturday=6. Monday=1.
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7 // Sunday → 7
	}
	// Days since Monday:
	delta := wd - 1
	return t.AddDate(0, 0, -delta)
}

func truncateMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func truncateYear(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
}
