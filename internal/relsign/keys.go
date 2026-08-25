package relsign

import (
	"slices"
	"strings"
)

// publicKeys is this build's trust root: newline-separated base64 key bodies --
// the SECOND line of a .pub file, one per line, and nothing else. The file at
// the repository root, ccdaddy.pub, is where these come from, and
// internal/relsign/pin_test.go fails if the two ever disagree.
//
// A const, and the slice derived from it is not a string, because -ldflags -X
// patches string VARIABLES and scripts/build-release.sh already patches two of
// them. A trust root a link line could swap is not pinned. It is also why this
// does not live in internal/buildinfo: that is the package the build stamps.
//
// Empty means enforcement is off, and that is fail-closed rather than a bypass:
// a build with no trust root has nothing to check a release against, so it
// refuses to replace itself at all.
//
// Rotation is a LIST, not a swap. A release ships a build carrying both the old
// key and the new one; only once enough of the fleet is on that build does a
// later release drop the old key and sign with the new one alone.
const publicKeys = ""

var parsedKeys = mustParseKeys(publicKeys)

// mustParseKeys panics rather than degrading to an empty root. The constant is
// a literal in this repository, so the only way it fails to parse is that
// somebody mistyped it -- and an empty root reads as "enforcement is off",
// which is the one state a typo must not be able to reach. Every `go test` run
// executes this package's init, so a bad paste is caught by the suite rather
// than by a user.
func mustParseKeys(s string) []PublicKey {
	var out []PublicKey
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, err := ParsePublicKey(line)
		if err != nil {
			panic("relsign: the pinned public key does not parse: " + err.Error())
		}
		out = append(out, k)
	}
	return out
}

// Keys returns this build's trust root, parsed from the compile-time constant.
// An empty slice means enforcement is off.
//
// A clone: a caller that held the slice itself would hold the trust root, and
// one append or index assignment anywhere would rewrite what every later
// verification runs against.
func Keys() []PublicKey { return slices.Clone(parsedKeys) }

// Enforced reports whether this build carries a trust root at all.
func Enforced() bool { return len(parsedKeys) > 0 }
