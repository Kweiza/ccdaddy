package relsign

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// b64n is a base64 payload of exactly n zero bytes. This table is about the
// GRAMMAR -- line count, prefixes, lengths, line endings -- so the payloads
// only have to be the right size, and using real signatures here would hide
// which check actually fired.
func b64n(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }

func fourLines(l1, l2, l3, l4 string) string {
	return l1 + "\n" + l2 + "\n" + l3 + "\n" + l4 + "\n"
}

func TestParseSignatureGrammar(t *testing.T) {
	const tc = "file:sha256sums.txt\tccdaddy:v1.2.3"
	good := fourLines(
		untrustedPrefix+"signature from ccdaddy release key",
		b64n(sigLen),
		trustedPrefix+tc,
		b64n(64),
	)

	tests := []struct {
		name   string
		body   string
		want   error
		wantTC string
	}{
		{"the shape the signer writes", good, nil, tc},
		{"no trailing newline", strings.TrimSuffix(good, "\n"), nil, tc},
		{"CRLF throughout", strings.ReplaceAll(good, "\n", "\r\n"), nil, tc},
		{
			"a trusted comment ending in a tab",
			fourLines(untrustedPrefix+"x", b64n(sigLen), trustedPrefix+tc+"\t", b64n(64)),
			nil, tc + "\t",
		},
		{"a fifth line", good + "extra\n", ErrMalformed, ""},
		{"two trailing newlines", good + "\n", ErrMalformed, ""},
		{"only three lines", untrustedPrefix + "x\n" + b64n(sigLen) + "\n" + trustedPrefix + tc + "\n", ErrMalformed, ""},
		{
			"no untrusted comment prefix",
			fourLines("signature from somewhere", b64n(sigLen), trustedPrefix+tc, b64n(64)),
			ErrMalformed, "",
		},
		{
			"no trusted comment prefix",
			fourLines(untrustedPrefix+"x", b64n(sigLen), tc, b64n(64)),
			ErrMalformed, "",
		},
		{
			"line 2 is not base64",
			fourLines(untrustedPrefix+"x", "!!!!", trustedPrefix+tc, b64n(64)),
			ErrMalformed, "",
		},
		{
			"line 2 is one byte short",
			fourLines(untrustedPrefix+"x", b64n(sigLen-1), trustedPrefix+tc, b64n(64)),
			ErrMalformed, "",
		},
		{
			"line 4 is one byte short",
			fourLines(untrustedPrefix+"x", b64n(sigLen), trustedPrefix+tc, b64n(63)),
			ErrMalformed, "",
		},
		{
			"line 2 is one byte over",
			fourLines(untrustedPrefix+"x", b64n(sigLen+1), trustedPrefix+tc, b64n(64)),
			ErrMalformed, "",
		},
		{
			"line 4 is one byte over",
			fourLines(untrustedPrefix+"x", b64n(sigLen), trustedPrefix+tc, b64n(65)),
			ErrMalformed, "",
		},
		{
			"four lines but total exceeds maxSigBytes",
			fourLines(untrustedPrefix+"x", b64n(sigLen), trustedPrefix+tc+strings.Repeat(" ", maxSigBytes), b64n(64)),
			ErrMalformed, "",
		},
		{"one byte over the bound", good + strings.Repeat("x", maxSigBytes+1-len(good)), ErrMalformed, ""},
		{"empty", "", ErrMalformed, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSignature([]byte(tc.body))
			if !errors.Is(err, tc.want) {
				t.Fatalf("parseSignature() error = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				return
			}
			if string(got.tc) != tc.wantTC {
				t.Errorf("trusted comment = %q, want %q", got.tc, tc.wantTC)
			}
		})
	}
}

// signByHand builds a .minisig the way the format says to, WITHOUT going
// through this package's own signer. The independence is the point: a test
// that signed through Sign and verified through Verify passes with both halves
// wrong in the same way. (The fixtures a real minisign produced, in
// golden_test.go, are what finally settle the wire format. This helper settles
// the logic.)
func signByHand(t *testing.T, priv ed25519.PrivateKey, keyNum [8]byte, alg string, content []byte, tc string) []byte {
	t.Helper()
	sig := ed25519.Sign(priv, content)
	body := append([]byte(alg), keyNum[:]...)
	body = append(body, sig...)
	global := ed25519.Sign(priv, append(append([]byte{}, sig...), tc...))
	return []byte(fourLines(
		untrustedPrefix+"test",
		base64.StdEncoding.EncodeToString(body),
		trustedPrefix+tc,
		base64.StdEncoding.EncodeToString(global),
	))
}

