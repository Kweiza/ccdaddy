package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A released section of CHANGELOG.md is immutable, and nothing said so.
//
// `20edc65` cut the old `## [Unreleased]` into `## [0.7.0]` and opened a fresh
// one. A branch that forked BEFORE that cut and edited CHANGELOG.md gets a
// CLEAN MERGE on rebase, and its new bullets land inside the released `[0.7.0]`
// body with no conflict marker. It happened: `012d65b0` carries an `### Added`
// bullet and a whole `### Changed` subsection — 117 lines — swallowed into
// 0.7.0, and its parent was already carrying them too, so the pollution
// survived more than one commit. A person found it by looking.
//
// This compares each released section against a digest recorded when it was
// cut, rather than against `main`. The difference is not stylistic:
// `.github/workflows/ci.yml` checks out with actions/checkout's default
// `fetch-depth: 1`, so on a feature branch's push run the only refs that exist
// are that branch's own. Measured on a depth-1 clone of a feature branch here:
// `git show main:CHANGELOG.md` is `fatal: invalid object name 'main'`. A gate
// that needs a baseline ref is a gate that cannot run where it is needed, and
// this repository has no other gate that reads one.

const releasedSums = "testdata/changelog-released.sums"

// updateSums rewrites the recorded digests from the file as it stands now.
//
//	go test ./scripts -run TestNoReleasedSection -update-sums -count=1
//
// A release cut is the one commit that may run it, and it runs in THAT commit:
// the cut adds a `## [x.y.z]` heading, and the heading and its digest are added
// together or the next commit fails on a section that has no record.
//
// Like the golden pages under internal/tui/testdata, this WRITES WHATEVER THE
// FILE SAID, a section somebody corrupted included. It is a transcription tool
// and never an oracle, and what makes it safe is that the diff it leaves is
// reviewed like any other change — which is what it is.
var updateSums = flag.Bool("update-sums", false,
	"rewrite testdata/changelog-released.sums from CHANGELOG.md as it stands")

// A released heading and the bytes under it.
//
// A SLICE and not a map, and the reason is a real hole rather than taste: keyed
// by version, two spans carrying the same version collapse to whichever was
// digested last, and the earlier one's bytes are compared to nothing at all. A
// second `## [0.7.0]` heading above the real one then carries any amount of new
// prose straight through the gate — the exact class of edit this file exists to
// stop — and `-update-sums` cannot see it either, because it writes the version
// twice and the reader dedupes it back down. Repeated versions are reported
// below instead.
type releasedSection struct {
	version string
	digest  string
}

