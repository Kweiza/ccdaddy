package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The first test in this repository that reads CHANGELOG.md.
//
// Nothing did before, and that is why its reference block had not moved since
// 0.2.0: five releases were cut, each adding a `## [x.y.z]` heading, and none
// of them added the link definition that heading needs. The headings rendered
// as literal bracketed text and `[Unreleased]` offered a diff five releases
// wide, on the file a reader opens to find out what changed.
//
// It is a link check and not a spell check on purpose. What went stale here is
// structural — a heading and a definition that have to be added together — and
// a structural rule is the only kind a test can hold.

var (
	// A released heading. The date is not matched: it varies in format across
	// this file's history and is not what this test is about.
	versionHeading = regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)\]`)
	// A reference definition, which markdown allows anywhere in the file. The
	// target is captured rather than merely required: the second test below
	// reads it, and a pattern that matched only its first byte would hand that
	// test the string "h".
	linkDefinition = regexp.MustCompile(`(?m)^\[([^\]]+)\]:\s+(\S+)`)
)

func changelog(t *testing.T) string {
	t.Helper()
	// The test binary runs in scripts/, and the file is one level up.
	body, err := os.ReadFile(filepath.Join("..", "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("reading CHANGELOG.md: %v", err)
	}
	return string(body)
}

func definitions(body string) map[string]bool {
	out := map[string]bool{}
	for _, m := range linkDefinition.FindAllStringSubmatch(body, -1) {
		out[m[1]] = true
	}
	return out
}

func TestEveryReleasedHeadingHasALinkDefinition(t *testing.T) {
	body := changelog(t)
	defs := definitions(body)

	var missing []string
	for _, m := range versionHeading.FindAllStringSubmatch(body, -1) {
		if !defs[m[1]] {
			missing = append(missing, m[1])
		}
	}
	if len(missing) > 0 {
		t.Errorf("released heading(s) with no link definition: %s\n"+
			"Each `## [x.y.z]` needs a matching `[x.y.z]: https://.../compare/...` at the\n"+
			"bottom of the file, or the heading renders as literal text and links nowhere.",
			strings.Join(missing, ", "))
	}
}

// The definition that goes wrong silently. A heading with no definition at
// least renders visibly as brackets; [Unreleased] pointing at an old tag
// renders as a working link to a diff that is five releases too wide, which
// nobody notices because it looks right.
func TestUnreleasedComparesAgainstTheNewestReleasedHeading(t *testing.T) {
	body := changelog(t)

	headings := versionHeading.FindAllStringSubmatch(body, -1)
	if len(headings) == 0 {
		t.Fatal("no released headings in CHANGELOG.md")
	}
	// Keep a Changelog orders newest first, so the first heading is the newest
	// release. Deriving it from the file rather than from git keeps this test
	// off the network and off the tag list.
	newest := headings[0][1]

	var unreleased string
	for _, m := range linkDefinition.FindAllStringSubmatch(body, -1) {
		if m[1] == "Unreleased" {
			unreleased = m[2]
		}
	}
	if unreleased == "" {
		t.Fatal("CHANGELOG.md has no [Unreleased] link definition")
	}
	if want := "compare/v" + newest + "...HEAD"; !strings.HasSuffix(unreleased, want) {
		t.Errorf("[Unreleased] is %q, want it to end in %q.\n"+
			"It compares against the newest released heading, %s, so the diff it offers is\n"+
			"what is actually unreleased rather than every commit since some older tag.",
			unreleased, want, newest)
	}
}
