//go:build windows

package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// moveFileEx is windows.MoveFileEx behind a package var, and the reason is
// HKLM rather than portability.
//
// MOVEFILE_DELAY_UNTIL_REBOOT writes PendingFileRenameOperations under
// HKLM\SYSTEM\CurrentControlSet\Control\Session Manager. On a CI runner and in
// a developer's own elevated shell that write SUCCEEDS, and every
// `ccdad uninstall --yes` test in uninstall_test.go reaches this line on
// Windows — so without a seam here `go test ./...` leaves one reboot-time
// delete per test in the registry of whatever machine ran the suite, for good.
// setuppath_windows_test.go's scratch keys are the same decision taken for
// HKCU, and this one matters more: that key is the user's, this one is the
// machine's.
//
// The seam is at MoveFileEx and NOT around the whole of the second step, so
// the real UTF16PtrFromString still runs and a test can read back the exact
// wide string this passes. One test deliberately leaves the var alone and
// asserts against the real registry — a stub can only prove what was asked
// for, never that Windows accepted it.
var moveFileEx = windows.MoveFileEx

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
//
// Both return values are meaningful when err is non-nil: scheduled reports
// whether the rename happened, and a caller that reads only the error tells a
// user their binary is still installed when it is not. runUninstall's switch
// is written around that.
func removeSelf(path string) (scheduled bool, err error) {
	aside := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".uninstalled")
	// A leftover from an earlier uninstall whose reboot never came is simply
	// overwritten: os.Rename is MoveFileEx with MOVEFILE_REPLACE_EXISTING on
	// Windows, which is not true of rename(2) on every platform and is the
	// reason this needs saying.
	//
	// It used to unlink the leftover first. That read as prudence and was dead
	// code: deleting the unlink kills no test, because there is no state in
	// which it changes the outcome. A leftover nothing holds is replaced by
	// the rename, and one still held by a process that has not exited refuses
	// the unlink for exactly the reason it would refuse the rename.
	// TestWindowsRemoveSelfOverwritesALeftoverFromAnEarlierUninstall is what
	// holds the replacing-rename claim up.
	if err := os.Rename(path, aside); err != nil {
		return false, fmt.Errorf("moving %s aside: %w", path, err)
	}

	wide, err := windows.UTF16PtrFromString(aside)
	if err != nil {
		return true, fmt.Errorf("%s was moved aside but could not be scheduled for deletion: %w", aside, err)
	}
	// A nil destination with DELAY_UNTIL_REBOOT means "delete at boot".
	if err := moveFileEx(wide, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT); err != nil {
		return true, fmt.Errorf(
			"%s was moved aside, but scheduling its deletion needs administrator rights (%w); delete it by hand", aside, err)
	}
	return true, nil
}
