// Package ccver names the Claude Code that is installed on this machine, and
// does it without running one.
//
// # Why this exists at all
//
// Three places in ccdad need the installed version, and all three need the same
// boundary rather than the number: 2.1.112 is the last release whose credential
// store reads the macOS Keychain, and the last that does not know
// CLAUDE_SECURESTORAGE_CONFIG_DIR. internal/cclink's keychain header carries the
// measurement and the consequences; this package answers "is this machine on
// that side of it".
//
// # Why nothing here spawns claude
//
// `claude --version` is the obvious source and it is the wrong one. doctor's
// rule is that a probe must not create or disturb what it probes, and the
// native launcher resolves — and can update itself — on invocation, so asking
// it its version can CHANGE its version. It is also a ~300 MB process started to
// render one diagnostic row.
//
// It is not needed, because the version is on the filesystem. Measured out of
// the installed 2.1.241 binary, which computes its own install layout as:
//
//	fpr()  = XDG_DATA_HOME ?? join(home, ".local", "share")
//	G8n()  = join(fpr(), "claude", "versions")     // where a version installs
//	q1e()  = join(home, ".local", "bin")           // the launcher directory
//	dan(e) = windows ? true
//	       : lstat(e).isSymbolicLink()
//	         && resolve(dirname(e), readlink(e)).includes(sep+"claude"+sep+"versions"+sep)
//	pan(e) = realpath(e).endsWith(".js") || realpath(e).includes("node_modules")
//
// dan is Claude Code's own "is this launcher mine" test and pan its "is this an
// npm shim"; nativeVersion and npmVersion below are those two predicates, read
// for the version they imply. A native install is a symlink into
// .../claude/versions/<VERSION>, so one readlink names it. An npm install of any
// era resolves into node_modules, so the package's own package.json names it.
//
// One place this does MORE than dan: dan wants a boolean, so one readlink level
// answers it, while a version has to name what actually runs. Describe therefore
// reads the version off the fully resolved chain and keeps the one-level target
// only as a fallback -- a versions/2.1.100 entry that is itself a link to
// versions/2.1.241 runs 2.1.241, and naming the link would be a WRONG version
// rather than an unknown one.
//
// To re-derive the layout against a newer build, take the byte offset first —
// a regex with a multi-thousand-character lookbehind over 229 MB does not
// return:
//
//	tr -d '\000' < ~/.local/share/claude/versions/<V> > cc.js
//	OFF=$(grep -abFo -m1 'function q1e(' cc.js | cut -d: -f1)
//	tail -c +$((OFF+1)) cc.js | head -c 900
//
// # What is deliberately NOT a source
//
// Claude Code reports its own version from a compiled-in VERSION constant,
// inlined at every use site, so there is no on-disk manifest to mirror. The
// three files that look like one are all something else: ~/.claude.json's
// lastOnboardingVersion is the release that last ran onboarding (2.1.123 on the
// machine this was written on, against an installed 2.1.241),
// lastReleaseNotesSeen is what the user has read, and
// ~/.claude/.last-update-result.json's version_to is what the updater last
// wrote. A pinned or hand-placed launcher makes all three disagree with what
// runs. They are corroboration, never the answer.
package ccver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// PackageName is the npm package every era of Claude Code ships under. It is
// checked rather than assumed: the walk up from a launcher passes through
// package.json files belonging to other things, and `claude install --local`
// writes one of them itself — ~/.claude/local/package.json is
// {"name":"claude-local","version":"0.0.1"}, which without this check would be
// reported as Claude Code 0.0.1.
const PackageName = "@anthropic-ai/claude-code"

// LastKeychainEra is the last Claude Code whose credential store reads the
// macOS Keychain in preference to .credentials.json, and the last that does not
// know CLAUDE_SECURESTORAGE_CONFIG_DIR. 2.1.113 removed the keychain backend and
// introduced the variable.
//
// Bisected twice, independently. internal/cclink/keychain.go found the boundary
// in the code, by fetching every release from 2.1.112 to 2.1.129 and searching
// for the backend. The npm registry's own metadata agrees from the outside: the
// package's bin entry is "cli.js" through 2.1.112 and "bin/claude.exe" from
// 2.1.113, where the platform binaries become optionalDependencies and the
// unpacked package drops from ~49 MB to ~132 KB.
var LastKeychainEra = Version{2, 1, 112}

// ErrNoClaudeCode is returned when no launcher could be found at all. It is
// distinct from "found one and could not name its version": a machine with no
// Claude Code is not a machine whose Claude Code is unreadable.
var ErrNoClaudeCode = errors.New("ccdad could not find a claude launcher")

