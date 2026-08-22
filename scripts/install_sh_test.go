package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The four assets install.sh can ever ask for. The other two targets are
// Windows, which is install.ps1's job.
var unixAssets = []string{
	"ccdad-darwin-amd64",
	"ccdad-darwin-arm64",
	"ccdad-linux-amd64",
	"ccdad-linux-arm64",
}

// fakeRelease stands in for a GitHub release. It records every path it is
// asked for, so a test can assert which asset the platform detection resolved
// to without inspecting the install directory.
type fakeRelease struct {
	mu       sync.Mutex
	requests []string

	files map[string][]byte // by base name
	sums  []byte            // served as sha256sums.txt; nil serves a 404
}

func (f *fakeRelease) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.Path)
	f.mu.Unlock()

	name := path.Base(r.URL.Path)
	if name == "sha256sums.txt" {
		if f.sums == nil {
			http.NotFound(w, r)
			return
		}
		w.Write(f.sums)
		return
	}
	body, ok := f.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write(body)
}

func (f *fakeRelease) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// fakeBinary is a shell script padded past install.sh's one-megabyte shape
// check, so it is both a plausible download and something the installer can
// actually execute afterwards.
func fakeBinary(marker string) []byte {
	body := "#!/usr/bin/env bash\n" +
		"case \"${1:-}\" in\n" +
		"--version) echo 'ccdad version " + marker + "' ;;\n" +
		"daemon) if [ -n \"${CCDAD_FAKE_LOG:-}\" ]; then echo \"daemon ${2:-}\" >>\"$CCDAD_FAKE_LOG\"; fi; exit 2 ;;\n" +
		"*) exit 2 ;;\n" +
		"esac\n"
	return append([]byte(body), []byte("# "+strings.Repeat("x", 1<<20)+"\n")...)
}

func sumsFor(files map[string][]byte) []byte {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		digest := sha256.Sum256(files[name])
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(digest[:]), name)
	}
	return []byte(b.String())
}

// newFakeRelease serves the four unix assets with a correct sums file.
func newFakeRelease(t *testing.T) *fakeRelease {
	t.Helper()
	files := make(map[string][]byte, len(unixAssets))
	for _, name := range unixAssets {
		files[name] = fakeBinary("9.9.9-" + name)
	}
	f := &fakeRelease{files: files, sums: sumsFor(files)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("CCDAD_TEST_ORIGIN", srv.URL)
	return f
}

// unameShim puts a uname(1) on PATH that answers for a platform this machine
// is not, which is the only way to exercise the other five mappings.
func unameShim(t *testing.T, system, machine string) string {
	t.Helper()
	dir := t.TempDir()
	body := "#!/usr/bin/env bash\ncase \"${1:-}\" in\n" +
		"-s) echo '" + system + "' ;;\n" +
		"-m) echo '" + machine + "' ;;\n" +
		"*) echo '" + system + "' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "uname"), []byte(body), 0o755); err != nil {
		t.Fatalf("writing the uname shim: %v", err)
	}
	return dir
}

func runInstallSh(t *testing.T, dir string, extra ...string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is the macOS and Linux path; Windows is install.ps1")
	}
	script, err := filepath.Abs(filepath.Join("..", "install.sh"))
	if err != nil {
		t.Fatalf("resolving install.sh: %v", err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		"CCDAD_BASE_URL="+os.Getenv("CCDAD_TEST_ORIGIN"),
		"CCDAD_INSTALL_DIR="+dir,
		"CCDAD_VERSION=",
	)
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// leftovers reports scratch directories install.sh's EXIT trap should have
// removed. They live in the install directory rather than /tmp so the final
// move is a same-filesystem rename, which makes them visible here.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var found []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ccdad-install.") {
			found = append(found, e.Name())
		}
	}
	return found
}

