package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installSmokeWorkflow(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "install-smoke.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// signatureJob returns the body of the one job that verifies the published
// signature, so the assertions below cannot be satisfied by text somewhere else
// in a workflow that is nearly seven hundred lines long.
//
// Jobs are two-space keys under `jobs:`, so the block runs to the next line
// that starts with exactly two spaces and a name. Parsing this much YAML by
// hand is the right trade here: a parser would be a third-party module, and
// this repository's go.mod is deliberately closed.
func signatureJob(t *testing.T) string {
	t.Helper()
	yml := installSmokeWorkflow(t)
	const head = "\n  signature:\n"
	i := strings.Index(yml, head)
	if i < 0 {
		t.Fatal("install-smoke.yml has no `signature:` job — nothing published is ever checked against the key this tree commits")
	}
	rest := yml[i+1:]
	for off := 1; ; {
		j := strings.Index(rest[off:], "\n  ")
		if j < 0 {
			return rest
		}
		off += j + 1
		line := rest[off:]
		if k := strings.IndexByte(line, '\n'); k >= 0 {
			line = line[:k]
		}
		// Two spaces then a key, not four: four is a step or a field inside
		// this job.
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") && strings.HasSuffix(strings.TrimRight(line, " "), ":") {
			return rest[:off]
		}
	}
}

// The release job signs, and internal/relsign verifies, and both are covered by
// tests that run on every push. What none of them can show is that the artifact
// which reached the releases page is one a stranger's minisign accepts against
// the key committed here. That crosses five boundaries no in-repo test spans —
// the secret in Actions, the signer in CI, the upload, the committed key, and a
// tool this repository did not write — and until this job existed the first
// proof of it was going to be somebody's failed upgrade.
func TestInstallSmokeVerifiesThePublishedSignature(t *testing.T) {
	job := signatureJob(t)

	if !strings.Contains(job, "needs: resolve") {
		t.Error("the signature job must take its tag from the resolve job, or it verifies a release nobody named")
	}
	if !strings.Contains(job, "ccdaddy.pub") {
		t.Error("the signature job must verify against ccdaddy.pub, the key THIS tree commits")
	}
	// Verifying against a key fetched from the same release would be a round
	// trip: whatever signed it would be accepted by whatever shipped with it.
	if strings.Contains(job, "releases/download/$TAG/ccdaddy.pub") {
		t.Error("the key must come from the checkout, never from the release under test")
	}
}

// The item this job answers named the tool choice as the whole point: our own
// verifier accepting our own signature proves less, because a signer and a
// verifier wrong in the same way pass every round trip. So the evidence has to
// come from a binary this repository did not produce.
func TestInstallSmokeUsesTheStockMinisignRatherThanOurOwn(t *testing.T) {
	job := signatureJob(t)

	if !strings.Contains(job, "apt-get download minisign") {
		t.Error("the signature job must fetch the stock minisign; a verifier from this tree cannot be the evidence")
	}
	// The rule is that the VERIFIER is not ours, which is narrower than "no
	// file of ours is touched" — the job reads ccdaddy.pub out of the checkout
	// and must. What it may not do is build or run any of this repository's
	// code, because then the thing under test would be checking itself.
	for _, ours := range []string{"go run", "go build", "go test", "minisign-sign"} {
		if strings.Contains(job, ours) {
			t.Errorf("the signature job builds or runs this repository's own code (%q); the point is a tool we did not write", ours)
		}
	}
}

// The release workflow calls this one with `needs: publish`, and a step that
// dies takes the job with it. That is the behaviour wanted here: a verification
// leg that warns is a verification leg that is scrolled past.
func TestInstallSmokeSignatureLegFailsLoudly(t *testing.T) {
	job := signatureJob(t)

	if strings.Contains(job, "continue-on-error") {
		t.Error("continue-on-error turns the one proof the pipeline works into a notice")
	}
	for _, line := range strings.Split(job, "\n") {
		if !strings.Contains(line, "minisign -V") {
			continue
		}
		if strings.Contains(line, "|| true") || strings.Contains(line, "|| echo") {
			t.Errorf("a verification that cannot fail is not a verification: %q", strings.TrimSpace(line))
		}
	}
}

// minisign answers "is this signature authentic", never "is this the release I
// asked for". The trusted comment carries the release name and is covered by
// the second signature, so it is trustworthy once minisign returns — but an
// origin that chooses what to serve can hand back an older release's genuine,
// correctly signed pair, every signature check passing. internal/relsign closes
// that with an exact tab-separated field match, and this job has to close it
// the same way or the two disagree about what a valid release is.
func TestInstallSmokeBindsTheSignatureToTheTag(t *testing.T) {
	job := signatureJob(t)

	if !strings.Contains(job, "Trusted comment") {
		t.Error("the job never reads the trusted comment, so it cannot tell this release from any other correctly signed one")
	}
	if !strings.Contains(job, `"ccdaddy:$TAG"`) {
		t.Error("the release field must be matched as ccdaddy:<tag>, the same field internal/relsign reads")
	}
	// -F, because the tag is data. `ccdaddy:v1.2.3` read as a pattern also
	// matches `ccdaddy:v1x2x3`, and the dot is in every tag this project cuts.
	// install.ps1 escapes for the same reason, one line away in the same repo.
	if !strings.Contains(job, "grep -Fxc") {
		t.Error("the trusted-comment match must be a fixed-string, whole-line match; a regex makes the dots in a tag into wildcards")
	}
}

// A leg that runs minisign and reports success proves that minisign ran. These
// two controls are what make it prove that minisign LOOKED: one changes the
// bytes the signature covers, the other changes the key it is checked against,
// and both must be refused. Without them, a wrong flag that verifies nothing
// passes this job forever.
func TestInstallSmokeSignatureLegCarriesItsControls(t *testing.T) {
	job := signatureJob(t)

	for _, want := range []struct{ marker, why string }{
		{"tampered", "a modified sums file must be refused, or the signature is not covering the content"},
		{"other.pub", "a different key must be refused, or -p is not being consulted"},
	} {
		if !strings.Contains(job, want.marker) {
			t.Errorf("the signature job has no %q control: %s", want.marker, want.why)
		}
	}
}
