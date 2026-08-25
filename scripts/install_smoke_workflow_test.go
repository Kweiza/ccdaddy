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
	// Stated as a property of every -p in the job rather than as one URL,
	// because the URL is not the only way to reach a downloaded key — and the
	// first version of this test named the URL and was satisfied by
	// `-p "$rel/ccdaddy.pub"`.
	for _, line := range strings.Split(job, "\n") {
		i := strings.Index(line, "-p ")
		if i < 0 {
			continue
		}
		key := strings.Fields(line[i+len("-p "):])
		if len(key) == 0 {
			continue
		}
		if strings.Contains(key[0], "$rel") || strings.Contains(key[0], "/rel/") {
			t.Errorf("the key is read out of the download directory: %q — it must come from the checkout, never from the release under test", strings.TrimSpace(line))
		}
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
	// Scanning for the verification command by name rather than for `minisign`:
	// the verification moved into a script, and a scan for `minisign -V` went
	// quietly vacuous the moment it did — it matched nothing and reported
	// success, which is the same failure this test exists to catch one level up.
	verifications := 0
	for _, line := range strings.Split(job, "\n") {
		if !strings.Contains(line, "verify-release-signature.sh") {
			continue
		}
		verifications++
		// The two controls INVERT the script — they require it to fail — so
		// they legitimately appear inside an `if`. What may never appear is a
		// soft arm that lets a failure through as a message.
		if strings.Contains(line, "|| true") || strings.Contains(line, "|| echo") {
			t.Errorf("a verification that cannot fail is not a verification: %q", strings.TrimSpace(line))
		}
	}
	if verifications == 0 {
		t.Error("the job runs no verification at all, so this test was passing over nothing")
	}
}

// minisign answers "is this signature authentic", never "is this the release I
// asked for". An origin that chooses what to serve can hand back an older
// release's genuine, correctly signed pair with every signature check passing,
// and sha256sums.txt carries no version of its own to tell them apart.
//
// That rule is NOT written in this workflow. It lives in
// scripts/verify-release-signature.sh, which has its own test — including the
// case a substring test gets wrong, driven against real minisign 0.11
// signatures. What this asserts is that the job delegates to it rather than
// growing a second copy of the rule inside a `run:` block, where nothing could
// test it.
func TestInstallSmokeDelegatesTheReleaseBindingToTheTestedScript(t *testing.T) {
	job := signatureJob(t)

	if !strings.Contains(job, "scripts/verify-release-signature.sh") {
		t.Error("the job does not call scripts/verify-release-signature.sh, so the release-binding rule is either missing or re-implemented untested")
	}
	// A second copy of the rule inline is the thing to catch: it would drift
	// from the script's, and only the script's has a test.
	for _, inline := range []string{"Trusted comment", "grep -Fxc"} {
		if strings.Contains(job, inline) {
			t.Errorf("the job carries its own copy of the trusted-comment rule (%q); the script owns it", inline)
		}
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
