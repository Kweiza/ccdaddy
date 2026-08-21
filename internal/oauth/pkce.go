package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCE is one proof-key pair for a single login attempt.
//
// The verifier never leaves this process until the token exchange, and it is
// never logged: holding it plus an intercepted authorization code is enough to
// mint a token. String and GoString below enforce that second half.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE generates a fresh pair. 32 random bytes encode to a 43-character
// verifier, inside RFC 7636's 43..128 range.
func NewPKCE() (PKCE, error) {
	verifier, err := randomBase64URL(32)
	if err != nil {
		return PKCE{}, fmt.Errorf("generating PKCE verifier: %w", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	return PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// String redacts the verifier so that a PKCE printed with %v, %+v, %s or %#v —
// in an error, a debug line, or a struct that embeds it — cannot leak it. The
// verifier is only ever read through the field.
func (p PKCE) String() string { return "PKCE{Verifier:REDACTED, Challenge:" + p.Challenge + "}" }

// GoString keeps %#v redacted too; without it fmt falls back to the raw struct.
func (p PKCE) GoString() string { return p.String() }

// NewState returns a CSRF state value for one login attempt.
func NewState() (string, error) {
	s, err := randomBase64URL(32)
	if err != nil {
		return "", fmt.Errorf("generating OAuth state: %w", err)
	}
	return s, nil
}

// randomBase64URL returns n cryptographically random bytes, base64url-encoded
// without padding — the encoding PKCE mandates.
//
// Since Go 1.24 crypto/rand.Read never returns an error; it panics if the system
// source fails. The error return here is future-proofing and keeps callers
// honest, not a live path.
func randomBase64URL(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
