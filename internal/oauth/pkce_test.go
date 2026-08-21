package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestNewPKCEChallengeIsS256OfVerifier(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE() = %v, want nil", err)
	}

	sum := sha256.Sum256([]byte(p.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if p.Challenge != want {
		t.Fatalf("Challenge = %q, want base64url(sha256(verifier)) = %q", p.Challenge, want)
	}
}

// RFC 7636 requires 43..128 characters; 32 random bytes base64url-encode to 43.
func TestNewPKCEVerifierLength(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(p.Verifier); n < 43 || n > 128 {
		t.Fatalf("len(Verifier) = %d, want between 43 and 128", n)
	}
}

// Base64url without padding: no '+', no '/', no '='.
func TestNewPKCEIsBase64URLUnpadded(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{p.Verifier, p.Challenge} {
		if strings.ContainsAny(s, "+/=") {
			t.Fatalf("%q contains a character outside the base64url alphabet", s)
		}
	}
}

func TestNewPKCEIsNotDeterministic(t *testing.T) {
	a, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if a.Verifier == b.Verifier {
		t.Fatal("two NewPKCE() calls produced the same verifier")
	}
}

// "The two values differ" is also satisfied by a counter or a seeded math/rand
// source. Two 32-byte crypto-random draws differ in ~31.9 of 32 byte positions;
// the chance of fewer than 16 differing is far below 1e-30, so this threshold is
// not flaky, and it is unreachable by any counter.
func TestNewPKCEVerifierIsHighEntropy(t *testing.T) {
	decode := func(s string) []byte {
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("verifier %q is not raw base64url: %v", s, err)
		}
		if len(b) != 32 {
			t.Fatalf("verifier decodes to %d bytes, want 32", len(b))
		}
		return b
	}
	a, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	x, y := decode(a.Verifier), decode(b.Verifier)
	diff := 0
	for i := range x {
		if x[i] != y[i] {
			diff++
		}
	}
	if diff < 16 {
		t.Fatalf("two verifiers differ in only %d of 32 bytes; the source is not cryptographically random", diff)
	}
}

// The type promises the verifier is never logged. Enforce it: Task 11 puts a
// PKCE inside a login struct that may well end up in a debug line or an error.
func TestNewPKCEDoesNotLeakVerifierWhenPrinted(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%s", "%#v", "%q"} {
		if s := fmt.Sprintf(format, p); strings.Contains(s, p.Verifier) {
			t.Errorf("fmt.Sprintf(%q, pkce) leaked the verifier: %s", format, s)
		}
	}
	if s := fmt.Sprintf("%v", struct{ P PKCE }{p}); strings.Contains(s, p.Verifier) {
		t.Errorf("an embedding struct leaked the verifier: %s", s)
	}
}

func TestNewStateIsRandomAndURLSafe(t *testing.T) {
	a, err := NewState()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewState()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two NewState() calls produced the same value")
	}
	if strings.ContainsAny(a, "+/=") {
		t.Fatalf("state %q contains a character outside the base64url alphabet", a)
	}
	if len(a) < 43 {
		t.Fatalf("len(state) = %d, want at least 43", len(a))
	}
}
