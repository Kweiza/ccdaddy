//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// setConsoleVT turns on ANSI escape processing for f's console (§10.3).
//
// A Windows console does not interpret escape sequences unless
// ENABLE_VIRTUAL_TERMINAL_PROCESSING is set on the handle, and the flag is off
// by default on a classic conhost — so without this every sequence ccdad
// prints arrives as literal text (`←[32m`) rather than as colour. Windows
// Terminal enables it for its own sessions, which is exactly why this is easy
// to believe unnecessary: the machines it is FOR are the ones nobody develops
// on.
//
// A handle that is not a console — a pipe, a file, the daemon's redirected
// stdout — fails GetConsoleMode, and that failure is the answer rather than an
// error: there is no console mode to widen. The mode is deliberately not
// restored on exit. A console object outlives the process that wrote to it, and
// leaving VT processing ON is what every terminal ships with anyway; putting it
// back would mean racing every other process sharing that console.
//
// The already-set early return is the one line here that no test can tell
// apart. A test does execute it — the second call in
// TestSetConsoleVTOnAConsoleThatAlreadyHasItChangesNothing takes exactly this
// branch — but executing it and distinguishing it are different things.
// Deleting it leaves SetConsoleMode writing the value the mode already holds,
// which every assertion in tty_windows_test.go still passes; it saves a syscall
// and nothing observable rests on it. The other two branches do rest on
// assertions there — in particular the OR, without which this would clear
// ENABLE_PROCESSED_OUTPUT and ENABLE_WRAP_AT_EOL_OUTPUT on its way past.
func setConsoleVT(f *os.File) error {
	if f == nil {
		return nil
	}
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return nil
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return nil
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}
