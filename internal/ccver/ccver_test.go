package ccver

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tempDir is tempDir(t) with the symlinks and short names taken out of it, and
// EVERY fixture in this package goes through it.
//
// WHY IT IS A HELPER RATHER THAN A NORMALISATION AT EACH COMPARISON. Describe
// reports Install.Target from filepath.EvalSymlinks, so a test that builds its
// expectation from a RAW tempDir(t) is comparing two spellings of one file, and
// the temp root is spelled differently on two of the three operating systems
// this suite runs on:
//
//	macOS    /var/folders/... against the resolved /private/var/folders/...
//	Windows  C:\Users\RUNNER~1\... against the expanded C:\Users\runneradmin\...
//
// Linux has neither, which is how four such assertions landed on main looking
// green and turned the macos-latest and windows-latest legs red. Normalising at
// each comparison would have fixed those four and left the fifth one to be
// written wrong; resolving the ROOT once means no assertion in this file can be
// asymmetric, whether or not it touches Target.
//
// Go's EvalSymlinks answers both spellings: the macOS one by following the
// symlink, and the Windows one because its evalSymlinks runs every component
// through FindFirstFile, which returns the long true-cased name.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// symlink makes a link, or says why the platform would not let it.
//
// Windows allows unprivileged symlinks only in Developer Mode, and a hard
// failure there would make this suite red for a reason that is not about ccdad.
// The skip is narrowed to the one platform and reported with the real error, so
// it cannot quietly stand in for coverage anywhere else.
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("this host will not create a symlink (Developer Mode off?): %v", err)
		}
		t.Fatal(err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func manifest(name, version string) string {
	return `{"name":` + `"` + name + `"` + `,"version":"` + version + `","bin":{"claude":"cli.js"}}`
}

// The native install this machine actually has: ~/.local/bin/claude is a symlink
// straight to <data home>/claude/versions/<VERSION>, and the version is the
// name of the thing it points at.
func TestNativeLauncherNamesItsVersion(t *testing.T) {
	root := tempDir(t)
	binary := filepath.Join(root, ".local", "share", "claude", "versions", "2.1.241")
	write(t, binary, "ELF")
	launcher := filepath.Join(root, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, binary, launcher)

	got := Describe(launcher)
	if !got.Known {
		t.Fatalf("Known = false, want the version: %s", got.Why)
	}
	if got.Version != (Version{2, 1, 241}) {
		t.Errorf("Version = %s, want 2.1.241", got.Version)
	}
	if got.Method != MethodNative {
		t.Errorf("Method = %q, want %q", got.Method, MethodNative)
	}
	if got.Target != binary {
		t.Errorf("Target = %q, want %q", got.Target, binary)
	}
}

// The installer writes the link relative on some machines, and Claude Code's own
// dan() resolves it against the LINK's directory rather than the process's
// working directory. A resolution that used the cwd would answer differently
// depending on where ccdad was started, which is the class of bug ccpath's
// header is about.
func TestNativeLauncherResolvesARelativeLinkAgainstItsOwnDirectory(t *testing.T) {
	root := tempDir(t)
	binary := filepath.Join(root, "share", "claude", "versions", "2.1.150")
	write(t, binary, "ELF")
	launcher := filepath.Join(root, "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join("..", "share", "claude", "versions", "2.1.150"), launcher)

	// Somewhere that is NOT the link's directory, so a cwd-relative resolution
	// resolves to a path that does not exist and cannot accidentally agree.
	t.Chdir(tempDir(t))

	got := Describe(launcher)
	if !got.Known || got.Version != (Version{2, 1, 150}) {
		t.Fatalf("Describe = %+v, want 2.1.150 — a relative link must resolve against %s", got, filepath.Dir(launcher))
	}
	// The version alone does NOT prove the resolution happened: the segment scan
	// finds "claude/versions/2.1.150" in the unresolved "../share/claude/..."
	// just as well, so an implementation that never resolved would pass the
	// assertion above. Target is the only output that differs, and doctor prints
	// it -- a report naming "../share/claude/versions/2.1.150" is relative to a
	// directory the reader has no way to know.
	if got.Target != binary {
		t.Errorf("Target = %q, want the resolved %q — the link was not resolved against its own directory",
			got.Target, binary)
	}
}

// An npm global install through 2.1.112: the bin entry is a symlink into
// lib/node_modules, and the package root is an ancestor of what it points at.
func TestNPMGlobalLauncherReadsTheePackageManifest(t *testing.T) {
	prefix := tempDir(t)
	pkg := filepath.Join(prefix, "lib", "node_modules", "@anthropic-ai", "claude-code")
	write(t, filepath.Join(pkg, "cli.js"), "#!/usr/bin/env node\n")
	write(t, filepath.Join(pkg, "package.json"), manifest(PackageName, "2.1.112"))
	launcher := filepath.Join(prefix, "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join(pkg, "cli.js"), launcher)

	got := Describe(launcher)
	if !got.Known || got.Version != (Version{2, 1, 112}) {
		t.Fatalf("Describe = %+v, want 2.1.112: %s", got, got.Why)
	}
	if got.Method != MethodNPM {
		t.Errorf("Method = %q, want %q", got.Method, MethodNPM)
	}
}

// From 2.1.113 the npm package stopped shipping cli.js and started shipping
// bin/claude.exe, which puts the launcher one level DEEPER inside the package
// than the era this walk was first written against. A walk that only looked at
// the launcher's own directory would answer "unknown" for every current npm
// install, which is most of them.
func TestNPMLauncherIsFoundFromInsideTheBinDirectory(t *testing.T) {
	prefix := tempDir(t)
	pkg := filepath.Join(prefix, "lib", "node_modules", "@anthropic-ai", "claude-code")
	write(t, filepath.Join(pkg, "bin", "claude.exe"), "MZ")
	write(t, filepath.Join(pkg, "package.json"), manifest(PackageName, "2.1.241"))

	got := Describe(filepath.Join(pkg, "bin", "claude.exe"))
	if !got.Known || got.Version != (Version{2, 1, 241}) {
		t.Fatalf("Describe = %+v, want 2.1.241: %s", got, got.Why)
	}
}

// A Windows npm shim is NOT a symlink -- npm writes a .cmd file beside the
// package tree -- so the version has to be reachable sideways rather than by
// resolving the launcher. Written as a path test rather than a Windows test on
// purpose: the layout is the same shape wherever npm puts a shim, and gating it
// on GOOS would leave it unexercised on the machine that runs this suite.
func TestAShimBesideNodeModulesFindsThePackage(t *testing.T) {
	prefix := tempDir(t)
	write(t, filepath.Join(prefix, "claude.cmd"), "@ECHO off\n")
	write(t, filepath.Join(prefix, "node_modules", "@anthropic-ai", "claude-code", "package.json"),
		manifest(PackageName, "2.1.200"))

	got := Describe(filepath.Join(prefix, "claude.cmd"))
	if !got.Known || got.Version != (Version{2, 1, 200}) {
		t.Fatalf("Describe = %+v, want 2.1.200: %s", got, got.Why)
	}
}

// The trap this walk exists to survive, and it is not hypothetical: Claude
// Code's own local installer WRITES ~/.claude/local/package.json as
// {"name":"claude-local","version":"0.0.1"}. That file is nearer the launcher
// than the real one, it parses, and its version field is a version number -- so
// a walk that took the first manifest it found would report 0.0.1 on every
// local install, which is on the keychain side of the boundary and would make
// `ccdad run` refuse to start on a perfectly current machine.
func TestTheClaudeLocalWrapperManifestIsNotMistakenForClaudeCode(t *testing.T) {
	local := tempDir(t)
	launcher := filepath.Join(local, "claude")
	write(t, launcher, "#!/bin/sh\nexec \""+local+"/node_modules/.bin/claude\" \"$@\"\n")
	write(t, filepath.Join(local, "package.json"), `{"name":"claude-local","version":"0.0.1","private":true}`)
	write(t, filepath.Join(local, "node_modules", "@anthropic-ai", "claude-code", "package.json"),
		manifest(PackageName, "2.1.180"))

	got := Describe(launcher)
	if got.Version == (Version{0, 0, 1}) {
		t.Fatalf("Describe read claude-local's own manifest and reported %s", got.Version)
	}
	if !got.Known || got.Version != (Version{2, 1, 180}) {
		t.Fatalf("Describe = %+v, want 2.1.180: %s", got, got.Why)
	}
}

// "ccdad could not tell" is a real answer with its own consequences -- doctor
// warns instead of failing, and run starts instead of refusing -- so it has to
// be reachable and has to say why.
func TestAnUnclassifiableLauncherIsUnknownAndSaysWhy(t *testing.T) {
	dir := tempDir(t)
	launcher := filepath.Join(dir, "claude")
	write(t, launcher, "#!/bin/sh\nexec /somewhere/else\n")

	got := Describe(launcher)
	if got.Known {
		t.Fatalf("Known = true (%s), want unknown for a launcher with no layout around it", got.Version)
	}
	if got.Method != MethodUnknown {
		t.Errorf("Method = %q, want %q", got.Method, MethodUnknown)
	}
	if !strings.Contains(got.Why, launcher) {
		t.Errorf("Why does not name the launcher it could not classify:\n%s", got.Why)
	}
	if got.PreSecureStorageDir() {
		t.Error("PreSecureStorageDir() = true for an unknown version — every caller acts on this, " +
			"and an install ccdad could not classify must not fire a refusal")
	}
}

// A version directory whose name is not a version is a layout ccdad recognised
// and could not read. It must not be silently treated as unknown-without-reason,
// because the remedy for it ("your launcher points somewhere odd") is different
// from the remedy for "no claude here".
func TestANonVersionDirectoryNameIsReportedRatherThanGuessed(t *testing.T) {
	root := tempDir(t)
	binary := filepath.Join(root, "share", "claude", "versions", "nightly")
	write(t, binary, "ELF")
	launcher := filepath.Join(root, "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, binary, launcher)

	got := Describe(launcher)
	if got.Known {
		t.Fatalf("Known = true (%s), want unknown for a directory named %q", got.Version, "nightly")
	}
	if got.Method != MethodNative {
		t.Errorf("Method = %q, want %q — the LAYOUT was recognised even though the name was not",
			got.Method, MethodNative)
	}
	if !strings.Contains(got.Why, "nightly") {
		t.Errorf("Why does not name what it found:\n%s", got.Why)
	}
}

// Two claude/versions runs in one path: one is context and one is the answer,
// and the answer is the one nearest the file. It is reachable in the ordinary
// way -- XDG_DATA_HOME pointed inside a directory that happens to be called
// claude/versions -- and a first-match scan would return that outer directory's
// next component as the version.
func TestTheVersionNearestTheFileWins(t *testing.T) {
	path := filepath.Join("home", "u", "claude", "versions", "data", "claude", "versions", "2.1.241")
	got, ok := versionSegment(path)
	if !ok {
		t.Fatalf("versionSegment(%q) found nothing", path)
	}
	if got != "2.1.241" {
		t.Errorf("versionSegment(%q) = %q, want 2.1.241 — the outer run is context, not the answer", path, got)
	}
}

// `.../claude/versions` with nothing after it is the directory itself, not a
// version inside it. Without the bound the segment scan reads one element past
// the end of the path.
func TestTheVersionsDirectoryItselfCarriesNoVersion(t *testing.T) {
	for _, path := range []string{
		filepath.Join("home", "u", ".local", "share", "claude", "versions"),
		filepath.Join("home", "u", "claude"),
		filepath.Join("home", "u", "versions", "2.1.241"),
	} {
		if got, ok := versionSegment(path); ok {
			t.Errorf("versionSegment(%q) = %q, true; want no version", path, got)
		}
	}
}

// doctor's first rule is that a probe must not create what it probes, and this
// package is called from inside doctor. The fixture puts the DIRECTORIES there
// and the files not: with the parent absent an O_CREATE open fails ENOENT on its
// own, so a test run against a missing tree would pass even if every read here
// were a create.
func TestDescribeCreatesNothing(t *testing.T) {
	root := tempDir(t)
	launcher := filepath.Join(root, "bin", "claude")
	write(t, launcher, "#!/bin/sh\n")
	for _, dir := range []string{
		filepath.Join(root, "bin", "node_modules", "@anthropic-ai", "claude-code"),
		filepath.Join(root, "node_modules", "@anthropic-ai", "claude-code"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	Describe(launcher)

	for _, path := range []string{
		filepath.Join(root, "package.json"),
		filepath.Join(root, "bin", "package.json"),
		filepath.Join(root, "bin", "node_modules", "@anthropic-ai", "claude-code", "package.json"),
		filepath.Join(root, "node_modules", "@anthropic-ai", "claude-code", "package.json"),
	} {
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("Describe created %s", path)
		}
	}
}

// The CONTAINING walk is bounded, so a launcher deep inside an unrelated tree
// stops rather than reading a package.json out of every ancestor up to the root.
//
// The manifest here is the "inside" shape -- an ancestor that IS the package
// root -- because that is the only candidate that climbs at all now. Written
// with the sideways shape, this test would pass without exercising any bound.
func TestTheContainingWalkIsBounded(t *testing.T) {
	root := tempDir(t)
	write(t, filepath.Join(root, "package.json"), manifest(PackageName, "2.1.180"))
	deep := root
	for i := 0; i < maxPackageWalk+2; i++ {
		deep = filepath.Join(deep, "d")
	}
	launcher := filepath.Join(deep, "claude")
	write(t, launcher, "#!/bin/sh\n")

	if got := Describe(launcher); got.Known {
		t.Fatalf("Describe walked past the %d-level bound and found %s", maxPackageWalk, got.Version)
	}
}

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Version
		ok   bool
	}{
		{"2.1.241", Version{2, 1, 241}, true},
		{"0.2.9", Version{0, 2, 9}, true},
		{" 2.1.112\n", Version{2, 1, 112}, true},
		// A prerelease is the release it is a prerelease OF. Semver ordering
		// would put 2.1.113-rc.1 BELOW 2.1.113 and therefore inside the
		// keychain era, which is the wrong answer about the release that
		// removed the keychain.
		{"2.1.113-rc.1", Version{2, 1, 113}, true},
		{"2.1.113+build.7", Version{2, 1, 113}, true},
		{"", Version{}, false},
		{"2.1", Version{}, false},
		{"2.1.241.3", Version{}, false},
		{"2.1.x", Version{}, false},
		// strconv.Atoi accepts a sign, so these two are the reason the digit
		// check exists rather than a bare Atoi.
		{"2.1.-1", Version{}, false},
		{"+2.1.3", Version{}, false},
		{"v2.1.3", Version{}, false},
		{"2..3", Version{}, false},
	} {
		got, ok := ParseVersion(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// The comparison is numeric per component, and the case that proves it is the
// one a string comparison gets backwards. It is not a contrived case: the
// boundary this package tests sits at a three-digit patch number, and every
// release before 2.1.100 is on the other side of that trap.
func TestCompareIsNumericRatherThanLexical(t *testing.T) {
	older, newer := Version{2, 1, 99}, Version{2, 1, 113}
	if "2.1.99" < "2.1.113" {
		t.Fatal("this test's premise is gone: string comparison now agrees, so it proves nothing")
	}
	if older.Compare(newer) != -1 {
		t.Errorf("Compare(2.1.99, 2.1.113) = %d, want -1", older.Compare(newer))
	}
	if newer.Compare(older) != 1 {
		t.Errorf("Compare(2.1.113, 2.1.99) = %d, want 1", newer.Compare(older))
	}
	if newer.Compare(newer) != 0 {
		t.Errorf("Compare(v, v) = %d, want 0", newer.Compare(newer))
	}
	// Each component in turn, so a comparison that only ever looked at the
	// patch would fail here rather than pass by fixture coincidence.
	for _, tc := range []struct{ a, b Version }{
		{Version{1, 9, 9}, Version{2, 0, 0}},
		{Version{2, 0, 9}, Version{2, 1, 0}},
	} {
		if tc.a.Compare(tc.b) != -1 || tc.b.Compare(tc.a) != 1 {
			t.Errorf("Compare(%s, %s) is not ordered", tc.a, tc.b)
		}
	}
}

// The boundary itself, on both sides and exactly on it. 2.1.112 is IN the era
// and 2.1.113 is not; getting that inclusive edge backwards inverts doctor's
// remedy and run's refusal at once.
func TestThePreSecureStorageDirBoundaryEndsAt2_1_112(t *testing.T) {
	for _, tc := range []struct {
		version Version
		era     bool
	}{
		{Version{1, 0, 36}, true},
		{Version{2, 1, 111}, true},
		{Version{2, 1, 112}, true},
		{Version{2, 1, 113}, false},
		{Version{2, 1, 241}, false},
		{Version{3, 0, 0}, false},
	} {
		in := Install{Version: tc.version, Known: true}
		if got := in.PreSecureStorageDir(); got != tc.era {
			t.Errorf("Install{%s}.PreSecureStorageDir() = %v, want %v", tc.version, got, tc.era)
		}
	}
	if LastPreSecureStorageDir.NextPatch() != (Version{2, 1, 113}) {
		t.Errorf("LastPreSecureStorageDir.NextPatch() = %s, want 2.1.113 — every 'upgrade to' message derives from it",
			LastPreSecureStorageDir.NextPatch())
	}
}

// Probe answers from PATH when PATH has one, so ccdad names the claude a shell
// would actually start rather than a copy it found by guessing.
func TestProbePrefersWhatPATHNames(t *testing.T) {
	// The home is sandboxed even though this test is about PATH, and that is
	// the point: without it fallbackLaunchers resolves the DEVELOPER's real
	// ~/.local/bin/claude, so reordering Probe to try the fallbacks first still
	// passed on any machine with no native install -- every CI box. The same
	// hazard the cli suite's isolate() was extended for, one package over.
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	root := tempDir(t)
	binary := filepath.Join(root, "share", "claude", "versions", "2.1.199")
	write(t, binary, "ELF")
	onPath := filepath.Join(root, "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(onPath), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, binary, onPath)

	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(string) (string, error) { return onPath, nil }

	got, err := Probe()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != (Version{2, 1, 199}) {
		t.Errorf("Probe() = %s, want 2.1.199", got.Version)
	}
}

// A native install whose directory was never added to PATH is still an install,
// and it is the state ccdad already reports on for its own binary. The fallback
// is what stops doctor from telling that user "no Claude Code here".
func TestProbeFallsBackToTheNativeLauncherLocation(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	binary := filepath.Join(home, ".local", "share", "claude", "versions", "2.1.170")
	write(t, binary, "ELF")
	launcher := filepath.Join(home, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, binary, launcher)

	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }

	got, err := Probe()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != (Version{2, 1, 170}) {
		t.Errorf("Probe() = %s, want 2.1.170 from %s", got.Version, launcher)
	}
	if got.Launcher != launcher {
		t.Errorf("Launcher = %q, want %q", got.Launcher, launcher)
	}
}

// The local installer's launcher, which is neither on PATH by default nor under
// .local/bin.
func TestProbeFallsBackToTheLocalInstallLauncher(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	local := filepath.Join(home, ".claude", "local")
	write(t, filepath.Join(local, "claude"), "#!/bin/sh\n")
	write(t, filepath.Join(local, "node_modules", "@anthropic-ai", "claude-code", "package.json"),
		manifest(PackageName, "2.1.90"))

	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }

	got, err := Probe()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != (Version{2, 1, 90}) {
		t.Errorf("Probe() = %s, want 2.1.90", got.Version)
	}
}

// The ~/.claude/local fallback is derived from HOME and NOT from
// CLAUDE_CONFIG_DIR, and that is the whole point of the line. Inside a `ccdad
// run` session CLAUDE_CONFIG_DIR names a directory ccdad created seconds
// earlier, so a config-dir-relative lookup would search a profile for an npm
// install -- the same class of wrong answer as the securestorage derivation that
// made doctor report "no legacy item" from inside a scoped session.
func TestTheLocalFallbackIgnoresCLAUDE_CONFIG_DIR(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	local := filepath.Join(home, ".claude", "local")
	write(t, filepath.Join(local, "claude"), "#!/bin/sh\n")
	write(t, filepath.Join(local, "node_modules", "@anthropic-ai", "claude-code", "package.json"),
		manifest(PackageName, "2.1.90"))

	// A scoped session: CLAUDE_CONFIG_DIR points at an empty directory with no
	// install under it at all.
	t.Setenv("CLAUDE_CONFIG_DIR", tempDir(t))

	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }

	got, err := Probe()
	if err != nil {
		t.Fatalf("Probe() failed inside a scoped session: %v", err)
	}
	if got.Version != (Version{2, 1, 90}) {
		t.Errorf("Probe() = %s, want 2.1.90 — the fallback must resolve against HOME", got.Version)
	}
}

// No launcher anywhere is its own answer, distinct from "found one and could not
// read it": doctor words the two rows differently and only one of them is about
// a Claude Code that exists.
func TestProbeReportsNoClaudeCodeDistinctly(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }

	got, err := Probe()
	if !errors.Is(err, ErrNoClaudeCode) {
		t.Fatalf("Probe() = %+v, %v; want ErrNoClaudeCode", got, err)
	}
}

// A directory named `claude` on PATH is not a launcher, and Describe would
// happily walk up from it. The fallback loop skips directories for that reason.
func TestProbeSkipsADirectoryNamedLikeALauncher(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin", "claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(home, ".claude", "local")
	write(t, filepath.Join(local, "claude"), "#!/bin/sh\n")
	write(t, filepath.Join(local, "node_modules", "@anthropic-ai", "claude-code", "package.json"),
		manifest(PackageName, "2.1.90"))

	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }

	got, err := Probe()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != (Version{2, 1, 90}) {
		t.Errorf("Probe() = %+v, want the local install — a directory is not a launcher", got)
	}
}

// The string doctor prints. It has to carry the version, the method and where
// the answer came from, because a user who disagrees with the verdict needs to
// see which file ccdad read.
func TestStringNamesTheVersionMethodAndPath(t *testing.T) {
	in := Install{
		Launcher: "/home/u/.local/bin/claude",
		Target:   "/home/u/.local/share/claude/versions/2.1.241",
		Method:   MethodNative,
		Version:  Version{2, 1, 241},
		Known:    true,
	}
	got := in.String()
	for _, want := range []string{"2.1.241", "native", in.Launcher, in.Target} {
		if !strings.Contains(got, want) {
			t.Errorf("String() does not carry %q:\n%s", want, got)
		}
	}

	unknown := Install{Launcher: "/usr/bin/claude", Method: MethodUnknown}
	if got := unknown.String(); strings.Contains(got, "0.0.0") {
		t.Errorf("String() rendered an unknown version as a version number:\n%s", got)
	}
}

// A manifest bigger than the cap is refused rather than read into memory, and
// refusing means "no version here" rather than a partial parse.
func TestAnOversizedManifestIsRefused(t *testing.T) {
	prefix := tempDir(t)
	pkg := filepath.Join(prefix, "node_modules", "@anthropic-ai", "claude-code")
	body := `{"name":"` + PackageName + `","version":"2.1.180","pad":"` +
		strings.Repeat("x", maxPackageJSON) + `"}`
	write(t, filepath.Join(pkg, "package.json"), body)
	launcher := filepath.Join(prefix, "claude")
	write(t, launcher, "#!/bin/sh\n")

	if got := Describe(launcher); got.Known {
		t.Fatalf("Describe read a %d-byte manifest and reported %s", len(body), got.Version)
	}
}

// A manifest that names Claude Code and carries no readable version is a found
// install whose version could not be read, and it must not be reported as an
// absence: "your launcher is not an npm install" sends a user looking in the
// wrong place when the package is right there.
func TestAClaudeCodeManifestWithNoVersionIsFoundButUnreadable(t *testing.T) {
	prefix := tempDir(t)
	write(t, filepath.Join(prefix, "node_modules", "@anthropic-ai", "claude-code", "package.json"),
		`{"name":"`+PackageName+`","description":"no version field"}`)
	launcher := filepath.Join(prefix, "claude")
	write(t, launcher, "#!/bin/sh\n")

	got := Describe(launcher)
	if got.Known {
		t.Fatalf("Known = true (%s) for a manifest with no version", got.Version)
	}
	if got.Method != MethodNPM {
		t.Errorf("Method = %q, want %q — the install WAS found, only its version was not",
			got.Method, MethodNPM)
	}
	if !strings.Contains(got.Why, "package.json") {
		t.Errorf("Why does not name the manifest it read:\n%s", got.Why)
	}
}

// The version has to name what RUNS, and one readlink level does not. A
// versions/2.1.100 entry that is itself a link to versions/2.1.241 executes
// 2.1.241 -- so answering 2.1.100 names a release the machine no longer has,
// and on that number `ccdad run` would refuse to start and doctor would fail.
// A wrong version is strictly worse than an unknown one, which is why the fully
// resolved chain is asked before the launcher's own link.
func TestAChainedVersionsEntryNamesTheVersionThatActuallyRuns(t *testing.T) {
	root := tempDir(t)
	versions := filepath.Join(root, ".local", "share", "claude", "versions")
	real := filepath.Join(versions, "2.1.241")
	write(t, real, "ELF")
	// The stale entry, kept as a link to the current binary.
	symlink(t, real, filepath.Join(versions, "2.1.100"))
	launcher := filepath.Join(root, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join(versions, "2.1.100"), launcher)

	got := Describe(launcher)
	if got.Version == (Version{2, 1, 100}) {
		t.Fatalf("Describe named the link (2.1.100) rather than the binary it runs (2.1.241)")
	}
	if !got.Known || got.Version != (Version{2, 1, 241}) {
		t.Fatalf("Describe = %+v, want 2.1.241: %s", got, got.Why)
	}
	// And the consequence, stated where it bites: 2.1.100 is inside the
	// keychain era and 2.1.241 is not, so getting this wrong is a refusal on a
	// machine that is fine.
	if got.PreSecureStorageDir() {
		t.Error("a current machine was classified as keychain-era")
	}
}

// The fallback the ordering above must not break: a versions entry that points
// OUT of the versions directory. Full resolution walks past the segment and
// loses the version; the launcher's own link still names what the installer
// wrote, and that is the best answer available.
func TestAVersionsEntryPointingOutsideStillNamesTheInstallersVersion(t *testing.T) {
	root := tempDir(t)
	elsewhere := filepath.Join(root, "opt", "claude-build", "claude")
	write(t, elsewhere, "ELF")
	versions := filepath.Join(root, ".local", "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, elsewhere, filepath.Join(versions, "2.1.190"))
	launcher := filepath.Join(root, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	// RELATIVE, and that is the point: this is the one path where the
	// launcher's own link is what carries the version, so it is the only place
	// nativeVersion's resolve-against-the-link's-directory is load-bearing. An
	// absolute fixture here would leave that resolution unexercised.
	symlink(t, filepath.Join("..", "share", "claude", "versions", "2.1.190"), launcher)
	t.Chdir(tempDir(t))

	got := Describe(launcher)
	if !got.Known || got.Version != (Version{2, 1, 190}) {
		t.Fatalf("Describe = %+v, want 2.1.190 — the one-level fallback is what keeps this answer: %s", got, got.Why)
	}
	if !filepath.IsAbs(got.Target) {
		t.Errorf("Target = %q, want an absolute path — doctor prints it, and a path relative to a "+
			"directory the reader cannot see is not an answer", got.Target)
	}
}

// A launcher that IS the binary -- the versions entry itself put on PATH --
// resolves to itself, and the report must not render that as "x -> x". String
// is the single place that decides whether to print the arrow; Install.Target
// always carries what the launcher resolved to.
func TestALauncherThatIsTheBinaryIsNotRenderedAsAnArrowToItself(t *testing.T) {
	root := tempDir(t)
	launcher := filepath.Join(root, ".local", "share", "claude", "versions", "2.1.241")
	write(t, launcher, "ELF")

	got := Describe(launcher)
	if !got.Known || got.Version != (Version{2, 1, 241}) {
		t.Fatalf("Describe = %+v, want 2.1.241: %s", got, got.Why)
	}
	if got.Target != launcher {
		t.Errorf("Target = %q, want %q — Target is what it resolved to, always", got.Target, launcher)
	}
	if strings.Contains(got.String(), "->") {
		t.Errorf("String() renders an arrow from a path to itself:\n%s", got.String())
	}
}

// THE WORST DEFECT THIS BRANCH HAD, and the one the review found: an unrelated
// ancestor's node_modules was credited to the launcher, with Known=true.
//
// The machine here is entirely healthy — the native 2.1.241 is installed and on
// PATH through a wrapper the user wrote — but the project the wrapper lives in
// pins an old CLI in its own node_modules. The sideways probe used to fire from
// every one of eight ancestor levels, so that project's 2.1.100 became "the
// installed Claude Code": inside the keychain era, so `ccdad run` refused to
// start and doctor ruled `fail`, on a machine where everything works. Any
// launcher that is neither a native symlink nor inside the package reaches
// $HOME within eight levels, so $HOME/node_modules was enough to do it.
func TestAnUnrelatedAncestorsPackageIsNotCreditedToTheLauncher(t *testing.T) {
	root := tempDir(t)
	project := filepath.Join(root, "projects", "app")
	// The project's own pinned copy, several levels above the launcher.
	write(t, filepath.Join(project, "node_modules", "@anthropic-ai", "claude-code", "package.json"),
		manifest(PackageName, "2.1.100"))
	launcher := filepath.Join(project, "bin", "claude")
	write(t, launcher, "#!/bin/sh\nexec \"$HOME/.local/bin/claude\" \"$@\"\n")

	got := Describe(launcher)
	if got.Known {
		t.Fatalf("Describe credited an unrelated tree's %s to %s", got.Version, launcher)
	}
	// The consequence is what makes it critical rather than cosmetic.
	if got.PreSecureStorageDir() {
		t.Error("a healthy machine was classified as keychain-era, which refuses `ccdad run` and fails doctor")
	}
}

// The same exposure one level up, which is the likeliest real shape: a shim
// launcher (asdf, volta, mise, a hand-written wrapper) in a directory whose
// PARENT happens to hold a node_modules with the package in it.
func TestASiblingNodeModulesOneLevelUpIsNotCredited(t *testing.T) {
	root := tempDir(t)
	write(t, filepath.Join(root, "node_modules", "@anthropic-ai", "claude-code", "package.json"),
		manifest(PackageName, "2.1.90"))
	launcher := filepath.Join(root, "shims", "claude")
	write(t, launcher, "#!/bin/sh\n")

	if got := Describe(launcher); got.Known {
		t.Fatalf("Describe credited %s from one level above the launcher", got.Version)
	}
}

// A launcher that does not resolve is unknown, not healthy. os.Readlink reads
// the link TEXT without stating what it points at, so a native launcher whose
// versions entry has been deleted — a cleanup, a half-finished update — used to
// report a confident version for a binary that is gone, on a machine where
// claude cannot start at all.
func TestADanglingNativeLauncherIsUnknownRatherThanHealthy(t *testing.T) {
	root := tempDir(t)
	versions := filepath.Join(root, ".local", "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(root, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	// The link is written; the target never exists.
	symlink(t, filepath.Join(versions, "2.1.241"), launcher)

	got := Describe(launcher)
	if got.Known {
		t.Fatalf("Describe reported %s for a launcher whose target is not there", got.Version)
	}
	if !strings.Contains(got.Why, launcher) {
		t.Errorf("Why does not name the launcher that could not be resolved:\n%s", got.Why)
	}
}

// The Windows native install, whose launcher is a COPY rather than a symlink:
// measured in 2.1.241 and unchanged in 2.1.112, the installer branches on
// startsWith("win32") and calls copyFile, falling through to symlink only off
// Windows. Nothing on disk records which versions entry it took — so the answer
// comes from the bytes, and this is the test that the answer is reached at all.
//
// Exercised on this platform rather than gated on GOOS, because the shape is
// what matters: a build tag here would ship the branch unexercised on the
// machine that runs the suite. ccver_windows_test.go covers the half that is
// genuinely about Windows, which is which HOME the launcher hangs off.
func TestANativeLauncherThatIsACopyNamesItsVersionFromTheBytes(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", "")
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	write(t, filepath.Join(versions, "2.1.240"), "ELF-240")
	write(t, filepath.Join(versions, "2.1.241"), "ELF-241")
	launcher := filepath.Join(home, ".local", "bin", "claude.exe")
	write(t, launcher, "ELF-241")

	got := Describe(launcher)
	if !got.Known {
		t.Fatalf("Describe could not name a copy whose bytes are one of the installed binaries: %s", got.Why)
	}
	if got.Version != (Version{2, 1, 241}) {
		t.Errorf("Version = %s, want 2.1.241", got.Version)
	}
	if got.Method != MethodNative {
		t.Errorf("Method = %q, want %q — a copy of a versions binary is still a native install",
			got.Method, MethodNative)
	}
	if !got.Copied {
		t.Errorf("Copied = false for a launcher that is a copy, so the report will claim it resolves somewhere")
	}
	if want := filepath.Join(versions, "2.1.241"); got.Target != want {
		t.Errorf("Target = %q, want the binary the bytes came from %q", got.Target, want)
	}
}

// THE CASE THAT RULES OUT EVERY CHEAPER SOURCE, and it is not hypothetical:
// this machine's versions directory holds a 2.1.240 and a 2.1.241 of exactly
// 342,636,848 bytes each with different sha256s.
//
// Claude Code compares SIZES to answer this question — its Windows update skips
// the copy entirely when the sizes match, and its orphan cleanup protects every
// same-size entry — so a size test would call this ambiguous, and "the newest
// entry wins" would call it 2.1.241. Both are wrong, and wrong in the direction
// that matters: the size-match skip is exactly why a Windows launcher can hold
// an OLDER build than the newest thing installed, which is the machine this
// fixture describes.
func TestACopyIsNamedByItsContentAndNotByItsSize(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", "")
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	// Same length, different bytes — the pair on the machine this was measured
	// on, in miniature.
	write(t, filepath.Join(versions, "2.1.240"), "ELF................240")
	write(t, filepath.Join(versions, "2.1.241"), "ELF................241")
	launcher := filepath.Join(home, ".local", "bin", "claude.exe")
	write(t, launcher, "ELF................240")

	got := Describe(launcher)
	if !got.Known {
		t.Fatalf("Describe could not tell two same-size binaries apart: %s", got.Why)
	}
	if got.Version != (Version{2, 1, 240}) {
		t.Fatalf("Version = %s, want 2.1.240 — the launcher holds the OLDER build's bytes, which is what "+
			"Claude Code's own size-match update skip leaves behind on Windows", got.Version)
	}
}

// A copy whose bytes are none of the installed binaries. Reachable in the
// ordinary way — the versions entry it came from was pruned, or the user put
// their own claude.exe there — and it must read as "a native install ccdad
// cannot pin to a version" rather than as a version or as no install at all,
// because those three send a reader to three different places.
func TestACopyMatchingNoInstalledBinaryIsUnknownAndSaysSo(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", "")
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	write(t, filepath.Join(versions, "2.1.241"), "ELF-241")
	launcher := filepath.Join(home, ".local", "bin", "claude.exe")
	write(t, launcher, "SOMETHING ELSE ENTIRELY")

	got := Describe(launcher)
	if got.Known {
		t.Fatalf("Describe named %s for a copy of nothing that is installed", got.Version)
	}
	if got.Method != MethodNative {
		t.Fatalf("Method = %q, want %q", got.Method, MethodNative)
	}
	if !strings.Contains(got.Why, versions) {
		t.Errorf("Why does not point at the directory the copy should have come from:\n%s", got.Why)
	}
	if strings.Contains(got.Why, "nor an npm install") {
		t.Errorf("a native install was described as not being one:\n%s", got.Why)
	}
}

// Two versions entries holding identical bytes: the launcher IS both, so the
// build is known and the release name is not. Guessing between them would put a
// version number on a coin flip, and this package's whole history is that a
// wrong version is worse than an unknown one — it drives `ccdad run` to refuse
// on a working machine.
func TestACopyOfTwoIdenticalBinariesIsUnknownRatherThanGuessed(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", "")
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	write(t, filepath.Join(versions, "2.1.240"), "ELF")
	write(t, filepath.Join(versions, "2.1.241"), "ELF")
	launcher := filepath.Join(home, ".local", "bin", "claude.exe")
	write(t, launcher, "ELF")

	got := Describe(launcher)
	if got.Known {
		t.Fatalf("Describe picked %s out of two byte-identical candidates", got.Version)
	}
	if got.Method != MethodNative {
		t.Fatalf("Method = %q, want %q", got.Method, MethodNative)
	}
	for _, want := range []string{"2.1.240", "2.1.241", versions} {
		if !strings.Contains(got.Why, want) {
			t.Errorf("Why does not name %q, so the reader cannot see what ccdad could not choose between:\n%s",
				want, got.Why)
		}
	}
}

// The interrupted install's leftovers are not releases. The atomic move writes
// <V>.tmp.<pid>.<millis>.<n> beside the versions entries and unlinks it on
// failure, so a crash leaves a REAL binary under a name that is not a version —
// and crediting the launcher to it would report ".tmp" as a release, or, worse,
// silently accept whatever ParseVersion made of it.
func TestAnInterruptedInstallsTempBinaryIsNotCreditedAsAVersion(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", "")
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	write(t, filepath.Join(versions, "2.1.241.tmp.4321.1755900000000.1"), "ELF-241")
	launcher := filepath.Join(home, ".local", "bin", "claude.exe")
	write(t, launcher, "ELF-241")

	got := Describe(launcher)
	if got.Known {
		t.Fatalf("Describe reported %s from a half-finished install's temp file", got.Version)
	}
	// Unknown alone does not pin the name filter -- native() would also refuse
	// to parse ".tmp.4321..." and land on Known=false. What the filter decides
	// is WHICH unknown: with it the reader is told the bytes matched nothing
	// installed, and without it they are told a version directory is oddly
	// named, about a launcher that resolves into no directory at all.
	if strings.Contains(got.Why, ".tmp.") {
		t.Errorf("Why credits the launcher to a temp file rather than saying its bytes matched no release:\n%s",
			got.Why)
	}
}

// THE INSTALLER RESERVES A VERSION NAME BEFORE IT HAS ANYTHING TO PUT IN IT:
// writeFile(join(versions, <V>), "", {flag:"wx"}), then it downloads. So a
// validly-named ZERO-BYTE versions entry exists for the whole download window
// and survives an interrupted install — and on Windows the launcher can be empty
// at the same moment, because the update renames the old claude.exe aside first
// and the destination holds nothing until bytes land.
//
// Two empty files hold the same bytes. Without the guard that is a MATCH, and
// the machine gets a confident wrong answer of the worst shape: pick a
// reservation for 2.1.112 and doctor rules `fail` while `ccdad run` REFUSES to
// start, both blaming a keychain-era Claude Code, on a machine whose actual
// fault is that its launcher is empty. Claude Code's own two readers of this
// directory refuse zero bytes for the same reason.
func TestAnEmptyLauncherIsNotCreditedWithAReservedVersion(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", "")
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	write(t, filepath.Join(versions, "2.1.112"), "")
	launcher := filepath.Join(home, ".local", "bin", "claude.exe")
	write(t, launcher, "")

	got := Describe(launcher)
	if got.Known {
		t.Fatalf("Describe named %s for an EMPTY launcher — matched against a reservation the installer "+
			"had not filled in yet", got.Version)
	}
	if got.PreSecureStorageDir() {
		t.Fatal("PreSecureStorageDir() is true for an empty launcher, so `ccdad run` would refuse and blame the " +
			"version on a machine whose launcher simply has no bytes")
	}
	if got.Method != MethodNative {
		t.Errorf("Method = %q, want %q — it is still a native install, just a broken one", got.Method, MethodNative)
	}
	if !strings.Contains(got.Why, "EMPTY") {
		t.Errorf("Why does not say the launcher is empty, so the reader is sent to the version instead of "+
			"to the install:\n%s", got.Why)
	}
}

// A reservation sitting beside the real binary must not cost the real match, and
// that is the half that says ONE zero-byte guard is enough: a launcher with
// bytes can never equal an empty file, so identifyCopy needs no second check in
// the loop.
//
// What this does NOT pin, said out loud because a size check next to a version
// invites the assumption: the size filter is cost, not correctness. Deleting it
// leaves this test passing — sameContents rejects the empty candidate on length
// — and only makes the reads longer.
func TestAReservationIsNotAMatchForALauncherThatHasBytes(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", "")
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	write(t, filepath.Join(versions, "2.1.112"), "")
	write(t, filepath.Join(versions, "2.1.241"), "ELF-241")
	launcher := filepath.Join(home, ".local", "bin", "claude.exe")
	write(t, launcher, "ELF-241")

	got := Describe(launcher)
	if !got.Known {
		t.Fatalf("a reservation beside the real binary made the real one unreadable: %s", got.Why)
	}
	if got.Version != (Version{2, 1, 241}) {
		t.Fatalf("Version = %s, want 2.1.241", got.Version)
	}
}

// THE PROPERTY THE ITEM BEHIND THIS WORK IS ABOUT, on every CI leg rather than
// only the Windows one. The shape is a Windows install and the platform is not:
// what matters is that a launcher named from its BYTES is Known, and that
// PreSecureStorageDir() therefore fires — which is what makes `ccdad run` refuse and
// doctor rule fail. Before the bytes were read this was Known=false on such a
// machine, so the refusal never fired and the session ran as the live login.
//
// ccver_windows_test.go has the same property against a real Windows runner with
// HOME and %USERPROFILE% apart; this one is here so the property is not resting
// on one leg out of three.
func TestACopyOfAPreVariableBinaryIsStillOnTheOldSide(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", "")
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	write(t, filepath.Join(versions, "2.1.112"), "ELF-112")
	launcher := filepath.Join(home, ".local", "bin", "claude.exe")
	write(t, launcher, "ELF-112")

	got := Describe(launcher)
	if !got.PreSecureStorageDir() {
		t.Fatalf("PreSecureStorageDir() = false for %s — the refusal `ccdad run` exists for does not fire: %s",
			got, got.Why)
	}
}

// XDG_DATA_HOME is where the binaries are, and the home-derived .local/share is
// not — fpr() reads the variable first, so a machine that sets it has its
// versions tree there and the launcher's copy came from THAT tree. A lookup that
// went straight to <home>/.local/share would compare the launcher against
// whatever an unused directory still held, which here is a different release
// entirely.
//
// The related property this does NOT reach — that the launcher's home and the
// versions tree's home must be the same one — needs two homes and therefore a
// Windows runner; ccver_windows_test.go holds it.
func TestTheVersionsTreeIsTheOneXDG_DATA_HOMENames(t *testing.T) {
	home := tempDir(t)
	elsewhere := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(elsewhere, ".local", "share"))
	write(t, filepath.Join(elsewhere, ".local", "share", "claude", "versions", "2.1.241"), "ELF-241")
	// The launcher's own home has a versions tree too, holding something else.
	write(t, filepath.Join(home, ".local", "share", "claude", "versions", "2.1.112"), "ELF-112")
	launcher := filepath.Join(home, ".local", "bin", "claude.exe")
	write(t, launcher, "ELF-241")

	got := Describe(launcher)
	if !got.Known || got.Version != (Version{2, 1, 241}) {
		t.Fatalf("Describe() = %s (known=%v), want 2.1.241 — XDG_DATA_HOME is where Claude Code puts the "+
			"binaries, so it is the tree the launcher's copy came from: %s", got.Version, got.Known, got.Why)
	}
}

// identifyCopy has to tell "this is not a match" from "I could not look", and
// the two reach the same caller. Neither shape is reachable through Describe --
// it only calls this after resolving the launcher and finding the versions
// directory -- so they are exercised here rather than left as branches that
// ship unrendered.
func TestIdentifyCopySaysWhenItCouldNotLookRatherThanThatNothingMatched(t *testing.T) {
	dir := tempDir(t)
	versions := filepath.Join(dir, ".local", "share", "claude", "versions")
	write(t, filepath.Join(versions, "2.1.241"), "ELF-241")
	launcher := filepath.Join(dir, ".local", "bin", "claude.exe")
	write(t, launcher, "ELF-241")

	if match, why := identifyCopy(filepath.Join(dir, "absent.exe"), versions); why == "" {
		t.Errorf("identifyCopy matched %q for a launcher that is not there", match)
	} else if !strings.Contains(why, "could not be read") {
		t.Errorf("a launcher that is not there reads as a failed match rather than a failed look:\n%s", why)
	}
	if match, why := identifyCopy(launcher, filepath.Join(dir, "no-such-versions")); why == "" {
		t.Errorf("identifyCopy matched %q against a versions directory that is not there", match)
	} else if !strings.Contains(why, "could not be listed") {
		t.Errorf("an unlistable versions directory reads as a failed match rather than a failed look:\n%s", why)
	}
}

// A copy renders without an arrow, and that is a claim about the machine rather
// than a style preference: "->" says the launcher resolves to the target, so a
// reader would conclude that removing the versions entry breaks claude. On
// Windows, the one platform where launchers are copies, it does not.
func TestStringRendersACopyWithoutAnArrow(t *testing.T) {
	in := Install{
		Launcher: `C:\Users\u\.local\bin\claude.exe`,
		Target:   `C:\Users\u\.local\share\claude\versions\2.1.241`,
		Method:   MethodNative,
		Version:  Version{2, 1, 241},
		Known:    true,
		Copied:   true,
	}
	got := in.String()
	if strings.Contains(got, "->") {
		t.Errorf("String() rendered a copy as a resolution:\n%s", got)
	}
	for _, want := range []string{"2.1.241", "native", in.Launcher, in.Target, "copy"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() does not carry %q:\n%s", want, got)
		}
	}
}

// sameContents is the exactness the whole copy branch rests on, so its edges are
// pinned here rather than only through Describe: a difference in the last byte,
// a file that is a PREFIX of the other, and a length that lands exactly on the
// chunk boundary — which is the one input where io.ReadFull ends with EOF and a
// zero-length read rather than with ErrUnexpectedEOF.
func TestSameContents(t *testing.T) {
	dir := tempDir(t)
	path := func(name, body string) string {
		p := filepath.Join(dir, name)
		write(t, p, body)
		return p
	}
	aligned := strings.Repeat("x", compareChunk)
	for _, c := range []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", path("a", "hello"), path("b", "hello"), true},
		{"last byte differs", path("c", "hellO"), path("d", "hellP"), false},
		{"one is a prefix of the other", path("e", "hell"), path("f", "hello"), false},
		{"both empty", path("g", ""), path("h", ""), true},
		{"exactly one chunk", path("i", aligned), path("j", aligned), true},
		{"one chunk against one chunk plus a byte", path("k", aligned), path("l", aligned+"!"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := sameContents(c.a, c.b)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("sameContents = %v, want %v", got, c.want)
			}
		})
	}

	if _, err := sameContents(filepath.Join(dir, "absent"), path("m", "x")); err == nil {
		t.Error("sameContents reported on a file that is not there without an error — identifyCopy would " +
			"then have to tell 'not a match' from 'could not look'")
	}
}

// The same launcher path with no versions directory beside it is NOT a native
// install, and must not be dressed up as one. Without this the branch above
// would claim every ~/.local/bin/claude on earth.
func TestALauncherInTheNativeDirectoryWithNoVersionsTreeIsStillUnknown(t *testing.T) {
	home := tempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", "")
	launcher := filepath.Join(home, ".local", "bin", "claude")
	write(t, launcher, "#!/bin/sh\n")

	got := Describe(launcher)
	if got.Method != MethodUnknown {
		t.Errorf("Method = %q, want %q — there is no versions tree to be a copy of", got.Method, MethodUnknown)
	}
}

// A launcher that is a symlink to something unrecognised has to say where it
// went, not just where it started: the reader's next step is to look at the
// target, and the target is the half only ccdad can see.
func TestAnUnclassifiableSymlinkNamesWhatItResolvedTo(t *testing.T) {
	root := tempDir(t)
	target := filepath.Join(root, "opt", "something", "claude-ish")
	write(t, target, "#!/bin/sh\n")
	launcher := filepath.Join(root, "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, target, launcher)

	got := Describe(launcher)
	if got.Known {
		t.Fatalf("Describe named %s for an unrecognised target", got.Version)
	}
	if !strings.Contains(got.Why, target) {
		t.Errorf("Why does not name what the launcher resolves to:\n%s", got.Why)
	}
}

// "The fallbacks were searched and there was nothing" and "there was no home
// directory, so neither path could even be spelled" are different answers, and
// doctor words them differently. Collapsing them made doctor assert a negative
// result for a search it never performed — and it is reachable, because
// ccpath.StoreHome returns from CCDAD_HOME without ever consulting the home.
func TestProbeDistinguishesAnUnsearchableHomeFromAnEmptyOne(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	saved := lookPath
	t.Cleanup(func() { lookPath = saved })
	lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }

	_, err := Probe()
	if err == nil {
		t.Fatal("Probe() succeeded with no home directory")
	}
	if errors.Is(err, ErrNoClaudeCode) {
		t.Error("an unsearchable home was reported as 'no claude launcher here', which is a result " +
			"for a search that never happened")
	}
}

// The claude-local trap, at the place where the name check is the ONLY thing
// standing between it and a wrong version.
//
// TestTheClaudeLocalWrapperManifestIsNotMistakenForClaudeCode stopped proving
// this once the sideways probe was restricted to the launcher's own directory:
// with a healthy local install, that probe finds the RIGHT manifest before the
// climb ever reaches the wrapper's. Deleting the name check then changed
// nothing, which is a test that had quietly stopped constraining the guard it
// was written for.
//
// The fixture that still reaches it is the local install whose node_modules is
// gone — pruned, half-uninstalled, interrupted. The climb then arrives at
// ~/.claude/local/package.json, which Claude Code's own local installer writes
// as {"name":"claude-local","version":"0.0.1"}. Without the name check that is
// "Claude Code 0.0.1": inside the keychain era, so doctor rules fail and `ccdad
// run` refuses, over a manifest describing a two-line shell wrapper.
func TestAPrunedLocalInstallDoesNotFallBackToTheWrapperManifest(t *testing.T) {
	local := tempDir(t)
	launcher := filepath.Join(local, "claude")
	write(t, launcher, "#!/bin/sh\nexec \""+local+"/node_modules/.bin/claude\" \"$@\"\n")
	write(t, filepath.Join(local, "package.json"), `{"name":"claude-local","version":"0.0.1","private":true}`)
	// No node_modules: the sideways probe misses and the climb is what runs.

	got := Describe(launcher)
	if got.Version == (Version{0, 0, 1}) {
		t.Fatalf("Describe reported the claude-local wrapper's own version as Claude Code's")
	}
	if got.Known {
		t.Fatalf("Describe = %+v, want unknown — nothing here declares a Claude Code version", got)
	}
	if got.PreSecureStorageDir() {
		t.Error("a wrapper manifest classified the machine as keychain-era")
	}
}
