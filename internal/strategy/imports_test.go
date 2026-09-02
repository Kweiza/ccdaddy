package strategy

import (
	"os/exec"
	"strings"
	"testing"
)

// The engine's dependency closure must not contain internal/cclink.
//
// cclink reads and rewrites Claude Code's credentials file. A Codex switch
// writes a pointer file and nothing else, and the way that claim is made
// checkable rather than merely stated is a package that CANNOT reach the
// credentials file because its import graph does not contain the package that
// does. strategy is the shared floor: the Codex switch records its stamp here,
// so a cclink edge here would put one in every closure above it.
//
// It asks the toolchain rather than reading imports itself, because the
// property is transitive: the edge that reappeared last time was two packages
// away, in internal/usage, and a source scan of this directory would have
// reported the tree clean.
func TestStrategyDependsOnNothingThatReadsTheCredentialsFile(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH, so the dependency closure cannot be asked for")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/Kweiza/ccdaddy/internal/strategy").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps = %v\n%s", err, out)
	}
	const forbidden = "github.com/Kweiza/ccdaddy/internal/cclink"
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == forbidden {
			t.Fatalf("%s is in the engine's dependency closure; the atomic write goes through "+
				"internal/atomicfile so that a package holding a switch cannot reach Claude Code's login\n%s",
				forbidden, out)
		}
	}
}
