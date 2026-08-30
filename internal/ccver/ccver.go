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
// # The launcher that is a copy
//
// On Windows the installer does not link. dan() is `windows ? true` there
// because there is nothing to readlink: the install copies versions/<V> to
// <home>/.local/bin/claude.exe, and no manifest, marker or link records which
// <V> it took. Claude Code answers that question for itself by comparing SIZES,
// in its Windows update and again in its orphan cleanup, and both are already
// wrong on the machine this was written on. identifyCopy carries the
// measurement; the ruling is that the bytes are the only exact source, and that
// they are cheap enough to read.
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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// LastKeychainEra is the last Claude Code that does not know
// CLAUDE_SECURESTORAGE_CONFIG_DIR. 2.1.113 introduced the variable.
//
// IT NO LONGER MEANS "the last one that reads the macOS Keychain", and the name
// is a leftover of when it meant both. The keychain half was measured again and
// is FALSE: 2.1.234, 2.1.238 and 2.1.251 each carry a whole keychain backend
// that spawns `security find-generic-password`, and the combinator still reads
// the item BEFORE .credentials.json. The backend was never removed.
//
// The two facts were bisected as one and only one of them held. The npm
// metadata that "agreed from the outside" -- bin entry "cli.js" through 2.1.112,
// "bin/claude.exe" from 2.1.113, unpacked size ~49 MB to ~132 KB -- dated the
// PACKAGING change, which is real and is not evidence about the backend. The
// code half searched a release for `name:"plaintext"`, found it, and stopped;
// the combinator is named `keychain-with-plaintext-fallback`, so finding the
// fallback proves nothing about the primary. See internal/cclink/keychain.go.
//
// Renaming it is worth doing and is not done here, because every caller of
// KeychainEra() reads it for the variable and would have to be re-read one by
// one to be sure none is still reading it for the keychain.
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
	// <data home>/claude/versions/<VERSION> -- or, on Windows, a byte-for-byte
	// COPY of one of the binaries in it, because that installer branches on
	// startsWith("win32") and calls copyFile where every other platform gets
	// symlink. Install.Copied says which of the two a given launcher is.
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
	// Target is where the launcher resolves to -- or, when Copied, the binary
	// its bytes were taken from, which is the nearest thing a copy has. It
	// EQUALS Launcher when the launcher is itself the binary, and is empty only
	// when the path could not be resolved at all. Whether a report prints it is String's decision, not
	// this field's: two places deciding the same thing is how one of them
	// becomes unreachable.
	Target string
	// Copied reports that Launcher is a byte-for-byte COPY of Target rather
	// than a link to it, which is the launcher the installer writes on
	// Windows. It is a field rather than a rendering decision taken inside
	// String, because the two shapes go out of step: replace the versions
	// entry and a symlink follows it while a copy does not, so a reader
	// checking ccdad's answer needs to know which of the two they have.
	Copied bool
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

	// A launcher that does not resolve is UNKNOWN, and this is checked before
	// the one-level fallback below rather than after it. os.Readlink does not
	// stat what it points at, so a dangling native launcher -- a versions entry
	// deleted by a cleanup, a half-finished update -- would otherwise be read
	// out of the link TEXT and reported as a healthy, known version for a
	// binary that is not there. Nothing on that machine can start claude at
	// all, and saying "ok, 2.1.241" is the confident wrong answer this package
	// is built to avoid.
	if realErr != nil {
		in.Why = fmt.Sprintf("%s could not be resolved: %v", launcher, realErr)
		return in
	}

	// The one readlink level is the FALLBACK, for the launcher whose versions
	// entry points OUT of the versions directory: full resolution walks past
	// the segment and loses the version, while the link the installer wrote
	// still names it.
	if target, version, ok := nativeVersion(launcher); ok {
		return in.native(target, version)
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

	// A native install whose launcher is a COPY rather than a link, which is
	// what Windows gets: measured in 2.1.241, the installer branches on
	// `startsWith("win32")` and does copyFile(installPath, launcher), falling
	// through to symlink() only off Windows. Neither of the two native paths
	// above can see through a copy, so the version comes from the bytes --
	// identifyCopy carries what that costs and why nothing cheaper is exact.
	//
	// Not gated on GOOS, so the Linux suite can exercise it, and because the
	// shape is what matters rather than the platform.
	if versions, ok := nativeLayoutFor(launcher); ok {
		in.Method = MethodNative
		match, why := identifyCopy(launcher, versions)
		if why != "" {
			in.Why = why
			return in
		}
		in.Copied = true
		return in.native(match, filepath.Base(match))
	}

	in.Why = fmt.Sprintf("%s is neither a symlink into a claude/versions directory nor an npm install of %s, "+
		"so ccdad has no way to read its version without running it", displayPath(launcher, real), PackageName)
	return in
}

