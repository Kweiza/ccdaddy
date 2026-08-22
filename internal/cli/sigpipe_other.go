//go:build windows || plan9

package cli

// ignoreSIGPIPE is a no-op where SIGPIPE does not exist: a closed pipe there
// surfaces as a write error already, with no signal to disarm first.
//
// Which error it surfaces as is NOT EPIPE -- see isBrokenPipe in
// brokenpipe_windows.go, which is the half of this that was missing.
func ignoreSIGPIPE() {}