// releasedSections returns one entry per `## [x.y.z]` heading, in file order.
//
// WHERE A SECTION ENDS, stated as the code has it rather than as it reads:
// at the next `## ` heading OF ANY KIND, at the start of the trailing link
// block, or at end of file. The link block is taken to begin at the first link
// definition after the LAST released heading, so a footer appended below it
// belongs to no section — without that, everything at the bottom of the file
// folded into `## [0.1.0-rc1]` and any line added down there was reported as
// the oldest release having changed, with advice to move it to `[Unreleased]`
// that did not apply to it. The exposure that remains, named rather than
// papered over: a link definition written INSIDE the final released section
// would shorten that section. Nothing has ever done it, and the definitions in
// this file have only ever been a block at the end.
//
// Trailing blank lines are trimmed, so whether the file ends in a newline is
// not a released section's business.
//
// Headings inside a fenced code block are not headings. A ```` ```md ```` block
// quoting `## [1.2.3]` is prose about the format, and reading it as a release
// invents a section whose failure tells the reader to regenerate the table —
// advice that, followed, records the phantom permanently.
func releasedSections(body string) []releasedSection {
	lines := strings.Split(body, "\n")

	fenced := make([]bool, len(lines))
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			// The fence line itself is inside neither the block nor the prose,
			// and it can never be a heading, so which side it lands on does
			// not matter. It is marked fenced for both.
			fenced[i] = true
			inFence = !inFence
			continue
		}
		fenced[i] = inFence
	}

	isHeading := func(i int) bool {
		return !fenced[i] && strings.HasPrefix(lines[i], "## ")
	}
	isVersion := func(i int) (string, bool) {
		if fenced[i] {
			return "", false
		}
		m := versionHeading.FindStringSubmatch(lines[i])
		if m == nil {
			return "", false
		}
		return m[1], true
	}

	// The last released heading, so the trailing link block can be found from
	// there rather than from the first definition anywhere in the file.
	lastVersion := -1
	for i := range lines {
		if _, ok := isVersion(i); ok {
			lastVersion = i
		}
	}
	linkBlock := len(lines)
	if lastVersion >= 0 {
		for i := lastVersion; i < len(lines); i++ {
			if !fenced[i] && linkDefinition.MatchString(lines[i]) {
				linkBlock = i
				break
			}
		}
	}

	type span struct {
		version    string
		start, end int
	}
	var spans []span
	for i := range lines {
		if i >= linkBlock {
			break
		}
		v, ok := isVersion(i)
		if !ok {
			// A `## ` heading that is not a release still ends the one above it.
			if isHeading(i) && len(spans) > 0 && spans[len(spans)-1].end > i {
				spans[len(spans)-1].end = i
			}
			continue
		}
		if n := len(spans); n > 0 && spans[n-1].end > i {
			spans[n-1].end = i
		}
		spans = append(spans, span{version: v, start: i, end: linkBlock})
	}

	out := make([]releasedSection, 0, len(spans))
	for _, s := range spans {
		var kept []string
		for _, line := range lines[s.start:s.end] {
			// Link definitions are changelog_links_test.go's to hold: every
			// released heading has one, and `[Unreleased]` points at the newest
			// release. The block legitimately grows on a cut and grew once on a
			// backfill, so a digest covering it would go red on a correct change
			// and teach the next reader to regenerate rather than to look.
			if linkDefinition.MatchString(line) {
				continue
			}
			kept = append(kept, line)
		}
		for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
			kept = kept[:len(kept)-1]
		}
		sum := sha256.Sum256([]byte(strings.Join(kept, "\n")))
		out = append(out, releasedSection{version: s.version, digest: hex.EncodeToString(sum[:])})
	}
	return out
}

func TestNoReleasedSectionHasChangedSinceItWasCut(t *testing.T) {
	sections := releasedSections(changelog(t))

	if *updateSums {
		var b strings.Builder
		for _, s := range sections {
			fmt.Fprintf(&b, "%s  %s\n", s.digest, s.version)
		}
		if err := os.WriteFile(filepath.FromSlash(releasedSums), []byte(b.String()), 0o644); err != nil {
			t.Fatalf("writing %s: %v", releasedSums, err)
		}
		t.Logf("wrote %d digest(s) to %s; read the diff before committing it", len(sections), releasedSums)
		return
	}

	for _, p := range releasedSumProblems(sections, recorded(t)) {
		t.Error(p)
	}
}

// releasedSumProblems is the comparison itself, lifted out of the test that
// runs it so that the REPORT can be tested and not merely the digest.
//
// It is worth the extra name. With the comparison inline, deleting the arm that
// reports a moved digest left the whole suite green: every fixture in the file
// fed the digest function directly, and nothing asked what the gate said. That
// is the shape a gate fails in — the check is correct and nobody is holding it.
func releasedSumProblems(sections []releasedSection, want map[string]string) []string {
	var out []string
	// Counted first, and a repeated version is reported INSTEAD of being
	// compared. One record cannot answer for two sections, so a digest verdict
	// on either of them is noise on top of the thing the reader has to fix.
	count := map[string]int{}
	for _, s := range sections {
		count[s.version]++
	}
	seen := map[string]bool{}
	for _, s := range sections {
		if count[s.version] > 1 {
			if !seen[s.version] {
				out = append(out, fmt.Sprintf(
					"`## [%s]` appears %d times in CHANGELOG.md.\n"+
						"One record cannot hold two sections, so whatever is under the second\n"+
						"heading would be compared to nothing at all. Merge them, or correct the\n"+
						"version on one.",
					s.version, count[s.version]))
			}
			seen[s.version] = true
			continue
		}
		switch {
		case want[s.version] == "":
			out = append(out, fmt.Sprintf(
				"`## [%s]` has no digest in %s.\n"+
					"If you are cutting a release, regenerate the table in the SAME commit:\n"+
					"  go test ./scripts -run TestNoReleasedSection -update-sums -count=1\n"+
					"and read the diff — it writes whatever the file says.",
				s.version, releasedSums))
		case want[s.version] != s.digest:
			out = append(out, fmt.Sprintf(
				"`## [%s]` no longer matches the bytes it was released with.\n"+
					"  recorded %s\n  now      %s\n"+
					"A released section is immutable. The usual cause is a rebase across a release\n"+
					"cut: git merges CHANGELOG.md cleanly and drops your new entry inside the\n"+
					"released section instead of `[Unreleased]`. Move it up to `[Unreleased]`.",
				s.version, want[s.version], s.digest))
		}
		seen[s.version] = true
	}
	for v := range want {
		if !seen[v] {
			out = append(out, fmt.Sprintf(
				"%s records `## [%s]`, which CHANGELOG.md no longer has.", releasedSums, v))
		}
	}
	return out
}

