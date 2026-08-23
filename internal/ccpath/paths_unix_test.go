//go:build !windows

package ccpath

import "testing"

// The two homes are ONE value here, and that is what makes the Windows split
// safe to introduce: os.UserHomeDir reads $HOME on Unix, so LayoutHome's
// HOME-first rule returns exactly what homeDir returns. %USERPROFILE% is set to
// something else on purpose — if this ever starts mattering off Windows, this
// test says so rather than a user's credential path moving quietly.
func TestTheTwoHomesAgreeOffWindows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", t.TempDir())

	config, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	layout, err := LayoutHome()
	if err != nil {
		t.Fatal(err)
	}
	if config != home || layout != home {
		t.Fatalf("Home() = %q and LayoutHome() = %q, want both %q", config, layout, home)
	}
}

// With no HOME there is no home to spell, and LayoutHome must not paper over it
// with "" the way Claude Code's `??` does. The error carries which variable to
// set, which is the only place that fact can still be attached to it.
func TestLayoutHomeErrorsWithNoHOMERatherThanReturningEmpty(t *testing.T) {
	t.Setenv("HOME", "")

	got, err := LayoutHome()
	if err == nil {
		t.Fatalf("LayoutHome() = %q with no HOME, want an error", got)
	}
	if got != "" {
		t.Errorf("LayoutHome() returned %q alongside its error", got)
	}
}
