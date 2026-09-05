package forecast

import (
	"os/exec"
	"strings"
	"testing"
)

// The forecast's dependency closure must not contain internal/strategy.
//
// This package measures the fleet -- burn rates, when the pool runs dry, how
// many seats it would take to hold. The engine decides which ONE account the
// next session goes to. The forecast reporting on a fleet whose own routing rule
// it imports would make the measurement a function of the thing it is supposed
// to be measuring, and the comments here have said so in four places without
// anything checking it.
//
// It is the OTHER HALF of TestTheEngineDoesNotImportTheForecast, and it lives
// here rather than beside it for a reason that was measured rather than
// reasoned. `go list -deps` runs in a subprocess and reads the tree on disk; the
// test binary's own inputs are what the cache keys on. Asked from a package
// whose sources are not part of this question, the answer is cached against
// inputs that cannot change when the answer does: with the assertion misplaced
// into internal/strategy, adding a strategy import to this package left it
// reporting `ok ... (cached)` while the correctly placed half went red in the
// same run.
func TestTheForecastDoesNotImportTheEngine(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH, so the dependency closure cannot be asked for")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/Kweiza/ccdaddy/internal/forecast").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps = %v\n%s", err, out)
	}
	const forbidden = "github.com/Kweiza/ccdaddy/internal/strategy"
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == forbidden {
			t.Fatalf("%s is in the forecast's dependency closure; a measurement of the fleet must "+
				"not depend on the rule that routes it\n%s", forbidden, out)
		}
	}
}
