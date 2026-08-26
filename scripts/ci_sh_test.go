package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// scripts/ci.sh is run for real by every CI job, which constrains the passing
// path and nothing else. What is not constrained is the part that matters: a
// check that has quietly stopped looking at anything still reports success,
// and a green board then means nothing. These tests are for the failing paths.
//
// `vet`, `test` and `cgo` are deliberately not exercised here. `test` would
// re-enter `go test` recursively, and the other two are minutes of compilation
// to re-prove what the CI job proves on every push.

// runCI runs scripts/ci.sh from dir — the repository root of a throwaway tree
// for the fmt tests, and the real one otherwise.
//
// The child's environment is ciEnv's rather than this process's, and that is
// the difference between a test describing the script and a test describing
// the machine it ran on. See ciEnv for what the inheritance was costing.
func runCI(t *testing.T, root string, args ...string) (string, int) {
	t.Helper()
	return runCIWithEnv(t, root, ciEnv(t), args...)
}

// ciTools is what ci.sh executes on the paths runCI reaches, and the list is
// the answer to "what does a pinned PATH have to carry": `fmt` runs gofmt and
// git, `cites` runs git, sed, grep and awk, and every arm runs dirname to
// resolve the script's own root. `go`, `mktemp`, `file` and `env` belong to
// check_cgo, and `claude` to check_plugin; runCI runs neither, and the plugin
// tests build their own PATH precisely so that they can control that answer.
var ciTools = []string{"git", "gofmt", "awk", "sed", "grep", "dirname"}

// ciPath is the directories those tools are actually in, measured on the
// machine rather than written down.
//
// A literal `/usr/bin:/bin` — which is what envWithoutClaude below can afford,
// because the plugin check needs no Go toolchain — is wrong here, and measured
// wrong: gofmt is in /usr/local/bin on the machine this was written on and in
// the runner's hosted toolcache on Actions, and neither of those is /usr/bin.
// What that costs is not a test that cannot run but one that passes for the
// wrong reason: ci.sh reports `gofmt exited 127` and returns 1, and 1 is the
// same status an unformatted tree returns.
func ciPath(t *testing.T) string {
	t.Helper()
	var dirs []string
	seen := map[string]bool{}
	for _, tool := range ciTools {
		full, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("ci.sh runs %s and this machine has none on PATH: %v", tool, err)
		}
		dir := filepath.Dir(full)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// ciEnv is the environment ci.sh runs under here, named rather than inherited.
//
// runCI used to hand the child os.Environ(). Three variables were doing real
// work inside that inheritance, and each is a measurement rather than a worry:
//
//   - GITHUB_ACTIONS is set on a runner, so `group` wrote `::group::` and
//     `::endgroup::` into the output every assertion in this file greps. The
//     suite was exercising one branch of `group` on Actions and the other on a
//     developer's machine — which is the class of difference this file exists
//     to stop. It is left out here and set by envWithActions alone, so the two
//     environments differ in exactly the variable under test.
//
//   - HOME. git does not NEED one: `fmt` and `cites` both pass under `env -i`
//     carrying nothing but PATH. It READS one, which is the problem, because
//     `--exclude-standard` honours core.excludesFile. Measured: against a HOME
//     whose global excludes name broken.go, the unformatted-file fixture is
//     reported as clean and ci.sh exits 0. Only for an UNTRACKED file —
//     `--cached` lists a staged one whatever the excludes say — which is
//     exactly the half of the fixture set throwawayRepoThenWrite exists for. A
//     throwaway HOME is what keeps that answer the check's rather than the
//     reader's. Carrying NO home is deterministic too, and measured so — git
//     then reads no global config at all, and the excludes test passes with
//     the entry deleted. It is not what is done, because no machine ci.sh runs
//     on has no HOME: a throwaway one is a clean machine, and an absent one is
//     a machine that does not exist. What holds the entry is the list test
//     below rather than a behaviour, and that is stated because it is the
//     weaker of the two kinds.
//
//   - LC_ALL, which is the one a pinned environment gets wrong by OMISSION. An
//     environment carrying no locale is the C locale, so pinning without
//     saying so would have moved every test in this file onto the
//     byte-oriented engine and deleted the other one from the suite with
//     nothing in the diff mentioning it. C is pinned deliberately — it is the
//     one locale every machine has — and the character-oriented engine is
//     named by the one test that needs both, rather than arriving by accident
//     through whoever's LANG. Measured: the whole suite is green under
//     LC_ALL=C.
//
// CCDAD_REQUIRE_CLAUDE is the fourth variable ci.sh reads and it is left out
// with nothing to weigh: it is read only by check_plugin, which runCI never
// runs.
func ciEnv(t *testing.T) []string {
	t.Helper()
	return []string{
		"PATH=" + ciPath(t),
		"HOME=" + t.TempDir(),
		"LC_ALL=C",
	}
}

// The three tests below are what makes the pinning red when it is undone. They
// are separate because they fail for three different reasons: one proves the
// pinned environment REACHES the child, one proves that what it keeps out
// CHANGES an answer, and one pins the list itself so a fourth variable cannot
// be added to the child's world without a diff saying so.

// GITHUB_ACTIONS is set here, in this process, which is the only way to ask the
// question on a machine that is not a runner. Every assertion in this file
// greps `out`; under the old runCI this fixture came back wrapped in
// `::group::` on Actions and bare everywhere else.
func TestTheChildDoesNotInheritThisProcessActionsMarkers(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	root := throwawayRepo(t, map[string]string{"internal/x/x.go": "package x\n"})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "::group::") || strings.Contains(out, "::endgroup::") {
		t.Errorf("the fold markers reached a runCI fixture from this process's environment:\n%s", out)
	}
}

// HOME, and this is the measurement that put it in ciEnv rather than a worry
// about it. git does not need a HOME; it READS one, and `--exclude-standard`
// honours core.excludesFile, so a developer whose global ignore file happens to
// name a fixture turns a red test green rather than noisy.
//
// The file is written AFTER `git add`, which is what makes it reachable: an
// ignore rule cannot hide a file `--cached` already lists.
func TestTheChildDoesNotInheritThisProcessGlobalGitExcludes(t *testing.T) {
	home := t.TempDir()
	writeFiles(t, home, map[string]string{
		".gitconfig":   "[core]\n\texcludesFile = " + filepath.Join(home, "globalignore") + "\n",
		"globalignore": "broken.go\n",
	})
	t.Setenv("HOME", home)

	root := throwawayRepoThenWrite(t,
		map[string]string{"internal/keep/keep.go": "package keep\n"},
		map[string]string{"internal/broken/broken.go": unformattedGo})

	out, code := runCI(t, root, "fmt")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — an unformatted file this process's git config hides is still "+
			"an unformatted file to the check\n%s", code, out)
	}
	if !strings.Contains(out, filepath.ToSlash(filepath.Join("internal", "broken", "broken.go"))) {
		t.Errorf("the check did not name the unformatted file:\n%s", out)
	}
}

