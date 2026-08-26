package scripts

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A comment that names a test is a citation, and it is the one kind `ci.sh
// cites` cannot see. That check refuses a pointer at a document this repository
// does not contain; this one refuses a pointer at a TEST this repository does
// not contain. The failure it is against is cheap and silent: rename a test,
// and every comment naming it goes false while the tree stays green, because
// nothing anywhere reads those names.
//
// It happened, and twice in one file pair: status.go and list.go both named a
// test that had been renamed out from under them, and `078b2bc` corrected both.
// The dead name is not written here, and it CANNOT be: each citation had it
// broken across a comment line wrap, so the name never existed as contiguous
// text in any file. `git log -S` over every ref finds nothing, which is the
// whole problem stated as a measurement -- a citation no grep can find is one
// no rename can be checked against.
//
// THERE IS NO EXEMPTION LIST, and that is a decision rather than an omission.
// The first thing this check does when it runs is refuse the paragraph you are
// reading if it spells a name nothing defines, which is a real cost: writing
// about a test that used to exist is a legitimate thing to want. Name the
// COMMIT instead, as the paragraph above does -- every reader of this
// repository has it, which is more than was ever true of the name.
//
// WHY A GO TEST AND NOT A SEVENTH ARM OF `cites`. That was the open decision
// and this is the answer, with the reason rather than a preference. `cites`
// deliberately does not know what a comment is -- ci.sh says so in as many
// words, and widening it would mean a parser per language. Here the question
// IS the parser's: what a comment is, what a string literal is, and what a test
// declaration is are all things go/ast answers exactly and a regex over every
// tracked file answers approximately. The cost is that a reader looking for the
// citation rule looks in ci.sh, so check_cites carries a pointer here.
//
// It still runs on every push: `ci.sh test` is `go test ./...`.

// citedName is a Go test's name as it appears in prose. The capital after the
// prefix is what keeps "Testing" and "Fuzzing" out: this is a NAME, not the
// word.
var citedName = regexp.MustCompile(`\b(?:Test|Benchmark|Fuzz|Example)[A-Z][A-Za-z0-9_]*`)

// runArgument is the one position in which an incomplete name is CORRECT, and
// it is not a concession: `-run` takes a regexp matched unanchored against
// every test name, so `go test ./internal/tui -run TestThePage -update` is how
// the seven golden pages are regenerated together, and expanding it to one full
// name would break the procedure the comment is documenting. Measured: three
// comments and one string literal in this tree are that shape, and one of them
// selects two tests on purpose.
//
// A name here is still checked -- it must select SOMETHING -- which is what
// keeps a renamed test from leaving a `-run` that silently runs nothing. A
// `-run` matching no test exits 0 with "no tests to run", so nothing else would
// notice.
var runArgument = regexp.MustCompile(`-(?:test\.)?run[= ]['"^(]*$`)

// citation is one name as it was written: where, and enough of its
// surroundings to tell the three ways of getting it wrong apart.
type citation struct {
	file string
	line int
	name string
	// nextWord is what the FOLLOWING comment line starts with, when this name
	// ended the line it was on. It is how a name split by a wrap is recognised
	// as the name it is rather than reported as a mystery.
	nextWord string
	// isRun records that the name sat in a `-run` argument, where a prefix is
	// the point.
	isRun bool
}

func TestACommentThatNamesATestNamesOneThatExists(t *testing.T) {
	root := ".."

	files := trackedFiles(t, root)
	defined, cites := readCitations(t, root, files)

	// Floors, not a census. They fail the one way this whole check can fail
	// silently -- a walk that read nothing and reported a clean tree -- and
	// they are set far below what is here (2715 tests defined and 146 names
	// cited on 2026-08-26) so that ordinary growth never touches them.
	if len(defined) < 500 {
		t.Fatalf("only %d tests found in this tree; the walk read nothing and a green here would mean nothing", len(defined))
	}
	if len(cites) < 50 {
		t.Fatalf("only %d cited names found; the comment and literal scan read nothing", len(cites))
	}

	for _, c := range cites {
		if defined[c.name] {
			continue
		}
		if c.isRun {
			if len(namesContaining(defined, c.name)) == 0 {
				t.Errorf("%s:%d: `-run %s` selects no test in this tree; a -run that matches nothing exits 0 with nothing run",
					c.file, c.line, c.name)
			}
			continue
		}
		if c.nextWord != "" && defined[c.name+c.nextWord] {
			t.Errorf("%s:%d: %s is %s with the name split across a comment line wrap; put it on one line, because a grep for the name finds nothing as it stands and that is how a rename goes quietly false",
				c.file, c.line, c.name, c.name+c.nextWord)
			continue
		}
		if longer := namesStartingWith(defined, c.name); len(longer) > 0 {
			t.Errorf("%s:%d: no test is named %s; %d name(s) begin with it, the first being %s. Name it in full, or write it as a -run pattern and mean that",
				c.file, c.line, c.name, len(longer), longer[0])
			continue
		}
		t.Errorf("%s:%d: no test in this tree is named %s", c.file, c.line, c.name)
	}
}

