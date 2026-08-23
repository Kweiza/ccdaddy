//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
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
// The kind is the trap §11.3 names. [Environment]::SetEnvironmentVariable and
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
	if len(updated) > maxEnvironmentValue {
		return false, fmt.Errorf(
			"your user PATH would be %d characters, past the %d a process environment can hold; "+
				"ccdad will not truncate it — remove some entries first",
			len(updated), maxEnvironmentValue)
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

// registerUserPath and unregisterUserPath are the two halves `setup-path` and
// `uninstall` call. They are two functions rather than one with a mode because
// they disagree at both ends — see userPathWithDir and userPathWithoutDir.
func registerUserPath(dir string) (bool, error) {
	return withUserPath(func(current string, expand func(string) string) (string, bool) {
		return userPathWithDir(current, dir, expand)
	})
}

func unregisterUserPath(dir string) (bool, error) {
	return withUserPath(func(current string, expand func(string) string) (string, bool) {
		return userPathWithoutDir(current, dir, expand)
	})
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
func setupPathPrint(cmd *cobra.Command, dir string, _ setupPathOptions) error {
	fmt.Fprintf(cmd.OutOrStdout(), "$env:Path = '%s;' + $env:Path\n", strings.ReplaceAll(dir, "'", "''"))
	fmt.Fprintln(cmd.ErrOrStderr(),
		"That line lasts only for the current PowerShell session. Run `ccdad setup-path` "+
			"with no flags to register it in your user PATH, which every new terminal reads.")
	return nil
}

func setupPathApply(cmd *cobra.Command, dir string, _ setupPathOptions) error {
	out := cmd.ErrOrStderr()
	changed, err := registerUserPath(dir)
	if err != nil {
		return err
	}
	onPath := onPathList(os.Getenv("PATH"), dir, livePathRules)
	if !changed {
		// Exit 3 keys off what is REGISTERED, never off the live %PATH% — see
		// the same decision in setuppath_unix.go.
		fmt.Fprintf(out, "Your user PATH already holds %s.\n", dir)
		if !onPath {
			fmt.Fprintln(out, "This terminal has not picked it up yet; open a new one.")
		}
		return WithCode(errSilent, ExitNothingToDo)
	}
	fmt.Fprintf(out, "Added %s to your user PATH.\n", dir)
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
	if dir == "" {
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
	removed, err := unregisterUserPath(dir)
	if err != nil || !removed {
		return nil, err
	}
	// A failed broadcast is not worth failing an uninstall over: the value is
	// already gone and a new terminal will see it.
	_ = broadcastEnvChange()
	return []string{`HKCU\` + environmentSubkey + `\Path`}, nil
}
