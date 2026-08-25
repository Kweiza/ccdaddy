package relsign

import (
	"crypto/ed25519"
	"strings"
	"testing"
)

// samplePubLine is a well-formed key body built here rather than read from the
// tree, so this file's assertions hold both before and after the real key is
// pinned into the constant.
func samplePubLine(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pubBody("Ed", [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, pub)
}

func TestMustParseKeysReadsALineSeparatedList(t *testing.T) {
	a, b := samplePubLine(t), samplePubLine(t)
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty means enforcement is off", "", 0},
		{"one key", a, 1},
		{"two keys, which is what rotation looks like", a + "\n" + b, 2},
		{"blank lines and stray whitespace are ignored", "\n  " + a + "  \n\n" + b + "\n", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(mustParseKeys(tc.in)); got != tc.want {
				t.Fatalf("mustParseKeys() gave %d keys, want %d", got, tc.want)
			}
		})
	}
}

// A trust root that does not parse must not degrade to an empty one, because an
// empty one reads as "enforcement is off" -- which is exactly the state a typo
// must never reach.
func TestMustParseKeysPanicsOnAKeyThatDoesNotParse(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("mustParseKeys returned instead of panicking on an unparseable key")
		}
		if !strings.Contains(r.(string), "does not parse") {
			t.Fatalf("panic message = %q, want it to say the pinned key does not parse", r)
		}
	}()
	mustParseKeys("this is not a minisign public key")
}

func TestKeysHandsOutACopyAndEnforcementFollowsTheList(t *testing.T) {
	saved := parsedKeys
	t.Cleanup(func() { parsedKeys = saved })

	parsedKeys = nil
	if Enforced() {
		t.Error("Enforced() is true with no keys")
	}
	if len(Keys()) != 0 {
		t.Error("Keys() is not empty with no keys")
	}

	parsedKeys = mustParseKeys(samplePubLine(t))
	if !Enforced() {
		t.Error("Enforced() is false with a key")
	}
	// The trust root must not be reachable through the value handed out. One
	// stray index assignment anywhere in the tree would otherwise rewrite what
	// every later verification runs against.
	got := Keys()
	got[0] = PublicKey{}
	if Keys()[0].KeyNum == [8]byte{} {
		t.Fatal("Keys() aliases the package's own trust root")
	}
}

// Whatever the constant holds today, these two answers describe the same fact
// and cannot disagree.
func TestEnforcedAgreesWithKeys(t *testing.T) {
	if Enforced() != (len(Keys()) > 0) {
		t.Fatalf("Enforced() = %v with %d keys", Enforced(), len(Keys()))
	}
}
