package relsign

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"
)

// pubBody assembles the base64 body of a .pub file's second line from its
// three fields, so a case can bend exactly one of them and leave the rest
// well-formed.
func pubBody(alg string, keyNum [8]byte, key []byte) string {
	raw := append([]byte(alg), keyNum[:]...)
	raw = append(raw, key...)
	return base64.StdEncoding.EncodeToString(raw)
}

func TestParsePublicKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	num := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	long := append(append([]byte{}, pub...), 0)

	tests := []struct {
		name string
		line string
		want error
	}{
		{"a real key", pubBody("Ed", num, pub), nil},
		{"surrounding whitespace", "  " + pubBody("Ed", num, pub) + "\n", nil},
		{"the prehashed algorithm", pubBody("ED", num, pub), ErrAlgorithm},
		{"one byte short", pubBody("Ed", num, pub[:31]), ErrMalformed},
		{"one byte long", pubBody("Ed", num, long), ErrMalformed},
		{"not base64", "!!" + pubBody("Ed", num, pub), ErrMalformed},
		{"empty", "", ErrMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePublicKey(tc.line)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ParsePublicKey() error = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				return
			}
			if got.KeyNum != num {
				t.Errorf("KeyNum = %x, want %x", got.KeyNum, num)
			}
			if !got.Key.Equal(pub) {
				t.Error("the ed25519 key does not round-trip")
			}
		})
	}
}
