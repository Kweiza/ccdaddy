// Package relsign verifies and produces the minisign signature this repository
// publishes beside sha256sums.txt.
//
// Both halves live in one package on purpose: the byte offsets of the wire
// format are declared once, so the signer and the verifier cannot drift into a
// release that signs something nothing can check. The signer half is used by
// scripts/minisign-sign; the verifier half is compiled into every ccdad.
package relsign

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// The refusals are distinguishable on purpose. "Signed by a key this build does
// not trust" and "the signature does not match" are the same word to a verifier
// and completely different words to a user: the first is an old binary meeting
// a rotated key, the second is bytes that were altered. A caller that can only
// say "verification failed" has to route both to the same remedy, and one of
// the two remedies is wrong.
var (
	ErrMalformed = errors.New("not a minisign signature")
	ErrAlgorithm = errors.New("unsupported signature algorithm")
	ErrKeyID     = errors.New("signed by a key this build does not trust")
	ErrSignature = errors.New("signature does not match")
	ErrRelease   = errors.New("signature is for a different release")
)

const (
	// The two-byte algorithm tag that opens a public key and a signature alike.
	//
	// "Ed" is minisign's LEGACY form: a plain ed25519 signature over the file's
	// own bytes. "ED" is the prehashed form, which signs a BLAKE2b-512 digest
	// instead -- and BLAKE2b is not in the standard library, so supporting it
	// would pull a module into a go.mod that does not have one, for a file of
	// about a kilobyte. Prehashing is a streaming property, not a strength
	// property, and the signed object here fits in memory many times over.
	//
	// The stock tool's two defaults point in opposite directions, which is why
	// the mode is pinned in source rather than left to a command line:
	// `minisign -V` ACCEPTS legacy (only -H refuses it), while `minisign -S`
	// SIGNS prehashed unless it is given -l.
	algLegacy    = "Ed"
	algPrehashed = "ED"

	// A public key body is alg(2) || key id(8) || ed25519 public key(32).
	pubKeyLen = 2 + 8 + ed25519.PublicKeySize

	// A signature body is alg(2) || key id(8) || ed25519 signature(64).
	sigLen = 2 + 8 + ed25519.SignatureSize

	// Both comment prefixes, including their trailing space. Their lengths are
	// what gets removed before the trusted comment is signed over, so they are
	// named rather than counted at each site.
	untrustedPrefix = "untrusted comment: "
	trustedPrefix   = "trusted comment: "

	// A .minisig is four short lines. The reference implementation bounds each
	// of its own comments at a kilobyte, so 16 KiB is two orders of magnitude
	// of headroom over anything legitimate -- and the point is not the number
	// but WHERE it is applied: before the body is split into lines, because a
	// split over an attacker-supplied body allocates in proportion to what the
	// attacker sent.
	maxSigBytes = 16 << 10
)

// PublicKey is one minisign public key: the 8-byte id a signature carries in
// clear, and the ed25519 key itself.
type PublicKey struct {
	KeyNum [8]byte
	Key    ed25519.PublicKey
}

// ParsePublicKey parses the base64 body of a minisign public key -- the SECOND
// line of a .pub file. The first line is an untrusted comment that nothing
// signs, and the stock tool's own loader skips it too.
func ParsePublicKey(b64Line string) (PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64Line))
	if err != nil {
		return PublicKey{}, fmt.Errorf("%w: public key is not base64: %v", ErrMalformed, err)
	}
	if len(raw) != pubKeyLen {
		return PublicKey{}, fmt.Errorf("%w: public key is %d bytes, want %d", ErrMalformed, len(raw), pubKeyLen)
	}
	if string(raw[0:2]) != algLegacy {
		return PublicKey{}, fmt.Errorf("%w: public key algorithm %q", ErrAlgorithm, raw[0:2])
	}
	var pk PublicKey
	copy(pk.KeyNum[:], raw[2:10])
	// Cloned rather than aliased into raw: a PublicKey outlives this call and
	// is held by package state, and a key that shares an array with a decode
	// buffer is a key some future caller can scribble on.
	pk.Key = ed25519.PublicKey(bytes.Clone(raw[10:pubKeyLen]))
	return pk, nil
}
