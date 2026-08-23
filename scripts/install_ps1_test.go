package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The two Windows assets. install.sh covers the other four.
var windowsAssets = []string{
	"ccdad-windows-amd64.exe",
	"ccdad-windows-arm64.exe",
}

// powershell finds an interpreter to run install.ps1 with. The script targets
// Windows PowerShell 5.1, which cannot be run here; pwsh executes the same
// language, and everything genuinely 5.1- or Windows-specific in the script is
// behind Test-CcdadOnWindows, which is exactly why that seam exists.
func powershell(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("neither pwsh nor powershell is installed")
	return ""
}

// dotSource runs a fragment with install.ps1's functions defined but nothing
// installed, and returns its trimmed output.
func dotSource(t *testing.T, fragment string) string {
	t.Helper()
	shell := powershell(t)
	script, err := filepath.Abs(filepath.Join("..", "install.ps1"))
	if err != nil {
		t.Fatalf("resolving install.ps1: %v", err)
	}
	body := ". '" + script + "' -NoRun\n" + fragment
	cmd := exec.Command(shell, "-NoProfile", "-NonInteractive", "-Command", body)
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("powershell: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestInstallPs1Parses(t *testing.T) {
	shell := powershell(t)
	script, err := filepath.Abs(filepath.Join("..", "install.ps1"))
	if err != nil {
		t.Fatalf("resolving install.ps1: %v", err)
	}
	out, err := exec.Command(shell, "-NoProfile", "-NonInteractive", "-Command",
		"$e = $null; [System.Management.Automation.Language.Parser]::ParseFile('"+script+
			"', [ref]$null, [ref]$e) | Out-Null; if ($e.Count) { $e | ForEach-Object { $_.ToString() }; exit 1 }",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("install.ps1 does not parse:\n%s", out)
	}

	// 5.1 decodes a piped script by guessing at its encoding, and a stray
	// non-ASCII byte turns the whole file into mojibake before the first line
	// runs. CRLF is the other half of the same problem, which .gitattributes
	// handles for install.sh and which nothing checks here.
	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("reading install.ps1: %v", err)
	}
	for i, b := range raw {
		if b > 0x7e || (b < 0x20 && b != '\n' && b != '\t') {
			line := 1 + strings.Count(string(raw[:i]), "\n")
			t.Fatalf("install.ps1 byte %d (line %d) is 0x%02x; the file must stay ASCII with LF endings", i, line, b)
		}
	}
}

// PROCESSOR_ARCHITECTURE reports x86 under WOW64 - a 32-bit PowerShell on
// 64-bit Windows, which several launchers still produce - so reading it first
// sends the installer after an asset that does not exist.
func TestInstallPs1ReadsArchitew6432First(t *testing.T) {
	for _, tc := range []struct{ w6432, arch, want string }{
		{"AMD64", "x86", "ccdad-windows-amd64.exe"},
		{"ARM64", "x86", "ccdad-windows-arm64.exe"},
		{"", "AMD64", "ccdad-windows-amd64.exe"},
		{"", "ARM64", "ccdad-windows-arm64.exe"},
		{"", "amd64", "ccdad-windows-amd64.exe"},
		{"", "x86", "ERROR: unsupported architecture: 'x86' (ccdad ships amd64 and arm64)"},
		{"", "IA64", "ERROR: unsupported architecture: 'IA64' (ccdad ships amd64 and arm64)"},
	} {
		name := tc.w6432 + "/" + tc.arch
		t.Run(name, func(t *testing.T) {
			got := dotSource(t, "try { Get-CcdadAssetName -Architew6432 '"+tc.w6432+
				"' -Architecture '"+tc.arch+"' } catch { \"ERROR: $($_.Exception.Message)\" }")
			if got != tc.want {
				t.Errorf("ARCHITEW6432=%q ARCHITECTURE=%q resolved to %q, want %q", tc.w6432, tc.arch, got, tc.want)
			}
			if strings.HasPrefix(tc.want, "ccdad-") && !slices.Contains(windowsAssets, tc.want) {
				t.Errorf("%q is not an asset the release ships", tc.want)
			}
		})
	}
}