// The list itself. The two tests above would both still pass if ciEnv grew a
// fourth entry — or handed the child os.Environ() with one key overridden —
// and this one says what the child's whole world is.
//
// LC_ALL is named as a VALUE and not merely as a key, because the value is the
// claim: C is the byte-oriented engine, it is the locale every machine has, and
// TestCICitesAllowsAnRFCSectionRange's comment is written against it.
func TestThePinnedEnvironmentNamesEveryVariableItCarries(t *testing.T) {
	t.Setenv("CCDAD_SOMETHING_THE_SCRIPT_MUST_NOT_SEE", "1")

	got := map[string]string{}
	for _, entry := range ciEnv(t) {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("%q is not a NAME=VALUE entry", entry)
		}
		if _, dup := got[key]; dup {
			t.Fatalf("%s is named twice; last-wins makes the first one unreadable", key)
		}
		got[key] = value
	}

	for _, key := range []string{"PATH", "HOME", "LC_ALL"} {
		if _, ok := got[key]; !ok {
			t.Errorf("the pinned environment carries no %s", key)
		}
	}
	if len(got) != 3 {
		t.Errorf("the pinned environment carries %d variables, want the 3 named above: %v", len(got), got)
	}
	if got["LC_ALL"] != "C" {
		t.Errorf("LC_ALL = %q, want C — the byte-oriented engine is what the section-range test is written against", got["LC_ALL"])
	}
	if got["HOME"] == os.Getenv("HOME") {
		t.Errorf("HOME = %q, which is this process's own", got["HOME"])
	}
}

// scriptTree is a directory holding a copy of ci.sh plus whatever files the
// caller asks for, and NOT a git repository. throwawayRepo builds on it.
//
// Used directly it is how a test asks what a check does when git REFUSES, and
// that question has an answer worth pinning: two checks build their file list
// from git, and the failure mode of a bad answer there is a check that looked
// at nothing and said so with exit 0.
func scriptTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()

	real, err := filepath.Abs("ci.sh")
	if err != nil {
		t.Fatalf("resolving ci.sh: %v", err)
	}
	body, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("reading ci.sh: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "ci.sh"), body, 0o755); err != nil {
		t.Fatal(err)
	}

	writeFiles(t, root, files)
	return root
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// throwawayRepo, and then more files written AFTERWARDS. The order is the whole
// point: `git add -A` has already run, so everything in `later` is on disk and
// in nobody's index — which is the state a developer is in between writing a
// file and staging it, and the state `cites` used to be blind to.
func throwawayRepoThenWrite(t *testing.T, files, later map[string]string) string {
	t.Helper()
	root := throwawayRepo(t, files)
	writeFiles(t, root, later)
	return root
}

// throwawayRepo is a git repository containing nothing but a copy of ci.sh and
// whatever files the caller asks for. `git init` is enough: `git ls-files
// --cached --others --exclude-standard` answers without a commit, and a commit
// would need an identity this test has no business configuring.
func throwawayRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := scriptTree(t, files)

	git := exec.Command("git", "init", "--quiet")
	git.Dir = root
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// STAGED, not merely written, and it is still worth doing even though both
	// checks now read `--cached --others --exclude-standard` and would see these
	// files either way: a staged fixture is the state a commit is in, which is
	// what CI runs against. throwawayRepoThenWrite is how a test asks the other
	// question deliberately. Still no commit: `git add` needs no identity, and
	// `git commit` would.
	//
	// It respects .gitignore, which is what keeps TestCIFmtIgnoresWhatGitIgnores
	// and TestCICitesIgnoresWhatGitIgnores describing the same machine.
	add := exec.Command("git", "add", "-A")
	add.Dir = root
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	return root
}

// Unformatted on purpose: a leading space before the package clause is enough
// for gofmt and survives any editor that might touch this file.
const unformattedGo = "package broken\n\n func Broken() {}\n"

func TestCIFmtReportsAnUnformattedFile(t *testing.T) {
	root := throwawayRepo(t, map[string]string{"internal/broken/broken.go": unformattedGo})

	out, code := runCI(t, root, "fmt")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — an unformatted tree is what this check is for\n%s", code, out)
	}
	if !strings.Contains(out, filepath.Join("internal", "broken", "broken.go")) {
		t.Errorf("stderr does not name the offending file:\n%s", out)
	}
	if !strings.Contains(out, "gofmt -w") {
		t.Errorf("stderr does not say how to fix it:\n%s", out)
	}
}

func TestCIFmtPassesOnAFormattedTree(t *testing.T) {
	root := throwawayRepo(t, map[string]string{"internal/fine/fine.go": "package fine\n"})

	out, code := runCI(t, root, "fmt")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
}

// The reason the file list comes from git rather than from `gofmt -l .`: a
// worktree, a scratch checkout or a build directory under the repository root
// is not this branch's code, and failing a clean tree because of one is how a
// gate gets switched off.
func TestCIFmtIgnoresWhatGitIgnores(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		".gitignore":                             "/worktrees/\n",
		"worktrees/other-branch/internal/x/x.go": unformattedGo,
		"internal/fine/fine.go":                  "package fine\n",
	})

	out, code := runCI(t, root, "fmt")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — an ignored directory is not this tree's code\n%s", code, out)
	}
	if strings.Contains(out, "other-branch") {
		t.Errorf("ci.sh looked inside an ignored directory:\n%s", out)
	}
}

// A Go file gofmt cannot PARSE, which is a different thing from one it would
// reformat and is the case every fixture above misses. `gofmt -l` exits 2 for
// it, and read bare under `set -e` that 2 became the script's own exit code --
// the code this repository reserves for a check name that does not exist. A
// check that ran and found a real problem was answering "you typed the check
// name wrong".
//
// The exact code is asserted, not merely "non-zero": 1 and 2 are both non-zero
// and the whole bug is which of them arrives.
func TestCIFmtReportsOneWhenAGoFileDoesNotParse(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/broken/broken.go": "package broken\n\nfunc {{{\n",
	})

	out, code := runCI(t, root, "fmt")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — a check that ran and found something reports 1, and 2 means an unknown check name\n%s", code, out)
	}
	if !strings.Contains(out, "gofmt exited 2") {
		t.Errorf("stderr does not say what gofmt actually exited with, which is the line that sends a reader to look for a file that does not parse:\n%s", out)
	}
}

// gofmt writes the parse error to stderr AND the merely-unformatted names to
// stdout, and exits 2 for the pair. The bare assignment threw the second away,
// so a developer with one unparseable file was never told about the other file
// that only needed `gofmt -w`.
//
// This is the test that rules out the cheap version of the fix: catching
// gofmt's status and returning 1 without keeping what it printed leaves this
// red.
func TestCIFmtStillNamesTheUnformattedFileWhenAnotherDoesNotParse(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/a/broken.go":      "package broken\n\nfunc {{{\n",
		"internal/b/unformatted.go": unformattedGo,
	})

	out, code := runCI(t, root, "fmt")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, filepath.Join("internal", "b", "unformatted.go")) {
		t.Errorf("gofmt named the unformatted file and the check dropped it; that file is the one the developer can actually fix:\n%s", out)
	}
}

// The failure that is worse than a wrong exit code, because it is GREEN. The
// file list came through a process substitution, whose status `set -e` cannot
// see, so a git that refused left check_fmt reporting "no Go files to format"
// and exiting 0.
//
// A tree that is not a repository is the cheapest reachable form of that. The
// assertion is on the code AND on the absence of the everything-is-fine line,
// because exit 0 is also what a genuinely empty tree returns.
func TestCIFmtFailsWhenGitCannotListTheTree(t *testing.T) {
	root := scriptTree(t, map[string]string{"internal/x/x.go": "package x\n"})

	out, code := runCI(t, root, "fmt")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — git refused, so this check looked at nothing and must not say it passed\n%s", code, out)
	}
	if strings.Contains(out, "no Go files to format") {
		t.Errorf("the check reported an empty tree when what actually happened is that git would not answer:\n%s", out)
	}
}

