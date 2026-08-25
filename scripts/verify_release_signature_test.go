package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// What scripts/verify-release-signature.sh adds on top of minisign, and why it
// needs a test of its own: minisign says a signature is authentic, and this
// says it is authentic FOR THE RELEASE ASKED ABOUT. Those come apart, and the
// case where they do is signed correctly by the right key, so nothing in the
// artifact catches it.
//
// The tool is stubbed rather than required. A test that skipped when minisign
// is absent would be a green light with nothing behind it on every machine
// without it — and the rule under test is this script's, not the tool's. The
// real tool is exercised too, at the bottom, but only as an addition: no
// assertion here depends on it being installed.

// stubMinisign puts a fake `minisign` first on PATH for one test. It prints
// what the real tool prints on the outcome asked for, and nothing else — the
// script reads two things from it, its exit code and its "Trusted comment:"
// line, and both are here.
func stubMinisign(t *testing.T, stdout string, code int) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(out, []byte(stdout), 0o644); err != nil {
		t.Fatalf("writing the canned minisign output: %v", err)
	}
	// Through a file, so a tab in the trusted comment survives verbatim — the
	// separator the field rule is built on would not survive being pasted into
	// a script body.
	body := "#!/usr/bin/env bash\ncat " + strconv.Quote(out) + "\nexit " + strconv.Itoa(code) + "\n"
	path := filepath.Join(dir, "minisign")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("writing the minisign stand-in: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// verifySignature runs the script and returns everything it said plus its exit
// code.
func verifySignature(t *testing.T, args ...string) (string, int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is an extensionless shebang script, which only the bash layer resolves")
	}
	script, err := filepath.Abs("verify-release-signature.sh")
	if err != nil {
		t.Fatalf("resolving verify-release-signature.sh: %v", err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running verify-release-signature.sh: %v\n%s", err, out)
		}
		code = exit.ExitCode()
	}
	return string(out), code
}

const verifiedHeader = "Signature and comment signature verified\n"

func TestVerifyReleaseSignatureAcceptsTheReleaseItNames(t *testing.T) {
	stubMinisign(t, verifiedHeader+"Trusted comment: file:sha256sums.txt\tccdaddy:v1.2.3\n", 0)

	out, code := verifySignature(t, "v1.2.3", "key.pub", "sums.txt", "sums.txt.minisig")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "names v1.2.3") {
		t.Errorf("the script said %q, which does not report which release it accepted", out)
	}
}

// The case the whole script exists for, and the one a substring test gets
// wrong. v1.2.30 is a real release one patch series from v1.2.3, signed by the
// same key, and `ccdaddy:v1.2.3` is a substring of `ccdaddy:v1.2.30`. An origin
// that chooses what to serve can answer "the latest is v1.2.3" and hand back
// the genuine v1.2.30 pair — or the reverse, which is the downgrade.
func TestVerifyReleaseSignatureRefusesANeighbouringRelease(t *testing.T) {
	stubMinisign(t, verifiedHeader+"Trusted comment: file:sha256sums.txt\tccdaddy:v1.2.30\n", 0)

	out, code := verifySignature(t, "v1.2.3", "key.pub", "sums.txt", "sums.txt.minisig")
	if code == 0 {
		t.Fatalf("a correctly signed v1.2.30 was accepted as v1.2.3\n%s", out)
	}
	if !strings.Contains(out, "does not name v1.2.3") {
		t.Errorf("the refusal does not say what was wrong: %q", out)
	}
}

// The dot is a wildcard to grep and a literal to everyone else, and every tag
// this project cuts has two of them. Without -F, `ccdaddy:v1.2.3` matches a
// trusted comment naming `ccdaddy:v1x2x3` — a release that does not exist,
// which is the point: the tag has to be compared as data.
func TestVerifyReleaseSignatureTreatsTheTagAsDataNotAsAPattern(t *testing.T) {
	stubMinisign(t, verifiedHeader+"Trusted comment: file:sha256sums.txt\tccdaddy:v1x2x3\n", 0)

	out, code := verifySignature(t, "v1.2.3", "key.pub", "sums.txt", "sums.txt.minisig")
	if code == 0 {
		t.Fatalf("the tag was read as a regular expression, so ccdaddy:v1x2x3 satisfied v1.2.3\n%s", out)
	}
}

