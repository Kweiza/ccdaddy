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
//
// The digest covers the heading and its body and EXCLUDES link definitions.
// changelog_links_test.go owns that block — every released heading has a
// definition, and [Unreleased] points at the newest release — and the block
// legitimately grows on a cut and on a backfill. Two tests, two questions, no
// overlap.

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

// releasedDigests returns one digest per `## [x.y.z]` heading, newest first.
func releasedDigests(body string) (versions []string, digests map[string]string) {
	digests = map[string]string{}
	lines := strings.Split(body, "\n")

	// Where each released heading starts, and where the section ends: at the
	// next `## ` heading of any kind, or at end of file.
	type span struct {
		version    string
		start, end int
	}
	var spans []span
	for i, line := range lines {
		m := versionHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if n := len(spans); n > 0 {
			spans[n-1].end = i
		}
		spans = append(spans, span{version: m[1], start: i, end: len(lines)})
	}

	for _, s := range spans {
		var kept []string
		for _, line := range lines[s.start:s.end] {
			// The one exclusion, and the reason is above: this block is
			// changelog_links_test.go's to hold.
			if linkDefinition.MatchString(line) {
				continue
			}
			kept = append(kept, line)
		}
		sum := sha256.Sum256([]byte(strings.Join(kept, "\n")))
		versions = append(versions, s.version)
		digests[s.version] = hex.EncodeToString(sum[:])
	}
	return versions, digests
}

func TestNoReleasedSectionHasChangedSinceItWasCut(t *testing.T) {
	body := changelog(t)
	versions, digests := releasedDigests(body)

	if *updateSums {
		var b strings.Builder
		for _, v := range versions {
			fmt.Fprintf(&b, "%s  %s\n", digests[v], v)
		}
		if err := os.WriteFile(filepath.FromSlash(releasedSums), []byte(b.String()), 0o644); err != nil {
			t.Fatalf("writing %s: %v", releasedSums, err)
		}
		t.Logf("wrote %d digest(s) to %s; read the diff before committing it", len(versions), releasedSums)
		return
	}

	recorded, err := os.ReadFile(filepath.FromSlash(releasedSums))
	if err != nil {
		t.Fatalf("reading %s: %v", releasedSums, err)
	}
	want := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(recorded)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("%s: malformed line %q, want `<sha256>  <version>`", releasedSums, line)
		}
		want[fields[1]] = fields[0]
	}

	for _, v := range versions {
		switch {
		case want[v] == "":
			t.Errorf("`## [%s]` has no digest in %s.\n"+
				"If you are cutting a release, regenerate the file in the same commit.", v, releasedSums)
		case want[v] != digests[v]:
			t.Errorf("`## [%s]` no longer matches the bytes it was released with.\n"+
				"  recorded %s\n  now      %s\n"+
				"A released section is immutable. The usual cause is a rebase across a release\n"+
				"cut: git merges CHANGELOG.md cleanly and drops your new entry inside the\n"+
				"released section instead of `[Unreleased]`. Move it up to `[Unreleased]`.",
				v, want[v], digests[v])
		}
	}
	for v := range want {
		if digests[v] == "" {
			t.Errorf("%s records `## [%s]`, which CHANGELOG.md no longer has.", releasedSums, v)
		}
	}
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

// The accident itself, in one line. `012d65b0` did it with 117.
func TestABulletDroppedIntoAReleasedSectionChangesItsDigest(t *testing.T) {
	body := changelog(t)
	_, before := releasedDigests(body)

	polluted := splice(t, body, "## [0.7.0]", "- **A bullet a rebase dropped in here.**")
	_, after := releasedDigests(polluted)

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
	_, before := releasedDigests(body)

	edited := splice(t, body, "## [Unreleased]", "\n### Added\n\n- **An ordinary new entry.**")
	versions, after := releasedDigests(edited)

	for _, v := range versions {
		if after[v] != before[v] {
			t.Errorf("`## [%s]` moved when only `[Unreleased]` was edited: %s -> %s",
				v, before[v], after[v])
		}
	}
}

// The exclusion this file decided, pinned in the direction that can go wrong
// silently. The block grows on every cut and grew once on a backfill —
// `main` gained link lines for older releases long after they shipped — so a
// digest that covered them would have gone red on a legitimate change and
// taught the next reader to regenerate rather than to look.
func TestALinkDefinitionAddedInsideAReleasedSectionMovesNoDigest(t *testing.T) {
	body := changelog(t)
	versions, before := releasedDigests(body)

	edited := splice(t, body, "## [0.7.0]",
		"[0.0.1]: https://github.com/Kweiza/ccdaddy/releases/tag/v0.0.1")
	_, after := releasedDigests(edited)

	for _, v := range versions {
		if after[v] != before[v] {
			t.Errorf("`## [%s]` moved when only a link definition was added: %s -> %s\n"+
				"changelog_links_test.go owns that block; this one must not.",
				v, before[v], after[v])
		}
	}
}