// recorded reads the committed table the gate compares against.
func recorded(t *testing.T) map[string]string {
	t.Helper()
	body, err := os.ReadFile(filepath.FromSlash(releasedSums))
	if err != nil {
		t.Fatalf("reading %s: %v", releasedSums, err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("%s: malformed line %q, want `<sha256>  <version>`", releasedSums, line)
		}
		out[fields[1]] = fields[0]
	}
	return out
}

// digestsByVersion is for the tests below, which ask whether one section moved.
// The gate itself does not use it, for the reason on releasedSection.
func digestsByVersion(sections []releasedSection) map[string]string {
	out := map[string]string{}
	for _, s := range sections {
		out[s.version] = s.digest
	}
	return out
}

// splice returns body with line inserted directly after the first line that
// contains after. It builds fixtures out of the real CHANGELOG.md rather than a
// miniature one: the shape being tested is a rebase dropping a bullet into a
// real released section, and a five-line stand-in cannot be wrong in the way
// the real file was.
func splice(t *testing.T, body, after, line string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.Contains(l, after) {
			out := append([]string{}, lines[:i+1]...)
			out = append(out, line)
			return strings.Join(append(out, lines[i+1:]...), "\n")
		}
	}
	t.Fatalf("no line containing %q in CHANGELOG.md", after)
	return ""
}

// spliceBefore is splice, inserting above the matched line instead of below it.
// It is what puts a fixture at the END of a section rather than the start.
func spliceBefore(t *testing.T, body, before, block string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.Contains(l, before) {
			out := append([]string{}, lines[:i]...)
			out = append(out, strings.Split(block, "\n")...)
			return strings.Join(append(out, lines[i:]...), "\n")
		}
	}
	t.Fatalf("no line containing %q in CHANGELOG.md", before)
	return ""
}

// A `## ` heading that is not a release still ends the section above it, and
// what it introduces belongs to no release. Without this the prose under a
// `## Notes` written at the bottom of `## [0.7.0]` would be digested as part of
// 0.7.0, and editing it later would report a released section as changed.
//
// The rule was stated in a comment here before anything held it: removing it
// left every other test in this file green.
func TestANonReleaseHeadingEndsTheSectionAboveIt(t *testing.T) {
	body := changelog(t)
	before := digestsByVersion(releasedSections(body))

	edited := spliceBefore(t, body, "## [0.6.1]",
		"## Notes\n\nSomething written under a heading that is not a release.\n")
	after := digestsByVersion(releasedSections(edited))

	if after["0.7.0"] != before["0.7.0"] {
		t.Errorf("`## [0.7.0]` swallowed the prose under a `## Notes` heading below it: %s -> %s",
			before["0.7.0"], after["0.7.0"])
	}
	if after["0.6.1"] != before["0.6.1"] {
		t.Errorf("`## [0.6.1]` moved: %s -> %s", before["0.6.1"], after["0.6.1"])
	}
}

