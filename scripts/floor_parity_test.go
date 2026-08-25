package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/release"
)

// The size floor exists three times in this repository, for the reason the
// checksum rules do: internal/release runs on the machine `ccdad update` is
// replacing, install.sh on a fresh Unix box and install.ps1 on a fresh Windows
// one, and no one of the three can reach the others.
//
// Two comments already assert that they must not drift — internal/release's own
// and the one at step 16 in internal/cli/update.go — and nothing executed it.
// Measured: raising release.MinAssetBytes from 1_000_000 to 3_000_000 left
// `go test ./...` at exit 0 with both installers still saying 1000000, which is
// a `ccdad update` accepting three times less than a fresh install will.
//
// The direction that matters is a Go floor BELOW the installers': it means
// `update` installs an asset a fresh install would refuse, on a machine that
// already trusts the binary doing the installing.
//
// The numbers are read OUT of the two installers rather than spelled a second
// time here, which is what scripts/sums_parity_test.go does with install.sh's
// grep expressions and for the same reason: a test that compares Go against
// somebody's memory of an installer goes green while the installer drifts.
func TestTheSizeFloorIsTheSameInAllThreeImplementations(t *testing.T) {
	for _, c := range []struct {
		file string
		// re must match exactly once, and its one capture group is the floor.
		// Anchored on the comparison itself rather than on the bare number, so
		// an unrelated 1000000 elsewhere in the file cannot satisfy it.
		re *regexp.Regexp
	}{
		{"install.sh", regexp.MustCompile(`\[ "\$size" -lt ([0-9]+) \]`)},
		{"install.ps1", regexp.MustCompile(`\$size -lt ([0-9]+)`)},
	} {
		t.Run(c.file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("..", c.file))
			if err != nil {
				t.Fatal(err)
			}
			m := c.re.FindAllStringSubmatch(string(body), -1)
			if len(m) != 1 {
				t.Fatalf("%s has %d size comparisons matching %v, want exactly 1 — this test can no "+
					"longer read the floor the installer actually applies", c.file, len(m), c.re)
			}
			got, err := strconv.ParseInt(m[0][1], 10, 64)
			if err != nil {
				t.Fatalf("%s's floor %q is not a number: %v", c.file, m[0][1], err)
			}
			if got != release.MinAssetBytes {
				t.Errorf("%s refuses anything under %d bytes and release.MinAssetBytes is %d.\n"+
					"A Go floor below the installer's lets `ccdad update` accept an asset a fresh "+
					"install would refuse; above it, `update` refuses what the installer just "+
					"installed.", c.file, got, release.MinAssetBytes)
			}
		})
	}
}
