//go:build windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// removeSelf gets the running binary out of the way on Windows, where a process
// cannot delete its own image.
//
// It is two steps, and the FIRST one is the one that matters. A running .exe
// cannot be deleted but CAN be renamed, so moving it aside takes it off PATH
// under its own name immediately and needs no privilege at all — the user's
// `ccdad` stops resolving the moment this returns.
//
// The second step asks the kernel to delete the leftover at the next restart.
// MOVEFILE_DELAY_UNTIL_REBOOT writes to PendingFileRenameOperations under
// HKLM, which a standard user cannot do — so this fails for exactly the
// installs that did not need administrator rights in the first place, and the
// failure is reported with the leftover's path rather than swallowed.
func removeSelf(path string) (scheduled bool, err error) {
	aside := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".uninstalled")
	// A leftover from an earlier uninstall would make the rename fail.
	_ = os.Remove(aside)
	if err := os.Rename(path, aside); err != nil {
		return false, fmt.Errorf("moving %s aside: %w", path, err)
	}

	wide, err := windows.UTF16PtrFromString(aside)
	if err != nil {
		return true, fmt.Errorf("%s was moved aside but could not be scheduled for deletion: %w", aside, err)
	}
	// A nil destination with DELAY_UNTIL_REBOOT means "delete at boot".
	if err := windows.MoveFileEx(wide, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT); err != nil {
		return true, fmt.Errorf(
			"%s was moved aside, but scheduling its deletion needs administrator rights (%w); delete it by hand", aside, err)
	}
	return true, nil
}
