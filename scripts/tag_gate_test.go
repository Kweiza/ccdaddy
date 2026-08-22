package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The tag gate is the release workflow's first job, and it is the only part of
// the release that decides anything. Inline in a `run:` block it could only be
// exercised by pushing a tag, which is the one thing it exists to make safe.
func runTagGate(t *testing.T, tag string) (string, string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the gate runs on the ubuntu-latest release runner")
	}
	script, err := filepath.Abs("tag-gate.sh")
	if err != nil {
		t.Fatalf("resolving tag-gate.sh: %v", err)
	}
	cmd := exec.Command("bash", script, tag)
	cmd.Dir = t.TempDir()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	return string(stdout), stderr.String(), err
}

func TestTagGateAcceptsStrictSemver(t *testing.T) {
	for _, tc := range []struct{ tag, version, prerelease string }{
		{"v0.0.0", "0.0.0", "false"},
		{"v1.2.3", "1.2.3", "false"},
		{"v10.20.30", "10.20.30", "false"},
		{"v1.0.0-rc1", "1.0.0-rc1", "true"},
		{"v1.0.0-alpha.1", "1.0.0-alpha.1", "true"},
		{"v1.0.0-0.3.7", "1.0.0-0.3.7", "true"},
		// A numeric prerelease identifier may not have a leading zero, but one
		// containing a letter may.
		{"v1.0.0-0a1", "1.0.0-0a1", "true"},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			stdout, stderr, err := runTagGate(t, tc.tag)
			if err != nil {
				t.Fatalf("tag-gate.sh %s: %v\n%s", tc.tag, err, stderr)
			}
			// The caller redirects stdout straight into $GITHUB_OUTPUT, so
			// anything else on it becomes a step output.
			want := "version=" + tc.version + "\nprerelease=" + tc.prerelease + "\n"
			if stdout != want {
				t.Errorf("tag-gate.sh %s wrote %q to stdout, want %q", tc.tag, stdout, want)
			}
		})
	}
}

func TestTagGateRefusesAnythingElse(t *testing.T) {
	const notSemver = "is not a strict semver tag"
	for _, tc := range []struct{ tag, wantMsg string }{
		// Called with no argument at all, which is a typo rather than a bad
		// tag and deserves to be told apart from one.
		{"", "no tag given"},
		{"1.2.3", notSemver},     // no v
		{"v1.2", notSemver},      // not three components
		{"v1.2.3.4", notSemver},  // four
		{"v01.2.3", notSemver},   // a leading zero
		{"v1.2.3-", notSemver},   // an empty prerelease
		{"v1.2.3-01", notSemver}, // a numeric prerelease identifier with a leading zero
		{"v1.2.3+build", notSemver},
		{"v1.2.3-rc1+build", notSemver}, // build metadata has to be percent-encoded in URLs
		{"vx.y.z", notSemver},
		{"release-1.2.3", notSemver},
		{"v1.2.3 ", notSemver},
		{" v1.2.3", notSemver},
		{"v1.2.3\nv9.9.9", notSemver}, // a newline would write a second GITHUB_OUTPUT line
	} {
		name := tc.tag
		if name == "" {
			name = "(no argument)"
		}
		t.Run(name, func(t *testing.T) {
			stdout, stderr, err := runTagGate(t, tc.tag)
			if err == nil {
				t.Fatalf("tag-gate.sh accepted %q and wrote %q", tc.tag, stdout)
			}
			if stdout != "" {
				t.Errorf("tag-gate.sh rejected %q but still wrote %q to stdout, which the caller "+
					"redirects into $GITHUB_OUTPUT", tc.tag, stdout)
			}
			if !strings.Contains(stderr, tc.wantMsg) {
				t.Errorf("tag-gate.sh said %q for %q, want a message containing %q", stderr, tc.tag, tc.wantMsg)
			}
		})
	}
}

// The gate and the installers have to agree about prereleases: the gate marks
// them, `releases/latest/download` skips what is marked, and both installers
// resolve latest through that path. A prerelease the gate did not mark would
// become the default install for everyone.
func TestTagGateMarksEveryPrerelease(t *testing.T) {
	for _, tag := range []string{"v1.0.0-rc1", "v0.1.0-alpha", "v2.0.0-beta.2"} {
		stdout, _, err := runTagGate(t, tag)
		if err != nil {
			t.Fatalf("tag-gate.sh %s: %v", tag, err)
		}
		if !strings.Contains(stdout, "prerelease=true") {
			t.Errorf("tag-gate.sh %s said %q, want prerelease=true", tag, stdout)
		}
	}
	for _, tag := range []string{"v1.0.0", "v0.1.0", "v2.0.0"} {
		stdout, _, err := runTagGate(t, tag)
		if err != nil {
			t.Fatalf("tag-gate.sh %s: %v", tag, err)
		}
		if !strings.Contains(stdout, "prerelease=false") {
			t.Errorf("tag-gate.sh %s said %q, want prerelease=false", tag, stdout)
		}
	}
}

// The workflow reads the gate's decision through $GITHUB_OUTPUT, so the two
// key names are a contract between a shell script and a YAML file with nothing
// in between to check them.
func TestReleaseWorkflowReadsTheKeysTheGateWrites(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatalf("reading release.yml: %v", err)
	}
	for _, want := range []string{
		"steps.tag.outputs.version",
		"steps.tag.outputs.prerelease",
		"needs.gate.outputs.version",
		"needs.gate.outputs.prerelease",
		"scripts/tag-gate.sh",
		"scripts/build-release.sh",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("release.yml does not mention %q", want)
		}
	}
}