// The same question of the other check that builds a list from git. It read
// `tracked=$(git ls-files)` bare, so git's own 128 left through `set -e` as the
// script's exit code -- a worse collision than the 2 above, because 128 is the
// range a shell also uses for "killed by a signal".
func TestCICitesFailsWhenGitCannotListTheTree(t *testing.T) {
	root := scriptTree(t, map[string]string{"internal/x/x.go": "package x\n"})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — git refused; 128 is git's number, not this script's\n%s", code, out)
	}
	if strings.Contains(out, "no line cites") {
		t.Errorf("the check reported a clean tree when git would not answer:\n%s", out)
	}
}

// envWithActions is ciEnv with GITHUB_ACTIONS set, so a test can read the fold
// markers. It differs from the environment every other test here runs under in
// that one variable and nothing else, which is what makes an assertion about
// the markers an assertion about `group`.
func envWithActions(t *testing.T) []string {
	t.Helper()
	return append(ciEnv(t), "GITHUB_ACTIONS=1")
}

// A ::group:: that is opened and never closed swallows everything after it, and
// `set -e` used to abort before `endgroup` on every failing path -- which is to
// say on exactly the runs somebody is reading the log for. Measured when this
// was written: seven group/endgroup pairs and five of them abortable between
// the halves, an ordinary `vet` or `test` failure included.
//
// Counting is what sees it. No assertion on the text can: the markers are
// balanced on the passing path, which is the only path the rest of this file
// exercises under Actions.
func TestTheFoldIsClosedWhenACheckFails(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/broken/broken.go": "package broken\n\nfunc {{{\n",
	})

	out, code := runCIWithEnv(t, root, envWithActions(t), "fmt")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	open := strings.Count(out, "::group::")
	closed := strings.Count(out, "::endgroup::")
	if open != closed {
		t.Errorf("%d ::group:: and %d ::endgroup:: — the failure is inside a fold that never closes, "+
			"so the one line the reader came for is hidden:\n%s", open, closed, out)
	}
	if open == 0 {
		t.Fatalf("no fold was opened at all, so this test proved nothing:\n%s", out)
	}
	// The script's own diagnostic belongs OUTSIDE the fold, which is the half of
	// the contract that counting cannot check.
	if i, j := strings.LastIndex(out, "::endgroup::"), strings.Index(out, "ci: gofmt exited"); j >= 0 && j < i {
		t.Errorf("the check's own explanation is inside the fold:\n%s", out)
	}
}

// The same, for a check whose failure arrives from git rather than from a tool
// with something to say. Without this, closing the fold could be wired into the
// gofmt path alone and every other leg would keep the bug.
//
// What it does NOT hold, stated because measuring found it out: counting alone
// is satisfied by any ONE of the three paths that close a fold, so this test
// goes red only if all three go at once. The ordering assertion in the test
// above is the one with teeth, and TestTheExitTrapClosesAnOpenFold is what
// holds the backstop no fixture can reach.
func TestTheFoldIsClosedWhenGitRefuses(t *testing.T) {
	root := scriptTree(t, map[string]string{"internal/x/x.go": "package x\n"})

	out, code := runCIWithEnv(t, root, envWithActions(t), "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	if open, closed := strings.Count(out, "::group::"), strings.Count(out, "::endgroup::"); open != closed {
		t.Errorf("%d ::group:: and %d ::endgroup:::\n%s", open, closed, out)
	}
}

func TestCIRefusesAnUnknownCheck(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}

	out, code := runCI(t, root, "flmt")
	// 2 is the repository's usage exit code, and it is what tells a typo in a
	// workflow file apart from a check that ran and found something.
	if code != 2 {
		t.Fatalf("exit %d, want 2\n%s", code, out)
	}
	if !strings.Contains(out, "unknown check: flmt") {
		t.Errorf("stderr does not name the check it did not recognize:\n%s", out)
	}
}

// The `cites` check has two shapes and they fail for different reasons, so both
// are exercised here. Its fixtures are written into a THROWAWAY repository
// rather than asserted against this one, for a reason worth stating: the only
// way to prove the check fires is to hand it a comment that trips it, and a
// comment that trips it cannot live in a tree the check runs over. That is also
// why scripts/ci_sh_test.go is one of the three paths ci.sh excludes from its
// own pathspec.

// The SPELLED shape, which is what the check has always caught.
func TestCICitesReportsASectionReference(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// The rule is in \u00a77.2 of the design document.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — a section reference is the original thing this check is for\n%s", code, out)
	}
	if !strings.Contains(out, filepath.Join("internal", "x", "x.go")) {
		t.Errorf("stderr does not name the offending file:\n%s", out)
	}
}

// A published standard is allowed to be cited by section, and the `grep -v` arm
// is the only thing that keeps the previous test's pattern from failing it.
func TestCICitesAllowsAnRFCSectionReference(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// The loopback rule is RFC 8252 \u00a77.3.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — CONTRIBUTING.md permits a citation to a published standard\n%s", code, out)
	}
}

// The RFC exemption was one exact spelling applied to a whole LINE, and it was
// wrong in both directions. These are the two directions.
//
// FALSE POSITIVES FIRST. A comma after the number, and the section written
// before the number, are how people actually write a citation, and both failed
// a rule CONTRIBUTING.md permits. A gate that fails on correct prose is a gate
// somebody switches off -- and the shape it pushes them toward, `RFC 6749
// Section 6`, has no section symbol at all and is invisible to this check in
// both directions, so the false positive costs more than the line it fails.
func TestCICitesAllowsAnRFCSectionWrittenWithAComma(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// See RFC 6749, \u00a76 for the flow.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — a comma is punctuation, not a different kind of citation\n%s", code, out)
	}
}

func TestCICitesAllowsASectionWrittenBeforeItsRFC(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// See \u00a76 of RFC 6749.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — the section still names an RFC, in the other order\n%s", code, out)
	}
}

// A RANGE, and the reason it is here is portability rather than English. The
// section symbol is two bytes, so `§+` reads as one \302 followed by
// repeated \247 on a byte-oriented engine and matches only the first half of
// `§§`. That is the same trap the two `\b` notes in ci.sh record: the
// arm fails on one platform while every test expecting a MISS still misses. An
// alternation of two literals is what this rules in.
// It is asked on BOTH ENGINES, and the byte-oriented one is the leg with teeth.
// Under a UTF-8 locale GNU sed reads § as one character, so `§+` matches `§§`
// perfectly well and this fixture passes with the quantifier in place --
// measured: swapping the alternation back to `§+` leaves the whole suite green
// under en_US.UTF-8 and reddens this test. A fixture that only ever ran in the
// developer's locale would pin nothing, which is the same shape as the macOS
// `\b` trap two comments in ci.sh already record: right on the leg you look at,
// silently off on the other.
//
// This is the C leg, and it needs no LC_ALL of its own now that ciEnv pins one:
// C is the locale every test in this file runs under, which is the byte-
// oriented engine made reachable from any machine. The other leg is the test
// below, and it is a separate function rather than a second call here so that a
// machine with no UTF-8 locale SKIPS visibly instead of quietly asking the same
// question twice.
func TestCICitesAllowsAnRFCSectionRange(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// Both apply: RFC 6749 \u00a7\u00a76 and 7.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — a range is one citation written once, and under the pinned C "+
			"locale the section symbol is two bytes, so a quantifier binds only the second of "+
			"them\n%s", code, out)
	}
}

