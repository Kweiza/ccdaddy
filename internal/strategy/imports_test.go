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
//
// It is NOT what keeps internal/forecast out of this closure, and the
// distinction is worth stating because it looks like it is. A forecast edge
// fails this gate today only by accident: internal/forecast reaches cclink
// through internal/history, whose single use of it is one call to
// cclink.WriteFileAtomic -- a two-line pass-through to atomicfile.WriteFile,
// which is already in this closure. An ordinary tidy-up of that call, which
// nobody would think to question, would open the forecast edge with this gate
// still green. TestTheEngineDoesNotImportTheForecast is the gate that asks the
// forecast question, and it exists because this one does not.
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

// The engine's dependency closure must not contain internal/forecast either,
// and this is a SEPARATE rule from the credentials one above.
//
// The forecast measures the fleet: how fast quota is being spent across every
// account, when it runs out, how many seats a user would have to buy. The engine
// decides which ONE account the next session goes to, from the readings in front
// of it. Those are different questions with different inputs, and the ranking
// deriving a threshold from a fleet-wide burn rate is the specific outcome this
// forbids -- the mode's whole promise is that a user can follow every derived
// number in `ccdad status`, and a figure that depends on a measurement taken
// over the last four hours of every other account is not one they can.
//
// It was checked rather than assumed that the two do not already meet: the
// perishability term hover derives credits the rotation with usable x 100 /
// window_length points an hour, which is forecast.replenish's expression
// character for character. Two packages had independently written down the same
// model of how a pool drains. That is the shape this gate exists to keep
// intentional -- one of them may be wrong, and they must be allowed to be wrong
// separately.
//
// The direction is asked HERE, from inside internal/strategy, and the other
// direction is asked from inside internal/forecast, which is not tidiness. A
// test that shells out to `go list -deps` for a package its own binary does not
// contain is cached against inputs that do not include the answer: measured, the
// misplaced half reported `(cached)` and stayed green while the correctly placed
// one went red in the same run.
func TestTheEngineDoesNotImportTheForecast(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH, so the dependency closure cannot be asked for")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/Kweiza/ccdaddy/internal/strategy").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps = %v\n%s", err, out)
	}
	const forbidden = "github.com/Kweiza/ccdaddy/internal/forecast"
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == forbidden {
			t.Fatalf("%s is in the engine's dependency closure; the ranking decides from the "+
				"readings in front of it and must not derive a threshold from a fleet measurement\n%s",
				forbidden, out)
		}
	}
}
