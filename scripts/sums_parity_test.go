package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/release"
)

// The rules for reading sha256sums.txt exist three times in this repository: in
// Go (internal/release), in shell (install.sh) and in PowerShell (install.ps1).
// Three copies of a security rule is not duplication to tidy away — each of the
// three runs on a machine the other two cannot reach — but it IS something one
// test has to hold together, because nothing else can. A Go verifier that
// accepted a row install.sh refuses would let `ccdad update` install what a
// fresh install would not, and neither implementation's own tests can see that.
//
// One corpus, three implementations, one expected answer per row.

// parityAsset is the asset every row below is looked up by, and it is one of
// install.sh's four on purpose: $ASSET reaches install.sh's grep UNQUOTED, so a
// name carrying a regex metacharacter would mean something there that it does
// not mean under Go's regexp.QuoteMeta or PowerShell's [regex]::Escape. That
// cannot arise in production — install.sh derives $ASSET from uname(1) and none
// of the four names it can produce contains one — so the metacharacter case is
// pinned per implementation instead, by
// TestInstallPs1ExtractsOnlyAnExactlyMatchingSumsLine's "the dot treated as a
// wildcard" row and by internal/release's own "a name with a regex
// metacharacter in the middle is data, not a pattern".
const parityAsset = "ccdad-linux-amd64"

type parityCase struct {
	name string
	// body is a whole sha256sums.txt, written to a file and then handed to each
	// implementation the way that implementation takes it.
	body string
	// wantShape is the shape check's answer; wantHash is the digest the lookup
	// must return, with "" meaning "not listed".
	wantShape bool
	wantHash  string
}

// parityCorpus is the generated corpus. The digests are computed rather than
// typed, so a row cannot pass by two implementations agreeing on a constant
// somebody mistyped in the test.
//
// There is deliberately NO CRLF row, and the reason is that the three
// implementations genuinely disagree about one and a shared row would therefore
// be asserting something false. Go's (?m)$ and GNU grep both refuse a line
// whose asset name is followed by a carriage return; ugrep matches it, so the
// shell answer is not even portable between grep implementations; and
// install.ps1 reads its lines with Get-Content, which strips \r\n as well as
// \n, so PowerShell never sees the carriage return and accepts the file. Each
// implementation's own table pins its own behaviour; this corpus covers what
// all three must agree on.
func parityCorpus() []parityCase {
	files := map[string][]byte{}
	for _, name := range unixAssets {
		files[name] = []byte("pretend " + name + " bytes\n")
	}
	files["LICENSE"] = []byte("license\n")
	files["NOTICE"] = []byte("notice\n")
	files["THIRD-PARTY-LICENSES.txt"] = []byte("notices\n")
	whole := string(sumsFor(files))

	d := sha256.Sum256(files[parityAsset])
	hex64 := hex.EncodeToString(d[:])
	row := hex64 + "  " + parityAsset

	return []parityCase{
		// A whole release, notice rows and all: they are inert under anchored
		// matching in all three, which is the point of anchoring rather than
		// counting lines.
		{"a whole release", whole, true, hex64},
		{"one row", row + "\n", true, hex64},
		{"no trailing newline", row, true, hex64},
		{"one space instead of two", hex64 + " " + parityAsset + "\n", false, ""},
		{"a tab instead of two spaces", hex64 + "\t" + parityAsset + "\n", false, ""},
		// Three spaces still satisfies the SHAPE check, which only asks for two
		// — and still fails the lookup, because the anchored row has the asset
		// name immediately after them. The two answers differing is the case.
		{"three spaces", hex64 + "   " + parityAsset + "\n", true, ""},
		{"uppercase hex", strings.ToUpper(hex64) + "  " + parityAsset + "\n", false, ""},
		{"63 digits", hex64[:63] + "  " + parityAsset + "\n", false, ""},
		{"65 digits", hex64 + "a  " + parityAsset + "\n", false, ""},
		{"a prefixed line", "sha256:" + row + "\n", false, ""},
		{"only a longer neighbour is listed", hex64 + "  " + parityAsset + ".exe\n", true, ""},
		{"trailing text after the name", row + " extra\n", true, ""},
		{"a well-formed line hides an unanchored one",
			hex64 + "  ccdad-darwin-arm64\n" + "sha256:" + row + "\n", true, ""},
		{"an html error page", "<html><head><title>403 Forbidden</title></head></html>\n", false, ""},
		{"empty", "", false, ""},
	}
}

// goSums is internal/release's answer.
func goSums(body, asset string) (bool, string) {
	shape := release.SumsLookLikeSums([]byte(body))
	hash, ok := release.ExpectedHash([]byte(body), asset)
	if !ok {
		hash = ""
	}
	return shape, hash
}