// The same fixture on the CHARACTER-oriented engine, and it is here because
// pinning the environment would otherwise have deleted that engine from the
// suite: an environment carrying no locale is the C locale, so every leg would
// have become the leg above.
//
// It has no teeth against `§+` — that quantifier is correct here, which is the
// whole point of the paragraph above — and it has teeth against the other
// direction, a pattern that works on bytes and not on characters. The risk runs
// both ways and only one way was covered.
func TestCICitesAllowsAnRFCSectionRangeUnderAUTF8Locale(t *testing.T) {
	locale := utf8Locale()
	if locale == "" {
		t.Skip("this machine lists no UTF-8 locale, so the character-oriented engine cannot be reached from it")
	}
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// Both apply: RFC 6749 \u00a7\u00a76 and 7.\nfunc X() {}\n",
	})

	out, code := runCIWithEnv(t, root, append(ciEnv(t), "LC_ALL="+locale), "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 under %s — the section symbol is one character there\n%s",
			code, locale, out)
	}
}

// utf8Locale is a UTF-8 locale this machine actually has, or "" when it lists
// none.
//
// Asked of `locale -a` rather than written down, because no single name is
// portable: C.UTF-8 exists on the Linux runner and not on macOS, glibc lists
// en_US.UTF-8 as `en_US.utf8`, and a name the machine does not have does not
// fail loudly — setlocale falls back to C and writes a warning into the very
// output these tests grep, so the leg would have gone on passing while
// measuring the engine it was written to avoid.
//
// The preference order only makes the choice deterministic across machines that
// have several; any UTF-8 locale reaches the character-oriented engine.
func utf8Locale() string {
	out, err := exec.Command("locale", "-a").Output()
	if err != nil {
		return ""
	}
	var have []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".utf-8") || strings.HasSuffix(lower, ".utf8") {
			have = append(have, name)
		}
	}
	for _, want := range []string{"c.utf-8", "c.utf8", "en_us.utf-8", "en_us.utf8"} {
		for _, name := range have {
			if strings.ToLower(name) == want {
				return name
			}
		}
	}
	if len(have) > 0 {
		return have[0]
	}
	return ""
}

// THE OTHER DIRECTION, and it is the one the old comment called a deliberate
// trade. The drop was per-LINE and ran after the whole spelled arm, so `RFC <n>
// §` anywhere on a line exempted that line from ALL THREE literals. Either
// half of this fixture alone fails; together they used to pass.
//
// This is what rules out the shape the fix is easy to get wrong: an exemption
// that still decides per line. Subtracting the citation and re-testing the
// remainder is the only implementation that leaves this red-when-broken.
func TestCICitesDoesNotLetAnRFCCitationLaunderASectionOnTheSameLine(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// See RFC 6749 \u00a76, and also \u00a77.2 of the design document.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — the second section names no standard, and the first does not vouch for it\n%s", code, out)
	}
}

// The same laundering, for the two literals that are not the section symbol.
// They were laundered too, which is the part the report of this bug understated:
// the filter dropped the line, and the line carried all three patterns.
func TestCICitesDoesNotLetAnRFCCitationLaunderANamedWorkItem(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// This is what task 47 asked for; the loopback rule is RFC 8252 \u00a77.3.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — an RFC citation on the line says nothing about \"task 47\" on the same line\n%s", code, out)
	}
}

// The exemption validates a SHAPE, and this is the edge where a shape stops
// being a citation. `RFC 9999 § anything at all` has a section symbol
// introducing prose rather than a section, and accepting it would make the
// exemption a way of writing `§` wherever you like.
//
// ONE section symbol on the line, and that is the whole design of this fixture.
// It read `RFC 9999 § anything at all, even §7.2 of the internal design doc.`
// first and it was BLIND: widening the accepted section body to anything still
// left the trailing `§7.2` on the line, so the check exited 1 either way and
// the assertion could not tell which symbol it was failing on. Measured --
// that mutation left the whole suite green. With the clause alone the line
// flips from 1 to 0 the moment the digit requirement goes.
//
// The RFC number itself is deliberately not checked for existence -- 9999 is
// unassigned and still exempt when it introduces a numbered section -- because
// whether a number was ever issued is not a question a grep can ask.
func TestCICitesRefusesASectionSymbolThatIntroducesNoSection(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// RFC 9999 \u00a7 anything at all.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — a section symbol followed by prose is not a citation\n%s", code, out)
	}
}

// THE FILE SET. It was five extensions, which is narrower than the rule
// CONTRIBUTING.md states and than this check's own name claims; thirty-seven
// tracked files sat outside it. Measured when it was widened, each of these
// carried a violation and the check exited 0.
//
// A file with NO extension is the case a pathspec of extensions cannot reach at
// all, so it is the one worth a test: `ccdad-entrypoint` is a real shell script
// in this repository and the .cmd fixtures, the .js generator, the licence text
// and the plugin manifests are the same question asked again.
func TestCICitesSearchesAFileWithNoExtension(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"ccdad-entrypoint": "#!/bin/sh\n# The layout is in \u00a77.2 of the design document.\nexec \"$@\"\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — a shell script is not exempt for having no dot in its name\n%s", code, out)
	}
	if !strings.Contains(out, "ccdad-entrypoint") {
		t.Errorf("stderr does not name the file:\n%s", out)
	}
}

// A BINARY file, and this is what makes the widened pathspec safe rather than
// merely wider. `git grep` prints `Binary file X matches` for one -- a line with
// no `file:line:` prefix and no offending text -- and the gate failed on
// assets/ccdaddy.png with a message nobody could act on. `-I` is the fix;
// excluding the one path would have worked until the next image.
//
// THE NAME MATTERS AND THE FIXTURE IS BUILT AROUND IT. An earlier version of
// this test used `assets/logo.png` and was blind: the spelled arm re-tests each
// line after subtracting the accepted citations, and `Binary file
// assets/logo.png matches` carries no `§`, no "the brief" and no "task n", so
// it is dropped there whether `-I` is passed or not. The notice quotes the
// PATH, so a path that carries a literal is what puts it back in scope --
// measured, this fixture exits 1 with the notice as its only finding when `-I`
// is removed, and 0 with it.
func TestCICitesDoesNotReportABinaryFile(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"assets/task 47 diagram.png": "PNG\x00\x00\u00a7 7.2\x00",
		"internal/x/x.go":            "package x\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — bytes inside an image are not a citation, and \"Binary file X matches\" is not a report\n%s", code, out)
	}
	if strings.Contains(out, "Binary file") {
		t.Errorf("git's binary notice reached the findings:\n%s", out)
	}
}

// The three exclusions, which no test reached before and which are load-bearing
// in production: each of those files has to contain the very strings this check
// fails on. Measured, dropping `:!CONTRIBUTING.md` fails the real tree on four
// of its own lines -- and left the whole suite green, because every fixture
// repository is built fresh and has no CONTRIBUTING.md in it.
//
// So this fixture puts one there.
func TestCICitesDoesNotFailTheDocumentThatStatesTheRule(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"CONTRIBUTING.md": "# Contributing\n\nThe gate fails on `\u00a7`, on \"the brief\", and on \"task *n*\",\n" +
			"and on `see docs/plans/2026-08-25-a-thing.md`.\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — a gate that cannot state what it forbids is worse than the exclusion\n%s", code, out)
	}
}

