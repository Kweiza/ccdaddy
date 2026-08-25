package relsign

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ccdaddy.pub and the constant in keys.go are one fact written in two places:
// the file is what a human downloads and what the README tells them to compare
// against, and the constant is what the binary actually checks against. A tree
// where they disagree ships a binary that refuses the releases its own
// repository publishes -- and nothing else in the suite would notice, because
// every other test in this package builds its own keys.
//
// This is the same shape as the other generated-file pins here: read the
// artefact as text, no new dependency, fail the moment the pair drifts.
func TestThePinnedKeyIsTheKeyCommittedAtTheRoot(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "ccdaddy.pub"))
	if err != nil {
		t.Fatalf("reading ccdaddy.pub: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("ccdaddy.pub has %d lines, want an untrusted comment and a key", len(lines))
	}
	if !strings.HasPrefix(lines[0], untrustedPrefix) {
		t.Errorf("ccdaddy.pub line 1 = %q, want an untrusted comment", lines[0])
	}
	committed, err := ParsePublicKey(lines[1])
	if err != nil {
		t.Fatalf("ccdaddy.pub does not parse: %v", err)
	}

	// Enforced() is checked before the membership search, not after: a
	// matching element in Keys() already implies len(Keys()) > 0, so if the
	// order were reversed an empty trust root would always be reported as "the
	// key is not in the trust root" and the "no trust root at all" branch below
	// would be unreachable by any input.
	if !Enforced() {
		t.Fatal("this build has no trust root at all")
	}

	// ContainsFunc rather than equality, so a build carrying both the old key
	// and the new one during a rotation still passes.
	same := func(k PublicKey) bool {
		return k.KeyNum == committed.KeyNum && k.Key.Equal(committed.Key)
	}
	if !slices.ContainsFunc(Keys(), same) {
		// %x formats the [8]byte array directly; there is no separate hex-encoding
		// helper in this package (verify.go's own key-id error does the same).
		t.Fatalf("the key in ccdaddy.pub (id %x) is not in this build's trust root; "+
			"paste the SECOND line of ccdaddy.pub into publicKeys in keys.go", committed.KeyNum)
	}
}
