//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The Windows half of `ccdad setup-path`. There is no startup file to write:
// the durable user PATH lives in HKCU\Environment, and install.ps1 has written
// it since it shipped (install.ps1:180-229). This is the same operation in Go,
// so that the command works for an install that did not come from install.ps1
// and so that `ccdad uninstall` has something to call.
//
// The decisions are all in setuppath.go and tested on Linux. What is here is
// only the registry access and the broadcast, which nothing but a Windows
// machine can execute.

// livePathRules reads %PATH% as cmd.exe and PowerShell do.
var livePathRules = windowsPathRules

// setupPathPlatformHelp is the half of --help that is only true here. There is
// no startup file and no marker block on Windows: the entry goes into the user
// PATH in the registry, and ccdad writes down that it put it there so that
// uninstall can take back that entry and only that entry.
const setupPathPlatformHelp = "It adds the directory to your user PATH in the registry and tells running\n" +
	"programs about the change. --print shows a PowerShell line that lasts only for\n" +
	"the current session; the registration this command makes is permanent.\n"

// setupPathFlags adds nothing on Windows: there is no shell whose startup file
// is being written, so --shell would be a flag that is accepted and ignored.
func setupPathFlags(*cobra.Command, *setupPathOptions) {}

// The registry location, behind two vars so a test on a Windows runner can
// point the whole operation at a scratch key. Without them, `go test ./...` on
// a contributor's own machine would leave one t.TempDir() in their real PATH
// per test, permanently — install_ps1_test.go:214-224 had to skip on Windows
// for exactly this, and a seam is the version of that which still tests
// something.
var (
	environmentRoot   = registry.CURRENT_USER
	environmentSubkey = "Environment"
)

// maxEnvironmentValue is the per-variable ceiling in a process environment
// block. Beyond it the write still succeeds and new processes get a PATH they
// cannot use, so this refuses rather than truncating — a truncated PATH is a
// machine where half the user's tools stop resolving, caused by ccdad.
const maxEnvironmentValue = 32767

// withUserPath opens HKCU\Environment, hands the current Path and its value
// kind to decide, and writes back whatever decide returns — with the SAME kind.
//
// The kind is the trap. [Environment]::SetEnvironmentVariable and
// registry.SetStringValue both write REG_SZ, and a Path that was REG_EXPAND_SZ
// loses %VAR% expansion for every entry in it the moment one of those runs.
// Go's advantage over PowerShell here is that reading is already raw:
// GetStringValue goes straight to RegQueryValueEx, which never expands, and
// returns the type alongside the value — so the one rule is never to call
// registry.ExpandString on what is about to be written back.
func withUserPath(decide func(current string, expand func(string) string) (string, bool)) (bool, error) {
	key, err := registry.OpenKey(environmentRoot, environmentSubkey,
		registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, fmt.Errorf(`opening HKCU\%s: %w`, environmentSubkey, err)
	}
	defer key.Close()

	current, kind, err := key.GetStringValue("Path")
	switch {
	case errors.Is(err, registry.ErrNotExist):
		// A profile that has never had a user PATH. install.ps1 creates it as
		// ExpandString, and these two must agree or the same machine ends up
		// with a different kind depending on which one ran first.
		current, kind = "", registry.EXPAND_SZ
	case errors.Is(err, registry.ErrUnexpectedType):
		return false, fmt.Errorf(
			`HKCU\%s\Path is registry type %d, which is neither REG_SZ nor REG_EXPAND_SZ; `+
				`ccdad will not overwrite a value it cannot read — fix it by hand`, environmentSubkey, kind)
	case err != nil:
		return false, fmt.Errorf(`reading HKCU\%s\Path: %w`, environmentSubkey, err)
	}

	updated, changed := decide(current, expandRegistryString)
	if !changed {
		return false, nil
	}
	// Counted in UTF-16 code units plus the terminator, which is what Windows
	// stores and what RegSetValueEx is handed. len() would count UTF-8 bytes,
	// so a user with CJK directories on their PATH would be refused a write
	// that fits — and told a "character" count that is not one.
	if n := len(utf16.Encode([]rune(updated))) + 1; n > maxEnvironmentValue {
		return false, fmt.Errorf(
			"your user PATH would be %d characters, past the %d a process environment can hold; "+
				"ccdad will not truncate it — remove some entries first",
			n-1, maxEnvironmentValue)
	}

	switch kind {
	case registry.EXPAND_SZ:
		err = key.SetExpandStringValue("Path", updated)
	default:
		err = key.SetStringValue("Path", updated)
	}
	if err != nil {
		return false, fmt.Errorf(`writing HKCU\%s\Path: %w`, environmentSubkey, err)
	}
	return true, nil
}

// expandRegistryString is registry.ExpandString with its error swallowed: a
// component that cannot be expanded is compared as it stands, which is what the
// raw comparison already did.
func expandRegistryString(s string) string {
	expanded, err := registry.ExpandString(s)
	if err != nil {
		return s
	}
	return expanded
}

