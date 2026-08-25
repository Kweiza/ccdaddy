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
// nothing in it reaches for an answer somebody else asked for either. This
// walks the package's own syntax to say so rather than trusting anyone to
// remember.
//
// That call is the most expensive line available to this binary. It puts stdin
// into raw mode with a deferred restore, writes a request to stdout, and waits
// -- and it does that TWICE. lipgloss's BackgroundColor loops over stdin and
// then stdout and runs both legs even where the two are the same file, with no
// in == out guard, at a two-second timeout each. FOUR seconds, on any terminal
// that answers neither the request nor the identity query it falls back to. On
// Windows it goes further and opens the attached console explicitly when stdio
// is redirected, rather than declining -- so it will cheerfully interrogate a
// console the invocation is not writing one byte to, and whatever guard exists
// is the caller's alone.
//
// DarkBackground is on the list beside the library's own names, and it is the
// entry with a measurement behind it. This package used to reach it, through a
// var in color.go that pointed at package tui's sync.OnceValue, on the argument
// that one cache beats two. The argument refuted itself on its own numbers: a
// cache is once per PROCESS, and every one-shot listing IS its own process, so
// nothing was ever cached and the full price was paid per invocation. Measured
// on a silent pty under the default `tui.theme = auto`: `list` 4.06 s,
// `status` 4.08 s, `doctor` 4.10 s, `daemon status` 4.07 s, against 0.08 s
// apiece with `theme = none`. Those commands take the defined dark default now
// -- the same answer the query itself gives when it declines to ask -- and
// `tui.theme = light` is the one line that answers for a reader who wants
// otherwise.
//
// So the ban is no longer only "do not write the query here". It is "do not go
// and get that answer at all", and the two halves are not the same rule: the
// first stops a second raw-mode call landing on whichever branch its author
// happened to guard, most likely the branch where both ends are terminals,
// which is the live-program path that must never block; the second stops a
// listing paying four seconds through somebody else's cache for a colour no
// pipe was ever going to show. The interactive dashboard still resolves auto,
// and still does it the only way that costs nothing: the reply reaches a live
// program as a message its event loop already asked for, in package tui, where
// every clause of the query's guard is stated as well.
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
		"DarkBackground":     true,
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
			t.Errorf("%s reaches %s: a one-shot listing takes the defined dark default and "+
				"asks nothing. The query costs four seconds on a terminal that does not "+
				"answer -- two per stdio end, both legs run -- and it is paid once per "+
				"process, which for these commands is once per invocation. The interactive "+
				"dashboard resolves auto from a message in package tui, at no cost at all",
				fset.Position(s.Pos()), s.Sel.Name)
			return true
		})
	}
}