func rewriteLine(t *testing.T, minisig []byte, i int, s string) []byte {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(string(minisig), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d", len(lines))
	}
	lines[i] = s
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	num := [8]byte{9, 8, 7, 6, 5, 4, 3, 2}
	otherNum := [8]byte{1, 1, 1, 1, 1, 1, 1, 1}
	key := PublicKey{KeyNum: num, Key: pub}
	other := PublicKey{KeyNum: otherNum, Key: otherPub}

	content := []byte("0123456789abcdef  ccdad-linux-amd64\n")
	good := signByHand(t, priv, num, algLegacy, content, TrustedComment("v1.2.3"))

	// The bypass: a valid line-2 signature with the trusted comment rewritten
	// afterwards. Only the line-4 check catches it, and the trusted comment is
	// where the release name lives -- so a verifier that skips line 4 turns the
	// downgrade guard into decoration.
	tampered := rewriteLine(t, good, 2, trustedPrefix+TrustedComment("v9.9.9"))

	flippedSig := func() []byte {
		lines := strings.Split(strings.TrimSuffix(string(good), "\n"), "\n")
		raw, err := base64.StdEncoding.DecodeString(lines[1])
		if err != nil {
			t.Fatal(err)
		}
		raw[len(raw)-1] ^= 0x01
		return rewriteLine(t, good, 1, base64.StdEncoding.EncodeToString(raw))
	}()

	tests := []struct {
		name    string
		keys    []PublicKey
		content []byte
		sig     []byte
		tag     string
		want    error
	}{
		{"the happy path", []PublicKey{key}, content, good, "v1.2.3", nil},
		{"rotation: the key is second in the list", []PublicKey{other, key}, content, good, "v1.2.3", nil},
		{"rotation: the key is first in the list", []PublicKey{key, other}, content, good, "v1.2.3", nil},
		{"a key this build does not carry", []PublicKey{other}, content, good, "v1.2.3", ErrKeyID},
		{"no keys at all", nil, content, good, "v1.2.3", ErrKeyID},
		{"one flipped content byte", []PublicKey{key}, []byte("1123456789abcdef  ccdad-linux-amd64\n"), good, "v1.2.3", ErrSignature},
		{"one flipped signature byte", []PublicKey{key}, content, flippedSig, "v1.2.3", ErrSignature},
		{"a tampered trusted comment", []PublicKey{key}, content, tampered, "v9.9.9", ErrSignature},
		{"the prehashed algorithm", []PublicKey{key}, content, signByHand(t, priv, num, algPrehashed, content, TrustedComment("v1.2.3")), "v1.2.3", ErrAlgorithm},
		{"an unknown algorithm", []PublicKey{key}, content, signByHand(t, priv, num, "XX", content, TrustedComment("v1.2.3")), "v1.2.3", ErrAlgorithm},
		{"v1.2.30 does not satisfy v1.2.3", []PublicKey{key}, content, signByHand(t, priv, num, algLegacy, content, TrustedComment("v1.2.30")), "v1.2.3", ErrRelease},
		{"v1.2.3 does not satisfy v1.2.30", []PublicKey{key}, content, good, "v1.2.30", ErrRelease},
		{"no ccdaddy field at all", []PublicKey{key}, content, signByHand(t, priv, num, algLegacy, content, "file:sha256sums.txt"), "v1.2.3", ErrRelease},
		{"an empty wanted release", []PublicKey{key}, content, good, "", ErrRelease},
		{"a trusted comment ending in a tab", []PublicKey{key}, content, signByHand(t, priv, num, algLegacy, content, TrustedComment("v1.2.3")+"\t"), "v1.2.3", nil},
		{"another key's signature, that key trusted", []PublicKey{other}, content, signByHand(t, otherPriv, otherNum, algLegacy, content, TrustedComment("v1.2.3")), "v1.2.3", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(tc.keys, tc.content, tc.sig, tc.tag); !errors.Is(err, tc.want) {
				t.Fatalf("Verify() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTrustedCommentIsTabSeparated(t *testing.T) {
	got := TrustedComment("v0.7.0")
	want := "file:sha256sums.txt\tccdaddy:v0.7.0"
	if got != want {
		t.Fatalf("TrustedComment() = %q, want %q", got, want)
	}
}
