package chaos

import (
	"os"
	"strconv"
	"testing"
)

// Iters returns how many iterations to run for a drill.
// Priority: CHAOS_ITERS env → CHAOS_FULL=1 uses full → testing.Short uses reduced → reduced default.
// Nightly CI sets CHAOS_FULL=1 for the flagship counts.
func Iters(t *testing.T, full, reduced int) int {
	t.Helper()
	if v := os.Getenv("CHAOS_ITERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("CHAOS_ITERS=%q invalid", v)
		}
		t.Logf("chaos iters from CHAOS_ITERS=%d (full would be %d)", n, full)
		return n
	}
	if os.Getenv("CHAOS_FULL") == "1" {
		t.Logf("chaos iters FULL=%d (CHAOS_FULL=1)", full)
		return full
	}
	if testing.Short() {
		t.Logf("chaos iters reduced=%d (-short; full=%d)", reduced, full)
		return reduced
	}
	// Default local/CI push: reduced. Nightly sets CHAOS_FULL=1.
	t.Logf("chaos iters reduced=%d (default; set CHAOS_FULL=1 for full=%d)", reduced, full)
	return reduced
}

// Seed returns CHAOS_SEED if set, otherwise fallback (typically time.Now().UnixNano()).
// Always logs the seed for deterministic reproduction.
func Seed(t *testing.T, fallback int64) int64 {
	t.Helper()
	if v := os.Getenv("CHAOS_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("CHAOS_SEED=%q invalid", v)
		}
		t.Logf("chaos seed=%d (from CHAOS_SEED)", n)
		return n
	}
	t.Logf("chaos seed=%d", fallback)
	return fallback
}