// Method is how Claude Code got onto this machine, in the terms Claude Code's
// own installation classifier uses. Its taxonomy is finer — it separates
// npm-global from npm-local and detects package managers by shelling out to
// them — and this deliberately is not, because nothing ccdad does turns on the
// difference. What turns on it is the version, and both npm shapes carry that
// in the same file.
type Method string

const (
	// MethodNative is the launcher the native installer writes: a symlink into
	// <data home>/claude/versions/<VERSION>.
	MethodNative Method = "native"
	// MethodNPM is any install whose launcher resolves into a node_modules
	// tree holding PackageName — npm global, npm local (~/.claude/local), and
	// the Windows .cmd shim beside such a tree.
	MethodNPM Method = "npm"
	// MethodUnknown is a launcher ccdad found and could not classify.
	MethodUnknown Method = "unknown"
)

// Version is a Claude Code release. Claude Code numbers releases
// MAJOR.MINOR.PATCH with no prerelease component in anything published so far;
// ParseVersion tolerates one rather than answering "unknown" if that ever
// changes.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// Compare orders two versions: -1 if v is older, 0 if equal, 1 if newer.
//
// Component-wise and numeric, which is the whole reason it exists rather than a
// string comparison: "2.1.99" sorts AFTER "2.1.113" as text, and the boundary
// this package exists to test sits at a three-digit patch number.
func (v Version) Compare(o Version) int {
	for _, pair := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		switch {
		case pair[0] < pair[1]:
			return -1
		case pair[0] > pair[1]:
			return 1
		}
	}
	return 0
}

// AtMost reports whether v is o or older.
func (v Version) AtMost(o Version) bool { return v.Compare(o) <= 0 }

// ParseVersion reads MAJOR.MINOR.PATCH.
//
// A `-rc.1`-style prerelease suffix is dropped rather than rejected, and the
// version is then the release it is a prerelease OF. Semver precedence would
// order 2.1.113-rc.1 BELOW 2.1.113, which for this package's one question would
// be the wrong answer: a prerelease of the release that removed the keychain
// backend does not have the keychain backend.
//
// That cut is also what makes strconv.Atoi safe to lean on here. Atoi accepts a
// leading sign, so "2.1.-1" would otherwise parse to a negative patch -- but the
// cut removes everything from the first '-' or '+' onward, so no sign ever
// reaches it and the component comes back empty instead. A separate digit check
// was written first and deleted: no input could distinguish it, which made it
// dead code that read like a guard.
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Version{}, false
	}
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var v Version
	for i, dst := range []*int{&v.Major, &v.Minor, &v.Patch} {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return Version{}, false
		}
		*dst = n
	}
	return v, true
}

// Install is what ccdad could learn about the Claude Code on this machine.
type Install struct {
	// Launcher is the path that named it — what PATH resolved, or the
	// well-known location the fallback found.
	Launcher string
	// Target is where the launcher resolves to. It EQUALS Launcher when the
	// launcher is itself the binary, and is empty only when the path could not
	// be resolved at all. Whether a report prints it is String's decision, not
	// this field's: two places deciding the same thing is how one of them
	// becomes unreachable.
	Target string
	// Method is how it was installed, as far as the layout shows.
	Method Method
	// Version is the release, when Known.
	Version Version
	// Known reports whether Version means anything. False is a real answer —
	// a launcher ccdad cannot classify — and callers must not treat it as an
	// old version or as a new one.
	Known bool
	// Why explains an unknown version in the words doctor prints. Empty when
	// Known.
	Why string
}

// Describe reports the version at a launcher path, reading only.
//
// Every branch stats or reads; nothing here executes, creates or writes. That
// is what lets doctor call it.
func Describe(launcher string) Install {
	in := Install{Launcher: launcher, Method: MethodUnknown}

	real, realErr := filepath.EvalSymlinks(launcher)

	// Native, and the FULLY RESOLVED path is asked first. dan() only needs a
	// boolean, so one readlink level is enough for it; a VERSION has to name
	// what actually runs, and those differ: a versions/2.1.100 entry that is
	// itself a link to versions/2.1.241 executes 2.1.241, so answering 2.1.100
	// would name a release this machine no longer has -- and a wrong version
	// drives a refusal in `ccdad run` and a failure in doctor, which is worse
	// than any "unknown". The last such segment on the chain wins, because it
	// is the one nearest the bytes that run.
	if realErr == nil {
		if version, ok := versionSegment(real); ok {
			return in.native(real, version)
		}
	}

	// The one readlink level is the FALLBACK, for the launcher whose versions
	// entry points OUT of the versions directory: full resolution walks past
	// the segment and loses the version, while the link the installer wrote
	// still names it.
	if target, version, ok := nativeVersion(launcher); ok {
		return in.native(target, version)
	}

	if realErr != nil {
		in.Why = fmt.Sprintf("%s could not be resolved: %v", launcher, realErr)
		return in
	}
	in.Target = real

	if version, where, ok := npmVersion(real); ok {
		in.Method = MethodNPM
		if v, parsed := ParseVersion(version); parsed {
			in.Version, in.Known = v, true
			return in
		}
		in.Why = fmt.Sprintf("%s gives its version as %q, which is not a version number", where, version)
		return in
	}

	in.Why = fmt.Sprintf("%s is neither a symlink into a claude/versions directory nor an npm install of %s, "+
		"so ccdad has no way to read its version without running it", displayPath(launcher, real), PackageName)
	return in
}