// The accident itself, in one line. `012d65b0` did it with 117.
func TestABulletDroppedIntoAReleasedSectionChangesItsDigest(t *testing.T) {
	body := changelog(t)
	before := digestsByVersion(releasedSections(body))

	polluted := splice(t, body, "## [0.7.0]", "- **A bullet a rebase dropped in here.**")
	after := digestsByVersion(releasedSections(polluted))

	if after["0.7.0"] == before["0.7.0"] {
		t.Error("a bullet added inside `## [0.7.0]` left its digest unchanged; " +
			"the gate would not have caught 012d65b0")
	}
	if after["0.6.1"] != before["0.6.1"] {
		t.Errorf("editing 0.7.0 moved 0.6.1's digest too: %s -> %s\n"+
			"Each section is digested over its own lines, so a report names the section a\n"+
			"reader has to go and fix.", before["0.6.1"], after["0.6.1"])
	}
}

// The gate has to be silent on the thing everybody does. A gate that fails on
// correct work is a gate somebody switches off.
func TestAnEntryAddedUnderUnreleasedMovesNoReleasedDigest(t *testing.T) {
	body := changelog(t)
	before := digestsByVersion(releasedSections(body))

	edited := splice(t, body, "## [Unreleased]", "\n### Added\n\n- **An ordinary new entry.**")
	sections := releasedSections(edited)
	after := digestsByVersion(sections)

	for _, s := range sections {
		if after[s.version] != before[s.version] {
			t.Errorf("`## [%s]` moved when only `[Unreleased]` was edited: %s -> %s",
				s.version, before[s.version], after[s.version])
		}
	}
}

// The exclusion this file decided, pinned in the direction that can go wrong
// silently. The block grows on every cut and grew once on a backfill — `main`
// gained link lines for older releases long after they shipped — so a digest
// that covered them would have gone red on a legitimate change and taught the
// next reader to regenerate rather than to look.
func TestALinkDefinitionAddedInsideAReleasedSectionMovesNoDigest(t *testing.T) {
	body := changelog(t)
	sections := releasedSections(body)
	before := digestsByVersion(sections)

	edited := splice(t, body, "## [0.7.0]",
		"[0.0.1]: https://github.com/Kweiza/ccdaddy/releases/tag/v0.0.1")
	after := digestsByVersion(releasedSections(edited))

	for _, s := range sections {
		if after[s.version] != before[s.version] {
			t.Errorf("`## [%s]` moved when only a link definition was added: %s -> %s\n"+
				"changelog_links_test.go owns that block; this one must not.",
				s.version, before[s.version], after[s.version])
		}
	}
}

// The bottom of the file is not the oldest release's business. Before the
// trailing block was excluded, appending one line below the link definitions
// reported `## [0.1.0-rc1]` as having changed and told the reader to move it to
// `[Unreleased]`, which was not where it came from.
func TestAFooterAppendedBelowTheLinkBlockMovesNoDigest(t *testing.T) {
	body := changelog(t)
	before := digestsByVersion(releasedSections(body))

	sections := releasedSections(body + "\nA note somebody appended at the bottom.\n")
	after := digestsByVersion(sections)

	for _, s := range sections {
		if after[s.version] != before[s.version] {
			t.Errorf("`## [%s]` moved when a footer was appended below the link block: %s -> %s",
				s.version, before[s.version], after[s.version])
		}
	}
}

// Whether the file ends in a newline is nobody's release. An editor that
// strips or adds one must not report a section nobody touched.
func TestTheTrailingNewlineIsNotAReleasedSectionsBusiness(t *testing.T) {
	body := changelog(t)
	before := digestsByVersion(releasedSections(body))
	after := digestsByVersion(releasedSections(strings.TrimRight(body, "\n")))

	for v, d := range before {
		if after[v] != d {
			t.Errorf("`## [%s]` moved when the trailing newline was removed: %s -> %s", v, d, after[v])
		}
	}
}