// UNTRACKED. The file set came from the index alone, so a new file carrying a
// citation gave exit 0 and the SAME file after `git add -N` gave 1 -- which made
// running this before staging worthless. check_fmt has never had that hole; it
// reads `--cached --others --exclude-standard`, and this check does now too.
func TestCICitesSearchesAFileThatIsNotYetStaged(t *testing.T) {
	root := throwawayRepoThenWrite(t,
		map[string]string{"internal/x/x.go": "package x\n"},
		map[string]string{"internal/y/y.go": "package y\n\n// The rule is in \u00a77.2 of the design document.\nfunc Y() {}\n"},
	)

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — the file is on disk and this check is what you run before staging it\n%s", code, out)
	}
}

// The bound on the above, and the reason it is not simply "read the working
// tree": a scratch directory somebody has told git to ignore is not this
// repository's content, and failing on one is how a gate gets switched off.
func TestCICitesIgnoresWhatGitIgnores(t *testing.T) {
	root := throwawayRepoThenWrite(t,
		map[string]string{".gitignore": "scratch/\n", "internal/x/x.go": "package x\n"},
		map[string]string{"scratch/notes.go": "package scratch\n\n// The rule is in \u00a77.2 of the design document.\nfunc S() {}\n"},
	)

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — .gitignore is where a working-tree gate stops\n%s", code, out)
	}
}

// The other half of widening the universe: a pointer RESOLVES against it too.
// Reading the index for the tracked list while searching the working tree would
// report `see notes-for-me.md` as unreachable for the minute between writing
// that file and staging it, which is a gate failing on correct work.
func TestCICitesResolvesAPointerToAFileThatIsNotYetStaged(t *testing.T) {
	root := throwawayRepoThenWrite(t,
		map[string]string{"internal/x/x.go": "package x\n"},
		map[string]string{
			"notes-for-me.md": "# a document that is really here\n",
			"internal/y/y.go": "package y\n\n// The measurement is not repeated here: see notes-for-me.md for it.\nfunc Y() {}\n",
		},
	)

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — the file it points at is right there on disk\n%s", code, out)
	}
}

// A PATTERN THE PLATFORM'S REGEX LIBRARY REFUSES, which is the fail-open this
// check is most exposed to and the one that would be hardest to notice.
//
// `git grep` exits 1 for "no matches" and 128 for "I could not read that
// pattern", and `if hits=$(git grep …)` read both as falsy — so an arm whose
// pattern one platform rejects checked nothing and the gate reported a clean
// tree, exit 0. That is not hypothetical here: two comments in ci.sh record
// macOS's git reading `\b` as a literal `b`, which turned a whole arm into a
// no-op on that leg while every test expecting a MISS still missed.
//
// The mutation is applied to a COPY of the script rather than asserted against
// the real patterns, because the real ones have to stay valid — the point is
// what happens to the check when a pattern is not, whatever the reason.
func TestCICitesFailsWhenGitRefusesAPattern(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// See some-other-notes-file for it.\nfunc X() {}\n",
	})

	script := filepath.Join(root, "scripts", "ci.sh")
	raw, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	// An unbalanced `{`, which every ERE implementation rejects.
	broken := strings.Replace(string(raw), "(-[A-Za-z0-9]+){2,}'", "(-[A-Za-z0-9]+){2,'", 1)
	if broken == string(raw) {
		t.Fatal("the pointer pattern is not where this test expects it; re-locate it by content")
	}
	if err := os.WriteFile(script, []byte(broken), 0o755); err != nil {
		t.Fatal(err)
	}

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — that arm read nothing, and reporting a clean tree for a "+
			"pattern git would not accept is how an arm goes quiet on one platform forever\n%s", code, out)
	}
	if strings.Contains(out, "no line cites") {
		t.Errorf("the check called the tree clean after git refused its pattern:\n%s", out)
	}
}

// The POINTED shape: a document named rather than spelled. This is the one that
// got through — internal/cclink/keychain.go pointed at a file in one person's
// notes directory, and no literal in the check matched a bare name.
func TestCICitesReportsADocumentNamedRatherThanSpelled(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// The measurement is not repeated here: see some-other-notes-file for it.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — a bare document name is unreachable to every other reader\n%s", code, out)
	}
	if !strings.Contains(out, "some-other-notes-file") {
		t.Errorf("stderr does not quote the pointer it objected to:\n%s", out)
	}
}

// A pointer whose target IS in the tree is allowed, because that is the rule
// rather than an exception to it. Without this arm the check would fail the
// first time somebody wrote "see" in front of a real file name, and a check
// that fails on correct code is a check somebody switches off.
func TestCICitesAllowsAPointerToAFileThisRepositoryHas(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"scripts/gen-cmd-shim-fixtures.js": "// a fixture generator\n",
		"internal/x/x.go":                  "package x\n\n// Regenerate them: see gen-cmd-shim-fixtures for the template.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — the file it points at is right there in the tree\n%s", code, out)
	}
}

// Hyphenated English is not a document name, and this is the test that rules
// out the implementation the pointed shape is easy to reach for: keying on the
// slug ALONE. Measured on this repository when the pattern was written, the
// bare-slug form matched 29 lines and nearly all of them read like these.
func TestCICitesDoesNotReportOrdinaryHyphenatedEnglish(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// writeMerged is the locked read-decide-merge-write all of these perform,\n" +
			"// and its sibling-temp-file-then-rename is what produces the mtime.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — this tree writes English that way everywhere\n%s", code, out)
	}
}

// THE ARITY, from below. `{2,}` — two hyphen groups, three segments — is the
// number that keeps the pointer arm from reporting this repository's own prose,
// and until this fixture existed nothing held it: `{2,}` moved to `{1,}` or to
// `{3,}` left every test in this file green while changing what the gate
// accepts in both directions.
//
// The test that looks like it should hold this cannot.
// TestCICitesDoesNotReportOrdinaryHyphenatedEnglish has no pointing phrase in
// front of its slugs, so no arity change can reach it at all — it constrains the
// phrase requirement and nothing else.
//
// This one reddens if the floor is raised to three groups.
func TestCICitesReportsANameWithTwoHyphenGroups(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// See keychain-notes-here for the measurement.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — three segments is the shortest thing this gate calls a document name\n%s", code, out)
	}
	if !strings.Contains(out, "keychain-notes-here") {
		t.Errorf("stderr does not quote the pointer it objected to:\n%s", out)
	}
}

// THE ARITY, from above, and this is the direction with a measured price. One
// hyphen group is not a document name, it is ordinary English behind a pointing
// phrase: relaxing the floor to `{1,}` fails seven correct lines of this tree,
// six of them behind "per" — `per rate-limit window`, `per five-hour cycle`,
// `per sub-key`, `per warm-up`.
//
// So this fixture is written the way those lines are written, and the cost of
// the floor is stated rather than hidden: `see keychain-notes` is a miss, and it
// is a miss this gate takes knowingly rather than fail seven lines that are
// right.
func TestCICitesDoesNotReportANameWithOneHyphenGroup(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// One threshold per rate-limit window, and no more.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — this tree writes English that way in seven places\n%s", code, out)
	}
}

// Case. `see Keychain-Notes-Here` walked straight through, because both
// character classes were `[a-z0-9]` — and a document named the way a person
// capitalises a title is exactly as unreachable as one in lower case.
//
// Adding `A-Z` was free and that is why it was taken: measured over the widened
// file set the pattern matches nothing with it that it did not match without it.
func TestCICitesReportsAnUppercaseName(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// See Keychain-Notes-Here for the measurement.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — a capital letter does not make a private note reachable\n%s", code, out)
	}
}

