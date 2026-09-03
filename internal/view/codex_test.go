package view

import (
	"testing"

	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// The TYPE column answers "how is this account metered", and for a Codex
// account the honest answer is not "subscription": every ChatGPT plan is one,
// so the word would be true of every codex row and would distinguish nothing
// from the Claude rows beside it. What a reader needs there is which side of
// the machine the row belongs to.
func TestTypeLabelNamesTheProviderForACodexAccount(t *testing.T) {
	r := Row{Account: store.Account{
		UUID: "cx-1", Provider: provider.Codex, Kind: identity.KindSubscription,
	}}
	if got := r.TypeLabel(); got != "codex" {
		t.Fatalf("TypeLabel = %q, want codex", got)
	}
}

func TestTypeLabelStillNamesTheKindForAClaudeAccount(t *testing.T) {
	for _, k := range []identity.Kind{identity.KindSubscription, identity.KindAPIKey} {
		r := Row{Account: store.Account{UUID: "cl-1", Provider: provider.Claude, Kind: k}}
		if got, want := r.TypeLabel(), k.String(); got != want {
			t.Fatalf("TypeLabel = %q, want %q", got, want)
		}
	}
}

// A row read out of a version-1 document has a zero Provider, which store.load
// fills in as Claude. Nothing here may treat the zero value as codex.
func TestTypeLabelOnAZeroProviderIsNotCodex(t *testing.T) {
	r := Row{Account: store.Account{UUID: "cl-1", Kind: identity.KindSubscription}}
	if got := r.TypeLabel(); got == "codex" {
		t.Fatal("TypeLabel = codex on a row with no provider set")
	}
}