func TestInstallShMapsEveryTargetItShips(t *testing.T) {
	for _, tc := range []struct{ system, machine, asset string }{
		{"Linux", "x86_64", "ccdad-linux-amd64"},
		{"Linux", "amd64", "ccdad-linux-amd64"},
		// clauth's bug: it accepts only x86_64 on Linux and bounces every
		// arm64 user, including every WSL arm64 user, since WSL correctly
		// reports Linux.
		{"Linux", "aarch64", "ccdad-linux-arm64"},
		{"Linux", "arm64", "ccdad-linux-arm64"},
		{"Darwin", "x86_64", "ccdad-darwin-amd64"},
		{"Darwin", "arm64", "ccdad-darwin-arm64"},
	} {
		t.Run(tc.system+"/"+tc.machine, func(t *testing.T) {
			release := newFakeRelease(t)
			dir := t.TempDir()
			shim := unameShim(t, tc.system, tc.machine)
			out, err := runInstallSh(t, dir, "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err != nil {
				t.Fatalf("install.sh: %v\n%s", err, out)
			}
			if !slices.Contains(release.asked(), "/latest/download/"+tc.asset) {
				t.Errorf("%s/%s asked for %v, want %s", tc.system, tc.machine, release.asked(), tc.asset)
			}
			installed, err := os.ReadFile(filepath.Join(dir, "ccdad"))
			if err != nil {
				t.Fatalf("reading the installed binary: %v", err)
			}
			if !strings.Contains(string(installed[:200]), "9.9.9-"+tc.asset) {
				t.Errorf("%s/%s installed some other asset", tc.system, tc.machine)
			}
			// install.sh reports the version by running what it just
			// installed, so this line is also the proof that the binary
			// arrived executable.
			if want := "installed ccdad version 9.9.9-" + tc.asset; !strings.Contains(out, want) {
				t.Errorf("install.sh said:\n%s\nwant a line containing %q", out, want)
			}
		})
	}
}

func TestInstallShRefusesAPlatformItDoesNotShip(t *testing.T) {
	for _, tc := range []struct{ system, machine, want string }{
		{"Linux", "i686", "unsupported architecture"},
		{"Linux", "armv7l", "unsupported architecture"},
		{"FreeBSD", "amd64", "unsupported operating system"},
		// Git Bash and MSYS report a Windows-shaped uname. The useful answer
		// there is the other installer, not "unsupported".
		{"MINGW64_NT-10.0", "x86_64", "install.ps1"},
		{"MSYS_NT-10.0", "x86_64", "install.ps1"},
	} {
		t.Run(tc.system+"/"+tc.machine, func(t *testing.T) {
			newFakeRelease(t)
			dir := t.TempDir()
			shim := unameShim(t, tc.system, tc.machine)
			out, err := runInstallSh(t, dir, "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err == nil {
				t.Fatalf("install.sh exited 0 on %s/%s:\n%s", tc.system, tc.machine, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("install.sh said:\n%s\nwant a message containing %q", out, tc.want)
			}
			if _, err := os.Stat(filepath.Join(dir, "ccdad")); err == nil {
				t.Error("install.sh installed something for a platform it does not ship")
			}
		})
	}
}

// Fail-closed is three distinct aborts, not one. Each is a different failure
// of the same guarantee, and any of them silently continuing would leave the
// installer verifying nothing.
func TestInstallShAbortsWhenItCannotVerifyTheDownload(t *testing.T) {
	good := func() map[string][]byte {
		return map[string][]byte{"ccdad-linux-amd64": fakeBinary("9.9.9-real")}
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*fakeRelease)
		wantMsg string
	}{
		{
			"the checksum file does not arrive",
			func(f *fakeRelease) { f.sums = nil },
			"cannot download",
		},
		{
			"the asset is not listed in it",
			func(f *fakeRelease) {
				f.sums = sumsFor(map[string][]byte{"ccdad-darwin-arm64": f.files["ccdad-linux-amd64"]})
			},
			"is not listed",
		},
		{
			"the hash does not match",
			func(f *fakeRelease) {
				f.sums = sumsFor(map[string][]byte{"ccdad-linux-amd64": fakeBinary("a different build")})
			},
			"checksum mismatch",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := good()
			release := &fakeRelease{files: files, sums: sumsFor(files)}
			tc.mutate(release)
			srv := httptest.NewServer(release)
			t.Cleanup(srv.Close)
			t.Setenv("CCDAD_TEST_ORIGIN", srv.URL)

			dir := t.TempDir()
			shim := unameShim(t, "Linux", "x86_64")
			out, err := runInstallSh(t, dir, "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err == nil {
				t.Fatalf("install.sh exited 0 having verified nothing:\n%s", out)
			}
			if !strings.Contains(out, tc.wantMsg) {
				t.Errorf("install.sh said:\n%s\nwant a message containing %q", out, tc.wantMsg)
			}
			if _, err := os.Stat(filepath.Join(dir, "ccdad")); err == nil {
				t.Error("install.sh installed a binary it could not verify")
			}
			if got := leftovers(t, dir); len(got) > 0 {
				t.Errorf("scratch directories left behind: %v", got)
			}
		})
	}
}

