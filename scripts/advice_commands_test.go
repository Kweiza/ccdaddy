package scripts

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cli"
)

// adviceCommand finds a ccdad command line the way a message writes one:
// quoted, with a backtick or an apostrophe around it. Unquoted "ccdad" is
// prose -- "the account ccdad serves codex from" -- and matching that would
// flag sentences that give no advice at all.
//
// The capture takes lowercase words only, so it stops at the first flag, at a
// placeholder like ACCOUNT, and at the closing quote. `ccdad add claude
// --no-browser` is therefore read as `ccdad add claude`, which is the half of
// it the command tree can answer for.
var adviceCommand = regexp.MustCompile("[`']ccdad ([a-z][a-z0-9-]*(?: [a-z][a-z0-9-]*)*)")

// TestEveryCommandNamedInAMessageIsOneTheTreeStillSpellsThatWay reads every
// string literal this binary can print and refuses one that names a command
// line the tree cannot walk.
//
// The failure it is against is the ordinary one after a rename: the command
// moves, its own tests move with it, and the sentences elsewhere in the tree
// that named the old spelling stay green forever, because nothing anywhere
// reads them. A user follows the advice and gets a usage error from the tool
// that gave it. `ccdad codex add` became `ccdad add codex` and eighteen
// messages went stale in one commit; `ccdad login` had never existed at all
// and doctor had been recommending it since the row was written.
//
// WHAT IS ASKED is resolution, and only that: every word before the first
// argument must name a command that is really there. A word left over at a
// GROUP is the failure -- a group's argument position holds subcommand names,
// so a word it does not know is a spelling that has moved. A word left over at
// a leaf is an argument and is none of this check's business, which is what
// lets `ccdad config set codex.exec_path` and `ccdad strategy headroom`
// through without a special case.
//
// WHAT IS NOT ASKED is whether a group refuses a bare invocation, and that is
// a limit rather than an oversight: `ccdad mcp` is a group that serves and
// `ccdad add` is a group that exits 2, and nothing in the tree tells the two
// apart without running them -- both carry a RunE. The messages that name a
// provider's login are held to naming one by the tests beside those messages.
//
// THE ONE ALLOWANCE is a tombstone: a message may name a spelling the tree no
// longer walks while it is saying what that spelling "is now", provided the
// replacement it names in the same breath is one the tree does walk. Such a
// message exists to be read by somebody who typed the dead words, and it is
// the only shape in which naming them is not telling anybody to run them.
//
// String literals only, and not comments. A comment naming a stale command
// line is wrong too, but it is wrong at a reader rather than at a user, and a
// paragraph of history is entitled to name the spelling it is history about.
func TestEveryCommandNamedInAMessageIsOneTheTreeStillSpellsThatWay(t *testing.T) {
	root := ".."
	files := trackedFiles(t, root)

	checked := 0
	for _, rel := range files {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		if strings.Contains(rel, "testdata/") {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(rel))
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("%s could not be parsed, so its messages went unread: %v", rel, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			type stray struct{ spelling, word, walked string }
			var strays []stray
			walked := 0
			for _, m := range adviceCommand.FindAllStringSubmatch(text, -1) {
				checked++
				word, path := strayWord(m[1])
				if word == "" {
					walked++
					continue
				}
				strays = append(strays, stray{m[1], word, path})
			}
			// A tombstone: it says what the dead spelling is now, and the
			// spelling it hands over is one the tree walks.
			if walked > 0 && strings.Contains(text, "is now") {
				return true
			}
			for _, s := range strays {
				t.Errorf("%s:%d: this message tells somebody to run `ccdad %s`, and %q is no command of `%s`",
					rel, fset.Position(lit.Pos()).Line, s.spelling, s.word, s.walked)
			}
			return true
		})
	}

	// A floor, not a census. It fails the one way this check can go quiet: a
	// walk that read nothing and called the tree clean.
	if checked < 20 {
		t.Fatalf("only %d command lines were found in this tree's messages; the walk read nothing "+
			"and a green here would mean nothing", checked)
	}
}

// strayWord names the first word a spelling could not spend, and the command
// path that could not spend it. Both are "" when the tree walked the whole
// path. Words a LEAF could not spend are arguments and are spent here too:
// only a group is entitled to refuse a word.
func strayWord(spelling string) (word, walked string) {
	root := cli.NewRootCmd()
	cmd, rest, err := root.Find(strings.Fields(spelling))
	if err != nil {
		// Find refuses before it walks, so nothing about the path was
		// established and the first word is as far as blame can be placed.
		return strings.Fields(spelling)[0], root.CommandPath()
	}
	if len(rest) > 0 && cmd.HasSubCommands() {
		return rest[0], cmd.CommandPath()
	}
	return "", ""
}
