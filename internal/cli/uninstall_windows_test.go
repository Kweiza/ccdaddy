//go:build windows

package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The Windows half of `ccdad uninstall`, executed against a binary that is
// really running.
//
// The item this file closes was written as "removeSelf has never been run".
// That was half right and the wrong half was the dangerous one: every
// `uninstall --yes` test in uninstall_test.go reaches removeSelf on the
// windows-latest leg, so the code RAN — with nothing asserted about it beyond
// uninstall_test.go's assertBinaryGone, which only checks that the original
// path is empty. A rename to any name at all satisfies that. So did, until
// this file, scheduling the WRONG path for deletion at the next restart, which
// is how a later reinstall gets deleted out from under a user by their own
// reboot.
//
// Two of the findings below came from RUNNING this, not from writing it: the
// leftover unlink removeSelf opened with was dead code, since os.Rename
// replaces an existing target on Windows; and the registry stores a path in a
// spelling neither the NT prefix nor an 8.3 TMP survives a text comparison of.
// Both are recorded where they were found rather than here.
//
// Three things here cannot be simulated anywhere else:
//
//   - that a running .exe refuses os.Remove and accepts os.Rename, which is
//     the entire reason removeSelf exists and the entire reason
//     uninstall_other.go is one line;
//   - that the delayed delete names the LEFTOVER and never the original path;
//   - that MoveFileEx with a nil destination is a delete rather than a move to
//     nowhere, read back out of the machine's own registry.
//
// THE HKLM SEAM IS NOT A CONVENIENCE, and it is a stronger version of the rule
// setuppath_windows_test.go keeps for HKCU. MOVEFILE_DELAY_UNTIL_REBOOT writes
// PendingFileRenameOperations under HKLM\SYSTEM\CurrentControlSet\Control\
// Session Manager. On a CI runner and in a contributor's elevated shell that
// write succeeds, so before the moveFileEx var existed `go test ./...` was
// queuing one reboot-time delete per uninstall test into the registry of
// whatever machine ran the suite, permanently. Every test below stubs it
// except TestWindowsRemoveSelfWritesOnePendingFileRenameOperation, which uses
// the real call precisely because a stub can prove what was asked for and
// never that Windows accepted it — and that one takes its own entry back.

// delayedDelete is one call to MoveFileEx, recorded as the arguments that
// reached it. The wide strings are read back rather than the Go strings kept,
// so UTF16PtrFromString is still on the path being tested.
type delayedDelete struct {
	source    string
	target    string
	nilTarget bool
	flags     uint32
}

