package scripts

import (
	"os/exec"
	"strings"
	"testing"
)

// TestRelsignReachesTheShippedBinary pins the fact the README's key-bootstrap
// one-liner in "Verifying the download" depends on: internal/relsign's
// compile-time key constant only reaches a built ccdad when something under
// cmd/ccdad actually imports the package.
//
// That check was cut from the README once already because it silently failed:
// nothing under cmd/ccdad imported relsign at the time, so the linker dropped
// the package -- and the pinned key with it -- and `grep -q ... && echo`
// printed nothing for every binary this repository could produce. It is back
// because internal/cli/update.go now imports relsign, but nothing before this
// test would notice if a later refactor stopped doing that, and the README's
// check would go back to failing the same way, silently, on somebody's
// terminal instead of in this suite.
//
// go list -deps is the cheap half of the proof: it is package-level
// reachability, not a built-and-grepped binary. That is still sufficient,
// because relsign's key constant feeds a package-level var initializer
// (mustParseKeys(publicKeys) in keys.go), and every linked package's
// initializers run unconditionally -- so a reachable relsign is a relsign
// whose key bytes are embedded, with nothing left to that also being true.
func TestRelsignReachesTheShippedBinary(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("locating go: %v", err)
	}
	cmd := exec.Command(goBin, "list", "-deps", "./cmd/ccdad")
	cmd.Dir = ".."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/ccdad: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "github.com/Kweiza/ccdaddy/internal/relsign") {
		t.Fatal("cmd/ccdad no longer imports internal/relsign, so the compiled binary no longer " +
			"carries the pinned release key -- the README's \"Verifying the download\" bootstrap " +
			"grep would fail silently again, the way it did before ccdad update existed")
	}
}
