package release

import "testing"

const goodSums = "" +
	"aa" + "00000000000000000000000000000000000000000000000000000000000000" + "  ccdad-linux-amd64\n" +
	"bb" + "00000000000000000000000000000000000000000000000000000000000000" + "  ccdad-linux-amd64.exe\n" +
	"cc" + "00000000000000000000000000000000000000000000000000000000000000" + "  ccdad-darwin-arm64\n" +
	"dd" + "00000000000000000000000000000000000000000000000000000000000000" + "  LICENSE\n" +
	"ee" + "00000000000000000000000000000000000000000000000000000000000000" + "  NOTICE\n" +
	"ff" + "00000000000000000000000000000000000000000000000000000000000000" + "  THIRD-PARTY-LICENSES.txt\n"

// The shape check is what catches a proxy's HTML error page before its bytes
// are treated as data. It is a per-line test on purpose: the release's sums
// file also covers LICENSE, NOTICE and THIRD-PARTY-LICENSES.txt, so nothing may
// depend on the line count or the ordering.
func TestSumsLookLikeSums(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want bool
	}{
		{"the real thing", goodSums, true},
		{"one row", "aa00000000000000000000000000000000000000000000000000000000000000  ccdad-linux-amd64\n", true},
		// A row that is not on the first line only matches under the (?m)
		// flag: without it, Go's regexp anchors "^" to the start of the
		// whole input rather than the start of each line, and this fixture
		// would go from true to false.
		{"a valid row that is not on the first line",
			"junk\naa00000000000000000000000000000000000000000000000000000000000000  ccdad-linux-amd64\n", true},
		{"an html error page", "<!doctype html>\n<html><body>404</body></html>\n", false},
		{"empty", "", false},
		{"one space", "aa00000000000000000000000000000000000000000000000000000000000000 ccdad-linux-amd64\n", false},
		{"a tab", "aa00000000000000000000000000000000000000000000000000000000000000\tccdad-linux-amd64\n", false},
		{"uppercase hex", "AA00000000000000000000000000000000000000000000000000000000000000  ccdad-linux-amd64\n", false},
		{"63 digits", "a0000000000000000000000000000000000000000000000000000000000000  ccdad-linux-amd64\n", false},
		// {64} without an upper bound (e.g. {64,}) still matches the first 64
		// of these 65 digits followed by the two spaces one further along,
		// which is exactly what this row supplies: a false positive that
		// only a fixture past 64 digits can expose.
		{"65 digits", "aa000000000000000000000000000000000000000000000000000000000000000  ccdad-linux-amd64\n", false},
		{"leading text on the line", "note: aa00000000000000000000000000000000000000000000000000000000000000  ccdad-linux-amd64\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := SumsLookLikeSums([]byte(c.in)); got != c.want {
				t.Errorf("SumsLookLikeSums() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestExpectedHash(t *testing.T) {
	for _, c := range []struct {
		name  string
		sums  string
		asset string
		want  string // "" means not listed
	}{
		{"the row for this asset", goodSums, "ccdad-linux-amd64",
			"aa00000000000000000000000000000000000000000000000000000000000000"},
		{"the .exe row is its own row", goodSums, "ccdad-linux-amd64.exe",
			"bb00000000000000000000000000000000000000000000000000000000000000"},
		{"a notice file is inert but present", goodSums, "NOTICE",
			"ee00000000000000000000000000000000000000000000000000000000000000"},
		{"a target this release does not carry", goodSums, "ccdad-windows-arm64.exe", ""},
		{"a prefix of a listed name", goodSums, "ccdad-linux-amd", ""},
		{"a name with a regex metacharacter is data, not a pattern", goodSums, "ccdad-linux-amd64.", ""},
		// The dot sits mid-string here, where an unescaped "." can match the
		// "4" of "ccdad-linux-amd64" and turn a lookup for a name nobody
		// published into a hit. The trailing-dot row above pins the "$"
		// anchor instead: its wildcard position has nothing after it to
		// match, so it cannot tell QuoteMeta apart from its absence.
		{"a name with a regex metacharacter in the middle is data, not a pattern", goodSums, "ccdad.linux-amd64", ""},
		{"one space", "aa00000000000000000000000000000000000000000000000000000000000000 ccdad-linux-amd64\n",
			"ccdad-linux-amd64", ""},
		{"65 digits", "aa000000000000000000000000000000000000000000000000000000000000000  ccdad-linux-amd64\n",
			"ccdad-linux-amd64", ""},
		{"uppercase hex", "AA00000000000000000000000000000000000000000000000000000000000000  ccdad-linux-amd64\n",
			"ccdad-linux-amd64", ""},
		{"trailing text after the name", "aa00000000000000000000000000000000000000000000000000000000000000  ccdad-linux-amd64 extra\n",
			"ccdad-linux-amd64", ""},
		{"a CRLF file", "aa00000000000000000000000000000000000000000000000000000000000000  ccdad-linux-amd64\r\n",
			"ccdad-linux-amd64", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ExpectedHash([]byte(c.sums), c.asset)
			if c.want == "" {
				if ok {
					t.Fatalf("ExpectedHash(%q) = %q, want not listed", c.asset, got)
				}
				return
			}
			if !ok {
				t.Fatalf("ExpectedHash(%q) reported not listed", c.asset)
			}
			if got != c.want {
				t.Errorf("ExpectedHash(%q) = %q, want %q", c.asset, got, c.want)
			}
		})
	}
}

// A well-shaped file that does not list this platform and a file that is not a
// sums file at all are DIFFERENT refusals, with different remedies. Nothing
// downstream can tell them apart if one function answers both.
func TestShapeAndListingAreSeparateQuestions(t *testing.T) {
	html := []byte("<!doctype html>\n")
	if SumsLookLikeSums(html) {
		t.Fatal("an HTML page passed the shape check")
	}
	if _, ok := ExpectedHash([]byte(goodSums), "ccdad-windows-arm64.exe"); ok {
		t.Fatal("a target this release does not carry was reported as listed")
	}
	if !SumsLookLikeSums([]byte(goodSums)) {
		t.Fatal("a real sums file failed the shape check, so 'not listed' can never be reached")
	}
	// Shape asks about the FILE; ExpectedHash asks about one asset's ROW. A
	// file that lists only some other asset is well-shaped and must still
	// pass here, or SumsLookLikeSums could be built by delegating to
	// ExpectedHash for one fixed asset name -- collapsing the two questions
	// this test exists to keep apart, and reporting a real (if foreign)
	// sums file as though it were the html/empty/malformed case above.
	onlyAnotherAsset := []byte("aa00000000000000000000000000000000000000000000000000000000000000  ccdad-windows-arm64.exe\n")
	if !SumsLookLikeSums(onlyAnotherAsset) {
		t.Fatal("a well-shaped file naming only another asset failed the shape check")
	}
}
