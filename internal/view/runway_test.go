// This file is package view_test rather than package view, and it is the only
// one in this directory that is. Timestamp exists so that the packages that
// render a moment spell one absolute layout once, so the test that matters is
// the one those packages can write: through the exported surface, with nothing
// unexported in reach. Its siblings stay in package view because they reach
// unexported state on Row.
package view_test

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/view"
)

// The zone is part of the rendering, not part of the machine. A test that
// asserts a bare local string passes in the author's zone and prints a
// different hour in CI, where TZ is unset.
func TestTimestampAlwaysCarriesItsZone(t *testing.T) {
	// time.Local is pinned for the duration of this test, and without that pin
	// the nil row below rules nothing out. Nothing sets TZ in CI, so time.Local
	// is UTC there, and an implementation that resolved nil to time.Local would
	// render identically to one that resolves it to UTC -- measured: with that
	// substitution in place the nil assertion passes under TZ=UTC and fails
	// only on a machine whose zone happens not to be UTC. Pinning makes the row
	// decide the same thing everywhere. No test in this package calls
	// t.Parallel(), so the assignment is contained; Cleanup puts it back.
	saved := time.Local
	time.Local = time.FixedZone("XYZ", -7*3600)
	t.Cleanup(func() { time.Local = saved })

	at := time.Date(2026, 8, 27, 5, 10, 0, 0, time.UTC)
	kst := time.FixedZone("KST", 9*3600)
	if got, want := view.Timestamp(at, kst), "2026-08-27 14:10 KST"; got != want {
		t.Errorf("Timestamp(kst) = %q, want %q", got, want)
	}
	if got, want := view.Timestamp(at, time.UTC), "2026-08-27 05:10 UTC"; got != want {
		t.Errorf("Timestamp(utc) = %q, want %q", got, want)
	}
	// A nil location is the caller's bug, not a reason to print a wrong hour.
	if got, want := view.Timestamp(at, nil), "2026-08-27 05:10 UTC"; got != want {
		t.Errorf("Timestamp(nil) = %q, want %q", got, want)
	}
}
