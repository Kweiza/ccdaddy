package browser

import (
	"errors"
	"io/fs"
	"slices"
	"testing"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// Spec §6.4 requires headless, SSH and container sessions to be detected so the
// browser attempt is skipped and the prompt is worded for a manual paste. The
// policy is separated from the process environment precisely so it can be
// asserted on any developer machine.
func TestAvailablePolicy(t *testing.T) {
	no := func() bool { return false }
	yes := func() bool { return true }

	for _, tc := range []struct {
		name string
		goos string
		env  map[string]string
		wsl  func() bool
		want bool
	}{
		{"macOS always", "darwin", nil, no, true},
		{"Windows always", "windows", nil, no, true},
		{"X11 session", "linux", map[string]string{"DISPLAY": ":0"}, no, true},
		{"Wayland session", "linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, no, true},
		{"bare TTY or SSH", "linux", nil, no, false},
		{"WSL without DISPLAY", "linux", nil, yes, true},
		// An SSH session with X11 forwarding sets DISPLAY, but that display is
		// on the machine the user is sitting at. Launching there puts the login
		// somewhere nobody is looking.
		{"SSH with X11 forwarding", "linux", map[string]string{"DISPLAY": "localhost:10.0", "SSH_CONNECTION": "10.0.0.1 22 10.0.0.2 22"}, no, false},
		{"SSH via SSH_TTY", "linux", map[string]string{"DISPLAY": ":0", "SSH_TTY": "/dev/pts/3"}, no, false},
		// macOS over SSH is still headless for our purposes.
		{"SSH into macOS", "darwin", map[string]string{"SSH_CONNECTION": "x"}, no, false},
		{"FreeBSD with X11", "freebsd", map[string]string{"DISPLAY": ":0"}, no, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := available(tc.goos, envFrom(tc.env), tc.wsl); got != tc.want {
				t.Fatalf("available(%q, %v) = %v, want %v", tc.goos, tc.env, got, tc.want)
			}
		})
	}
}

func TestIsWSLPolicy(t *testing.T) {
	missing := func(string) ([]byte, error) { return nil, fs.ErrNotExist }
	microsoft := func(string) ([]byte, error) {
		return []byte("Linux version 5.15.0-microsoft-standard-WSL2"), nil
	}
	plain := func(string) ([]byte, error) {
		return []byte("Linux version 6.8.0-124-generic"), nil
	}

	for _, tc := range []struct {
		name string
		goos string
		env  map[string]string
		read func(string) ([]byte, error)
		want bool
	}{
		{"WSL1 sets WSL_DISTRO_NAME", "linux", map[string]string{"WSL_DISTRO_NAME": "Ubuntu"}, missing, true},
		{"WSL2 sets WSL_INTEROP", "linux", map[string]string{"WSL_INTEROP": "/run/WSL/1_interop"}, missing, true},
		{"proc version names microsoft", "linux", nil, microsoft, true},
		{"plain linux is not WSL", "linux", nil, plain, false},
		{"unreadable proc version is not WSL", "linux", nil, missing, false},
		// The empty env var must not count. A tautological substring check made
		// this arm pass for every input in an earlier draft.
		{"empty WSL_DISTRO_NAME is not WSL", "linux", map[string]string{"WSL_DISTRO_NAME": ""}, plain, false},
		// /proc/version is meaningless off Linux and must not even be read.
		{"never claims WSL on darwin", "darwin", map[string]string{"WSL_DISTRO_NAME": "Ubuntu"}, microsoft, false},
		{"never claims WSL on freebsd", "freebsd", nil, microsoft, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWSLFrom(tc.goos, envFrom(tc.env), tc.read); got != tc.want {
				t.Fatalf("isWSLFrom(%q) = %v, want %v", tc.goos, got, tc.want)
			}
		})
	}
}

func TestCommandForEveryPlatform(t *testing.T) {
	const u = "https://example.com/?a=1&b=2"
	for _, tc := range []struct {
		goos     string
		wsl      bool
		wantName string
		wantArgs []string
	}{
		{"darwin", false, "open", []string{u}},
		// rundll32 rather than `cmd /c start`: start treats & as a separator and
		// mangles a query string, and it flashes a console window.
		{"windows", false, "rundll32", []string{"url.dll,FileProtocolHandler", u}},
		{"linux", false, "xdg-open", []string{u}},
		{"freebsd", false, "xdg-open", []string{u}},
		{"openbsd", false, "xdg-open", []string{u}},
		{"linux", true, "rundll32.exe", []string{"url.dll,FileProtocolHandler", u}},
		// A GOOS with no answer must say so, so Open can report it instead of
		// launching something that does not exist.
		{"js", false, "", nil},
		{"plan9", false, "", nil},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			name, args := commandFor(tc.goos, "", tc.wsl, u)
			if name != tc.wantName || !slices.Equal(args, tc.wantArgs) {
				t.Fatalf("commandFor(%q, wsl=%v) = (%q, %v), want (%q, %v)",
					tc.goos, tc.wsl, name, args, tc.wantName, tc.wantArgs)
			}
		})
	}
}

