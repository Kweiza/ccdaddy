package relsign

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// secBody assembles the base64 body of a secret key file's second line, so a
// case can bend exactly one field.
func secBody(alg string, keyNum [8]byte, priv []byte) string {
	raw := append([]byte(alg), keyNum[:]...)
	raw = append(raw, priv...)
	return base64.StdEncoding.EncodeToString(raw)
}

func TestParseSecretKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	num := [8]byte{2, 4, 6, 8, 10, 12, 14, 16}

	tests := []struct {
		name string
		body string
		want error
	}{
		{"a real key", secBody("Ed", num, priv), nil},
		{"surrounding whitespace", " " + secBody("Ed", num, priv) + "\n", nil},
		{"the prehashed algorithm", secBody("ED", num, priv), ErrAlgorithm},
		{"one byte short", secBody("Ed", num, priv[:63]), ErrMalformed},
		{"not base64", "!!" + secBody("Ed", num, priv), ErrMalformed},
		{"empty", "", ErrMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSecretKey(tc.body)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ParseSecretKey() error = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				return
			}
			if got.KeyNum != num {
				t.Errorf("KeyNum = %x, want %x", got.KeyNum, num)
			}
			if !got.Key.Equal(priv) {
				t.Error("the ed25519 key does not round-trip")
			}
		})
	}
}

func TestSignRoundTripsThroughVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	num := [8]byte{3, 1, 4, 1, 5, 9, 2, 6}
	sk, err := ParseSecretKey(secBody("Ed", num, priv))
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("0123456789abcdef  ccdad-linux-amd64\n")

	sig, err := sk.Sign(content, TrustedComment("v1.2.3"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(string(sig), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("Sign wrote %d lines, want 4:\n%s", len(lines), sig)
	}
	if !strings.HasPrefix(lines[0], untrustedPrefix) {
		t.Errorf("line 1 = %q, want an untrusted comment", lines[0])
	}
	if want := trustedPrefix + TrustedComment("v1.2.3"); lines[2] != want {
		t.Errorf("line 3 = %q, want %q", lines[2], want)
	}
	if !strings.HasSuffix(string(sig), "\n") {
		t.Error("Sign did not end the file with a newline")
	}

	keys := []PublicKey{{KeyNum: num, Key: pub}}
	if err := Verify(keys, content, sig, "v1.2.3"); err != nil {
		t.Fatalf("Verify of a freshly signed file: %v", err)
	}
	if err := Verify(keys, content, sig, "v1.2.4"); !errors.Is(err, ErrRelease) {
		t.Fatalf("Verify against the wrong tag = %v, want %v", err, ErrRelease)
	}
}

// A newline in the trusted comment would end line 3 early and turn one
// signature into a five-line file -- silently, at signing time, discovered by
// whoever tried to install the release.
func TestSignRefusesALineBreakInTheTrustedComment(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sk, err := ParseSecretKey(secBody("Ed", [8]byte{1}, priv))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []string{"a\nb", "a\rb", "a\n"} {
		if _, err := sk.Sign([]byte("x"), tc); !errors.Is(err, ErrMalformed) {
			t.Errorf("Sign(%q) = %v, want %v", tc, err, ErrMalformed)
		}
	}
}

// secondLine is how both file bodies are consumed everywhere else: the pin
// test reads it out of ccdaddy.pub, and the release job reads it out of a
// repository secret.
func secondLine(t *testing.T, file string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(file, "\n"), "\n")
	if len(lines) != 2 {
		// The length, not the body: this helper is also called on the SECRET
		// file GenerateKey produced, and that body is an ed25519 private key.
		// It is an ephemeral per-run key, not the release key, but a test
		// failure is not the place to start printing key material anyway.
		t.Fatalf("file has %d lines, want 2 (%d bytes total)", len(lines), len(file))
	}
	if !strings.HasPrefix(lines[0], untrustedPrefix) {
		t.Fatalf("line 1 = %q, want an untrusted comment", lines[0])
	}
	return lines[1]
}

func TestGenerateKeyProducesAMatchingPair(t *testing.T) {
	pubFile, secFile, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	pk, err := ParsePublicKey(secondLine(t, pubFile))
	if err != nil {
		t.Fatalf("the generated public key does not parse: %v", err)
	}
	sk, err := ParseSecretKey(secondLine(t, secFile))
	if err != nil {
		t.Fatalf("the generated secret key does not parse: %v", err)
	}
	if pk.KeyNum != sk.KeyNum {
		t.Fatalf("key ids differ: public %x, secret %x", pk.KeyNum, sk.KeyNum)
	}
	if !strings.Contains(pubFile, keyIDHex(pk.KeyNum)) {
		t.Errorf("the public key's comment does not carry its own key id %s", keyIDHex(pk.KeyNum))
	}

	content := []byte("0123456789abcdef  ccdad-linux-amd64\n")
	sig, err := sk.Sign(content, TrustedComment("v1.2.3"))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := Verify([]PublicKey{pk}, content, sig, "v1.2.3"); err != nil {
		t.Fatalf("a freshly generated pair does not verify its own signature: %v", err)
	}
}

func TestGenerateKeyIsDifferentEveryTime(t *testing.T) {
	a, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two calls produced the same public key")
	}
}