// installShPatterns reads install.sh's two grep expressions OUT of install.sh,
// rather than spelling a second copy of them here.
//
// That is the difference between a test that compares Go against install.sh and
// one that compares Go against somebody's memory of install.sh: if the
// installer's rule drifts, the corpus below goes red, instead of staying green
// against a copy nobody updated. Each expression must appear exactly once, and
// not finding it is a failure with a sentence rather than a silent fallback.
func installShPatterns(t *testing.T) (shape, row string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	one := func(re *regexp.Regexp, what string) string {
		m := re.FindAllStringSubmatch(string(body), -1)
		if len(m) != 1 {
			t.Fatalf("install.sh has %d %s expressions, want exactly 1 — this test can no longer "+
				"drive the rule the installer actually runs", len(m), what)
		}
		return m[0][1]
	}
	return one(regexp.MustCompile(`grep -Eq '([^']*)'`), "shape"),
		one(regexp.MustCompile(`grep -E "([^"]*)"`), "row lookup")
}

// shellSums is install.sh's answer: its own two expressions, against a real
// file, through a real shell. The row lookup keeps the whole pipeline — head,
// cut and the `|| true` — because the pipeline is the rule and not just the
// pattern, and it runs under bash so that ${ASSET} expands and \$ unescapes
// exactly as they do in the installer.
func shellSums(t *testing.T, shape, row, path, asset string) (bool, string) {
	t.Helper()
	env := append(os.Environ(), "ASSET="+asset, "SUMS="+path)

	c := exec.Command("bash", "-c", `grep -Eq '`+shape+`' "$SUMS"`)
	c.Env = env
	looksLikeSums := c.Run() == nil

	c = exec.Command("bash", "-c", `grep -E "`+row+`" "$SUMS" | head -n 1 | cut -d' ' -f1 || true`)
	c.Env = env
	out, err := c.Output()
	if err != nil {
		t.Fatalf("running install.sh's row lookup: %v", err)
	}
	return looksLikeSums, strings.TrimSpace(string(out))
}

// ps1Sums is install.ps1's answer, reached the way install.ps1 reaches it:
// Get-Content over the file, then the two functions. Handing the functions a
// hand-built array instead would test a call install.ps1 never makes.
func ps1Sums(t *testing.T, path, asset string) (bool, string) {
	t.Helper()
	out := dotSource(t, "$lines = @(Get-Content -LiteralPath '"+path+"')\n"+
		"if (Test-CcdadSumsShape -Lines $lines) { 'SHAPE=1' } else { 'SHAPE=0' }\n"+
		"$h = Get-CcdadExpectedHash -Lines $lines -Asset '"+asset+"'\n"+
		"if ($null -eq $h) { 'HASH=' } else { \"HASH=$h\" }")

	var shape bool
	var hash string
	seen := 0
	for _, line := range strings.Split(out, "\n") {
		switch line = strings.TrimSpace(line); {
		case line == "SHAPE=1":
			shape, seen = true, seen+1
		case line == "SHAPE=0":
			shape, seen = false, seen+1
		case strings.HasPrefix(line, "HASH="):
			hash, seen = strings.TrimPrefix(line, "HASH="), seen+1
		}
	}
	if seen != 2 {
		t.Fatalf("install.ps1 answered %q, want one SHAPE= line and one HASH= line", out)
	}
	return shape, hash
}

// powershellPath finds an interpreter without skipping the test.
//
// install_ps1_test.go's powershell(t) is the skipping variant, and it is right
// for a test whose only subject is install.ps1. This one is for a test with
// three subjects, where a machine without PowerShell must cost one leg rather
// than the whole comparison.
func powershellPath() string {
	for _, name := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func TestSumsRulesAgreeAcrossGoAndBothInstallers(t *testing.T) {
	posix := runtime.GOOS != "windows"
	for _, tool := range []string{"bash", "grep", "head", "cut"} {
		if _, err := exec.LookPath(tool); err != nil {
			posix = false
		}
	}
	var shapePat, rowPat string
	if posix {
		shapePat, rowPat = installShPatterns(t)
	}
	pwsh := powershellPath()
	if !posix && pwsh == "" {
		t.Skip("neither a POSIX shell with grep nor PowerShell is on this machine, so there is no " +
			"second implementation to compare Go against, and a green result would mean nothing")
	}

	dir := t.TempDir()
	for i, c := range parityCorpus() {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("sums-%02d.txt", i))
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}

			if shape, hash := goSums(c.body, parityAsset); shape != c.wantShape || hash != c.wantHash {
				t.Errorf("internal/release: shape=%v hash=%q, want shape=%v hash=%q",
					shape, hash, c.wantShape, c.wantHash)
			}
			if posix {
				if shape, hash := shellSums(t, shapePat, rowPat, path, parityAsset); shape != c.wantShape || hash != c.wantHash {
					t.Errorf("install.sh's rules: shape=%v hash=%q, want shape=%v hash=%q — a divergence "+
						"here is a verification hole rather than a formatting difference",
						shape, hash, c.wantShape, c.wantHash)
				}
			}
			if pwsh != "" {
				if shape, hash := ps1Sums(t, path, parityAsset); shape != c.wantShape || hash != c.wantHash {
					t.Errorf("install.ps1's rules: shape=%v hash=%q, want shape=%v hash=%q — a divergence "+
						"here is a verification hole rather than a formatting difference",
						shape, hash, c.wantShape, c.wantHash)
				}
			}
		})
	}
}