// The record of what ccdad put on the user PATH, and the whole reason removal
// is safe here.
//
// Unix has the marker fence: a block is ccdad's because ccdad's markers are
// around it. A registry PATH component carries no such evidence — it is a bare
// directory string — so removing "the directory the binary is in" would strip
// entries ccdad never added. That is not a corner case, it is the ordinary one:
// `go install` puts ccdad.exe in %USERPROFILE%\go\bin beside every other Go
// tool the user has, a zip install goes wherever the user already keeps their
// tools, and both directories are on PATH because the USER put them there.
// Removing that on uninstall breaks every unrelated program in it.
//
// So registration writes down what it added, and removal takes back only that.
// An entry ccdad did not record is left alone and said so, which is the safe
// direction: a stale PATH entry costs a lookup, a deleted one costs the user
// their toolchain.
var ccdadSubkey = `Software\ccdad`

const pathEntryValue = "PathEntry"

// recordPathEntry remembers the directory ccdad just added.
func recordPathEntry(dir string) error {
	key, _, err := registry.CreateKey(environmentRoot, ccdadSubkey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf(`recording the PATH entry under HKCU\%s: %w`, ccdadSubkey, err)
	}
	defer key.Close()
	return key.SetStringValue(pathEntryValue, dir)
}

// recordedPathEntry is the directory ccdad recorded adding, if any.
func recordedPathEntry() (string, bool) {
	key, err := registry.OpenKey(environmentRoot, ccdadSubkey, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer key.Close()
	dir, _, err := key.GetStringValue(pathEntryValue)
	if err != nil || dir == "" {
		return "", false
	}
	return dir, true
}

func forgetPathEntry() {
	key, err := registry.OpenKey(environmentRoot, ccdadSubkey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer key.Close()
	_ = key.DeleteValue(pathEntryValue)
}

// ccdadAddedPathEntry reports whether ccdad recorded adding exactly dir.
// Compared under the same rules as the PATH components themselves, so a
// trailing backslash or a difference in case still matches.
func ccdadAddedPathEntry(dir string) bool {
	recorded, ok := recordedPathEntry()
	return ok && windowsPathRules.same(recorded, dir)
}

// registerUserPath and unregisterUserPath are the two halves `setup-path` and
// `uninstall` call. They are two functions rather than one with a mode because
// they disagree at both ends — see userPathWithDir and userPathWithoutDir.
func registerUserPath(dir string) (bool, error) {
	added, err := withUserPath(func(current string, expand func(string) string) (string, bool) {
		return userPathWithDir(current, dir, expand)
	})
	if err != nil || !added {
		// Deliberately NOT recorded when the entry was already there. A user
		// whose %USERPROFILE%\go\bin is on PATH because they put it there runs
		// setup-path, gets exit 3, and must not thereby hand ccdad permission
		// to delete that entry later.
		return added, err
	}
	if err := recordPathEntry(dir); err != nil {
		return true, err
	}
	return true, nil
}

func unregisterUserPath(dir string) (bool, error) {
	if !ccdadAddedPathEntry(dir) {
		return false, nil
	}
	removed, err := withUserPath(func(current string, expand func(string) string) (string, bool) {
		return userPathWithoutDir(current, dir, expand)
	})
	if err != nil {
		return removed, err
	}
	forgetPathEntry()
	return removed, nil
}

// broadcastEnvChange tells running processes the environment changed. Without
// it, only processes started after the next sign-out see the new PATH.
//
// A package var so a test can watch the policy — that a failed broadcast warns
// and does not fail the command — without a desktop to broadcast to.
var broadcastEnvChange = sendSettingChange

var procSendMessageTimeoutW = windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")

func sendSettingChange() error {
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
		timeoutMS       = 5000
	)
	// Find first. LazyProc.Call resolves through mustFind, which PANICS when
	// the DLL or the export is missing — on a Server Core or container image
	// that would take down a command whose registry write has already
	// succeeded.
	if err := procSendMessageTimeoutW.Find(); err != nil {
		return err
	}
	lParam, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return err
	}
	var result uintptr
	// SendMessageTimeout, never SendMessage: a plain broadcast waits for every
	// top-level window on the desktop, so one hung modal hangs ccdad with no
	// way to interrupt it.
	ret, _, callErr := procSendMessageTimeoutW.Call(
		uintptr(hwndBroadcast),
		uintptr(wmSettingChange),
		0,
		uintptr(unsafe.Pointer(lParam)),
		uintptr(smtoAbortIfHung),
		uintptr(timeoutMS),
		uintptr(unsafe.Pointer(&result)),
	)
	// The primary return decides. LazyProc.Call builds its error from
	// GetLastError unconditionally and it is documented as always non-nil, so
	// consulting callErr first reports every successful broadcast as a failure.
	if ret == 0 {
		// LazyProc.Call builds callErr from GetLastError unconditionally and it
		// is documented as always non-nil, so on a failure that did not set one
		// it is Errno(0) — which formats as "The operation completed
		// successfully" and would send a bug report in exactly the wrong
		// direction. Replaced with what is actually known.
		//
		// The remaining error text is best-effort and says so here rather than
		// pretending otherwise: last-error is per-OS-thread, and clearing it
		// before the call would only be sound with runtime.LockOSThread held
		// across both calls, which is not worth taking for a warning that
		// costs the user one new terminal.
		if errno, ok := callErr.(syscall.Errno); ok && errno == 0 {
			return errors.New("a window on the desktop did not answer within 5s")
		}
		return callErr
	}
	return nil
}

