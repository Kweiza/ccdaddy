package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// Windows compares paths without regard to case, and the containment test has
// to agree with the platform or a session reached by a differently-cased
// spelling stops being recognised as one.
//
// This lives here rather than behind a `goos` variable, and the history is the
// argument: the first version of scoped.go hand-folded the paths, gated on
// such a variable, and its comment said filepath.Rel is case-sensitive on
// Windows. Rel compares elements with sameWord, which is strings.EqualFold in
// path_windows.go — so the fold was dead code, and the test that "covered" it
// was only ever exercising the simulation. An assertion that runs on the
// platform is the only kind that can say anything here.
func TestWindowsASessionSpeltInADifferentCaseIsStillThatSession(t *testing.T) {
	container := `C:\Users\dev\.ccdad\sessions`
	session := filepath.Join(strings.ToUpper(container), "acct-1-1234567890")
	if !inside(container, session) {
		t.Fatalf("%s was not recognised as inside %s; a differently-cased spelling of a session is that session on this platform", session, container)
	}
	// And the sibling trap still holds with the folding in play.
	if inside(container, filepath.Join(strings.ToUpper(container)+"-OLD", "acct-1-1")) {
		t.Fatal("a case-folded sibling of the sessions container was treated as being inside it")
	}
}
