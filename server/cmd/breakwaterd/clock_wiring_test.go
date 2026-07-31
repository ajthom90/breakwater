package main

import (
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/clock"
)

// TestProductionUsesSystemClock is the hard safety requirement for M5:
// it must be impossible to run production breakwaterd on a fake clock.
// productionClock() is the only wiring path; it must return IsSystem.
func TestProductionUsesSystemClock(t *testing.T) {
	c := productionClock()
	if !clock.IsSystem(c) {
		t.Fatal("productionClock must return clock.System() — fake clocks are test-only")
	}
	// Sanity: Fake is not system.
	if clock.IsSystem(clock.NewFake(time.Now())) {
		t.Fatal("Fake incorrectly reports IsSystem")
	}
}
