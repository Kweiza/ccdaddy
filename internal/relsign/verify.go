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