// setupPathPrint is --print on Windows. It emits a PROCESS-scope PowerShell
// line, which lasts only for the current session, and says so — the durable
// form is this command without --print.
//
// What it must never emit is `setx`: setx reads the Machine+User
// concatenation, writes REG_SZ (destroying %VAR% expansion), and truncates at
// 1024 characters. Handing a user that line is handing them the defect this
// command exists to avoid.
func setupPathPrint(cmd *cobra.Command, dirs []string, _ setupPathOptions) error {
	for _, dir := range dirs {
		fmt.Fprintf(cmd.OutOrStdout(), "$env:Path = '%s;' + $env:Path\n", strings.ReplaceAll(dir, "'", "''"))
	}
	fmt.Fprintln(cmd.ErrOrStderr(),
		"That line lasts only for the current PowerShell session. Run `ccdad setup-path` "+
			"with no flags to register it in your user PATH, which every new terminal reads.")
	return nil
}

// dirs holds at most one entry here, and the loop below is written for that
// rather than around it. The second entry the derived set can carry is the
// codex shim's directory, and there is no shim on Windows: `ccdad codex shim
// install` refuses there, so the record registeredDirs keys on is never
// written. A loop is still what is written, because a `dirs[0]` would be a
// silent truncation the day that changes.
func setupPathApply(cmd *cobra.Command, dirs []string, _ setupPathOptions) error {
	out := cmd.ErrOrStderr()
	anyChanged, allOnPath := false, true
	for _, dir := range dirs {
		changed, err := registerUserPath(dir)
		if err != nil {
			return err
		}
		if !onPathList(os.Getenv("PATH"), dir, livePathRules) {
			allOnPath = false
		}
		if !changed {
			fmt.Fprintf(out, "Your user PATH already holds %s.\n", dir)
			continue
		}
		anyChanged = true
		fmt.Fprintf(out, "Added %s to your user PATH.\n", dir)
	}
	onPath := allOnPath
	if !anyChanged {
		// Exit 3 keys off what is REGISTERED, never off the live %PATH% — see
		// the same decision in setuppath_unix.go.
		if !onPath {
			fmt.Fprintln(out, "This terminal has not picked it up yet; open a new one.")
		}
		return WithCode(errSilent, ExitNothingToDo)
	}
	if err := broadcastEnvChange(); err != nil {
		// The write is already durable. A failed broadcast costs the user one
		// new terminal, which is not worth failing a command over.
		fmt.Fprintf(out, "The change could not be announced to running programs (%v); "+
			"open a new terminal to pick it up.\n", err)
		return nil
	}
	fmt.Fprintln(out, "Open a new terminal to pick it up. Note that ccdad is APPENDED, "+
		"so another ccdad.exe earlier on PATH still wins.")
	return nil
}

// pathRegistrations reports whether the user PATH holds dir, in the one form
// `ccdad uninstall` can show a user before it asks. It reads and writes
// nothing.
//
// Unlike the Unix half there is no fence to recognise: a registry PATH entry is
// only ccdad's by virtue of being the directory ccdad is installed in, so an
// unknown dir finds nothing.
func pathRegistrations(dir string) ([]string, error) {
	if dir == "" || !ccdadAddedPathEntry(dir) {
		return nil, nil
	}
	key, err := registry.OpenKey(environmentRoot, environmentSubkey, registry.QUERY_VALUE)
	if err != nil {
		return nil, nil
	}
	defer key.Close()
	current, _, err := key.GetStringValue("Path")
	if err != nil {
		return nil, nil
	}
	if _, changed := userPathWithoutDir(current, dir, expandRegistryString); !changed {
		return nil, nil
	}
	return []string{`HKCU\` + environmentSubkey + `\Path`}, nil
}

// unregisterPath removes dir from the user PATH and announces the change, so
// that a terminal open across the uninstall stops resolving a binary that is no
// longer there.
func unregisterPath(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	if !ccdadAddedPathEntry(dir) {
		// Silent when there is nothing on PATH to talk about; otherwise say why
		// it is being left, so a user who expected it gone knows to remove it.
		if onPathList(os.Getenv("PATH"), dir, livePathRules) {
			return nil, fmt.Errorf(
				"%s is on your PATH, but ccdad has no record of putting it there, so it is left alone "+
					"(remove it yourself if you want it gone)", dir)
		}
		return nil, nil
	}
	removed, err := unregisterUserPath(dir)
	if err != nil || !removed {
		return nil, err
	}
	// A failed broadcast is not worth failing an uninstall over: the value is
	// already gone and a new terminal will see it.
	_ = broadcastEnvChange()
	return []string{`HKCU\` + environmentSubkey + `\Path`}, nil
}