// The bound the case widening deliberately stops at, and the measurement behind
// it: adding `_` to the classes reports `see keychain_security_test.go.`, which
// is a CORRECT citation of a file this repository tracks — the slug arm resolves
// its target without the trailing-punctuation strip the docpath arm has, so the
// full stop comes along and resolves to nothing. One false positive is the whole
// price of a shape nobody here writes, so it is not paid.
func TestCICitesDoesNotReportAnUnderscoredName(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// See keychain_notes_here for the measurement.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — underscores are how this tree names Go files, not documents\n%s", code, out)
	}
}

// The POINTED shape written as a PATH, which is how a plan is actually cited
// and which the slug pattern cannot see: it keys on `[A-Za-z0-9]+(-[A-Za-z0-9]+){2,}`
// immediately after the pointing phrase, and `docs/` ends that token at the
// slash before a single hyphen group has matched.
//
// `docs/` is the case that matters. It is in .git/info/exclude and no commit in
// this repository has ever contained a file under it, so every reference to one
// resolves for exactly the person whose machine it is on -- the thing this
// check exists to stop.
func TestCICitesReportsAPlanCitedByPath(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// The ordering is not re-argued here: " +
			"see docs/plans/2026-08-25-self-upgrade.md for why it is that way.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — a plan cited by path is unreachable to every other reader\n%s", code, out)
	}
	if !strings.Contains(out, "docs/plans/2026-08-25-self-upgrade.md") {
		t.Errorf("stderr does not quote the path it objected to:\n%s", out)
	}
}

// The same rule as the slug arm, in the direction that keeps the check usable:
// a path this repository HAS is a pointer that resolves for everyone.
func TestCICitesAllowsAPathThisRepositoryHas(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"docs/plans/2026-08-25-self-upgrade.md": "# a plan that is really here\n",
		"internal/x/x.go": "package x\n\n// The ordering is not re-argued here: " +
			"see docs/plans/2026-08-25-self-upgrade.md for it.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — the document it points at is right there in the tree\n%s", code, out)
	}
}

// A `docs/` pointer that ends a sentence, and this is the ONE shape the
// trailing-punctuation strip exists for. Measured: `docs/[A-Za-z0-9._/-]+` has
// `.` inside the class, so `see docs/plans/a-thing.md.` yields the target
// `docs/plans/a-thing.md.` with the full stop attached, which resolves to
// nothing and fails a correct line.
//
// A first version of this test used `See SECURITY.md.` -- the one such line in
// this repository -- and it WAS blind at the time, because the `.md` half of
// the pattern then ended at `.md` and the full stop was never inside the match.
// Deleting the strip left the whole suite green.
//
// That is history rather than a fact about the pattern, and it stopped being
// true when the `.md` half gained its `([^A-Za-z0-9]|$)` boundary: a boundary
// CONSUMES what it matches, so the stop is inside the match now and the strip
// is load-bearing for both halves. Measured -- deleting the strip today fails
// the next test rather than leaving it green. The `docs/` fixture stays because
// it is the one that was never blind.
func TestCICitesAllowsADocumentPointerThatEndsASentence(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"docs/plans/2026-08-25-a-thing.md": "# a plan that is really here\n",
		"internal/x/x.go": "package x\n\n// The order is set out in " +
			"see docs/plans/2026-08-25-a-thing.md.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — the trailing full stop is punctuation, not part of the path\n%s", code, out)
	}
}

// The shape the repository actually carries, in `.github/ISSUE_TEMPLATE`. It
// proves that a bare document name ending a sentence resolves -- and it does so
// through the trailing strip, not around it. The comment here used to claim the
// extension half could not swallow a full stop; the `([^A-Za-z0-9]|$)` boundary
// consumes one, so it can and does, and this test is red without the strip.
func TestCICitesAllowsAMarkdownPointerThatEndsASentence(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"SECURITY.md":     "# how to report a vulnerability\n",
		"internal/x/x.go": "package x\n\n// Report it privately instead: see SECURITY.md.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — the full stop is outside the match, not part of the name\n%s", code, out)
	}
}

// The `docs/` half of the pattern, alone. A plan is as often named without its
// extension as with it, and the `.md` half cannot see that spelling.
func TestCICitesReportsAPlanUnderDocsCitedWithoutAnExtension(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// The twelve tasks are listed there: " +
			"see docs/plans/2026-08-25-self-upgrade-part3 for the order.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — nothing under docs/ has ever been in this repository\n%s", code, out)
	}
}

// The `.md` half, alone: a document outside docs/ that this repository does not
// have. Without this the extension arm could be deleted and only the docs/
// spelling would still be caught.
func TestCICitesReportsAMarkdownDocumentThisRepositoryDoesNotHave(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// The numbers are not repeated here: " +
			"see notes/2026-08-25-measurement.md for them.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — that document is on somebody's machine, not in the tree\n%s", code, out)
	}
}

// The scope this arm deliberately does not take, and the measurement behind it:
// a pattern that flagged any unresolved path matched six lines in this tree and
// four were wrong -- `os/types_windows.go` is the Go standard library,
// `tools/call` is a method name on the wire, `internal/cli` is a directory, and
// one was a filename with the sentence's full stop stuck to it. This check is
// about DOCUMENTS. Whether a comment names a code path correctly is a different
// question with a different answer, and it is not this one.
func TestCICitesDoesNotReportACodePathThatIsNotADocument(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// The execute bit is not reported, per os/types_windows.go -- and the\n" +
			"// middleware runs exactly once per tools/call on the wire.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — neither target is a document this repository could contain\n%s", code, out)
	}
}

// A document this repository HAS, named the way a person names it. Resolving a
// bare name only at the repository root failed this: `.github/PULL_REQUEST_-
// TEMPLATE.md` is tracked, `see PULL_REQUEST_TEMPLATE.md` was reported as
// "something no reader outside this machine has", and CONTRIBUTING.md promises
// the opposite. The slug arm has always resolved a bare name this way.
func TestCICitesAllowsADocumentTrackedOutsideTheRepositoryRoot(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		".github/PULL_REQUEST_TEMPLATE.md": "# what a pull request should say\n",
		"internal/x/x.go": "package x\n\n// The checklist is not repeated here: " +
			"see PULL_REQUEST_TEMPLATE.md for it.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — the document is tracked, in .github/\n%s", code, out)
	}
}

// A PATH is matched whole, and this is the implementation that shape rules out:
// resolving by suffix. `docs/x.md` must not be answered by an unrelated
// `vendor/docs/x.md` that happens to end the same way.
func TestCICitesResolvesADocumentPathWholeAndNotBySuffix(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"vendor/docs/plans/a-thing.md": "# somebody else's copy\n",
		"internal/x/x.go": "package x\n\n// The order is set out elsewhere: " +
			"see docs/plans/a-thing.md for it.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — this repository has no docs/plans/a-thing.md\n%s", code, out)
	}
}

// The pointing phrase is required, not the target shape alone. This is the
// docpath arm's analogue of the ordinary-hyphenated-English test, and without
// it the phrase could be dropped from the pattern with every other test green
// while two correct lines in this repository went red.
func TestCICitesDoesNotReportADocumentPathWithNoPointingPhrase(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// The generator writes docs/plans/a-thing.md " +
			"and README.md into the output directory.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — naming a path is not pointing a reader at it\n%s", code, out)
	}
}

// `.mdx` is not `.md`. Without a boundary after the extension the pattern
// matches the prefix, and a correct citation of a tracked file is reported
// under a name nobody wrote.
func TestCICitesDoesNotTruncateALongerExtensionToMarkdown(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"notes/guide.mdx": "# a document with a longer extension\n",
		"internal/x/x.go": "package x\n\n// The walkthrough lives elsewhere: " +
			"see notes/guide.mdx for it.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 0 {
		t.Fatalf("exit %d, want 0 — the file is tracked and its name is not guide.md\n%s", code, out)
	}
}

