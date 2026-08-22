package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// THIRD-PARTY-LICENSES.txt is a generated file, and a generated file that
// nothing checks is a file that is wrong from the first `go get` onwards --
// silently, in the one artifact whose whole job is to be legally accurate.
//
// The check is a regeneration and a byte comparison rather than a spot check on
// module names: a dependency that reflows its own license text changes what has
// to ship, and a names-only assertion would not notice.
func TestThirdPartyLicensesIsCurrent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the generator is bash; it runs on the two POSIX CI legs")
	}
	if testing.Short() {
		t.Skip("regeneration lists deps for six targets and can reach the module proxy")
	}

	script, err := filepath.Abs("third-party-licenses.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Written to stdout rather than over the file: a test that repaired the
	// tree would pass while leaving the repository's committed copy stale,
	// which is the state it exists to detect.
	cmd := exec.Command("bash", script, "-")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	generated, err := cmd.Output()
	if err != nil {
		t.Fatalf("third-party-licenses.sh -: %v\n%s", err, stderr.String())
	}

	committed, err := os.ReadFile(filepath.Join("..", "THIRD-PARTY-LICENSES.txt"))
	if err != nil {
		t.Fatalf("reading the committed file: %v", err)
	}
	if string(committed) != string(generated) {
		t.Errorf("THIRD-PARTY-LICENSES.txt is stale: %d committed bytes, %d generated. "+
			"Run scripts/third-party-licenses.sh and commit the result.",
			len(committed), len(generated))
	}
}

// The generator and the release build must agree on the target list, because
// the notice obligation is about the binaries that are PUBLISHED. If
// build-release.sh gains a seventh target and the generator does not, the new
// binary ships without whatever its platform-specific dependencies require --
// which is exactly how mousetrap would have been missed.
func TestThirdPartyLicensesCoversEveryReleaseTarget(t *testing.T) {
	targets := func(t *testing.T, name string) string {
		t.Helper()
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if after, ok := strings.CutPrefix(line, "targets="); ok {
				return strings.Trim(after, `"`)
			}
		}
		t.Fatalf("%s has no targets= line", name)
		return ""
	}

	release := targets(t, "build-release.sh")
	generator := targets(t, "third-party-licenses.sh")
	if release != generator {
		t.Errorf("target lists disagree:\n  build-release.sh:        %s\n  third-party-licenses.sh: %s",
			release, generator)
	}
}