// The three aborts above are only as good as the line the hash is extracted
// with. Each case here is a sums file that a looser regex, or none, would
// accept.
func TestInstallShIsNotFooledByANearMissSumsFile(t *testing.T) {
	asset := fakeBinary("9.9.9-real")
	correct := sumsFor(map[string][]byte{"ccdad-linux-amd64": asset})
	digest := sha256.Sum256(asset)
	hex64 := hex.EncodeToString(digest[:])

	for _, tc := range []struct {
		name    string
		sums    []byte
		wantMsg string
	}{
		{
			// sha256sum and `shasum -a 256` both emit two spaces. A one-space
			// regex matches nothing, which is safe; a one-space FILE with a
			// two-space regex is the same abort, and pins that the installer
			// never falls back to a looser match.
			"one space instead of two",
			[]byte(strings.Replace(string(correct), "  ", " ", 1)),
			"is not a checksum file",
		},
		{
			// Without the trailing anchor this line matches ccdad-linux-amd64.
			"only a longer neighbour is listed",
			[]byte(hex64 + "  ccdad-linux-amd64.exe\n"),
			"is not listed",
		},
		{
			// Without the leading anchor this matches as a substring.
			"the asset appears only as a suffix",
			[]byte("sha256:" + hex64 + "  ccdad-linux-amd64\n"),
			"is not a checksum file",
		},
		{
			// The shape check passes on the first line, so only the leading
			// anchor on the extraction stops the second one being read as our
			// asset's hash.
			"a well-formed line hides an unanchored one",
			[]byte(hex64 + "  ccdad-darwin-arm64\n" + "sha256:" + hex64 + "  ccdad-linux-amd64\n"),
			"is not listed",
		},
		{
			// `releases/latest/download` is a redirect, and a proxy that
			// answers it with its own error page produces exactly this.
			"an HTML error page",
			[]byte("<html><head><title>403 Forbidden</title></head></html>\n"),
			"is not a checksum file",
		},
		{
			"uppercase hex",
			[]byte(strings.ToUpper(hex64) + "  ccdad-linux-amd64\n"),
			"is not a checksum file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := &fakeRelease{
				files: map[string][]byte{"ccdad-linux-amd64": asset},
				sums:  tc.sums,
			}
			srv := httptest.NewServer(release)
			t.Cleanup(srv.Close)
			t.Setenv("CCDAD_TEST_ORIGIN", srv.URL)

			dir := t.TempDir()
			shim := unameShim(t, "Linux", "x86_64")
			out, err := runInstallSh(t, dir, "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err == nil {
				t.Fatalf("install.sh accepted this sums file:\n%s\noutput:\n%s", tc.sums, out)
			}
			if !strings.Contains(out, tc.wantMsg) {
				t.Errorf("install.sh said:\n%s\nwant a message containing %q", out, tc.wantMsg)
			}
			if _, err := os.Stat(filepath.Join(dir, "ccdad")); err == nil {
				t.Error("install.sh installed a binary it could not verify")
			}
		})
	}
}

// A proxy that answers the asset redirect with an error page produces a
// download that is the wrong shape long before it is the wrong hash. Saying so
// is worth a branch of its own, because "checksum mismatch" sends the reader
// looking for a compromised release.
func TestInstallShRejectsADownloadThatIsNotABinary(t *testing.T) {
	page := []byte("<html><body>404 Not Found</body></html>\n")
	files := map[string][]byte{"ccdad-linux-amd64": page}
	release := &fakeRelease{files: files, sums: sumsFor(files)}
	srv := httptest.NewServer(release)
	t.Cleanup(srv.Close)
	t.Setenv("CCDAD_TEST_ORIGIN", srv.URL)

	dir := t.TempDir()
	shim := unameShim(t, "Linux", "x86_64")
	out, err := runInstallSh(t, dir, "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err == nil {
		t.Fatalf("install.sh installed a 40-byte HTML page:\n%s", out)
	}
	if !strings.Contains(out, "not a ccdad binary") {
		t.Errorf("install.sh said:\n%s\nwant it to name the shape, not the checksum", out)
	}
}