// native fills in a launcher that resolved into a claude/versions directory.
//
// A version directory named something that is not a version is reported rather
// than guessed at: the LAYOUT was recognised, so the remedy ("your launcher
// points somewhere odd") differs from the remedy for a launcher with no layout
// around it at all.
func (in Install) native(target, version string) Install {
	in.Method = MethodNative
	in.Target = target
	if v, parsed := ParseVersion(version); parsed {
		in.Version, in.Known = v, true
		return in
	}
	in.Why = fmt.Sprintf("its launcher resolves into %s, whose version directory is named %q rather than a version number",
		filepath.Dir(target), version)
	return in
}

// KeychainEra reports whether this install is one where the macOS Keychain
// shadows .credentials.json and CLAUDE_SECURESTORAGE_CONFIG_DIR does nothing.
//
// An unknown version is NOT the era. Every caller acts on this — doctor rules
// `fail`, run refuses to start — and "ccdad could not classify this install"
// must not fire either of those on a machine that works.
func (in Install) KeychainEra() bool {
	return in.Known && in.Version.AtMost(LastKeychainEra)
}

// String renders the install for a diagnostic line.
func (in Install) String() string {
	version := "an unreadable version"
	if in.Known {
		version = in.Version.String()
	}
	where := in.Launcher
	if in.Target != "" && in.Target != in.Launcher {
		where = fmt.Sprintf("%s -> %s", in.Launcher, in.Target)
	}
	return fmt.Sprintf("Claude Code %s (%s) at %s", version, in.Method, where)
}

// lookPath resolves the launcher on PATH. It is a var so a test can describe a
// machine with a claude on it without putting one there.
var lookPath = exec.LookPath

// Probe finds the claude this machine would run and describes it.
//
// PATH first, because that is what `ccdad run` execs and what a user's shell
// starts. The two fallbacks are the launcher locations Claude Code's own
// diagnostics fall back to in the same order, for the machine where claude is
// installed but its directory was never added to PATH — which is a state ccdad
// already reports on for its own binary.
func Probe() (Install, error) {
	if path, err := lookPath("claude"); err == nil {
		return Describe(path), nil
	}
	for _, path := range fallbackLaunchers() {
		info, err := os.Lstat(path)
		if err != nil || info.IsDir() {
			continue
		}
		return Describe(path), nil
	}
	return Install{}, ErrNoClaudeCode
}

// fallbackLaunchers are the well-known launcher paths, in the order Claude
// Code's own installation report tries them.
//
// Both are derived from the HOME directory rather than from CLAUDE_CONFIG_DIR,
// and the second one deliberately: inside a `ccdad run` session CLAUDE_CONFIG_DIR
// names the session's own directory, so resolving ~/.claude/local through it
// would look for an npm install inside a directory ccdad created seconds ago.
// Claude Code's installation report joins homedir() for the same path.
func fallbackLaunchers() []string {
	home, err := ccpath.Home()
	if err != nil {
		return nil
	}
	names := []string{"claude"}
	// The native launcher is claude.exe on Windows. exec.LookPath applies
	// PATHEXT for the PATH case; this list is the fallback's equivalent, and
	// it is unconditional because naming a file that cannot exist on the
	// running platform costs one failed Lstat.
	names = append(names, "claude.exe", "claude.cmd")
	var out []string
	for _, name := range names {
		out = append(out, filepath.Join(home, ".local", "bin", name))
	}
	out = append(out, filepath.Join(home, ".claude", "local", "claude"))
	return out
}

