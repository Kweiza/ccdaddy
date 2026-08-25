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
func runCI(t *testing.T, root string, args ...string) (string, int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// The Windows leg runs this script for real, through the bash that
		// ships with Git. It cannot run it like THIS: exec hands bash an
		// absolute C:\… path, and `dirname` on a backslash path answers ".".
		t.Skip("ci.sh is invoked by path here, which Git Bash cannot resolve")
	}
	script := filepath.Join(root, "scripts", "ci.sh")
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	// Not the repository: the script has to locate its own root, because CI
	// calls it from the workspace root and a developer from anywhere.
	cmd.Dir = t.TempDir()
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

// throwawayRepo is a git repository containing nothing but a copy of ci.sh and
// whatever files the caller asks for. `git init` is enough: `git ls-files
// --cached --others --exclude-standard` answers without a commit, and a commit
// would need an identity this test has no business configuring.
func throwawayRepo(t *testing.T, files map[string]string) string {
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

	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git := exec.Command("git", "init", "--quiet")
	git.Dir = root
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// STAGED, not merely written. `fmt` reads `git ls-files --cached --others`
	// and sees an untracked file; `cites` reads `git grep` and `git ls-files`,
	// and both of those see the INDEX only -- so without this every cites test
	// would pass by searching nothing. Still no commit: `git add` needs no
	// identity, and `git commit` would.
	//
	// It respects .gitignore, which is what keeps TestCIFmtIgnoresWhatGitIgnores
	// describing the same machine it did before.
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

	var cfg string
	for _, line := range strings.Split(string(recorded), "\n") {
		if after, ok := strings.CutPrefix(line, "CFG "); ok {
			cfg = after
			break
		}
	}
	if cfg == "" {
		t.Fatalf("the smoke leg ran with no CLAUDE_CONFIG_DIR, so it installed into whatever "+
			"Claude Code the machine already has:\n%s", recorded)
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