func TestInstallPs1BuildsTheDownloadUrl(t *testing.T) {
	const base = "https://github.com/Kweiza/ccdaddy/releases"
	for _, tc := range []struct{ version, want string }{
		{"", base + "/latest/download"},
		{"v1.2.3", base + "/download/v1.2.3"},
		{"1.2.3", base + "/download/v1.2.3"},
		{" 1.2.3 ", base + "/download/v1.2.3"},
	} {
		t.Run("version="+tc.version, func(t *testing.T) {
			got := dotSource(t, "Get-CcdadDownloadBase -BaseUrl '"+base+"' -Version '"+tc.version+"'")
			if got != tc.want {
				t.Errorf("CCDAD_VERSION=%q gave %q, want %q", tc.version, got, tc.want)
			}
		})
	}
}

// The same near-miss table install.sh is held to. The two implementations of
// this regex are the whole verification story on their platforms, and they
// have to reject the same things.
func TestInstallPs1ExtractsOnlyAnExactlyMatchingSumsLine(t *testing.T) {
	const asset = "ccdad-windows-amd64.exe"
	// Letters on purpose: a digits-only digest has no case, so an uppercase
	// line would be indistinguishable from a lowercase one.
	const good = "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234"

	for _, tc := range []struct{ name, line, want string }{
		{"exact", good + "  " + asset, good},
		{"one space", good + " " + asset, "NULL"},
		{"three spaces", good + "   " + asset, "NULL"},
		{"a longer neighbour", good + "  " + asset + ".sig", "NULL"},
		{"a prefixed line", "sha256:" + good + "  " + asset, "NULL"},
		{"uppercase hex", strings.ToUpper(good) + "  " + asset, "NULL"},
		// [regex]::Escape on the asset name: without it the dot in ".exe" is
		// a wildcard and this line satisfies the lookup.
		{"the dot treated as a wildcard", good + "  ccdad-windows-amd64Xexe", "NULL"},
		{"a short hash", good[:63] + "  " + asset, "NULL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dotSource(t, "$r = Get-CcdadExpectedHash -Lines @('"+tc.line+
				"') -Asset '"+asset+"'; if ($null -eq $r) { 'NULL' } else { $r }")
			if got != tc.want {
				t.Errorf("line %q gave %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// The installer's last instruction has to be one the reader can run. Its
// install.sh twin (TestInstallShNeverEditsAShellProfile) pins the same shape:
// a bare `ccdad setup-path` cannot resolve on a machine whose install directory
// is not yet on PATH.
func TestInstallPs1PointsAtSetupPathByPath(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	// Comment lines are skipped: the script explains the command as well as
	// printing it, and only what it PRINTS is the user's instruction.
	pointer := ""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, "setup-path") {
			pointer = line
			break
		}
	}
	if pointer == "" {
		t.Fatal("install.ps1 never points at `ccdad setup-path`, so a user whose PATH write failed " +
			"is told nothing about the command that would fix it")
	}
	if !strings.Contains(pointer, "$target") && !strings.Contains(pointer, "$installDir") {
		t.Errorf("install.ps1 says:\n%s\nthe setup-path pointer does not name the binary by path, and a "+
			"bare `ccdad` cannot resolve on the machine this branch fires for", pointer)
	}
}

func TestInstallPs1TellsAnErrorPageFromASumsFile(t *testing.T) {
	const good = "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234"
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"a checksum file", good + "  ccdad-windows-amd64.exe", "True"},
		{"an HTML error page", "<html><body>403 Forbidden</body></html>", "False"},
		{"one space", good + " ccdad-windows-amd64.exe", "False"},
		{"uppercase hex", strings.ToUpper(good) + "  ccdad-windows-amd64.exe", "False"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dotSource(t, "Test-CcdadSumsShape -Lines @('"+tc.line+"')")
			if got != tc.want {
				t.Errorf("Test-CcdadSumsShape on %q = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// The entry install.ps1's own read makes possible, and used to miss.
//
// Add-CcdadToUserPath reads the value with DoNotExpandEnvironmentNames, which
// is correct - writing an expanded PATH back would freeze every %VAR% in it to
// today's value. But $Directory arrives fully expanded (Join-Path
// $env:LOCALAPPDATA ...), so comparing only the raw text meant a user whose
// Path held `%LOCALAPPDATA%\Programs\ccdad` failed the equality test and got a
// second, expanded copy appended on EVERY install. It is also the copy `ccdad
// uninstall` would then have to find.
//
// This is what caught it, and it runs on Linux: .NET's
// ExpandEnvironmentVariables handles %VAR% syntax on every platform.
func TestInstallPs1MatchesAPathEntryThatIsStillUnexpanded(t *testing.T) {
	const local = `C:\Users\a\AppData\Local`
	const dir = local + `\Programs\ccdad`
	for _, tc := range []struct{ name, current string }{
		{"the only entry", `%LOCALAPPDATA%\Programs\ccdad`},
		{"among others", `C:\Windows;%LOCALAPPDATA%\Programs\ccdad;C:\Windows\System32`},
		{"with a trailing backslash", `%LOCALAPPDATA%\Programs\ccdad\`},
		// The PATH text's case is covered above by "among others" through the
		// OrdinalIgnoreCase compare. The VARIABLE NAME's case (`%localappdata%`)
		// deliberately is not: Windows resolves environment variables
		// case-insensitively and Linux does not, so under pwsh here that row
		// would assert the host's rule rather than the installer's. It is
		// verified on Windows by the install-smoke workflow or not at all.
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dotSource(t, "$env:LOCALAPPDATA = '"+local+"'; "+
				"$r = Get-CcdadUpdatedPath -Current '"+tc.current+"' -Directory '"+dir+
				"'; if ($null -eq $r) { 'NULL' } else { $r }")
			if got != "NULL" {
				t.Errorf("PATH %q already holds %q unexpanded, but install.ps1 appended a second copy: %q",
					tc.current, dir, got)
			}
		})
	}

	// The mirror image, so this does not pass by matching everything: a
	// different directory that merely expands to a near neighbour must still be
	// appended -- and the entry that survives is written back UNEXPANDED, which
	// is the property the raw read exists to protect.
	got := dotSource(t, "$env:LOCALAPPDATA = '"+local+"'; "+
		"$r = Get-CcdadUpdatedPath -Current '%LOCALAPPDATA%\\Programs\\ccdad2' -Directory '"+dir+
		"'; if ($null -eq $r) { 'NULL' } else { $r }")
	if want := `%LOCALAPPDATA%\Programs\ccdad2;` + dir; got != want {
		t.Errorf("a longer sibling was treated as the entry: got %q, want %q", got, want)
	}
}

// install.ps1 must record the entry it added, or `ccdad uninstall` leaves it
// behind: a registry PATH component carries no evidence of who put it there,
// so removal is gated on the record rather than on the directory's contents.
// The two writers -- this script and internal/cli's registerUserPath -- must
// write the same value under the same key, because either may be the one that
// registered the entry uninstall later removes.
func TestInstallPs1RecordsThePathEntryItAdded(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`CreateSubKey('Software\ccdad')`, `SetValue('PathEntry'`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("install.ps1 does not %s; every entry it adds becomes one `ccdad uninstall` "+
				"cannot prove is ccdad's, so it is left on the user's PATH forever", want)
		}
	}
	// The Go side, so the two cannot drift apart without a test saying so.
	goBody, err := os.ReadFile(filepath.Join("..", "internal", "cli", "setuppath_windows.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`Software\ccdad`, `pathEntryValue = "PathEntry"`} {
		if !strings.Contains(string(goBody), want) {
			t.Errorf("setuppath_windows.go does not carry %s, so it no longer agrees with install.ps1", want)
		}
	}
}

// The decision half of the PATH write. The registry half - keeping the value
// kind and reading with DoNotExpandEnvironmentNames - cannot be exercised off
// Windows, which is why the decision was split out of it.
func TestInstallPs1AppendsToUserPathWithoutDuplicating(t *testing.T) {
	const dir = `C:\Users\a\AppData\Local\Programs\ccdad`
	for _, tc := range []struct{ name, current, want string }{
		{"empty", "", dir},
		{"appended", `C:\Windows;C:\Windows\System32`, `C:\Windows;C:\Windows\System32;` + dir},
		{"a trailing separator is not doubled", `C:\Windows;`, `C:\Windows;` + dir},
		{"already present", `C:\Windows;` + dir, "NULL"},
		{"already present in another case", `C:\Windows;` + strings.ToUpper(dir), "NULL"},
		{"already present with a trailing backslash", `C:\Windows;` + dir + `\`, "NULL"},
		// A REG_EXPAND_SZ PATH holds %VAR% references verbatim. They have to
		// survive being read and written back, or every one of them is frozen
		// to today's expansion.
		{"unexpanded references are carried through", `%USERPROFILE%\bin;C:\Windows`, `%USERPROFILE%\bin;C:\Windows;` + dir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dotSource(t, "$r = Get-CcdadUpdatedPath -Current '"+tc.current+"' -Directory '"+dir+
				"'; if ($null -eq $r) { 'NULL' } else { $r }")
			if got != tc.want {
				t.Errorf("PATH %q + %q = %q, want %q", tc.current, dir, got, tc.want)
			}
		})
	}
}

// runInstallPs1 drives the whole installer against a fake release. Everything
// Windows-only is behind Test-CcdadOnWindows, so the download, the three
// aborts and the replace-a-running-exe dance all execute here.
func runInstallPs1(t *testing.T, dir string, env map[string]string) (string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Not a portability problem — the opposite. Everything behind
		// Test-CcdadOnWindows becomes live here, and the last of those is
		// Add-CcdadToUserPath, which appends $installDir to the real
		// HKCU\Environment PATH. On a runner that is merely pointless; on a
		// contributor's own machine, `go test ./...` would leave one t.TempDir()
		// in their PATH per test, permanently, and the broadcast would make it
		// take effect immediately.
		//
		// The pwsh-on-Linux run exercises the download, the verification, the
		// aborts and the replace dance, all of which are platform-independent.
		// The published installer against a real Windows box is the
		// install-smoke workflow's job, where the machine is disposable.
		t.Skip("install.ps1 writes the user's real PATH on Windows; the install-smoke workflow owns that")
	}
	shell := powershell(t)
	script, err := filepath.Abs(filepath.Join("..", "install.ps1"))
	if err != nil {
		t.Fatalf("resolving install.ps1: %v", err)
	}
	// Windows PowerShell 5.1 renders a terminating error as its message
	// followed by a location. PowerShell 7 defaults to ConciseView, which
	// wraps the message across gutter-prefixed lines and colours it, and no
	// substring match survives that. Both preferences are set on the HOST
	// rather than in install.ps1: `irm | iex` runs in the user's own session,
	// where a script that rewrites their error rendering is a script that
	// broke their console.
	cmd := exec.Command(shell, "-NoProfile", "-NonInteractive", "-Command",
		"$ErrorView = 'NormalView'; if ($null -ne $PSStyle) { $PSStyle.OutputRendering = 'PlainText' }; & '"+script+"'")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		"CCDAD_INSTALL_DIR="+dir,
		"CCDAD_VERSION=",
		"PROCESSOR_ARCHITEW6432=",
		"PROCESSOR_ARCHITECTURE=AMD64",
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func newWindowsRelease(t *testing.T) (*fakeRelease, string) {
	t.Helper()
	files := make(map[string][]byte, len(windowsAssets))
	for _, name := range windowsAssets {
		files[name] = fakeBinary("9.9.9-" + name)
	}
	f := &fakeRelease{files: files, sums: sumsFor(files)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func TestInstallPs1InstallsAndVerifies(t *testing.T) {
	release, origin := newWindowsRelease(t)
	dir := t.TempDir()
	out, err := runInstallPs1(t, dir, map[string]string{"CCDAD_BASE_URL": origin})
	if err != nil {
		t.Fatalf("install.ps1: %v\n%s", err, out)
	}
	if !slices.Contains(release.asked(), "/latest/download/ccdad-windows-amd64.exe") {
		t.Errorf("asked for %v, want the amd64 asset", release.asked())
	}
	body, err := os.ReadFile(filepath.Join(dir, "ccdad.exe"))
	if err != nil {
		t.Fatalf("reading the installed binary: %v", err)
	}
	if !strings.Contains(string(body[:200]), "9.9.9-ccdad-windows-amd64.exe") {
		t.Error("some other asset was installed")
	}
	if got := leftovers(t, dir); len(got) > 0 {
		t.Errorf("staging directories left behind: %v", got)
	}
}

func TestInstallPs1AbortsWhenItCannotVerifyTheDownload(t *testing.T) {
	const asset = "ccdad-windows-amd64.exe"
	for _, tc := range []struct {
		name    string
		mutate  func(*fakeRelease)
		wantMsg string
	}{
		{"the checksum file does not arrive", func(f *fakeRelease) { f.sums = nil }, "cannot download"},
		{"the asset is not listed in it", func(f *fakeRelease) {
			f.sums = sumsFor(map[string][]byte{"ccdad-windows-arm64.exe": f.files[asset]})
		}, "is not listed"},
		{"the hash does not match", func(f *fakeRelease) {
			f.sums = sumsFor(map[string][]byte{asset: fakeBinary("a different build")})
		}, "checksum mismatch"},
		{"the checksum file is an error page", func(f *fakeRelease) {
			f.sums = []byte("<html><body>403</body></html>\n")
		}, "is not a checksum file"},
		{"the asset is an error page", func(f *fakeRelease) {
			page := []byte("<html><body>404</body></html>\n")
			f.files[asset] = page
			f.sums = sumsFor(map[string][]byte{asset: page})
		}, "not a ccdad binary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string][]byte{asset: fakeBinary("9.9.9-real")}
			release := &fakeRelease{files: files, sums: sumsFor(files)}
			tc.mutate(release)
			srv := httptest.NewServer(release)
			t.Cleanup(srv.Close)

			dir := t.TempDir()
			out, err := runInstallPs1(t, dir, map[string]string{"CCDAD_BASE_URL": srv.URL})
			if err == nil {
				t.Fatalf("install.ps1 exited 0 having verified nothing:\n%s", out)
			}
			if !strings.Contains(out, tc.wantMsg) {
				t.Errorf("install.ps1 said:\n%s\nwant a message containing %q", out, tc.wantMsg)
			}
			if _, err := os.Stat(filepath.Join(dir, "ccdad.exe")); err == nil {
				t.Error("install.ps1 installed a binary it could not verify")
			}
			if got := leftovers(t, dir); len(got) > 0 {
				t.Errorf("staging directories left behind: %v", got)
			}
		})
	}
}

// A running .exe cannot be overwritten, but it can be renamed, so the upgrade
// is stop-the-daemon, rename aside, move in, best-effort delete. The stop runs
// the OLD binary, which may predate the daemon command group and exit 2.
func TestInstallPs1ReplacesAnExistingInstall(t *testing.T) {
	_, origin := newWindowsRelease(t)
	dir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "stop.log")
	if err := os.WriteFile(filepath.Join(dir, "ccdad.exe"), fakeBinary("0.0.1-old"), 0o755); err != nil {
		t.Fatalf("planting the old binary: %v", err)
	}

	out, err := runInstallPs1(t, dir, map[string]string{
		"CCDAD_BASE_URL": origin,
		"CCDAD_FAKE_LOG": logFile,
	})
	if err != nil {
		t.Fatalf("install.ps1: %v\n%s", err, out)
	}
	stopped, err := os.ReadFile(logFile)
	if err != nil || !strings.Contains(string(stopped), "daemon stop") {
		t.Errorf("the old binary was not asked to stop its daemon (log %q, err %v) - "+
			"the old daemon would keep running old code and holding the singleton lock", stopped, err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "ccdad.exe"))
	if err != nil {
		t.Fatalf("reading the installed binary: %v", err)
	}
	if strings.Contains(string(body[:200]), "0.0.1-old") {
		t.Error("install.ps1 left the old binary in place")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ccdad-old.") {
			t.Errorf("%s was renamed aside but never deleted, and nothing was holding it", e.Name())
		}
	}
}

func TestInstallPs1PinsTheVersionItIsGiven(t *testing.T) {
	release, origin := newWindowsRelease(t)
	dir := t.TempDir()
	out, err := runInstallPs1(t, dir, map[string]string{
		"CCDAD_BASE_URL": origin,
		"CCDAD_VERSION":  "1.2.3",
	})
	if err != nil {
		t.Fatalf("install.ps1: %v\n%s", err, out)
	}
	want := "/download/v1.2.3/ccdad-windows-amd64.exe"
	if !slices.Contains(release.asked(), want) {
		t.Errorf("asked for %v, want %s", release.asked(), want)
	}
}

// Verified by hand in the sums file the two installers share: the hex is
// lowercase, and Get-FileHash answers in uppercase. Comparing them without
// folding the case makes every install fail with "checksum mismatch".
func TestInstallPs1FoldsGetFileHashesUppercase(t *testing.T) {
	body := fakeBinary("9.9.9-case")
	digest := sha256.Sum256(body)
	lower := hex.EncodeToString(digest[:])
	if strings.ToUpper(lower) == lower {
		t.Skip("this digest has no letters in it")
	}
	got := dotSource(t, "$p = Join-Path '"+t.TempDir()+"' 'probe'; "+
		"[System.IO.File]::WriteAllBytes($p, [byte[]]@(1,2,3)); "+
		"(Get-FileHash -Algorithm SHA256 -LiteralPath $p).Hash")
	if got != strings.ToUpper(got) {
		t.Fatalf("Get-FileHash answered %q; this test exists because it answers in uppercase", got)
	}
}

// $ErrorActionPreference = 'Stop'. Most PowerShell cmdlets report failure as a
// NON-terminating error, which without this preference writes a red line to
// the error stream and carries straight on - so an install directory that
// cannot be created would be followed by a download, a verification and a
// "installed ccdad" that installed nothing. Refusing before the 8 MB transfer
// is the visible half of that.
func TestInstallPs1AbortsBeforeDownloadingWhenItCannotCreateTheInstallDir(t *testing.T) {
	release, origin := newWindowsRelease(t)

	parent := filepath.Join(t.TempDir(), "read-only")
	if err := os.Mkdir(parent, 0o555); err != nil {
		t.Fatalf("creating %s: %v", parent, err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o755) })
	if err := os.WriteFile(filepath.Join(parent, "probe"), nil, 0o644); err == nil {
		t.Skip("this user can write to a mode-0555 directory, so there is nothing to refuse")
	}

	dir := filepath.Join(parent, "ccdad")
	out, err := runInstallPs1(t, dir, map[string]string{"CCDAD_BASE_URL": origin})
	if err == nil {
		t.Fatalf("install.ps1 exited 0 with nowhere to install to:\n%s", out)
	}
	if asked := release.asked(); len(asked) > 0 {
		t.Errorf("install.ps1 downloaded %v before finding out it had nowhere to put it", asked)
	}
}