// A version heading quoted inside a fenced block is prose about the format.
// Read as a release it invents a section, and the failure it produces tells the
// reader to regenerate — which records the phantom for good.
func TestAVersionHeadingInsideAFencedBlockIsNotARelease(t *testing.T) {
	body := changelog(t)
	before := releasedSections(body)

	edited := splice(t, body, "## [Unreleased]",
		"\nThe format is:\n\n```md\n## [9.9.9] — 2099-01-01\n```\n")
	after := releasedSections(edited)

	if len(after) != len(before) {
		t.Fatalf("a heading inside a ```md block invented %d section(s); got %d, want %d",
			len(after)-len(before), len(after), len(before))
	}
	if problems := releasedSumProblems(after, recorded(t)); len(problems) != 0 {
		t.Errorf("quoting the heading format failed the gate:\n%s", strings.Join(problems, "\n"))
	}
}

// Two headings for one version cannot both be held by one record, and the one
// that loses is compared to nothing. That is a way to carry arbitrary prose
// past this gate, so it is reported rather than silently deduped.
func TestARepeatedVersionHeadingIsReported(t *testing.T) {
	body := changelog(t)
	edited := splice(t, body, "## [Unreleased]",
		"\n## [0.7.0] — 2026-08-25\n\n- **Prose under a second heading for a released version.**\n")

	problems := releasedSumProblems(releasedSections(edited), recorded(t))

	if len(problems) == 0 {
		t.Fatal("a second `## [0.7.0]` heading carried new prose past the gate")
	}
	if len(problems) != 1 {
		t.Fatalf("got %d problem(s), want exactly 1 — a repeated version is reported instead\n"+
			"of a digest verdict on either copy:\n%s", len(problems), strings.Join(problems, "\n"))
	}
	if !strings.Contains(problems[0], "appears 2 times") {
		t.Errorf("the report does not say the heading is repeated:\n%s", problems[0])
	}
}

// What a reader actually gets. `012d65b0` polluted one section and left ten
// alone, and a report that named all eleven would be no better than the diff
// nobody ran.
func TestTheReportNamesOnlyTheSectionWhoseDigestMoved(t *testing.T) {
	polluted := splice(t, changelog(t), "## [0.7.0]", "- **A bullet a rebase dropped in here.**")

	problems := releasedSumProblems(releasedSections(polluted), recorded(t))

	if len(problems) != 1 {
		t.Fatalf("got %d problem(s), want exactly 1:\n%s", len(problems), strings.Join(problems, "\n"))
	}
	if !strings.Contains(problems[0], "`## [0.7.0]`") {
		t.Errorf("the report does not name the section a reader has to fix:\n%s", problems[0])
	}
	if !strings.Contains(problems[0], "[Unreleased]") {
		t.Errorf("the report does not say where the entry belongs instead:\n%s", problems[0])
	}
}

// The cut path. A new heading with no digest must say what to do, because the
// person who sees it is mid-release and the answer is one command.
func TestTheReportAsksForARegenerationWhenASectionHasNoRecord(t *testing.T) {
	sections := releasedSections(changelog(t))
	want := recorded(t)
	delete(want, "0.7.0")

	problems := releasedSumProblems(sections, want)

	if len(problems) != 1 {
		t.Fatalf("got %d problem(s), want exactly 1:\n%s", len(problems), strings.Join(problems, "\n"))
	}
	if !strings.Contains(problems[0], "no digest in") {
		t.Errorf("a section with no record is not reported as one:\n%s", problems[0])
	}
	if !strings.Contains(problems[0], "-update-sums") {
		t.Errorf("the report does not give the command that fixes it:\n%s", problems[0])
	}
}

// The other direction, which is what a reverted or renamed heading looks like.
// Without this arm the table could name a section nobody has and stay green.
func TestTheReportNamesARecordWhoseSectionIsGone(t *testing.T) {
	sections := releasedSections(changelog(t))
	want := recorded(t)
	want["9.9.9"] = strings.Repeat("0", 64)

	problems := releasedSumProblems(sections, want)

	if len(problems) != 1 {
		t.Fatalf("got %d problem(s), want exactly 1:\n%s", len(problems), strings.Join(problems, "\n"))
	}
	if !strings.Contains(problems[0], "9.9.9") {
		t.Errorf("a record with no section is not reported as one:\n%s", problems[0])
	}
}