// The daemon-stop step runs the OLD binary, and every ccdad released before
// the daemon command group answers `unknown command "daemon"` and exits 2. If
// that non-zero exit aborted the install, every upgrade from an older ccdad
// would fail.
func TestInstallShUpgradesOverABinaryWithNoDaemonCommand(t *testing.T) {
	newFakeRelease(t)
	dir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "stop.log")
	if err := os.WriteFile(filepath.Join(dir, "ccdad"), fakeBinary("0.0.1-old"), 0o755); err != nil {
		t.Fatalf("planting the old binary: %v", err)
	}

	shim := unameShim(t, "Linux", "x86_64")
	out, err := runInstallSh(t, dir,
		"PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CCDAD_FAKE_LOG="+logFile,
	)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	stopped, err := os.ReadFile(logFile)
	if err != nil || !strings.Contains(string(stopped), "daemon stop") {
		t.Errorf("the old binary was not asked to stop its daemon (log %q, err %v) — "+
			"leaving the old daemon running old code and holding the singleton lock", stopped, err)
	}
	installed, err := os.ReadFile(filepath.Join(dir, "ccdad"))
	if err != nil {
		t.Fatalf("reading the installed binary: %v", err)
	}
	if strings.Contains(string(installed[:200]), "0.0.1-old") {
		t.Error("install.sh left the old binary in place")
	}
}

// `curl | bash` has the script itself on stdin, so the installer cannot ask
// permission, and a startup file it guessed at is a file it can corrupt. It
// prints a PATH warning and hands over a line to paste instead.
//
// It used to name `ccdad setup-path`, and this test pinned that. The command
// does not exist in the tree: the installer's last instruction was one the
// freshly installed binary answers with `unknown command "setup-path"`, which
// is a worse ending than asking for a copy and paste. What is pinned now is
// that the advice is ACTIONABLE — the real install directory, in a line the
// user can run. Point both back at `ccdad setup-path` when that command lands.
func TestInstallShNeverEditsAShellProfile(t *testing.T) {
	newFakeRelease(t)
	home := t.TempDir()
	profiles := []string{".bashrc", ".bash_profile", ".zshrc", ".profile", ".config/fish/config.fish"}
	for _, name := range profiles {
		p := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte("# untouched\n"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}

	dir := t.TempDir() // deliberately outside HOME
	shim := unameShim(t, "Linux", "x86_64")
	out, err := runInstallSh(t, dir,
		"PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+home,
	)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	for _, name := range profiles {
		body, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(body) != "# untouched\n" {
			t.Errorf("install.sh edited %s:\n%s", name, body)
		}
	}
	if !strings.Contains(out, "export PATH=") || !strings.Contains(out, dir) {
		t.Errorf("install.sh said:\n%s\nwant an actionable PATH line naming %s, for an install dir off PATH", out, dir)
	}
	// The advice must not name a subcommand the binary does not have. This is
	// the assertion the previous one was the inverse of, and it is the one that
	// caught the defect.
	if strings.Contains(out, "setup-path") {
		t.Errorf("install.sh said:\n%s\nit points at `ccdad setup-path`, which is not a command in this tree", out)
	}
	if !strings.Contains(out, "ccdad uninstall") {
		t.Errorf("install.sh said:\n%s\nwant it to point uninstall at `ccdad uninstall`, "+
			"not at deleting the binary — there is a daemon and a token directory", out)
	}
}

func TestInstallShPinsTheVersionItIsGiven(t *testing.T) {
	for _, given := range []string{"v1.2.3", "1.2.3"} {
		t.Run(given, func(t *testing.T) {
			release := newFakeRelease(t)
			dir := t.TempDir()
			shim := unameShim(t, "Linux", "x86_64")
			out, err := runInstallSh(t, dir,
				"PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"),
				"CCDAD_VERSION="+given,
			)
			if err != nil {
				t.Fatalf("install.sh: %v\n%s", err, out)
			}
			want := "/download/v1.2.3/ccdad-linux-amd64"
			if !slices.Contains(release.asked(), want) {
				t.Errorf("CCDAD_VERSION=%s asked for %v, want %s", given, release.asked(), want)
			}
		})
	}
}

// The scratch directory is created inside the install directory, not in /tmp,
// so the final move is a same-filesystem rename: /tmp is a different
// filesystem on most distributions, and a cross-device mv degrades to a copy,
// which is ETXTBSY over a running binary. That is not directly observable on
// one filesystem, but `mktemp -d` with a template ignores TMPDIR while a bare
// `mktemp -d` does not, so pointing TMPDIR at nothing tells the two apart.
func TestInstallShDoesNotStageThroughTmp(t *testing.T) {
	newFakeRelease(t)
	dir := t.TempDir()
	shim := unameShim(t, "Linux", "x86_64")
	out, err := runInstallSh(t, dir,
		"PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+filepath.Join(t.TempDir(), "does-not-exist"),
	)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s\nstaging through TMPDIR would also mean a cross-device move at the end", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ccdad")); err != nil {
		t.Fatalf("nothing installed: %v", err)
	}
}