// nativeLayoutFor reports the versions directory that belongs to the SAME home
// the launcher sits under, when the launcher sits in a native launcher
// directory at all.
//
// PAIRED, and not two independent lookups. The installer writes
// <home>/.local/bin/claude.exe and <home>/.local/share/claude/versions from one
// home in one operation, so a launcher under one home and a versions tree under
// another are two different installs and the bytes of the first are not a copy
// of anything in the second. On Windows with HOME set both homes exist and both
// can hold a real install, which is exactly when asking the two questions
// separately answers about two machines at once.
func nativeLayoutFor(launcher string) (versions string, ok bool) {
	dir := filepath.Dir(launcher)
	homes, err := layoutHomes()
	if err != nil {
		return "", false
	}
	for _, home := range homes {
		if !sameDir(dir, filepath.Join(home, ".local", "bin")) {
			continue
		}
		if dir, found := versionsDirIn(home); found {
			return dir, true
		}
	}
	return "", false
}

// versionsDirIn is Claude Code's G8n() -- <data home>/claude/versions -- for one
// home, when that directory is actually there.
//
// The data home is XDG_DATA_HOME or <home>/.local/share, as fpr() computes it.
// XDG_DATA_HOME wins over the home for the same reason it does there, which is
// also why this takes a home and does not resolve one: with the variable set
// every home gives the same answer, and with it unset the answer has to be the
// one belonging to the launcher's own home.
func versionsDirIn(home string) (string, bool) {
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	dir := filepath.Join(data, "claude", "versions")
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return dir, true
}

// layoutHomes are the homes a native install can be under, in search order.
//
// TWO OF THEM, AND CLAUDE CODE IS WHY: it INSTALLS under lAi()'s home and
// SEARCHES under a plain os.homedir(). Measured in 2.1.241 -- Cst() builds the
// install layout from q1e()/G8n(), which join lAi().home (`env.HOME ??
// os.homedir()`), while every place it looks for an install it did not just
// perform joins os.homedir() directly: bTE()'s launcher fallback stats
// join(os.homedir(), ".local/bin/claude") after its PATH lookup, and STE()'s
// installation report pushes type "native" at join(os.homedir(), ".local",
// "bin", "claude") and its orphan scan at join(os.homedir(), ".local", "share",
// "claude").
//
// So on Windows with HOME set BOTH directories can hold a real install, and
// which one does depends on the shell the installer -- or the last auto-update
// -- ran from: PowerShell has no HOME and installs under %USERPROFILE%, an
// MSYS2 or Cygwin shell has one and installs under it. Searching only the
// layout home loses the first, and searching only the config home loses the
// second. Discovery is not allowed to lose an install that is there, so it
// covers both, layout home first because that is the shell the user is in.
//
// On Unix this is one entry: the two resolvers are the same value there.
//
// An error only when NEITHER can be spelled. A home that resolves is a home
// that can be searched, and reporting "no home directory" while holding one is
// the same false negative as the search itself missing. The error is never nil
// alongside an empty list, and that rests on ccpath rather than on a guard here:
// LayoutHome falls back to the same lookup Home uses, so LayoutHome failing
// means Home failed too.
func layoutHomes() ([]string, error) {
	layout, layoutErr := ccpath.LayoutHome()
	home, homeErr := ccpath.Home()
	var homes []string
	if layoutErr == nil {
		homes = append(homes, layout)
	}
	if homeErr == nil && !sameDir(home, layout) {
		homes = append(homes, home)
	}
	if len(homes) == 0 {
		return nil, homeErr
	}
	return homes, nil
}

