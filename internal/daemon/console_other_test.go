//go:build !windows

package daemon

// consoleAttached has no meaning off Windows: a console is a Windows object,
// and the nearest unix equivalent — the controlling terminal a child is severed
// from by setsid — is what the session assertion in spawn_test.go covers.
//
// It exists so that the child's report carries the same fields everywhere. The
// field is read by exactly one test, and that test is windows-tagged.
func consoleAttached() bool { return false }
