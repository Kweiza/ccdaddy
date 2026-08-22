// Package scripts tests the shell and PowerShell entry points that ship
// alongside the binary. It has no non-test source: the artefacts under test are
// scripts/build-release.sh, install.sh and install.ps1, and the tests exist
// because nothing else in the repository constrains them.
package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The six assets §11.5 ships. Both installers compute these names
// independently, so this list is the contract between three files.
var releaseAssets = []string{
	"ccdad-darwin-amd64",
	"ccdad-darwin-arm64",
	"ccdad-linux-amd64",
	"ccdad-linux-arm64",
	"ccdad-windows-amd64.exe",
	"ccdad-windows-arm64.exe",
}

const (
	stampedVersion = "9.9.9-test"
	stampedCommit  = "0123456789abcdef0123456789abcdef01234567"
)

// The line shape both installers extract with: sixty-four lowercase hex, TWO
// spaces, then the asset name and nothing else. \S rather than . is deliberate
// — it makes a CRLF sums file, which .gitattributes exists to prevent, fail
// here rather than in a user's shell.
var sumsLine = regexp.MustCompile(`^([0-9a-f]{64})  (\S+)$`)

// buildRelease runs scripts/build-release.sh into a fresh directory and returns
// it. The directory is pre-seeded with a stale asset and a stale sums file,
// because a dist/ carried between runs is the normal case on a developer's
// machine and on a re-run of a release job.
func buildRelease(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("cross-compiles six targets")
	}
	script, err := filepath.Abs("build-release.sh")
	if err != nil {
		t.Fatalf("resolving build-release.sh: %v", err)
	}
	dist := t.TempDir()
	for name, body := range map[string]string{
		"ccdad-freebsd-amd64": "a target this repository does not ship",
		"sha256sums.txt":      "stale contents\n",
	} {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", name, err)
		}
	}

	cmd := exec.Command("bash", script, dist)
	// Deliberately not the repository: the script is invoked from the workspace
	// root by Actions and from anywhere by a human, so it has to locate its own
	// repository root rather than trust the working directory.
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		"VERSION="+stampedVersion,
		"COMMIT="+stampedCommit,
		// A release job has this set; the version must come from VERSION here
		// regardless.
		"GITHUB_REF_NAME=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build-release.sh: %v\n%s", err, out)
	}
	return dist
}

func TestBuildReleaseShipsSixAssetsAndAMatchingSumsFile(t *testing.T) {
	dist := buildRelease(t)

	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatalf("reading %s: %v", dist, err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	slices.Sort(got)
	want := append(slices.Clone(releaseAssets), "sha256sums.txt")
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("dist contains %v, want %v — the asset names are what both installers compute, "+
			"and a leftover from an earlier run must not survive into the sums file", got, want)
	}
}

func TestBuildReleaseSumsFileIsTheFormatTheInstallersParse(t *testing.T) {
	dist := buildRelease(t)

	raw, err := os.ReadFile(filepath.Join(dist, "sha256sums.txt"))
	if err != nil {
		t.Fatalf("reading sha256sums.txt: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")

	listed := make(map[string]string, len(lines))
	for i, line := range lines {
		m := sumsLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("sha256sums.txt line %d = %q, want %q — the installers match "+
				"^[0-9a-f]{64}  ${ASSET}$, so one space or a stray \\r verifies nothing at all",
				i+1, line, "<64 lowercase hex><space><space><asset>")
		}
		listed[m[2]] = m[1]
	}

	names := make([]string, 0, len(listed))
	for name := range listed {
		names = append(names, name)
	}
	slices.Sort(names)
	if !slices.Equal(names, releaseAssets) {
		t.Fatalf("sha256sums.txt lists %v, want %v", names, releaseAssets)
	}

	for name, sum := range listed {
		body, err := os.ReadFile(filepath.Join(dist, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		digest := sha256.Sum256(body)
		if got := hex.EncodeToString(digest[:]); got != sum {
			t.Errorf("%s: sums file says %s, bytes on disk hash to %s", name, sum, got)
		}
	}
}

// The defect this pins is silent. §11.5 shipped `-X main.version=…`, and
// cmd/ccdad/main.go declares no `version` symbol; the linker accepts an
// unmatched -X without a warning, so the build succeeds, the release publishes,
// and every binary reports "dev". Only running one and reading its version
// catches it.
func TestBuildReleaseStampsTheVersionIntoTheBinary(t *testing.T) {
	dist := buildRelease(t)

	asset := "ccdad-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	if !slices.Contains(releaseAssets, asset) {
		t.Skipf("no release asset for %s/%s to run natively", runtime.GOOS, runtime.GOARCH)
	}

	out, err := exec.Command(filepath.Join(dist, asset), "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", asset, err, out)
	}
	got := strings.TrimSpace(string(out))
	want := "ccdad version " + stampedVersion + " (" + stampedCommit[:12] + ")"
	if got != want {
		t.Errorf("%s --version = %q, want %q — a binary that answers \"dev\" here was linked "+
			"against a symbol that does not exist", asset, got, want)
	}
}

