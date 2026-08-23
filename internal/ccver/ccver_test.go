package ccver

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
	root := t.TempDir()
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
	root := t.TempDir()
	binary := filepath.Join(root, "share", "claude", "versions", "2.1.150")
	write(t, binary, "ELF")
	launcher := filepath.Join(root, "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join("..", "share", "claude", "versions", "2.1.150"), launcher)

	// Somewhere that is NOT the link's directory, so a cwd-relative resolution
	// resolves to a path that does not exist and cannot accidentally agree.
	t.Chdir(t.TempDir())

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
	prefix := t.TempDir()
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
	prefix := t.TempDir()
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
	prefix := t.TempDir()
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
	local := t.TempDir()
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
	dir := t.TempDir()
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
	if got.KeychainEra() {
		t.Error("KeychainEra() = true for an unknown version — every caller acts on this, " +
			"and an install ccdad could not classify must not fire a refusal")
	}
}

// A version directory whose name is not a version is a layout ccdad recognised
// and could not read. It must not be silently treated as unknown-without-reason,
// because the remedy for it ("your launcher points somewhere odd") is different
// from the remedy for "no claude here".
func TestANonVersionDirectoryNameIsReportedRatherThanGuessed(t *testing.T) {
	root := t.TempDir()
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
	root := t.TempDir()
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

// The walk is bounded so that a launcher in an unrelated place stops rather than
// reading a package.json out of every ancestor up to the filesystem root.
func TestThePackageWalkIsBounded(t *testing.T) {
	root := t.TempDir()
	deep := root
	for i := 0; i < maxPackageWalk+2; i++ {
		deep = filepath.Join(deep, "d")
	}
	launcher := filepath.Join(deep, "claude")
	write(t, launcher, "#!/bin/sh\n")
	// The only manifest is further up than the bound allows.
	write(t, filepath.Join(root, "node_modules", "@anthropic-ai", "claude-code", "package.json"),
		manifest(PackageName, "2.1.180"))

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
func TestTheKeychainEraEndsAt2_1_112(t *testing.T) {
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
		if got := in.KeychainEra(); got != tc.era {
			t.Errorf("Install{%s}.KeychainEra() = %v, want %v", tc.version, got, tc.era)
		}
	}
	if LastKeychainEra.NextPatch() != (Version{2, 1, 113}) {
		t.Errorf("LastKeychainEra.NextPatch() = %s, want 2.1.113 — every 'upgrade to' message derives from it",
			LastKeychainEra.NextPatch())
	}
}

// Probe answers from PATH when PATH has one, so ccdad names the claude a shell
// would actually start rather than a copy it found by guessing.
func TestProbePrefersWhatPATHNames(t *testing.T) {
	root := t.TempDir()
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
	home := t.TempDir()
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
	home := t.TempDir()
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	local := filepath.Join(home, ".claude", "local")
	write(t, filepath.Join(local, "claude"), "#!/bin/sh\n")
	write(t, filepath.Join(local, "node_modules", "@anthropic-ai", "claude-code", "package.json"),
		manifest(PackageName, "2.1.90"))

	// A scoped session: CLAUDE_CONFIG_DIR points at an empty directory with no
	// install under it at all.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

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
	home := t.TempDir()
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
	home := t.TempDir()
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
	prefix := t.TempDir()
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
	prefix := t.TempDir()
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
	root := t.TempDir()
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
	if got.KeychainEra() {
		t.Error("a current machine was classified as keychain-era")
	}
}

// The fallback the ordering above must not break: a versions entry that points
// OUT of the versions directory. Full resolution walks past the segment and
// loses the version; the launcher's own link still names what the installer
// wrote, and that is the best answer available.
func TestAVersionsEntryPointingOutsideStillNamesTheInstallersVersion(t *testing.T) {
	root := t.TempDir()
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
	t.Chdir(t.TempDir())

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
	root := t.TempDir()
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
