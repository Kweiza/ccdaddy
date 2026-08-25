package relsign

import (
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
