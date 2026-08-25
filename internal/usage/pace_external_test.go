// This file is package usage_test rather than package usage, and it is the only
// one in this directory that is: the thing under test here is REACHABILITY
// across the package boundary, and an in-package test cannot fail for the
// reason this one exists. Its sibling pace_test.go stays in package usage
// because it reaches unexported fields of Window.
package usage_test

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/usage"
)

func TestWindowLengthIsReachableFromOutsideThePackage(t *testing.T) {
	for _, c := range []struct {
		name usage.WindowName
		want time.Duration
		ok   bool
	}{
		{usage.WindowFiveHour, 5 * time.Hour, true},
		{usage.WindowSevenDay, 7 * 24 * time.Hour, true},
		{usage.ScopedWindowName(usage.ScopeModel, "Fable"), 7 * 24 * time.Hour, true},
		// cinder_cove's resets_at is an expiry, not a rollover, so it has no
		// length and a caller must be told so rather than given a plausible one.
		{usage.WindowCinderCove, 0, false},
	} {
		got, ok := usage.WindowLength(c.name)
		if ok != c.ok || got != c.want {
			t.Errorf("WindowLength(%q) = %v, %v; want %v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}
