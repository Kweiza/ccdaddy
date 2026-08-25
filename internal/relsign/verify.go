package relsign

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
)

// signature is a parsed .minisig. Nothing here has been checked yet -- every
// field is still attacker-controlled, the trusted comment included, and it
// stays that way until the second ed25519 check in Verify passes.
type signature struct {
	alg    [2]byte
	keyNum [8]byte
	sig    []byte // 64 bytes, over the signed file's own bytes
	tc     []byte // the trusted comment, prefix and line ending removed
	global []byte // 64 bytes, over sig || tc
}

// parseSignature splits a .minisig into its four fields.
//
// Strict, and bounded before it is strict. Padding after line four is REFUSED
// rather than ignored: a parser that skips what it does not understand is a
// parser an attacker can hold a conversation with.
func parseSignature(minisig []byte) (signature, error) {
	var s signature
	if len(minisig) > maxSigBytes {
		return s, fmt.Errorf("%w: %d bytes, over the %d-byte bound", ErrMalformed, len(minisig), maxSigBytes)
	}
	lines := strings.Split(string(minisig), "\n")
	// Exactly one trailing newline is normal and is the only thing allowed
	// after line four.
	if len(lines) == 5 && lines[4] == "" {
		lines = lines[:4]
	}
	if len(lines) != 4 {
		return s, fmt.Errorf("%w: %d lines, want 4", ErrMalformed, len(lines))
	}
	// Split on "\n" has already removed exactly one line terminator. What can
	// remain is one "\r", and exactly one is what comes off -- never the
	// " \t\r\n" set. A trusted comment legally ends in a tab, it verifies under
	// the stock tool, and it is one keystroke away from the tab-separated
	// grammar this repository signs with, so a wider trim would reject a valid
	// signature and report it as tampering.
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	// A missing prefix is malformed, not a signature mismatch. Trimming a
	// prefix that is not there is a silent no-op, and it would report the wrong
	// refusal to a user whose remedy differs between the two.
	if !strings.HasPrefix(lines[0], untrustedPrefix) {
		return s, fmt.Errorf("%w: line 1 is not an untrusted comment", ErrMalformed)
	}
	if !strings.HasPrefix(lines[2], trustedPrefix) {
		return s, fmt.Errorf("%w: line 3 is not a trusted comment", ErrMalformed)
	}
	body, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		return s, fmt.Errorf("%w: line 2 is not base64: %v", ErrMalformed, err)
	}
	if len(body) != sigLen {
		return s, fmt.Errorf("%w: line 2 is %d bytes, want %d", ErrMalformed, len(body), sigLen)
	}
	global, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil {
		return s, fmt.Errorf("%w: line 4 is not base64: %v", ErrMalformed, err)
	}
	if len(global) != ed25519.SignatureSize {
		return s, fmt.Errorf("%w: line 4 is %d bytes, want %d", ErrMalformed, len(global), ed25519.SignatureSize)
	}
	copy(s.alg[:], body[0:2])
	copy(s.keyNum[:], body[2:10])
	s.sig = body[10:sigLen]
	s.tc = []byte(strings.TrimPrefix(lines[2], trustedPrefix))
	s.global = global
	return s, nil
}

// releaseField is the tab-separated field the verifier reads. Only this one is
// checked: the file: field beside it is decoration for whoever reads
// `minisign -Vm` output, and a verifier that validated it too would break the
// first release whose comment grows a field.
const releaseField = "ccdaddy:"

// TrustedComment is the exact trusted-comment string for a tag, and the one
// definition the signer and the verifier share.
//
// It exists because sha256sums.txt names no version. Without a signed field
// naming the release, an old release's checksums and its signature stay a
// genuine, correctly signed pair forever -- so an origin that chooses what to
// serve can answer "the latest is v9.9.9" and hand back the authentic v0.4.0
// pair, every signature check passing, the user quietly downgraded to a version
// whose bugs are public.
//
// No timestamp field. A unix stamp would make the .minisig the one release
// artifact that is not reproducible, so re-running a publish job would clobber
// different bytes while every other asset stayed byte-identical.
func TrustedComment(tag string) string {
	return "file:sha256sums.txt\t" + releaseField + tag
}

// trustedCommentNames reports whether tc carries exactly one tab-separated
// field naming this release.
//
// Exact field, never a substring: strings.Contains(tc, "ccdaddy:v1.2.3") is
// also true of "ccdaddy:v1.2.30", which is a real release name one patch
// series away.
func trustedCommentNames(tc, tag string) bool {
	want := releaseField + tag
	n := 0
	for _, f := range strings.Split(tc, "\t") {
		if f == want {
			n++
		}
	}
	return n == 1
}

// Verify checks a minisign signature over content against any of keys, and
// requires the trusted comment to name wantRelease (e.g. "v0.7.0").
//
// TWO ed25519 checks, not one. The first covers the file; the second covers the
// trusted comment, and without it the comment is editable by anyone -- which is
// the whole downgrade guard, because the release name lives there.
//
// keys is a LIST because that is what makes key rotation possible: a release
// ships a build carrying both the old key and the new one, and only once enough
// of the fleet is on that build does a later release drop the old key. A
// verifier that accepted exactly one key would strand every binary already in
// the field on the day the key changed.
func Verify(keys []PublicKey, content, minisig []byte, wantRelease string) error {
	// An empty tag is an error rather than a skip. As a skip, a zero value
	// anywhere upstream would silently switch off the only check that binds a
	// genuine (sums, signature) pair to the release it was published as.
	if wantRelease == "" {
		return fmt.Errorf("%w: no release was named", ErrRelease)
	}
	s, err := parseSignature(minisig)
	if err != nil {
		return err
	}
	switch string(s.alg[:]) {
	case algLegacy:
	case algPrehashed:
		return fmt.Errorf("%w: this build verifies only the legacy form; install the "+
			"current release with the installer", ErrAlgorithm)
	default:
		return fmt.Errorf("%w: %q", ErrAlgorithm, s.alg[:])
	}
	// The key id is compared BEFORE either signature check. Skipping it does not
	// let anything through -- a foreign key's signature fails the ed25519 check
	// too -- but it turns "signed by a key I do not trust" into "signature does
	// not match": the same refusal reached by luck instead of by rule, told to
	// the user as tampering when the honest answer is that their ccdad predates
	// the key.
	var key PublicKey
	found := false
	for _, k := range keys {
		if k.KeyNum == s.keyNum {
			key = k
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: key id %x", ErrKeyID, s.keyNum)
	}
	if !ed25519.Verify(key.Key, content, s.sig) {
		return ErrSignature
	}
	signed := make([]byte, 0, len(s.sig)+len(s.tc))
	signed = append(signed, s.sig...)
	signed = append(signed, s.tc...)
	if !ed25519.Verify(key.Key, signed, s.global) {
		return fmt.Errorf("%w: the trusted comment is not signed by the same key", ErrSignature)
	}
	// Only now is the trusted comment worth reading. Until line 4 verified, it
	// was text an attacker chose.
	if !trustedCommentNames(string(s.tc), wantRelease) {
		return fmt.Errorf("%w: %q does not name %s", ErrRelease, s.tc, wantRelease)
	}
	return nil
}