// trackedFiles is the same universe check_cites searches, for the same reason:
// a citation in a file you have just written should be checked, so `--others`
// is here and the index alone is not enough.
//
// The status is taken rather than assumed. A git that refuses would otherwise
// hand back an empty list, and an empty list is a clean tree to every loop
// below it.
func trackedFiles(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git could not list %s, so nothing was checked: %v", root, err)
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

// readCitations walks the tree once and answers both halves: every test this
// repository DEFINES, and every place a name of that shape is WRITTEN.
//
// Defined means declared in a _test.go file, because that is the only place
// `go test` looks. A func of that shape declared in an ordinary file is not a
// test, and a comment pointing at it as one would be exactly the false claim
// this check exists to refuse.
//
// WHAT IS SEARCHED, and the boundary is measured rather than drawn by taste:
//
//   - Go COMMENTS, which is where nearly every citation lives.
//   - Go STRING LITERALS, because three of them carry a real citation and one
//     is load-bearing: internal/tui/add_test.go re-executes itself with
//     `-test.run=TestTheHelperProcessOnlyExits`, and a rename there leaves a
//     subprocess that runs no test and exits 0.
//   - Shell scripts, because ci.sh's own comments cite tests by name.
//
// NOT Markdown, and that is the interesting exclusion. Two names live in .md
// files here. CHANGELOG.md's sits in a RELEASED section, which
// TestNoReleasedSectionHasChangedSinceItWasCut makes immutable -- so a check
// telling you to fix it would be telling you to break another one. And
// CONTRIBUTING.md's is a two-letter placeholder in a sentence about how to
// describe a mutation, which is prose about the shape and not a claim about any
// test. A gate that fails on correct prose is a gate somebody switches off.
func readCitations(t *testing.T, root string, files []string) (map[string]bool, []citation) {
	t.Helper()
	defined := map[string]bool{}
	var cites []citation

	for _, rel := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		switch {
		case strings.HasSuffix(rel, ".go"):
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				// Not skipped quietly: a file this check could not read is a
				// file whose citations went unchecked, and `ci.sh fmt` is
				// already red on a tracked Go file that does not parse.
				t.Errorf("%s could not be parsed, so its citations were not checked: %v", rel, err)
				continue
			}
			if strings.HasSuffix(rel, "_test.go") {
				for _, decl := range file.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if !ok || fn.Recv != nil {
						continue
					}
					if citedName.FindString(fn.Name.Name) == fn.Name.Name {
						defined[fn.Name.Name] = true
					}
				}
			}
			cites = append(cites, commentCitations(fset, file, rel)...)
			cites = append(cites, literalCitations(fset, file, rel)...)

		case strings.HasSuffix(rel, ".sh"):
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s could not be read, so its citations were not checked: %v", rel, err)
				continue
			}
			// Whole text rather than the comments alone. A shell script has no
			// test declarations, so every name of this shape in one is a
			// citation whatever quoting it sits inside, and knowing where a
			// `#` starts a comment in shell is the parser-per-language problem
			// this check chose the other side of.
			for i, line := range strings.Split(string(raw), "\n") {
				for _, c := range lineCitations(line, rel, i+1, "") {
					cites = append(cites, c)
				}
			}
		}
	}
	sort.Slice(cites, func(i, j int) bool {
		if cites[i].file != cites[j].file {
			return cites[i].file < cites[j].file
		}
		return cites[i].line < cites[j].line
	})
	return defined, cites
}

// commentCitations reads a comment GROUP at a time, because the wrap case is
// only visible across two of its lines.
func commentCitations(fset *token.FileSet, file *ast.File, rel string) []citation {
	var cites []citation
	for _, group := range file.Comments {
		var lines []string
		var numbers []int
		for _, comment := range group.List {
			at := fset.Position(comment.Slash).Line
			if strings.HasPrefix(comment.Text, "//") {
				lines = append(lines, strings.TrimPrefix(comment.Text, "//"))
				numbers = append(numbers, at)
				continue
			}
			inner := strings.TrimSuffix(strings.TrimPrefix(comment.Text, "/*"), "*/")
			for i, line := range strings.Split(inner, "\n") {
				lines = append(lines, line)
				numbers = append(numbers, at+i)
			}
		}
		for i, line := range lines {
			next := ""
			if i+1 < len(lines) {
				next = strings.TrimLeft(lines[i+1], " \t*")
			}
			cites = append(cites, lineCitations(line, rel, numbers[i], next)...)
		}
	}
	return cites
}

// literalCitations reads string literals. A raw literal spanning lines reports
// the line it OPENS on, which is close enough to send a reader to the right
// place and is not worth a second position map.
func literalCitations(fset *token.FileSet, file *ast.File, rel string) []citation {
	var cites []citation
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		text, err := strconv.Unquote(lit.Value)
		if err != nil {
			text = lit.Value
		}
		at := fset.Position(lit.Pos()).Line
		for _, line := range strings.Split(text, "\n") {
			cites = append(cites, lineCitations(line, rel, at, "")...)
		}
		return true
	})
	return cites
}

// lineCitations is every name on one line, with the two facts about its
// surroundings that the report needs.
func lineCitations(line, rel string, number int, next string) []citation {
	var cites []citation
	for _, at := range citedName.FindAllStringIndex(line, -1) {
		c := citation{
			file:  rel,
			line:  number,
			name:  line[at[0]:at[1]],
			isRun: runArgument.MatchString(line[:at[0]]),
		}
		if at[1] == len(line) && next != "" {
			c.nextWord = leadingWord(next)
		}
		cites = append(cites, c)
	}
	return cites
}

// leadingWord is the identifier a line begins with, or "" when it begins with
// anything else. It is only ever joined to a name that ended the previous line,
// which is what a wrapped identifier looks like.
func leadingWord(line string) string {
	end := 0
	for end < len(line) {
		c := line[end]
		if c == '_' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
			end++
			continue
		}
		break
	}
	return line[:end]
}

func namesStartingWith(defined map[string]bool, prefix string) []string {
	var found []string
	for name := range defined {
		if strings.HasPrefix(name, prefix) && name != prefix {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	return found
}

// namesContaining, and not namesStartingWith, because that is what `-run`
// itself does: the pattern is matched unanchored against each test's name.
func namesContaining(defined map[string]bool, pattern string) []string {
	var found []string
	for name := range defined {
		if strings.Contains(name, pattern) {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	return found
}