// nativeVersion is Claude Code's dan() predicate, read for the version it
// implies: the launcher must be a symlink, and its target — resolved against
// the link's own directory, exactly as dan() resolves it — must pass through a
// claude/versions directory.
// The Lstat is dan()'s own first clause and is kept for faithfulness rather than
// for effect: measured by mutation, deleting it changes no observable answer on
// this platform, because os.Readlink refuses a non-symlink two lines down and
// the function returns false either way. It is recorded here so the next reader
// does not delete it as dead code and does not write a test for it that cannot
// fail -- it is a deliberate seam, not an untested branch.
func nativeVersion(launcher string) (target, version string, ok bool) {
	info, err := os.Lstat(launcher)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", "", false
	}
	link, err := os.Readlink(launcher)
	if err != nil {
		return "", "", false
	}
	target = link
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(launcher), target)
	}
	target = filepath.Clean(target)
	version, ok = versionSegment(target)
	return target, version, ok
}

// versionSegment finds a `claude/versions/<V>` run in a path and returns <V>.
//
// The LAST such run wins. A user whose data home is itself under a directory
// called claude/versions -- or a resolved chain that passes through two of them
// -- has one segment that is context and one that is the answer, and the answer
// is always the one nearest the file.
//
// filepath is the right splitter here and only here: every path this sees comes
// from a stat or a readlink on the RUNNING machine, so the separator the
// package chose at build time is that machine's. Anything that parsed the other
// platform's paths would have to spell both separators by hand — cmdshim.go
// carries that rule and the reason for it.
func versionSegment(path string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	// i+2 must be a real element: `.../claude/versions` with nothing after it
	// is the directory itself, not a version in it.
	for i := len(parts) - 3; i >= 0; i-- {
		if parts[i] == "claude" && parts[i+1] == "versions" && parts[i+2] != "" {
			return parts[i+2], true
		}
	}
	return "", false
}

// maxPackageWalk bounds the climb from a launcher to the package that owns it.
// A launcher sits at most a couple of levels inside its package or beside the
// node_modules holding it; the bound is what stops a launcher in an unrelated
// place from walking to the filesystem root reading files.
const maxPackageWalk = 8

// maxPackageJSON caps a package.json read. npm's own manifests are kilobytes;
// this is the same refusal cclink applies to the credentials file, for the same
// reason — a path that happened to name a huge file is not something a
// diagnostic should read into memory.
const maxPackageJSON = 1 << 20

// npmVersion walks up from a resolved launcher looking for Claude Code's own
// package manifest, and returns the version it declares and the file it came
// from.
//
// Two shapes at every level, because the launcher is inside the package on some
// installs and beside the tree containing it on others:
//
//   - inside: an npm global bin symlink resolves to
//     <prefix>/lib/node_modules/@anthropic-ai/claude-code/cli.js (<=2.1.112) or
//     .../bin/claude.exe (>=2.1.113), so an ancestor IS the package root.
//   - beside: a Windows .cmd shim is not a symlink and stays at <prefix>, with
//     the package under <prefix>/node_modules/. So is ~/.claude/local/claude,
//     the sh script `claude install --local` writes, whose package sits in
//     <local>/node_modules/.
func npmVersion(launcher string) (version, where string, ok bool) {
	dir := filepath.Dir(launcher)
	for i := 0; i < maxPackageWalk; i++ {
		candidates := []string{
			filepath.Join(dir, "package.json"),
			filepath.Join(dir, "node_modules", filepath.FromSlash(PackageName), "package.json"),
		}
		for _, path := range candidates {
			if v, found := readPackageVersion(path); found {
				return v, path, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", "", false
}

// readPackageVersion reads a package.json and reports whether it is Claude
// Code's own, along with whatever it declares as a version.
//
// The name check is the whole safety property. Without it the first ancestor
// manifest wins, and on a local install that is ~/.claude/local/package.json,
// which Claude Code writes itself as {"name":"claude-local","version":"0.0.1"} —
// a version number, of the wrong thing, that would make every machine with a
// local install look like it was on the keychain side of the boundary.
//
// The version is NOT part of the found test, deliberately. A manifest that names
// the right package and carries no readable version means "this is the install,
// and its version could not be read" — which is a different answer from "there
// is no install here", and it is the one that tells a user where to look. Making
// found depend on the version would report the first and print the second.
func readPackageVersion(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPackageJSON))
	if err != nil {
		return "", false
	}
	var manifest struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", false
	}
	if manifest.Name != PackageName {
		return "", false
	}
	return manifest.Version, true
}

// displayPath names a launcher, adding where it resolved to when that differs.
func displayPath(launcher, real string) string {
	if real == "" || real == launcher {
		return launcher
	}
	return fmt.Sprintf("%s (which resolves to %s)", launcher, real)
}

// NextPatch is the release immediately after v. It exists so the one place
// LastKeychainEra is written stays the one place: every message that says
// "upgrade to 2.1.113 or later" derives that number from the boundary rather
// than repeating it, which is what stops the two from drifting apart.
func (v Version) NextPatch() Version { return Version{v.Major, v.Minor, v.Patch + 1} }