// compareChunk is how much of each file sameContents holds at a time. A
// megabyte is large enough that the syscall count is noise against the read
// itself and small enough that two of them are not worth thinking about.
const compareChunk = 1 << 20

// identifyCopy names the versions entry a launcher is a byte-for-byte COPY of,
// or says why there is no single answer. It is how a Windows native install
// gets a version at all.
//
// # Why the bytes, and why nothing cheaper
//
// The Windows installer leaves NOTHING on disk that names the version it put in
// the launcher. Measured out of 2.1.241 and confirmed unchanged in 2.1.112: the
// install writes versions/<V> and copies it to <home>/.local/bin/claude.exe,
// the staging directory is deleted on success, and the claude.exe.old.<millis>
// files the update leaves behind are swept on the next start. There is no
// manifest, no marker and no link.
//
// CLAUDE CODE ITSELF ANSWERS THIS QUESTION BY SIZE, IN TWO PLACES, AND THAT IS
// THE REASON NOT TO. Its Windows update returns early -- no copy at all -- when
// stat(launcher).size === stat(newVersion).size, and its orphan cleanup
// protects every versions entry whose size matches the launcher's. Both are
// wrong on the machine this was written on, whose versions directory holds
//
//	2.1.240   342,636,848 bytes   sha256 1386169d...
//	2.1.241   342,636,848 bytes   sha256 0771bd86...
//
// -- the same size, different builds. Two consequences follow, and the second
// is why "the newest entry wins" is not a fallback either: a Windows user
// updating 2.1.240 -> 2.1.241 keeps a launcher holding 2.1.240's bytes, so the
// binary that RUNS is genuinely older than the newest thing installed, and only
// the bytes say so.
//
// So: compare CONTENT. That is exact rather than probable, and exactness is the
// whole point -- a WRONG version here drives `ccdad run` to refuse on a working
// machine, which is the defect this package's history is mostly about.
//
// The size test in the loop below is a COST filter and nothing else. Equal
// content implies equal size, so it removes candidates the comparison would
// have rejected anyway; deleting it changes no answer and only makes the reads
// longer. Nothing correct rests on it, and the comment says so because a size
// check sitting next to a version is exactly what a reader would mistake for
// the decision.
//
// # What it costs
//
// One pass over the launcher and over each same-size candidate, and the size
// filter is what keeps that list at one or two entries. Measured on the pair
// above: eliminating the wrong candidate reads to byte 89,141,249 where they
// first differ and takes 0.06s; confirming the right one reads all 342 MB and
// takes about a fifth of a second warm. `ccdad run` then execs a binary of that
// exact size, so this is a fraction of a read the caller was about to do anyway.
//
// # The empty file, which is a real entry and not a corner case
//
// A versions entry is RESERVED before it is downloaded: the installer does
// writeFile(join(versions, <V>), "", {flag:"wx"}) and only then fetches, so a
// validly-named ZERO-BYTE versions/<V> exists for the whole download window and
// survives an interrupted install. A Windows launcher can be empty too -- the
// update renames the old claude.exe aside FIRST and the new one is an empty
// destination until bytes land. Two empty files hold the same bytes, so without
// this guard an interrupted install matches a reservation and Known=true names a
// release the launcher does not hold: doctor rules `fail` and `ccdad run`
// refuses, blaming a keychain-era Claude Code on a machine whose actual fault is
// that its launcher is empty.
//
// Both of Claude Code's own readers of this directory refuse zero bytes for the
// same reason -- its newest-version scan takes an entry only when
// isFile() && size > 0, and its "is the launcher valid" probe requires
// size !== 0 -- so this is mirroring them rather than inventing a rule.
//
// ONE guard rather than two, and deliberately: refusing the empty LAUNCHER is
// enough, because the size filter already drops every zero-byte candidate for
// any launcher that has bytes. A second `candidate.Size() == 0` skip in the loop
// below was written first and deleted -- no input could reach it, which made it
// dead code that read like a guard.
//
// It stats, lists and reads. Like everything else here it creates nothing.
func identifyCopy(launcher, versions string) (match, why string) {
	info, err := os.Stat(launcher)
	if err != nil {
		return "", fmt.Sprintf("%s could not be read to compare it against the binaries in %s: %v",
			launcher, versions, err)
	}
	if info.Size() == 0 {
		return "", fmt.Sprintf("%s is a native install whose launcher is EMPTY — zero bytes, so it cannot "+
			"execute at all, and an install interrupted between renaming the old launcher aside and copying "+
			"the new one in looks exactly like this. Re-run the Claude Code installer; the binaries in %s are "+
			"still there", launcher, versions)
	}
	entries, err := os.ReadDir(versions)
	if err != nil {
		return "", fmt.Sprintf("%s is a native install whose launcher is a COPY rather than a symlink, and %s "+
			"could not be listed to say which binary it was copied from: %v", launcher, versions, err)
	}
	var matches []string
	for _, entry := range entries {
		// The name has to be a version, because the name IS the answer -- the
		// same trust the symlink path places in it. It also drops the
		// <V>.tmp.<pid>.<millis>.<n> files the installer's atomic move leaves
		// when it is interrupted, which are real binaries under names that are
		// not releases.
		if _, ok := ParseVersion(entry.Name()); !ok {
			continue
		}
		candidate, err := entry.Info()
		if err != nil || !candidate.Mode().IsRegular() || candidate.Size() != info.Size() {
			continue
		}
		path := filepath.Join(versions, entry.Name())
		if same, err := sameContents(launcher, path); err == nil && same {
			matches = append(matches, path)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], ""
	case 0:
		return "", fmt.Sprintf("%s is a native install whose launcher is a COPY of one of the binaries in %s "+
			"rather than a symlink into it, which is what the installer writes on Windows — and its bytes match "+
			"none of the versions still installed there, so nothing on disk says which release it is",
			launcher, versions)
	default:
		// Two versions entries holding identical bytes: one release published
		// twice, or a hand-made copy. The launcher IS both, so the build is
		// known and the release name is not, and this package does not pick
		// between two names it cannot tell apart.
		return "", fmt.Sprintf("%s is a native install whose launcher is a COPY of one of the binaries in %s, "+
			"and %s are byte-for-byte identical to it and to each other — so its bytes name a build that is "+
			"installed under more than one release name", launcher, versions, strings.Join(baseNames(matches), " and "))
	}
}