// Both stamps are fail-closed for the same reason: the linker and
// buildinfo.String() between them turn a missing stamp into a plausible wrong
// answer rather than an error. A version that cannot be determined would
// silently become "dev", and a commit that cannot be determined would silently
// become whatever tree debug.ReadBuildInfo() saw. Neither is discoverable once
// a tag is public, so the script refuses instead.
func TestBuildReleaseRefusesToBuildWithoutAStampToApply(t *testing.T) {
	// The guards are reached only where git cannot answer, so the script is
	// copied out of the repository to be run.
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Run(); err == nil {
		t.Skipf("%s is inside a git repository, so neither guard can be reached", root)
	}
	if err := os.Mkdir(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatalf("creating scripts dir: %v", err)
	}
	body, err := os.ReadFile("build-release.sh")
	if err != nil {
		t.Fatalf("reading build-release.sh: %v", err)
	}
	script := filepath.Join(root, "scripts", "build-release.sh")
	if err := os.WriteFile(script, body, 0o755); err != nil {
		t.Fatalf("writing %s: %v", script, err)
	}

	for _, tc := range []struct {
		name    string
		version string
		want    string
	}{
		{"no version", "", "cannot determine a version to stamp"},
		{"no commit", stampedVersion, "cannot determine a commit to stamp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", script, t.TempDir())
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(),
				"VERSION="+tc.version,
				"COMMIT=",
				"GITHUB_REF_NAME=",
			)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("build-release.sh exited 0 with nothing to stamp:\n%s", out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("build-release.sh said:\n%s\nwant a message containing %q", out, tc.want)
			}
		})
	}
}

// The sums file is generated by globbing what landed, not by replaying the
// target list, so that a build which loses one target still publishes a sums
// file describing its own assets rather than one naming a binary nobody can
// download. That difference is invisible on a clean build — both spellings
// produce identical output — so it can only be pinned by making a target fail.
func TestBuildReleaseStillDescribesItselfWhenATargetFails(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles five targets")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the shim below is an extensionless shebang script, which only the bash layer resolves")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("locating go: %v", err)
	}
	// A go(1) that refuses exactly one target and is otherwise the real thing.
	shimDir := t.TempDir()
	shim := "#!/usr/bin/env bash\n" +
		"if [ \"${GOOS:-}\" = darwin ] && [ \"${GOARCH:-}\" = arm64 ]; then\n" +
		"\techo 'shim: this target is broken' >&2\n" +
		"\texit 1\n" +
		"fi\n" +
		"exec " + goBin + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "go"), []byte(shim), 0o755); err != nil {
		t.Fatalf("writing the go shim: %v", err)
	}

	script, err := filepath.Abs("build-release.sh")
	if err != nil {
		t.Fatalf("resolving build-release.sh: %v", err)
	}
	dist := t.TempDir()
	cmd := exec.Command("bash", script, dist)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		"VERSION="+stampedVersion,
		"COMMIT="+stampedCommit,
		"GITHUB_REF_NAME=",
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("build-release.sh exited 0 with a target unbuilt:\n%s", out)
	}
	if !strings.Contains(string(out), "failed for: darwin/arm64") {
		t.Errorf("build-release.sh said:\n%s\nwant it to name the target that failed", out)
	}

	shipped := slices.DeleteFunc(slices.Clone(releaseAssets), func(a string) bool {
		return a == "ccdad-darwin-arm64"
	})
	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatalf("reading %s: %v", dist, err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	slices.Sort(got)
	want := append(slices.Clone(shipped), "sha256sums.txt")
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("dist contains %v, want %v", got, want)
	}

	raw, err := os.ReadFile(filepath.Join(dist, "sha256sums.txt"))
	if err != nil {
		t.Fatalf("reading sha256sums.txt: %v", err)
	}
	var listed []string
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		m := sumsLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("sha256sums.txt line %q does not parse", line)
		}
		listed = append(listed, m[2])
	}
	slices.Sort(listed)
	if !slices.Equal(listed, shipped) {
		t.Fatalf("sha256sums.txt lists %v, want %v — a sums file must not name an asset the release does not have", listed, shipped)
	}
}

// The one seam between the three files: build-release.sh decides the asset
// names, and each installer computes its own half of the list independently.
// Nothing else would notice the two halves drifting apart, because each side's
// tests are self-consistent - the release would simply publish assets no
// installer asks for.
func TestTheInstallersBetweenThemCoverEveryAsset(t *testing.T) {
	both := append(slices.Clone(unixAssets), windowsAssets...)
	slices.Sort(both)
	if !slices.Equal(both, releaseAssets) {
		t.Errorf("install.sh and install.ps1 between them resolve to %v, but the release ships %v", both, releaseAssets)
	}
}