// Exactly one field, not at least one. A comment carrying the name twice is not
// something this project's signer can produce, so a comment that does is one
// somebody else assembled — and "at least one" is the reading under which
// appending a second field is free.
func TestVerifyReleaseSignatureRefusesTheNameTwice(t *testing.T) {
	stubMinisign(t, verifiedHeader+"Trusted comment: ccdaddy:v1.2.3\tccdaddy:v1.2.3\n", 0)

	if out, code := verifySignature(t, "v1.2.3", "key.pub", "sums.txt", "sums.txt.minisig"); code == 0 {
		t.Fatalf("a trusted comment naming the release twice was accepted\n%s", out)
	}
}

// A verified signature with no trusted comment at all is authentic and says
// nothing about which release it belongs to, which is the same failure as
// naming the wrong one.
func TestVerifyReleaseSignatureRefusesAVerifiedFileWithNoTrustedComment(t *testing.T) {
	stubMinisign(t, verifiedHeader, 0)

	out, code := verifySignature(t, "v1.2.3", "key.pub", "sums.txt", "sums.txt.minisig")
	if code == 0 {
		t.Fatalf("a signature carrying no release name was accepted\n%s", out)
	}
	if !strings.Contains(out, "no trusted comment") {
		t.Errorf("the refusal does not say what was missing: %q", out)
	}
}

// The direction that must never be softened: if minisign refuses, the script
// refuses, and it does not go on to read a trusted comment out of whatever the
// tool printed on its way out.
func TestVerifyReleaseSignatureFailsWhenMinisignFails(t *testing.T) {
	stubMinisign(t, "Signature verification failed\nTrusted comment: file:sha256sums.txt\tccdaddy:v1.2.3\n", 1)

	out, code := verifySignature(t, "v1.2.3", "key.pub", "sums.txt", "sums.txt.minisig")
	if code == 0 {
		t.Fatalf("minisign refused the signature and the script reported success\n%s", out)
	}
	if strings.Contains(out, "verifies against") {
		t.Errorf("the script claimed success over a failed verification: %q", out)
	}
}

// An empty tag is refused rather than treated as "any release". The caller most
// likely to pass one is a workflow whose upstream step produced nothing, and as
// a skip it would switch off the only check binding a pair to its release.
func TestVerifyReleaseSignatureRefusesAnEmptyTag(t *testing.T) {
	stubMinisign(t, verifiedHeader+"Trusted comment: file:sha256sums.txt\tccdaddy:v1.2.3\n", 0)

	if out, code := verifySignature(t, "", "key.pub", "sums.txt", "sums.txt.minisig"); code == 0 {
		t.Fatalf("an empty tag was accepted as naming a release\n%s", out)
	}
}

// The stub above proves this script's rule. It cannot prove that the rule is
// applied to what the real tool really prints — the header wording, the
// "Trusted comment: " prefix, the tab. This closes that, using the golden
// fixtures internal/relsign keeps: real signatures from a real minisign 0.11.
//
// It is an ADDITION, never the only coverage: every assertion above runs
// whether or not minisign is installed, so this skipping costs nothing. That is
// the only shape in which a skip is honest here.
func TestVerifyReleaseSignatureAgainstTheRealToolAndTheGoldenFixtures(t *testing.T) {
	if _, err := exec.LookPath("minisign"); err != nil {
		t.Skip("minisign is not installed; the rule itself is covered by the tests above, which stub it")
	}
	data := func(name string) string {
		p, err := filepath.Abs(filepath.Join("..", "internal", "relsign", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name string
		tag  string
		key  string
		sig  string
		want int
	}{
		{"the pair it was issued for", "v1.2.3", "release.pub", "sums.txt.minisig", 0},
		// Correctly signed, by the right key, for the wrong release.
		{"a neighbouring release", "v1.2.3", "release.pub", "sums.txt.v1230.minisig", 1},
		// Correctly signed by a key that is not the trust root.
		{"a signature from another key", "v1.2.3", "release.pub", "sums.txt.other.minisig", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, code := verifySignature(t, tt.tag, data(tt.key), data("sums.txt"), data(tt.sig))
			if code != tt.want {
				t.Errorf("exit = %d, want %d\n%s", code, tt.want, out)
			}
		})
	}
}