// baseNames reduces paths to their last elements, which for a versions entry is
// the release name.
func baseNames(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	return out
}

// sameContents reports whether two files hold the same bytes.
//
// A streaming compare rather than a hash of each side, and the reason is
// measured rather than stylistic: two same-size binaries diverge long before
// their end -- the pair in identifyCopy's comment parts a quarter of the way in
// -- so a compare stops there while a hash has to read both files whole. On that
// pair the compare costs 0.06s against 0.78s to sha256 one side. A hash would
// also invite a cache and a truncated digest, and neither of those is exact.
//
// An unreadable file is an error and never a match: identifyCopy treats it as a
// candidate that did not answer, which leaves the version unknown rather than
// crediting it.
func sameContents(a, b string) (bool, error) {
	fileA, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fileA.Close()
	fileB, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fileB.Close()

	bufA := make([]byte, compareChunk)
	bufB := make([]byte, compareChunk)
	for {
		readA, errA := io.ReadFull(fileA, bufA)
		readB, errB := io.ReadFull(fileB, bufB)
		if readA != readB || !bytes.Equal(bufA[:readA], bufB[:readB]) {
			return false, nil
		}
		// io.ReadFull ends a short final chunk with ErrUnexpectedEOF and an
		// exactly-aligned one with EOF, so both count as "this file ended".
		endA := errors.Is(errA, io.EOF) || errors.Is(errA, io.ErrUnexpectedEOF)
		endB := errors.Is(errB, io.EOF) || errors.Is(errB, io.ErrUnexpectedEOF)
		if errA != nil && !endA {
			return false, errA
		}
		if errB != nil && !endB {
			return false, errB
		}
		if endA || endB {
			return endA && endB, nil
		}
	}
}

