//go:build windows || plan9

package cli

// ignoreSIGPIPE is a no-op where SIGPIPE does not exist. A closed pipe on
// Windows surfaces as a write error already, which ExecuteWith handles.
func ignoreSIGPIPE() {}
