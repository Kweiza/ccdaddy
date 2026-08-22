package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