// BROWSER is conventionally a PATH-style list whose entries may carry a %s
// placeholder. Treating the whole value as an executable path breaks both of
// the common real forms.
func TestCommandForHonoursTheBrowserConvention(t *testing.T) {
	const u = "https://example.com"
	for _, tc := range []struct {
		name     string
		goos     string
		env      string
		wantName string
		wantArgs []string
	}{
		{"bare path", "linux", "/usr/bin/my-browser", "/usr/bin/my-browser", []string{u}},
		{"placeholder", "linux", "firefox %s", "firefox", []string{u}},
		{"placeholder mid-args", "linux", "flatpak run org.mozilla.firefox %s", "flatpak", []string{"run", "org.mozilla.firefox", u}},
		{"list takes the first", "linux", "/usr/bin/chromium:/usr/bin/firefox", "/usr/bin/chromium", []string{u}},
		{"list with placeholder", "linux", "chromium %s:firefox %s", "chromium", []string{u}},
		{"windows uses a semicolon list", "windows", `C:\browser.exe;D:\other.exe`, `C:\browser.exe`, []string{u}},
		// Whitespace-only falls through to the platform default rather than
		// launching "".
		{"blank falls through", "linux", "   ", "xdg-open", []string{u}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, args := commandFor(tc.goos, tc.env, false, u)
			if name != tc.wantName || !slices.Equal(args, tc.wantArgs) {
				t.Fatalf("commandFor(%q, %q) = (%q, %v), want (%q, %v)",
					tc.goos, tc.env, name, args, tc.wantName, tc.wantArgs)
			}
		})
	}
}

// Spec §10.1 rejects pkg/browser precisely because it leaks the launcher's
// stderr into the login prompt. Keeping all three streams nil routes them to
// os.DevNull — the whole reason this package is hand-rolled. Stdin matters too:
// the paste reader is blocked on the terminal's stdin and a launcher that
// inherited it would fight for the same bytes.
func TestNewCmdDiscardsLauncherStreams(t *testing.T) {
	t.Setenv("BROWSER", "/nonexistent/browser")

	cmd := newCmd("https://example.com")
	if cmd == nil {
		t.Fatal("newCmd returned nil")
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatalf("streams = (%v, %v, %v), want all nil so exec routes them to os.DevNull",
			cmd.Stdin, cmd.Stdout, cmd.Stderr)
	}
}

// A failed launch must be reported, not panic and not block. Login discards the
// error by design — the URL is already on screen — so the only thing that must
// hold here is that it returns at all.
func TestOpenReportsAnUnlaunchableBrowser(t *testing.T) {
	t.Setenv("BROWSER", "/nonexistent/ccdad-test-browser")

	err := Open("https://example.com")
	if err == nil {
		t.Fatal("Open() = nil, want an error for a browser that does not exist")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Logf("Open() = %v (not fs.ErrNotExist, acceptable)", err)
	}
}

// Available reads the real environment; it must answer without panicking. The
// policy itself is covered by TestAvailablePolicy.
func TestAvailableAnswers(t *testing.T) {
	_ = Available()
}

// Windows program paths routinely contain spaces and Windows has no BROWSER
// command-line convention, so splitting the entry on whitespace launches a
// prefix of the path and the browser silently never opens.
func TestBrowserEnvKeepsAWindowsPathWithSpaces(t *testing.T) {
	const exe = `C:\Program Files\Mozilla Firefox\firefox.exe`

	name, args, ok := fromBrowserEnv("windows", exe, "https://example.com")
	if !ok {
		t.Fatal("fromBrowserEnv() = false, want the entry used")
	}
	if name != exe {
		t.Fatalf("executable = %q, want the whole path %q", name, exe)
	}
	if len(args) != 1 || args[0] != "https://example.com" {
		t.Fatalf("args = %v, want just the URL", args)
	}
	// Unix keeps the command-line convention: whitespace separates arguments.
	name, args, ok = fromBrowserEnv("linux", "firefox --new-tab", "https://example.com")
	if !ok || name != "firefox" || len(args) != 2 || args[0] != "--new-tab" {
		t.Fatalf("unix split = %q %v %v, want firefox with its argument", name, args, ok)
	}
}
