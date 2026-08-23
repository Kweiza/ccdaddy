//go:build !windows

package cli

import "testing"

// quarantineDelayedDelete has nothing to quarantine off Windows: removeSelf is
// an os.Remove here, no registry is involved, and the file it deletes is one
// the test made.
//
// The pair exists so that stubExecutable — which every uninstall test goes
// through, and which is in a file with no build tag — can hold the seam
// without growing one. uninstall_windows_test.go has the half that matters.
func quarantineDelayedDelete(*testing.T) {}
