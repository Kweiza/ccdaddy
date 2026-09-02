package provider

import (
	"strings"
	"testing"
)

// The two names are the wire form. They appear in accounts.toml, in every
// --json payload and in an export document, so they are a compatibility
// commitment rather than an implementation detail: renaming one would make
// every stored account unreadable by the build that wrote it.
func TestTheTwoProviderNamesAreTheWireForm(t *testing.T) {
	if Claude.String() != "claude" {
		t.Errorf("Claude.String() = %q, want claude", Claude.String())
	}
	if Codex.String() != "codex" {
		t.Errorf("Codex.String() = %q, want codex", Codex.String())
	}
}

// Parse is strict on purpose. The empty string is the ZERO value of the field
// this parses into, and a zero that resolved to Claude here would make a
// version-2 document with a missing provider read as a working Claude account
// -- which is the one reading the store's version rules exist to refuse.
func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    ID
		wantErr bool
	}{
		{"claude", "claude", Claude, false},
		{"codex", "codex", Codex, false},
		{"empty", "", "", true},
		{"unknown", "gemini", "", true},
		{"wrong case", "Claude", "", true},
		{"padded", " codex", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = %v, nil; want an error", tc.in, got)
				}
				if got != "" {
					t.Errorf("Parse(%q) returned %q alongside its error; want the zero ID", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) = %v, want nil", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Parse(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The error names what was rejected. A refusal that does not quote the value
// is unactionable in the one place this fires: a hand-edited accounts.toml.
func TestParseNamesWhatItRejected(t *testing.T) {
	_, err := Parse("gemini")
	if err == nil {
		t.Fatal("Parse(gemini) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "gemini") {
		t.Errorf("Parse error = %q, want it to quote the value it refused", err)
	}
	if !strings.Contains(err.Error(), "claude") || !strings.Contains(err.Error(), "codex") {
		t.Errorf("Parse error = %q, want it to name both providers it accepts", err)
	}
}

func TestValid(t *testing.T) {
	for _, tc := range []struct {
		in   ID
		want bool
	}{
		{Claude, true},
		{Codex, true},
		{ID(""), false},
		{ID("gemini"), false},
	} {
		if got := tc.in.Valid(); got != tc.want {
			t.Errorf("ID(%q).Valid() = %v, want %v", string(tc.in), got, tc.want)
		}
	}
}