// One line is one problem. A hyphenated document name ending in `.md` trips the
// slug arm, which stops before the extension, AND the path arm -- and the same
// file:line printed twice reads as two things to go and fix.
func TestCICitesPrintsALineOnceWhenBothArmsFlagIt(t *testing.T) {
	root := throwawayRepo(t, map[string]string{
		"internal/x/x.go": "package x\n\n// The measurement is not repeated here: " +
			"see some-other-notes-file.md for it.\nfunc X() {}\n",
	})

	out, code := runCI(t, root, "cites")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	if n := strings.Count(out, "some-other-notes-file.md for it."); n != 1 {
		t.Errorf("the offending line is printed %d times, want 1:\n%s", n, out)
	}
}

// runCIWithEnv is runCI with the child's environment named explicitly. The
// plugin check branches on whether `claude` is on PATH, and a test that cannot
// control PATH describes the developer's machine rather than the check.
func runCIWithEnv(t *testing.T, root string, env []string, args ...string) (string, int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("ci.sh is invoked by path here, which Git Bash cannot resolve")
	}
	script := filepath.Join(root, "scripts", "ci.sh")
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running ci.sh %v: %v\n%s", args, err, out)
		}
		code = exit.ExitCode()
	}
	return string(out), code
}

// A PATH with no claude on it. The installer puts claude under $HOME/.local/bin
// and never in /usr/bin or /bin, so this is the deterministic way to describe a
// machine that has not got one -- which is most developers' machines and is the
// case the skip exists for.
func envWithoutClaude(t *testing.T, extra ...string) []string {
	t.Helper()
	return append([]string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}, extra...)
}

// A fake claude that records its argv and answers the smoke question, so a test
// can assert that the check LOOKED at something. A check that has quietly
// stopped looking still reports success, which is what this whole file is for.
func fakeClaude(t *testing.T, log string, exitCode int) []string {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >>'" + log + "'\n" +
		// The config directory the check handed us, so a test can check it was
		// a throwaway one AND that the throwaway was thrown away.
		"if [ -n \"${CLAUDE_CONFIG_DIR:-}\" ]; then printf 'CFG %s\\n' \"$CLAUDE_CONFIG_DIR\" >>'" + log + "'; fi\n" +
		"case \"$1 $2\" in 'plugin details') echo 'MCP servers (1)  ccdad' ;; esac\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + t.TempDir()}
}

// A fake claude that refuses ONE subcommand and answers the rest normally, so a
// test can ask whether the check reads THAT call's status. `fakeClaude` takes a
// single code and fails everything, which cannot tell a check that reads every
// status from one that reads only the first.
//
// failWhen is a shell `case` pattern matched against the whole argv.
func fakeClaudeRefusing(t *testing.T, log, failWhen string) []string {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >>'" + log + "'\n" +
		"if [ -n \"${CLAUDE_CONFIG_DIR:-}\" ]; then printf 'CFG %s\\n' \"$CLAUDE_CONFIG_DIR\" >>'" + log + "'; fi\n" +
		"case \"$*\" in " + failWhen + ") exit 1 ;; esac\n" +
		"case \"$1 $2\" in 'plugin details') echo 'MCP servers (1)  ccdad' ;; esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + t.TempDir()}
}

func TestCIPluginSkipsLoudlyWhenClaudeIsNotInstalled(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	out, code := runCIWithEnv(t, root, envWithoutClaude(t), "plugin")
	if code != 0 {
		t.Fatalf("exit %d, want 0 -- a developer without claude still has to be able to run ci.sh\n%s", code, out)
	}
	if !strings.Contains(out, "claude is not installed") {
		t.Errorf("the skip is silent, which is the one result that looks like coverage and is not:\n%s", out)
	}
}

func TestCIPluginFailsWhenClaudeIsRequiredAndAbsent(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	out, code := runCIWithEnv(t, root, envWithoutClaude(t, "CCDAD_REQUIRE_CLAUDE=1"), "plugin")
	if code != 1 {
		t.Fatalf("exit %d, want 1 -- on the leg that just installed claude, a skip means the install "+
			"step broke and the manifests went back to being validated by nobody\n%s", code, out)
	}
	if !strings.Contains(out, "CCDAD_REQUIRE_CLAUDE") {
		t.Errorf("the failure does not name the variable that caused it:\n%s", out)
	}
}

func TestCIPluginValidatesBothTheMarketplaceAndThePluginDirectory(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "argv")
	out, code := runCIWithEnv(t, root, fakeClaude(t, log, 0), "plugin")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	recorded, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the check never invoked claude at all: %v", err)
	}
	for _, want := range []string{"plugin validate --strict .", "plugin validate --strict plugins"} {
		if !strings.Contains(string(recorded), want) {
			t.Errorf("claude was never asked %q; a source naming a directory that is not there "+
				"passes the marketplace validate silently, with nothing validated:\n%s", want, recorded)
		}
	}
	// And the smoke, which is the only leg that proves Claude Code READ the
	// file the manifest names rather than merely liking the manifest's shape.
	for _, want := range []string{"plugin marketplace add", "plugin install ccdad@ccdaddy", "plugin details ccdad@ccdaddy"} {
		if !strings.Contains(string(recorded), want) {
			t.Errorf("the smoke leg never ran %q:\n%s", want, recorded)
		}
	}
}

func TestCIPluginFailsWhenTheValidatorDoes(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "argv")
	out, code := runCIWithEnv(t, root, fakeClaude(t, log, 1), "plugin")
	if code == 0 {
		t.Fatalf("exit 0 with a validator that refused; the check swallows its own answer\n%s", out)
	}
}

// Each `claude plugin validate` call's STATUS is read, and there are two of
// them. `TestCIPluginValidatesBothTheMarketplaceAndThePluginDirectory` proves
// only that both were INVOKED -- it reads the argv log -- and
// TestCIPluginFailsWhenTheValidatorDoes uses a fake that refuses everything, so
// a check that read the first status and dropped the second would pass both.
// Measured: `|| true` on either validate line left the whole suite green.
//
// The second call is the guard for a mistyped source. A source naming a
// directory that is not there passes the marketplace validate silently, with
// nothing validated at all -- so the call whose status is easiest to lose is the
// one that matters most.
func TestCIPluginFailsWhenEitherValidateRefuses(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		failWhen string
	}{
		{"the marketplace", "'plugin validate --strict .'"},
		{"the plugin directory", "'plugin validate --strict plugins'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "argv")
			out, code := runCIWithEnv(t, root, fakeClaudeRefusing(t, log, tc.failWhen), "plugin")
			if code != 1 {
				t.Fatalf("exit %d, want 1 — the validator refused %s and the check went on\n%s", code, tc.name, out)
			}
		})
	}
}

// WHICH non-zero, and this is the test that holds the exit-code convention for
// every check that shells out. `claude` is somebody else's binary and its status
// is theirs to change: measured before this, fakes exiting 2, 3 and 7 came
// straight out of ci.sh as 2, 3 and 7. A validator that started using 2 for its
// own purposes would have made this script report "you typed the check name
// wrong" on every push.
//
// The test above accepts any non-zero code, so all three of those pass it. This
// one is the difference between "the check noticed" and "the check answered in
// this script's own vocabulary", and it is the only test anywhere that reaches
// `run`.
func TestCIPluginReportsOneWhenTheValidatorExitsTwo(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "argv")
	out, code := runCIWithEnv(t, root, fakeClaude(t, log, 2), "plugin")
	if code != 1 {
		t.Fatalf("exit %d, want 1 — the check ran; 2 is reserved for a check name this script does not have\n%s", code, out)
	}
	if !strings.Contains(out, "exited 2") {
		t.Errorf("stderr does not name the code the validator actually used, which is the line that "+
			"tells a reader this was the tool and not a typo:\n%s", out)
	}
}

