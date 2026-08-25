package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// stubConsoleUTF8 answers the console read for a test, both ways.
//
// The var it swaps is the one place this binary asks what the attached console
// can carry, and on the two platforms that are not Windows its real body is a
// constant -- so without the seam, half the table below would be untestable on
// the machine most of this is written on.
func stubConsoleUTF8(t *testing.T, utf8 bool) {
	t.Helper()
	saved := consoleUTF8
	t.Cleanup(func() { consoleUTF8 = saved })
	consoleUTF8 = func() bool { return utf8 }
}

// The console read gets its production caller here and nowhere else in this
// commit, which is the whole reason this wiring lands before anything paints.
//
// Both rows, and neither is redundant. ConsoleUTF8's safe answer is false and
// false is also its zero value, so the false row alone passes against a field
// nobody ever assigned -- that is what the true row rules out. And the true row
// alone passes against a field wired to a literal true, which would hand the
// Unicode set to a CP437 console -- that is what the false row rules out. One
// row is not a test of wiring; two are.
func TestTheConsoleAnswerReachesTheOptionsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		utf8 bool
	}{
		{"a console that can carry UTF-8", true},
		{"a console that cannot", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			stubTTYs(t, false, false)
			stubConsoleUTF8(t, tc.utf8)

			if o := tuiOptions(NewRootCmd()); o.ConsoleUTF8 != tc.utf8 {
				t.Fatalf("Options.ConsoleUTF8 = %v for a console that answered %v; the code "+
					"page read has no production caller, so the glyph picker decides with "+
					"a default instead of with an answer", o.ConsoleUTF8, tc.utf8)
			}
		})
	}
}

// Nothing in this package asks the terminal what colour its background is, and
// this walks the package's own syntax to say so rather than trusting anyone to
// remember.
//
// That call is the most expensive line available to this binary: it puts stdin
// into raw mode with a deferred restore, writes a request to stdout, and blocks
// for up to two seconds on a terminal that answers neither it nor the identity
// query it falls back to. On Windows it goes further and opens the attached
// console explicitly when stdio is redirected, rather than declining -- so it
// will cheerfully interrogate a console the invocation is not writing one byte
// to, and whatever guard exists is the caller's alone.
//
// It is paid ONCE PER PROCESS, and it is paid through the seam internal/tui
// exports rather than by anyone writing the call themselves. tui.DarkBackground
// is a sync.OnceValue wrapping the query with the guard it needs -- terminals
// required on both ends of stdio, dark returned when it declined to ask -- and
// package cli reaches that one answer through the darkBackground var in
// color.go, which POINTS AT it. The event loop does not pay it at all, because
// the reply reaches a live program as a message.
//
// That is why this ban survives package cli acquiring a caller. It never
// forbade package cli from KNOWING the answer; it forbids package cli from
// ASKING for it a second time. Two queries in two packages are two caches and
// two two-second budgets: a process that rendered a listing and then a
// dashboard would pay four seconds, and a second one written here would land on
// whichever branch its author happened to guard -- most likely the branch where
// both ends are terminals, which is the live-program path that must never
// block, leaving the redirected path it was scoped to never asking. One query,
// behind one guard, in the package that owns the terminal.
//
// This is a walk and not a grep because the naive form is not the only form: a
// query parked in a package-level value and reached through the name reads as
// a selector too, and the walk deliberately does not distinguish a call from a
// mention. A later commit that believes package cli needs one of these has to
// come back and change this test on purpose, which is the point of it.
func TestNothingInThisPackageAsksTheTerminalWhatColourItsBackgroundIs(t *testing.T) {
	forbidden := map[string]bool{
		"HasDarkBackground":  true,
		"HasLightBackground": true,
		"BackgroundColor":    true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		// Every non-test .go file, including the ones whose build tags exclude
		// them from this platform's build: syntax parses on all of them, so
		// this asks the same question of the Windows half on Linux.
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			s, is := n.(*ast.SelectorExpr)
			if !is || !forbidden[s.Sel.Name] {
				return true
			}
			t.Errorf("%s reaches %s: the terminal's background colour is queried once per "+
				"process, by tui.DarkBackground, under the guard that lives with it -- a "+
				"caller in this package reaches that answer through the darkBackground var "+
				"in color.go and never asks again", fset.Position(s.Pos()), s.Sel.Name)
			return true
		})
	}
}
