package relsign

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The files in testdata were produced by the real minisign CLI. That is the
// whole reason this file exists: every other test in this package signs with
// the same offsets it verifies with, so a wire format that is wrong in both
// halves passes all of them and only these bytes notice. Reordering the fields
// inside the signature body is a change no other test in the package can see.
//
// They are marked `-text` in .gitattributes, because eol=lf would otherwise
// rewrite bytes a signature covers.
const (
	goldenTag   = "v1.2.3"
	goldenTag30 = "v1.2.30"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return b
}

func fixtureKey(t *testing.T, name string) PublicKey {
	t.Helper()
	lines := strings.Split(strings.TrimRight(string(fixture(t, name)), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("%s has %d lines, want an untrusted comment and a key", name, len(lines))
	}
	k, err := ParsePublicKey(lines[1])
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return k
}

func sigLines(t *testing.T, sig []byte) []string {
	t.Helper()
	l := strings.Split(strings.TrimSuffix(string(sig), "\n"), "\n")
	if len(l) != 4 {
		t.Fatalf("fixture has %d lines, want 4", len(l))
	}
	return l
}

func joinSig(l []string) []byte { return []byte(strings.Join(l, "\n") + "\n") }

func flipByte(b []byte, i int) []byte {
	out := append([]byte{}, b...)
	out[i] ^= 0x01
	return out
}

// flipSigByte bends the ed25519 signature on line 2 and leaves the algorithm
// and the key id alone, so what fails is the signature check and not one of the
// guards standing in front of it.
func flipSigByte(t *testing.T, sig []byte) []byte {
	t.Helper()
	l := sigLines(t, sig)
	raw, err := base64.StdEncoding.DecodeString(l[1])
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01
	l[1] = base64.StdEncoding.EncodeToString(raw)
	return joinSig(l)
}

// retag rewrites the trusted comment and leaves both signatures untouched --
// the exact edit a verifier that skipped the line-4 check would accept.
func retag(t *testing.T, sig []byte, tag string) []byte {
	t.Helper()
	l := sigLines(t, sig)
	l[2] = trustedPrefix + TrustedComment(tag)
	return joinSig(l)
}

// prehash flips line 2's algorithm bytes from "Ed" to "ED". A genuine prehashed
// signature also covers a BLAKE2b digest rather than the file, but the branch
// under test is the two-byte comparison and it runs before any ed25519
// verification, so a real one and this one take the same path. Producing a real
// one would need BLAKE2b, which is the dependency this whole design refuses.
func prehash(t *testing.T, sig []byte) []byte {
	t.Helper()
	l := sigLines(t, sig)
	raw, err := base64.StdEncoding.DecodeString(l[1])
	if err != nil {
		t.Fatal(err)
	}
	copy(raw[0:2], algPrehashed)
	l[1] = base64.StdEncoding.EncodeToString(raw)
	return joinSig(l)
}

func dropPrefix(t *testing.T, sig []byte, line int) []byte {
	t.Helper()
	l := sigLines(t, sig)
	l[line] = strings.TrimPrefix(strings.TrimPrefix(l[line], untrustedPrefix), trustedPrefix)
	return joinSig(l)
}

// pad grows the body past the bound with junk AFTER a complete, valid
// signature: the bound has to be applied before the body is split, so a
// well-formed prefix must not save it.
// bloat grows a signature past a byte bound from INSIDE its trusted comment.
// Appending to the end instead would only add a fifth line, and the line-count
// check would then reject the file whether or not the size bound exists -- so
// the case would pass while proving nothing about the bound it is named for.
func bloat(t *testing.T, sig []byte, n int) []byte {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(string(sig), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("fixture is %d lines, want 4", len(lines))
	}
	lines[2] += strings.Repeat("x", n-len(sig))
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestGoldenSignaturesFromTheStockTool(t *testing.T) {
	sums := fixture(t, "sums.txt")
	release := fixtureKey(t, "release.pub")
	other := fixtureKey(t, "other.pub")
	valid := fixture(t, "sums.txt.minisig")

	tests := []struct {
		name    string
		keys    []PublicKey
		content []byte
		sig     []byte
		tag     string
		want    error
	}{
		{"the signature a release publishes", []PublicKey{release}, sums, valid, goldenTag, nil},
		{"rotation: the key is second in the list", []PublicKey{other, release}, sums, valid, goldenTag, nil},
		{"rotation: the key is first in the list", []PublicKey{release, other}, sums, valid, goldenTag, nil},
		{"a trusted comment ending in a tab", []PublicKey{release}, sums, fixture(t, "sums.txt.tabtc.minisig"), goldenTag, nil},
		{"another key's signature", []PublicKey{release}, sums, fixture(t, "sums.txt.other.minisig"), goldenTag, ErrKeyID},
		{"that key, trusted", []PublicKey{other}, sums, fixture(t, "sums.txt.other.minisig"), goldenTag, nil},
		{"v1.2.30 does not satisfy v1.2.3", []PublicKey{release}, sums, fixture(t, "sums.txt.v1230.minisig"), goldenTag, ErrRelease},
		{"v1.2.30 satisfies itself", []PublicKey{release}, sums, fixture(t, "sums.txt.v1230.minisig"), goldenTag30, nil},
		{"v1.2.3 does not satisfy v1.2.30", []PublicKey{release}, sums, valid, goldenTag30, ErrRelease},
		{"one flipped content byte", []PublicKey{release}, flipByte(sums, 3), valid, goldenTag, ErrSignature},
		{"one flipped signature byte", []PublicKey{release}, sums, flipSigByte(t, valid), goldenTag, ErrSignature},
		{"a tampered trusted comment", []PublicKey{release}, sums, retag(t, valid, "v9.9.9"), "v9.9.9", ErrSignature},
		{"the prehashed algorithm", []PublicKey{release}, sums, prehash(t, valid), goldenTag, ErrAlgorithm},
		{"CRLF line endings", []PublicKey{release}, sums, []byte(strings.ReplaceAll(string(valid), "\n", "\r\n")), goldenTag, nil},
		{"a fifth line", []PublicKey{release}, sums, append(append([]byte{}, valid...), "extra\n"...), goldenTag, ErrMalformed},
		{"no untrusted comment prefix", []PublicKey{release}, sums, dropPrefix(t, valid, 0), goldenTag, ErrMalformed},
		{"no trusted comment prefix", []PublicKey{release}, sums, dropPrefix(t, valid, 2), goldenTag, ErrMalformed},
		{"one byte over the bound", []PublicKey{release}, sums, bloat(t, valid, maxSigBytes+1), goldenTag, ErrMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(tc.keys, tc.content, tc.sig, tc.tag); !errors.Is(err, tc.want) {
				t.Fatalf("Verify() = %v, want %v", err, tc.want)
			}
		})
	}
}

// The trusted comment in the golden file is the string TrustedComment builds.
// If these ever differ, the signer and the fixtures are describing two
// different grammars and one of them is what ships.
func TestTheGoldenTrustedCommentIsTheOneTheSignerWrites(t *testing.T) {
	l := sigLines(t, fixture(t, "sums.txt.minisig"))
	if want := trustedPrefix + TrustedComment(goldenTag); l[2] != want {
		t.Fatalf("golden line 3 = %q, want %q", l[2], want)
	}
}