// ntPrefix is the spelling MoveFileEx stores a path under. It is not always at
// the front: the reboot processor keeps bookkeeping of its own ahead of it, and
// a queued delete comes back off a runner as `*1\??\C:\dir\file`.
const ntPrefix = `\??\`

// samePath reports whether an operand -- recorded from a stubbed call, or read
// back out of the machine's queue -- is this path.
//
// Two things make those texts differ from what removeSelf was handed, and both
// were found by running this file rather than by reading it. The registry
// carries the NT spelling behind that marker. And the runner's TMP is an 8.3
// short path, C:\Users\RUNNER~1\..., which t.TempDir inherits and which
// Windows expands on the way in -- so comparing the two as text compares two
// spellings of one file and finds them different.
func samePath(t *testing.T, operand, path string) bool {
	t.Helper()
	if i := strings.Index(operand, ntPrefix); i >= 0 {
		operand = operand[i+len(ntPrefix):]
	}
	return strings.EqualFold(longPath(t, operand), longPath(t, path))
}

// longPath expands the 8.3 components of a path that exists.
//
// A path that does NOT exist comes back unchanged, and that is the right
// answer rather than a fallback: the assertion asking "is this the install
// path" is asking about a path removeSelf has already renamed away, and both
// sides of that comparison are then the short spelling the test wrote.
func longPath(t *testing.T, p string) string {
	t.Helper()
	wide, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	n, err := windows.GetLongPathName(wide, nil, 0)
	if err != nil || n == 0 {
		return p
	}
	buf := make([]uint16, n)
	n, err = windows.GetLongPathName(wide, &buf[0], n)
	if err != nil || n == 0 || n > uint32(len(buf)) {
		return p
	}
	return windows.UTF16ToString(buf[:n])
}

// recordDelayedDeletes points moveFileEx at a recorder that answers with
// `answer` and touches no registry at all.
func recordDelayedDeletes(t *testing.T, answer error) *[]delayedDelete {
	t.Helper()
	var seen []delayedDelete
	saved := moveFileEx
	t.Cleanup(func() { moveFileEx = saved })
	moveFileEx = func(from, to *uint16, flags uint32) error {
		call := delayedDelete{flags: flags, nilTarget: to == nil}
		if from != nil {
			call.source = windows.UTF16PtrToString(from)
		}
		if to != nil {
			call.target = windows.UTF16PtrToString(to)
		}
		seen = append(seen, call)
		return answer
	}
	return &seen
}

// quarantineDelayedDelete is what uninstall_test.go's stubExecutable calls, so
// that the cross-platform uninstall tests stop writing to the machine's HKLM
// on the windows leg. uninstall_other_test.go has the do-nothing half.
func quarantineDelayedDelete(t *testing.T) {
	t.Helper()
	_ = recordDelayedDeletes(t, nil)
}

// startRunningBinary puts a real, executing image at path.
//
// cmd.exe is copied rather than built, because what is needed is an image
// Windows has mapped into a live process and any PE will do — building one
// would spend a compiler on a property the copy already has. `/k` with an open
// stdin pipe sits at a prompt indefinitely, so the process outlives the test
// body rather than the test racing its exit.
//
// CreateProcess maps the image before it returns, so the lock is in place the
// moment Start does; no polling is needed and none would be safe, since the
// only cheap probe for "is it locked" is an os.Remove that would delete the
// file when the answer was no.
func startRunningBinary(t *testing.T, path string) {
	t.Helper()
	source := os.Getenv("COMSPEC")
	if source == "" {
		source = filepath.Join(os.Getenv("SystemRoot"), `System32\cmd.exe`)
	}
	image, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("reading %s to make a running binary out of: %v", source, err)
	}
	if err := os.WriteFile(path, image, 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	cmd := exec.Command(path, "/k")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("opening stdin for %s: %v", path, err)
	}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", path, err)
	}
	// Before t.TempDir's own cleanup, which is registered earlier and so runs
	// later: RemoveAll cannot delete a mapped image, and a leaked process would
	// turn every test in this file into a t.TempDir removal failure.
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

// asidePath is the leftover removeSelf is expected to leave, spelled out here
// rather than taken from the code so that a rename of the leftover has to be
// made here too.
func asidePath(binary string) string {
	return filepath.Join(filepath.Dir(binary), "."+filepath.Base(binary)+".uninstalled")
}

// The whole reason this file, uninstall_windows.go and uninstall_other.go all
// exist. If Windows ever let a process delete its own image, removeSelf would
// be a one-line os.Remove like every other platform's — and if it ever stopped
// allowing the rename, uninstall would have no way to take ccdad off PATH at
// all and would have to say so instead of reporting success.
func TestWindowsRemoveSelfRenamesABinaryThatIsRunning(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "ccdad.exe")
	startRunningBinary(t, binary)
	calls := recordDelayedDeletes(t, nil)

	// The control, and it has to come first: if this succeeds the premise is
	// gone and every assertion below would be measuring an ordinary file.
	if err := os.Remove(binary); err == nil {
		t.Fatal("Windows deleted a running .exe; removeSelf's whole reason for existing is that it cannot")
	}

	scheduled, err := removeSelf(binary)
	if err != nil {
		t.Fatalf("removeSelf on a running binary: %v", err)
	}
	if !scheduled {
		t.Error("scheduled = false; on Windows the leftover is always there to schedule")
	}
	if _, err := os.Stat(binary); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s is still under its own name, so `ccdad` still resolves: %v", binary, err)
	}
	if _, err := os.Stat(asidePath(binary)); err != nil {
		t.Errorf("the leftover is not at %s: %v", asidePath(binary), err)
	}
	if len(*calls) != 1 {
		t.Fatalf("MoveFileEx called %d times, want once", len(*calls))
	}
}

// The leftover's name and its contents. A rename to a random name would pass
// uninstall_test.go's assertBinaryGone just as well, and would leave a user
// with an 8 MB file they cannot identify if the reboot-time delete never runs.
func TestWindowsRemoveSelfNamesTheLeftoverAfterTheBinary(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "ccdad.exe")
	if err := os.WriteFile(binary, []byte("the binary that was installed"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordDelayedDeletes(t, nil)

	if _, err := removeSelf(binary); err != nil {
		t.Fatalf("removeSelf: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != ".ccdad.exe.uninstalled" {
		t.Fatalf("the install directory holds %v, want exactly [.ccdad.exe.uninstalled]", names)
	}
	// Same volume, same directory: a leftover moved to %TEMP% would be a
	// cross-volume copy, which a running image does not permit at all.
	body, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the binary that was installed" {
		t.Errorf("the leftover holds %q; it should be the binary, renamed and not rewritten", body)
	}
}

// WHICH path is handed to the delayed delete, and it is the assertion with the
// worst failure mode behind it. A reboot-time delete queued against the
// ORIGINAL path deletes whatever is at that path when the machine next starts
// — which, for a user who uninstalled and then reinstalled, is their new
// binary, removed by their own reboot with nothing on screen to connect the
// two.
func TestWindowsRemoveSelfSchedulesTheLeftoverAndNotTheInstallPath(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "ccdad.exe")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := recordDelayedDeletes(t, nil)

	if _, err := removeSelf(binary); err != nil {
		t.Fatalf("removeSelf: %v", err)
	}

	if len(*calls) != 1 {
		t.Fatalf("MoveFileEx called %d times, want once", len(*calls))
	}
	call := (*calls)[0]
	if samePath(t, call.source, binary) {
		t.Error("the install path itself is queued for deletion at the next restart; a reinstall would be deleted by the user's next reboot")
	}
	if !samePath(t, call.source, asidePath(binary)) {
		t.Errorf("MoveFileEx was given %q, want the leftover %q", call.source, asidePath(binary))
	}
	// A nil destination is what makes this a DELETE. Any non-nil destination,
	// the empty string included, is a move, and a move to nowhere is an error
	// at boot rather than the cleanup this is for.
	if !call.nilTarget {
		t.Errorf("MoveFileEx was given the destination %q; a delayed DELETE is a nil destination", call.target)
	}
	if call.flags != windows.MOVEFILE_DELAY_UNTIL_REBOOT {
		t.Errorf("flags = %#x, want MOVEFILE_DELAY_UNTIL_REBOOT (%#x)", call.flags, uint32(windows.MOVEFILE_DELAY_UNTIL_REBOOT))
	}
}

// The second uninstall on a machine where the first one's reboot never came,
// and the assertion that removeSelf does NOT have to clear the leftover first.
//
// This test is why that unlink is gone. removeSelf opened with an os.Remove of
// the leftover under a comment saying the rename would otherwise fail -- true
// of rename(2) in the general case, and not of Go's os.Rename on Windows,
// which is MoveFileEx with MOVEFILE_REPLACE_EXISTING. Deleting the unlink
// killed no test, here or anywhere, because there is no state in which it
// changes the outcome: a leftover nothing holds is replaced by the rename, and
// one still held by a live process refuses the unlink for exactly the reason
// it would refuse the rename. What is left is the claim the code now rests on.
func TestWindowsRemoveSelfOverwritesALeftoverFromAnEarlierUninstall(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "ccdad.exe")
	if err := os.WriteFile(binary, []byte("the second install"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asidePath(binary), []byte("the first install, still awaiting a reboot"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordDelayedDeletes(t, nil)

	scheduled, err := removeSelf(binary)
	if err != nil {
		t.Fatalf("removeSelf with a leftover already in place: %v", err)
	}
	if !scheduled {
		t.Error("scheduled = false")
	}
	body, err := os.ReadFile(asidePath(binary))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the second install" {
		t.Errorf("the leftover holds %q, want the binary this uninstall moved aside", body)
	}
}

// A rename that could not happen is the one case where nothing was removed,
// and the message has to name the path the user still has to deal with. The
// delayed delete must not be reached either: queueing a leftover that was
// never created is a reboot-time delete pointed at a path that may well be
// occupied by something else by then.
func TestWindowsRemoveSelfReportsThePathItCouldNotMove(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "ccdad.exe")
	calls := recordDelayedDeletes(t, nil)

	scheduled, err := removeSelf(binary)
	if err == nil {
		t.Fatal("removeSelf reported success for a binary that is not there")
	}
	if scheduled {
		t.Error("scheduled = true after the rename failed; the caller would tell the user to reboot for a file that does not exist")
	}
	if !strings.Contains(err.Error(), binary) {
		t.Errorf("the error is %q; it has to name %s, which is what the user is left holding", err, binary)
	}
	if len(*calls) != 0 {
		t.Errorf("MoveFileEx was called %d times after the rename failed, want none", len(*calls))
	}
}

// The standard user's uninstall, which is every install that did not need
// elevation in the first place: the rename needs no privilege and the HKLM
// write does. Both return values carry information here, and a caller reading
// only the error gets it backwards — the binary IS off PATH.
func TestWindowsRemoveSelfSaysTheBinaryMovedWhenSchedulingIsRefused(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "ccdad.exe")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	recordDelayedDeletes(t, windows.ERROR_ACCESS_DENIED)

	scheduled, err := removeSelf(binary)
	if err == nil {
		t.Fatal("removeSelf reported success though the delete could not be scheduled")
	}
	if !scheduled {
		t.Error("scheduled = false, but the rename succeeded; the caller cannot tell the user their binary is gone")
	}
	if _, statErr := os.Stat(binary); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("%s survived a partial removeSelf: %v", binary, statErr)
	}
	for _, want := range []string{asidePath(binary), "administrator rights", "by hand"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error is %q, want it to contain %q", err, want)
		}
	}
}

// runUninstall's report for the arm above, end to end. The switch used to
// print "could not be removed" for this outcome, which is false in the way
// that matters: `ccdad` has already stopped resolving, and a user told
// otherwise reinstalls over a machine that is already clean.
func TestUninstallOnWindowsSaysTheBinaryIsGoneWhenOnlyTheCleanupFailed(t *testing.T) {
	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	bin := fakeBinary(t)
	seedAccount(t, "u-1", "work@example.com")
	// After fakeBinary, so this stub is the one in place: stubExecutable
	// installs a quiet one, and the later t.Cleanup runs first.
	recordDelayedDeletes(t, windows.ERROR_ACCESS_DENIED)

	code, _, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, errOut, top)
	}
	if strings.Contains(errOut, "could not be removed") {
		t.Errorf("uninstall said the binary could not be removed, though it was renamed aside:\n%s", errOut)
	}
	for _, want := range []string{"is gone from that path", asidePath(bin)} {
		if !strings.Contains(errOut, want) {
			t.Errorf("uninstall said:\n%s\nwant a report containing %q", errOut, want)
		}
	}
}

const (
	sessionManagerKey  = `SYSTEM\CurrentControlSet\Control\Session Manager`
	pendingRenameValue = "PendingFileRenameOperations"
)

// The quarantine stubExecutable installs, asserted as the property rather than
// as the call.
//
// It is the one thing in this file with no failure of its own: a missing
// quarantine breaks no test anywhere, it just leaves the machine that ran the
// suite with one reboot-time delete per uninstall test, forever. Something has
// to fail when it goes, or it will go.
//
// A runner is elevated and a contributor's shell may not be, and the assertion
// is honest either way: unelevated, MoveFileEx would have been refused and the
// queue would be unchanged for a second reason. The leg that can actually
// catch a regression here is the one that could actually cause it.
func TestWindowsAStubbedUninstallLeavesTheMachineQueueAlone(t *testing.T) {
	before := pendingFileRenames(t)

	isolate(t)
	stubDaemonWorld(t, &fakeDaemon{})
	stubEnvironment(t, false, false)
	bin := fakeBinary(t)
	seedAccount(t, "u-1", "work@example.com")

	code, _, errOut, top := runRoot(t, "uninstall", "--yes")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\n%s%s", code, ExitOK, errOut, top)
	}
	// The uninstall really did reach removeSelf; this is not passing because
	// nothing happened.
	if _, err := os.Stat(bin); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s was not renamed aside, so removeSelf was never reached: %v", bin, err)
	}

	after := pendingFileRenames(t)
	if len(after) != len(before) {
		t.Errorf("the machine's reboot-time delete queue went from %d entries to %d; "+
			"an ordinary `go test ./...` may not write HKLM\\%s", len(before), len(after), sessionManagerKey)
	}
}

// pendingFileRenames reads the machine's queue of reboot-time file operations.
// Reading it needs no privilege; the value is absent on a machine with nothing
// queued, which is a normal state and not an error.
func pendingFileRenames(t *testing.T) []string {
	t.Helper()
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, sessionManagerKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf(`opening HKLM\%s: %v`, sessionManagerKey, err)
	}
	defer key.Close()
	values, _, err := key.GetStringsValue(pendingRenameValue)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf(`reading HKLM\%s\%s: %v`, sessionManagerKey, pendingRenameValue, err)
	}
	return values
}

// dropPendingRename takes back exactly the entry naming aside, and leaves
// every other queued operation where it was.
//
// The easy version — snapshot the value, run, write the snapshot back — is the
// wrong one. PendingFileRenameOperations is where an in-progress Windows
// Update and every mid-install MSI keep their work, so a wholesale rewrite
// would silently drop whatever was queued while this test ran. The value is a
// flat list of PAIRS: source, then destination, with an empty destination
// meaning delete.
func dropPendingRename(t *testing.T, aside string) {
	t.Helper()
	entries := pendingFileRenames(t)
	kept := make([]string, 0, len(entries))
	found := false
	for i := 0; i < len(entries); i += 2 {
		if !found && samePath(t, entries[i], aside) {
			found = true
			continue
		}
		kept = append(kept, entries[i])
		if i+1 < len(entries) {
			kept = append(kept, entries[i+1])
		}
	}
	if !found {
		return
	}

	key, err := registry.OpenKey(registry.LOCAL_MACHINE, sessionManagerKey, registry.SET_VALUE)
	if err != nil {
		t.Errorf(`this test queued %s for deletion at the next restart and could not take it back (opening HKLM\%s: %v); `+
			`the path is inside a temporary directory, so the reboot will find nothing there`, aside, sessionManagerKey, err)
		return
	}
	defer key.Close()
	if len(kept) == 0 {
		if err := key.DeleteValue(pendingRenameValue); err != nil {
			t.Errorf(`deleting HKLM\%s\%s: %v`, sessionManagerKey, pendingRenameValue, err)
		}
		return
	}
	if err := key.SetStringsValue(pendingRenameValue, kept); err != nil {
		t.Errorf(`writing HKLM\%s\%s back without %s: %v`, sessionManagerKey, pendingRenameValue, aside, err)
	}
}

// The one test that lets the real MoveFileEx run, and the only evidence
// anywhere that the second half of removeSelf does what its comment says.
//
// Everything else in this file asserts against a recorder, which proves what
// removeSelf ASKED FOR and can never prove that Windows accepted it: a flag
// the kernel rejects, a nil destination that turns out not to mean delete, or
// a call that quietly no-ops would all look identical through a stub.
//
// It runs whatever the privilege level, because both answers are worth
// asserting and neither is a reason to skip. Elevated, the entry is in the
// machine's queue and gets taken back. Unelevated, MoveFileEx is refused and
// the queue must be untouched — a removeSelf that reported success there would
// tell a standard user a reboot finishes the job when nothing was ever queued.
func TestWindowsRemoveSelfWritesOnePendingFileRenameOperation(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "ccdad.exe")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	aside := asidePath(binary)
	before := pendingFileRenames(t)
	t.Cleanup(func() { dropPendingRename(t, aside) })

	scheduled, err := removeSelf(binary)
	if !scheduled {
		t.Fatalf("the rename did not happen: %v", err)
	}
	after := pendingFileRenames(t)

	if err != nil {
		if len(after) != len(before) {
			t.Errorf("scheduling was refused (%v) but the queue went from %d entries to %d", err, len(before), len(after))
		}
		t.Logf("MOVEFILE_DELAY_UNTIL_REBOOT was refused, which is the standard user's path: %v", err)
		return
	}

	// Two entries added, in order, and nothing displaced: the source names the
	// leftover and the destination is empty, which is how the kernel spells
	// "delete this at boot".
	if len(after) != len(before)+2 {
		t.Fatalf("the queue went from %d entries to %d; one delayed delete is one source and one empty destination", len(before), len(after))
	}
	if !samePath(t, after[len(after)-2], aside) {
		t.Errorf("the queued source is %q, want the leftover %s", after[len(after)-2], aside)
	}
	if dest := after[len(after)-1]; dest != "" {
		t.Errorf("the queued destination is %q, want empty — an empty destination is what makes this a delete", dest)
	}
	for i, entry := range before {
		if after[i] != entry {
			t.Fatalf("entry %d of the machine's queue changed from %q to %q; this test may only append its own", i, entry, after[i])
		}
	}
}