// The discriminator itself. An inline server object in plugin.json validates,
// installs, runs -- and reports MCP servers (0), because Claude Code never
// counted it. That number is the only observable difference between the form
// this repository ships and the form it deliberately does not, so a check that
// stopped reading it would pass on the wrong manifest.
func TestCIPluginFailsWhenTheInstalledPluginDeclaresNoServer(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	body := "#!/bin/sh\n" +
		"case \"$1 $2\" in 'plugin details') echo 'MCP servers (0)' ;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + t.TempDir()}

	out, code := runCIWithEnv(t, root, env, "plugin")
	if code == 0 {
		t.Fatalf("exit 0 for a plugin declaring no MCP server; the smoke leg is not reading its answer\n%s", out)
	}
	if !strings.Contains(out, "declares no MCP server") {
		t.Errorf("the failure does not say what was wrong:\n%s", out)
	}
}

// The smoke leg installs a plugin, and it must do that somewhere that is not
// the developer's own Claude Code and is not left behind afterwards. Both
// halves are asserted here because the second is the one nothing else would
// notice: a leaked mktemp directory is invisible until a machine runs out of
// them.
//
// The cleanup it depends on is shared with the cgo check through ONE `trap …
// EXIT` handler, because bash keeps one handler per signal and a second trap
// would silently replace the first. That is exactly the kind of arrangement
// that works until somebody tidies it, so it is pinned rather than commented.
func TestThePluginCheckInstallsIntoAThrowawayConfigDirectoryAndRemovesIt(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "argv")
	out, code := runCIWithEnv(t, root, fakeClaude(t, log, 0), "plugin")
	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	recorded, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}

	// EVERY logged CLAUDE_CONFIG_DIR, not the first. This loop stopped at the
	// first match, and the smoke leg makes three calls: measured, `claude plugin
	// marketplace add` could lose its CLAUDE_CONFIG_DIR and run against the
	// developer's REAL Claude Code while install and details still carried the
	// throwaway, and this test stayed green. Not installing into somebody's own
	// Claude Code is the entire reason this test exists.
	//
	// The fake logs a CFG line only when the variable is set, so a call that
	// lost it shows up as a MISSING line rather than a wrong one -- which is why
	// the count is asserted as well as the value.
	var cfgs []string
	for _, line := range strings.Split(string(recorded), "\n") {
		if after, ok := strings.CutPrefix(line, "CFG "); ok {
			cfgs = append(cfgs, after)
		}
	}
	const smokeCalls = 3 // marketplace add, install, details
	if len(cfgs) != smokeCalls {
		t.Fatalf("%d of the %d smoke invocations carried CLAUDE_CONFIG_DIR; one that does not "+
			"runs against whatever Claude Code the machine already has:\n%s", len(cfgs), smokeCalls, recorded)
	}
	cfg := cfgs[0]
	for _, other := range cfgs[1:] {
		if other != cfg {
			t.Fatalf("the smoke leg used more than one config directory (%q and %q); only the one "+
				"the cleanup trap knows about gets removed:\n%s", cfg, other, recorded)
		}
	}
	if strings.HasPrefix(cfg, root) {
		t.Errorf("the throwaway config directory is inside the repository: %s", cfg)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Errorf("%s survived the run; the cleanup trap did not remove it (stat err: %v)", cfg, err)
	}
}

// Every check the usage block names, `all` runs.
//
// This reads the script instead of executing it, and the reason is arithmetic:
// `all` is gofmt, vet, the whole race suite and six cross-compiles, minutes of
// work to re-prove what the CI jobs prove on every push. What can be checked
// for nothing is the composition -- and a check wired into the dispatch and
// forgotten in `all` runs for nobody who types the default, which is everybody
// running this before a push.
func TestEveryCheckTheUsageNamesIsAlsoRunByAll(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "ci.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)

	allArm, ok := betweenOnce(script, "\tall)\n", "\t\t;;\n")
	if !ok {
		t.Fatal("the `all)` arm is not where this test expects it; re-locate it by content")
	}
	for _, name := range []string{"fmt", "vet", "test", "cgo", "cites", "plugin"} {
		if !strings.Contains(script, "\t"+name+") check_"+name+" ;;") {
			t.Errorf("`%s` is named in the usage block but has no dispatch arm", name)
			continue
		}
		if !strings.Contains(allArm, "check_"+name+"\n") {
			t.Errorf("`all` does not run check_%s; a default run therefore skips it:\n%s", name, allArm)
		}
	}
}

// betweenOnce returns the text between the first open and the next close, and
// reports whether both were found.
func betweenOnce(s, open, close string) (string, bool) {
	i := strings.Index(s, open)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// ONE `trap … EXIT` in the whole script, which is the assertion the comment
// beside `cleanup` asks for and that no behavioural test can make.
//
// bash keeps one handler per signal, so a trap installed by a new check does
// not chain — it REPLACES the one already there, and the check whose temp
// directory the old handler removed silently stops being cleaned up. Measured:
// giving the plugin check its own trap leaves every other test in this file
// green, because a check that installs its own trap does clean up after
// itself. Only counting them sees it.
func TestTheScriptInstallsExactlyOneExitTrap(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "ci.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var traps []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "trap ") && strings.HasSuffix(trimmed, "EXIT") {
			traps = append(traps, trimmed)
		}
	}
	if len(traps) != 1 {
		t.Errorf("ci.sh installs %d EXIT traps (%v); a second one replaces the first rather than "+
			"chaining, so whatever the first cleaned up stops being cleaned up. Add an arm to "+
			"cleanup instead.", len(traps), traps)
	}
}

// The EXIT trap closes a fold that is still open, and this is a TEXT assertion
// for the same reason the one above is: no behavioural test in this file can
// reach it.
//
// That is measured rather than assumed. Deleting `endgroup` from cleanup leaves
// the whole suite green, and so does deleting it from `run` — both of the
// general backstops can be removed at once and nothing notices. What the two
// fold tests actually hold is the INLINE close each check does for itself:
// TestTheFoldIsClosedWhenACheckFails bites through its ordering assertion, which
// only check_fmt's own `endgroup` satisfies, and its counting half is satisfied
// by any one of the three paths.
//
// The backstop is for what no fixture produces: a signal, and a `return` added
// inside a fold by a check nobody has written yet. A guard for the unwritten
// case cannot have a fixture, so it gets this instead of a comment asking to be
// left alone.
func TestTheExitTrapClosesAnOpenFold(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "ci.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body, ok := betweenOnce(string(raw), "cleanup() {\n", "\n}\n")
	if !ok {
		t.Fatal("cleanup() is not where this test expects it; re-locate it by content")
	}
	if !strings.Contains(body, "endgroup") {
		t.Errorf("cleanup() does not close an open fold, so a check that dies inside one — from a "+
			"signal, or from a `return` a future check adds — leaves ::group:: open and "+
			"everything after it swallowed. Nothing else in this file catches that; the fold "+
			"tests are satisfied by the inline closes.\ncleanup() is:\n%s", body)
	}
}
