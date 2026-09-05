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
// THE ONE ALLOWANCE is a tombstone, and it is granted to the STALE SPELLING
// rather than to the message that holds it. The dead words have to be followed
// immediately by "is now" and by a replacement that itself resolves, the way
// `'ccdad codex add' is now 'ccdad add codex'` is. Such a message exists to be
// read by somebody who typed the dead words, and that handover is the only
// shape in which naming them is not telling anybody to run them.
//
// Granting it to the message instead is what this used to do, and that was a
// blanket exemption rather than an allowance: any resolving `ccdad ...`
// anywhere in the string, plus the words "is now" anywhere in it, bought a
// pass for every stale spelling beside them, tied to neither. A line reading
// "`ccdad codex add` for an account ccdad serves; `ccdad status` is now the
// way to see it" was waved through -- it sends the reader to a command that
// exits 2, and the two halves that excused it have nothing to do with each
// other. The tree still holds one literal that matched by accident, in
// checkAccountsFile, which carries no stale spelling today and was one rewrap
// away from carrying an unguarded one.
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
			strays, named := straysIn(text)
			checked += named
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

// TestTheTombstoneAllowanceIsTiedToTheStaleSpelling holds the allowance to the
// one shape it is for, from every side that shape has.
//
// The tree's own tombstone is the control and has to keep passing: the message
// an old `ccdad codex add` invocation now dies with names the dead spelling on
// purpose, and the walk above would be unusable if it flagged that. The three
// refusals are the ways a message can carry the words without doing the work
// -- a resolving command somewhere else in the sentence, a handover that comes
// later rather than in the same breath, and a handover to a spelling that is
// itself dead. Each of those really does send a reader to a command that
// exits 2.
//
// None of the four can be written as a real message in the tree, which is why
// they are written here: three of them are defects by construction, and the
// walk above reads every message there is.
func TestTheTombstoneAllowanceIsTiedToTheStaleSpelling(t *testing.T) {
	const handover = "'ccdad codex add' is now 'ccdad add codex', beside 'ccdad add claude'"
	if strays, named := straysIn(handover); len(strays) > 0 {
		t.Errorf("a tombstone handing the reader `ccdad add codex` was flagged anyway: %v (of %d command lines read)",
			strays, named)
	}

	for _, tc := range []struct {
		name string
		text string
	}{
		{
			"a resolving command elsewhere in the sentence",
			"`ccdad codex add` for an account ccdad serves; `ccdad status` is now the way to see it.",
		},
		{
			"a handover that arrives later rather than in the same breath",
			"`ccdad codex add` for an account ccdad serves; the spelling is now `ccdad add codex`.",
		},
	} {
		strays, _ := straysIn(tc.text)
		if len(strays) != 1 {
			t.Fatalf("%s: advice to run `ccdad codex add` was excused; %d strays, want 1", tc.name, len(strays))
		}
		if strays[0].spelling != "codex add" {
			t.Errorf("%s: flagged %q, want the stale spelling `ccdad codex add`", tc.name, strays[0].spelling)
		}
	}

	// A handover to a second dead spelling is no handover: both halves name
	// something a reader cannot run, so both are the failure this walk is for.
	const deadHandover = "'ccdad login' is now 'ccdad codex add'"
	if strays, _ := straysIn(deadHandover); len(strays) != 2 {
		t.Errorf("a tombstone pointing at another dead spelling left %d strays, want both flagged: %v",
			len(strays), strays)
	}
}

// stray is a command line a message names and the tree cannot walk: the
// spelling as the message writes it, the first word nothing could spend, and
// the command path that could not spend it.
type stray struct{ spelling, word, walked string }

// straysIn reads one message and returns the stale command lines it tells
// somebody to run. The second return is how many command lines the message
// named at all -- stale, current and tombstoned alike -- which is what the
// floor counts, so a walk that stopped reading cannot pass as a clean tree.
func straysIn(text string) (strays []stray, named int) {
	for _, m := range adviceCommand.FindAllStringSubmatchIndex(text, -1) {
		named++
		spelling := text[m[2]:m[3]]
		word, path := strayWord(spelling)
		if word == "" {
			continue
		}
		if handsOverToACommandThatWalks(text[m[3]:]) {
			continue
		}
		strays = append(strays, stray{spelling, word, path})
	}
	return strays, named
}

// tombstoneHandover matches what a tombstone puts immediately after the dead
// spelling: the quote that closed it, the words "is now", and the replacement
// it hands the reader. Anchored, so it reads the text touching the stale
// spelling and nothing further along the sentence.
var tombstoneHandover = regexp.MustCompile("^[`']? is now [`']ccdad ([a-z][a-z0-9-]*(?: [a-z][a-z0-9-]*)*)")

// handsOverToACommandThatWalks reads what follows a stale spelling and reports
// whether that spelling is a tombstone: whether the message goes straight on
// to say what it is now, and names a replacement the tree really walks. A
// handover to a second dead spelling is no handover at all.
func handsOverToACommandThatWalks(after string) bool {
	m := tombstoneHandover.FindStringSubmatch(after)
	if m == nil {
		return false
	}
	word, _ := strayWord(m[1])
	return word == ""
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
