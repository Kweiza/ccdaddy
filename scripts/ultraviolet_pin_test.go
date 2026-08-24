package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinnedBubbletea and pinnedUltraviolet are this test's own record of the
// pair go.mod carries right now. ultraviolet has no tags, so bubbletea's
// stated version is the only signal that the renderer underneath it was
// reviewed along with it -- moving one without the other is a silent
// renderer swap, and the failure mode is visual corruption on a terminal
// this repository cannot observe, not a build break.
const (
	pinnedBubbletea   = "charm.land/bubbletea/v2 v2.0.9"
	pinnedUltraviolet = "github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886"
)

// TestTheRendererPinMovesOnlyWithBubbletea reads go.mod as text -- no new
// module dependency for a test that exists to catch an unreviewed one -- and
// fails the moment either require line drifts from this test's own
// constants, so a `go get -u` that swaps only one of the pair is caught here
// rather than discovered on somebody else's terminal.
func TestTheRendererPinMovesOnlyWithBubbletea(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	mod := string(data)
	for _, want := range []string{pinnedBubbletea, pinnedUltraviolet} {
		if !strings.Contains(mod, want) {
			t.Fatalf("go.mod no longer contains the require line %q -- if bubbletea moved, "+
				"ultraviolet must move in the SAME commit (it has no tags, so bubbletea's "+
				"version is the only review signal it gets), and both constants in this test "+
				"update together", want)
		}
	}
}
