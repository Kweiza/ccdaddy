//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// The half that cannot be written off Windows: neither syscall nor
// x/sys/windows compiles elsewhere, so this is where the literals in spawn.go
// are checked against a definition ccdad does not own.
func TestTheFlagLiteralsMatchTheWindowsDefinitions(t *testing.T) {
	if flagDetachedProcess != windows.DETACHED_PROCESS {
		t.Errorf("flagDetachedProcess = %#x, x/sys/windows says %#x", flagDetachedProcess, windows.DETACHED_PROCESS)
	}
	if flagCreateNewProcessGroup != windows.CREATE_NEW_PROCESS_GROUP {
		t.Errorf("flagCreateNewProcessGroup = %#x, x/sys/windows says %#x", flagCreateNewProcessGroup, windows.CREATE_NEW_PROCESS_GROUP)
	}
	if flagCreateNewProcessGroup != syscall.CREATE_NEW_PROCESS_GROUP {
		t.Errorf("flagCreateNewProcessGroup = %#x, syscall says %#x", flagCreateNewProcessGroup, syscall.CREATE_NEW_PROCESS_GROUP)
	}
}

// This runs only on Windows, so everywhere else it is compiled by
// `GOOS=windows go vet ./...` and no further. It pins the flag set against
// being emptied or widened, which is worth having because the alternative is
// nothing at all — but it asserts a struct, not an operating system. Whether
// DETACHED_PROCESS actually severs the console is what the post-release
// install smoke suite is for.
func TestDetachSetsExactlyTheTwoCreationFlags(t *testing.T) {
	cmd := exec.Command("cmd.exe")
	if err := detach(cmd); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("detach left SysProcAttr nil, so the child would inherit the console")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow is false")
	}
	want := uint32(flagDetachedProcess | flagCreateNewProcessGroup)
	if got := cmd.SysProcAttr.CreationFlags; got != want {
		t.Errorf("CreationFlags = %#x, want %#x — DETACHED_PROCESS is what keeps the child alive "+
			"when the console window closes, and the pair is deliberately exactly two", got, want)
	}
}

// DETACHED_PROCESS observed rather than spelled.
//
// TestDetachSetsExactlyTheTwoCreationFlags above proves the flag reaches
// SysProcAttr, and TestTheFlagLiteralsMatchTheWindowsDefinitions proves the
// number is the right one. Neither proves the operating system acted on it, and
// nothing else can: Go hands CreateProcess an explicit inherited-handle list,
// so the pipe assertion in spawn_test.go — the closest thing to a behavioural
// test Spawn has — would go green on Windows with both flags deleted.
//
// A console is what DETACHED_PROCESS takes away, so a console is what this
// needs to take it away FROM. ensureConsole supplies one rather than asking
// whether the runner brought one, for the reason written there.
func TestSpawnLeavesTheChildWithNoConsole(t *testing.T) {
	ensureConsole(t)

	report, _ := spawnViaAChildThatExits(t)

	if got := report["console"]; got != "false" {
		t.Errorf("the detached child reports console=%q, want \"false\" — DETACHED_PROCESS did not take, "+
			"so the daemon shares the console it was started from and dies with it on CTRL_CLOSE_EVENT", got)
	}
}
