package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func releaseWorkflow(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The signing step's POSITION is the invariant, not merely its presence. After
// the build, because sha256sums.txt has to be final. Before the attestation,
// because that step's `subject-path: dist/*` is what puts the signature under
// the provenance claim as well. Moving it one step in either direction is
// silent: the release still publishes, and what is missing is visible only to
// somebody who tries to verify.
func TestReleaseWorkflowSignsBetweenBuildingAndAttesting(t *testing.T) {
	yml := releaseWorkflow(t)

	at := -1
	for _, want := range []string{
		"name: Build the release assets",
		"name: Sign the checksums",
		"name: Attest the build provenance",
	} {
		i := strings.Index(yml, want)
		if i < 0 {
			t.Fatalf("release.yml has no step %q", want)
		}
		if i < at {
			t.Fatalf("step %q is out of order: signing runs after the build and before the attestation", want)
		}
		at = i
	}

	// A header that asserts the opposite of what the workflow does is worse
	// than no header: it is the first thing a reader trusts.
	if strings.Contains(yml, "no minisign") {
		t.Error("the header still says this repository publishes no minisign signature, which this workflow now does")
	}
}

// A secret interpolated into a `run:` block is substituted before bash ever
// sees the line, which puts it in the command the runner executes. The gate job
// already states that rule for the tag name; this pins it for the one value in
// this workflow that is actually secret.
func TestReleaseWorkflowPassesTheSigningKeyThroughTheEnvironment(t *testing.T) {
	yml := releaseWorkflow(t)
	if !strings.Contains(yml, "MINISIGN_SECRET_KEY: ${{ secrets.MINISIGN_SECRET_KEY }}") {
		t.Error("the signing key must reach the step through env:, not through its command line")
	}
	for _, line := range strings.Split(yml, "\n") {
		if strings.Contains(line, "minisign-sign") && strings.Contains(line, "${{") {
			t.Errorf("the signer's command line interpolates a workflow expression: %q", line)
		}
	}
}
