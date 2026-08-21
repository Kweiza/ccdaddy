// Package browser opens a URL in the user's browser.
//
// Hand-rolled rather than taken from a library, for two reasons: the popular one
// leaks the launcher's stderr into the terminal, which lands in the middle of a
// login prompt, and detecting a headless machine matters more here than the
// launch itself. Roughly thirty lines is a fair trade.
//
// The policy decisions — is a browser plausible, which command opens a URL — are
// pure functions of an OS name and an environment lookup. Reading the real
// process environment happens only in the two exported wrappers, so the policy
// can be asserted on any machine instead of only on the one running the test.
package browser

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Available reports whether opening a browser is plausible on this machine.
//
// A false answer is not fatal: ccdad prints the URL and waits for a pasted code
// either way. It only decides whether to attempt the launch and how to word the
// prompt.
func Available() bool { return available(runtime.GOOS, os.Getenv, isWSL) }

func available(goos string, getenv func(string) string, wsl func() bool) bool {
	// An SSH session with X11 forwarding sets DISPLAY, but that display belongs
	// to the machine the user is sitting at, not this one. A browser launched
	// here opens somewhere nobody is looking, so this is checked before the
	// platforms that otherwise always say yes.
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" {
		return false
	}

	switch goos {
	case "darwin", "windows":
		return true
	}
	// Linux and the BSDs: a graphical session needs one of these. A container or
	// a bare TTY has neither.
	if getenv("DISPLAY") == "" && getenv("WAYLAND_DISPLAY") == "" {
		// WSL is the exception: no DISPLAY, but the Windows shell can still open
		// a URL.
		return wsl()
	}
	return true
}

// isWSL reports whether this is a WSL session, where there is no DISPLAY but the
// Windows shell can still open a URL.
func isWSL() bool { return isWSLFrom(runtime.GOOS, os.Getenv, os.ReadFile) }

func isWSLFrom(goos string, getenv func(string) string, readFile func(string) ([]byte, error)) bool {
	if goos != "linux" {
		// /proc/version says nothing about WSL anywhere else, and on the BSDs it
		// is another OS's file entirely.
		return false
	}
	// WSL1 sets WSL_DISTRO_NAME; WSL2 sets both it and WSL_INTEROP.
	if getenv("WSL_DISTRO_NAME") != "" || getenv("WSL_INTEROP") != "" {
		return true
	}
	data, err := readFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft")
}

// Open launches url. Failure is not fatal to a login — the caller has already
// printed the URL.
func Open(url string) error {
	cmd := newCmd(url)
	if cmd == nil {
		return fmt.Errorf("no way to open a browser on %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opening a browser: %w", err)
	}
	// Do not wait: some launchers block for the lifetime of the browser. Reaping
	// in the background keeps the child from lingering as a zombie.
	go func() { _ = cmd.Wait() }()
	return nil
}

// newCmd builds the launch command with every stream discarded. It returns nil
// when this platform has no way to open a URL.
func newCmd(url string) *exec.Cmd {
	name, args := commandFor(runtime.GOOS, os.Getenv("BROWSER"), isWSL(), url)
	if name == "" {
		return nil
	}
	cmd := exec.Command(name, args...)
	// Discard the launcher's streams. xdg-open and its friends write warnings to
	// stderr that would land in the middle of the login prompt, and a nil Stdin
	// matters just as much: the paste reader is blocked on the terminal's stdin
	// and a launcher that inherited it would fight for the same bytes. os/exec
	// wires each nil stream to os.DevNull.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	return cmd
}

// commandFor picks the launch command. It is a pure function of the platform,
// the BROWSER value and the WSL flag so every branch is reachable from a test on
// any host.
//
// An empty name means this platform has no answer, which Open reports rather
// than trying to execute.
func commandFor(goos, browserEnv string, wsl bool, url string) (string, []string) {
	if name, args, ok := fromBrowserEnv(goos, browserEnv, url); ok {
		return name, args
	}
	switch goos {
	case "darwin":
		return "open", []string{url}
	case "windows":
		// rundll32 rather than `cmd /c start`: start treats & as a separator and
		// mangles a query string, and it flashes a console window.
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "js", "plan9", "wasip1":
		return "", nil
	default:
		if wsl {
			// The Windows shell reached through interop. wslview would be
			// friendlier but needs a PATH lookup, which would put an
			// environment dependency back into this function.
			return "rundll32.exe", []string{"url.dll,FileProtocolHandler", url}
		}
		return "xdg-open", []string{url}
	}
}

// fromBrowserEnv honours the BROWSER convention: a PATH-style list whose entries
// may carry a %s placeholder for the URL. Treating the whole value as one
// executable path breaks both common real forms ("firefox %s" and "/a:/b"), and
// the failure is silent because Login discards the launch error.
func fromBrowserEnv(goos, browserEnv, url string) (string, []string, bool) {
	if browserEnv == "" {
		return "", nil, false
	}
	sep := ":"
	if goos == "windows" {
		sep = ";"
	}
	entry := strings.Split(browserEnv, sep)[0]
	// On Unix, BROWSER's convention is a command line: whitespace separates the
	// executable from its arguments. Windows has no such convention, and a
	// program path with a space in it is the norm rather than the exception —
	// splitting "C:\Program Files\x\b.exe" on whitespace launches
	// "C:\Program", which fails silently.
	var fields []string
	if goos == "windows" {
		if t := strings.TrimSpace(entry); t != "" {
			fields = []string{t}
		}
	} else {
		fields = strings.Fields(entry)
	}
	if len(fields) == 0 {
		// Whitespace only. Fall through to the platform default rather than
		// trying to launch "".
		return "", nil, false
	}

	args := fields[1:]
	substituted := false
	for i, a := range args {
		if strings.Contains(a, "%s") {
			args[i] = strings.ReplaceAll(a, "%s", url)
			substituted = true
		}
	}
	if !substituted {
		args = append(args, url)
	}
	return fields[0], args, true
}
