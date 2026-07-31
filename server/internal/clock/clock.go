// Package clock provides an injectable time source for retention and scheduling.
//
// Production breakwaterd wires System() only. Fake clocks exist solely for tests
// and the time-warp harness — there is no env-var override, no global mutable
// clock, and no test hook reachable from main. IsSystem reports whether a Clock
// is the production real clock; production wiring tests assert that.
package clock

import (
	"sync"
	"time"
)

// Clock is the time source for retention keep-set evaluation, grace windows,
// schedule evaluation, and scrub rotation. Implementations must be safe for
// concurrent use.
type Clock interface {
	Now() time.Time
}

// systemClock is the unexported production clock. Only System() constructs it.
type systemClock struct{}

// System returns the production wall-clock (UTC). This is the only clock
// breakwaterd may wire.
func System() Clock {
	return systemClock{}
}

// Now returns the current UTC time.
func (systemClock) Now() time.Time {
	return time.Now().UTC()
}

// IsSystem reports whether c is the production system clock.
// Used by wiring tests to prove main never installs a Fake.
func IsSystem(c Clock) bool {
	_, ok := c.(systemClock)
	return ok
}

// Fake is a controllable clock for tests and the time-warp harness.
// Not constructible from production main.
type Fake struct {
	mu sync.Mutex
	t  time.Time
}

// NewFake starts at t (converted to UTC). If t is zero, starts at Unix epoch UTC.
func NewFake(t time.Time) *Fake {
	if t.IsZero() {
		t = time.Unix(0, 0).UTC()
	} else {
		t = t.UTC()
	}
	return &Fake{t: t}
}

// Now returns the fake clock's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Set jumps the fake clock to t (UTC).
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = t.UTC()
}

// Advance moves the fake clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}