// sameDir compares two directory paths.
//
// CASE-INSENSITIVE ON WINDOWS, and that is not a nicety. The two sides come from
// independent producers -- one is filepath.Dir of whatever spelling %PATH%
// carried into exec.LookPath, the other is <home>/.local/bin built from a HOME
// or %USERPROFILE% the user may have typed themselves -- and Windows treats
// `C:\Users\u` and `c:\users\u` as one directory while filepath.Clean does
// not fold either case or the volume letter. A launcher found under one
// spelling and a home written in the other would answer "this is not a native
// install" about a native install, on the one platform the copy branch exists
// for. The repository already rules this way twice: setuppath.go compares a
// %PATH% component against this same ~/.local/bin with foldCase on Windows, and
// scoped.go records that filepath.Rel folds elements with EqualFold there.
// Claude Code does it too -- its own PATH-membership check lowercases both sides
// on Windows before comparing.
//
// NOT folded off Windows, because there .local/bin and .local/Bin are two
// directories and folding them would make a launcher in one look like an
// install in the other.
//
// An empty right-hand side never matches, so an unresolvable home cannot make
// every launcher look native.
func sameDir(a, b string) bool {
	if b == "" {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
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

// KeychainEra reports whether this install is one where
// CLAUDE_SECURESTORAGE_CONFIG_DIR does nothing. Despite the name it says
// NOTHING about the keychain any more -- every release reads that item, so
// there is no era to be on. LastKeychainEra says why the name survives.
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
	switch {
	case in.Copied && in.Target != "":
		// Never an arrow: a copy does not resolve anywhere, and printing one
		// would tell a user that deleting the versions entry breaks the
		// launcher, which on Windows is the one platform where it does not.
		where = fmt.Sprintf("%s, a byte-for-byte copy of %s", in.Launcher, in.Target)
	case in.Target != "" && in.Target != in.Launcher:
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
	fallbacks, err := fallbackLaunchers()
	if err != nil {
		// NOT ErrNoClaudeCode. "there is no claude in the two places the
		// installers write one" is a result, and this is the absence of a
		// search: without a home directory neither path could even be spelled.
		// doctor words the two differently, and the difference is the whole
		// reason this is not collapsed.
		return Install{}, err
	}
	for _, path := range fallbacks {
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
// Both are derived from a HOME directory rather than from CLAUDE_CONFIG_DIR,
// and the second one deliberately: inside a `ccdad run` session CLAUDE_CONFIG_DIR
// names the session's own directory, so resolving ~/.claude/local through it
// would look for an npm install inside a directory ccdad created seconds ago.
//
// WHICH home is two answers rather than one, and the two halves of this list
// take different ones.
//
//   - .local/bin is the LAYOUT path, and it is searched under every home
//     layoutHomes names -- which on Windows is two, because Claude Code installs
//     under one and searches under the other. layoutHomes carries the
//     measurement.
//   - ~/.claude/local is a CONFIG-side path: `claude install --local` writes it
//     under the same home its config root Hn() joins, which is a plain
//     os.homedir() and therefore ccpath.Home. Searching it under the layout home
//     as well would be reach with no install shape behind it, and this package
//     has already paid once for a probe that reached further than any real
//     install goes -- see npmVersion.
//
// It is skipped, rather than the whole list refused, when only the config home
// is unspellable: a layout home that resolved is still a home that can be
// searched.
func fallbackLaunchers() ([]string, error) {
	homes, err := layoutHomes()
	if err != nil {
		return nil, err
	}
	// The native launcher is claude.exe on Windows. exec.LookPath applies
	// PATHEXT for the PATH case; this list is the fallback's equivalent, and
	// it is unconditional because naming a file that cannot exist on the
	// running platform costs one failed Lstat.
	names := []string{"claude", "claude.exe", "claude.cmd"}
	var out []string
	for _, home := range homes {
		for _, name := range names {
			out = append(out, filepath.Join(home, ".local", "bin", name))
		}
	}
	if config, err := ccpath.Home(); err == nil {
		out = append(out, filepath.Join(config, ".claude", "local", "claude"))
	}
	return out, nil
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

// maxPackageWalk bounds the climb from a launcher to the package that CONTAINS
// it. Two levels is every shape that exists: <pkg>/cli.js through 2.1.112 puts
// the launcher in the package root, and <pkg>/bin/claude.exe from 2.1.113 puts
// it one directory in. The third is slack.
//
// It used to be 8, and eight was not slack — it was exposure. See npmVersion.
const maxPackageWalk = 3

// maxPackageJSON caps a package.json read. npm's own manifests are kilobytes;
// this is the same refusal cclink applies to the credentials file, for the same
// reason — a path that happened to name a huge file is not something a
// diagnostic should read into memory.
const maxPackageJSON = 1 << 20

// npmVersion finds the Claude Code package a resolved launcher belongs to, and
// returns the version it declares and the file that declared it.
//
// TWO SHAPES, AND THEY GET DIFFERENT REACH. That asymmetry is the whole
// function, and getting it wrong was this branch's worst defect.
//
//   - INSIDE, which climbs. An npm global bin symlink resolves to
//     <prefix>/lib/node_modules/@anthropic-ai/claude-code/cli.js (<=2.1.112) or
//     .../bin/claude.exe (>=2.1.113), so an ANCESTOR of the launcher is the
//     package root. This is self-limiting: an ancestor only matches when the
//     launcher genuinely lives inside that package.
//
//   - BESIDE, which does NOT climb. A Windows .cmd shim is not a symlink and
//     stays at <prefix> with the package under <prefix>/node_modules/; so does
//     ~/.claude/local/claude, the sh script `claude install --local` writes,
//     whose package sits in <local>/node_modules/. In BOTH the package is in
//     the launcher's OWN directory, so this candidate is tried at level 0 and
//     nowhere else.
//
// Probing "beside" at every level was pure exposure with no install shape
// behind it, and it produced the one outcome this package exists to prevent: a
// WRONG version, returned with Known=true. A user with the native 2.1.241 who
// puts a wrapper script at ~/projects/app/bin/claude, in a project that pins
// @anthropic-ai/claude-code 2.1.100 in its own node_modules, got 2.1.100 --
// which is inside the keychain era, so `ccdad run` refused to start and doctor
// ruled `fail`, on a machine where everything works. Any launcher that is
// neither a native symlink nor inside the package -- an asdf or volta shim, a
// hand-written wrapper -- reached $HOME within the old eight levels, so
// $HOME/node_modules/@anthropic-ai/claude-code was enough to do it.
//
// Note what is NOT the fix. Claude Code's own pan() requires the realpath to
// end in .js or contain node_modules, and adopting that as a precondition would
// reject both BESIDE shapes outright -- a .cmd shim and an sh script are
// neither. Departing from pan() here is deliberate; the reach of the sideways
// probe was the bug.
func npmVersion(launcher string) (version, where string, ok bool) {
	dir := filepath.Dir(launcher)
	if v, found := readPackageVersion(besideManifest(dir)); found {
		return v, besideManifest(dir), true
	}
	for i := 0; i < maxPackageWalk; i++ {
		path := filepath.Join(dir, "package.json")
		if v, found := readPackageVersion(path); found {
			return v, path, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", "", false
}

// besideManifest is the package manifest for a tree sitting in dir itself.
func besideManifest(dir string) string {
	return filepath.Join(dir, "node_modules", filepath.FromSlash(PackageName), "package.json")
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
