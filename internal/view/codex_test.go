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

// A machine with no codex accounts renders exactly what it rendered before this
// field existed. That is not politeness: seven byte-compared dashboard pages
// and every `ccdad status` fixture in the tree assert those bytes, and a clause
// that appeared unconditionally would move all of them for a machine that has
// no second provider on it.
func TestTheActiveLineIsUnchangedWithNoCodexAccount(t *testing.T) {
	s := Snapshot{ActiveLabel: "work@example.com (work)"}
	if got := s.ActiveLine(); got != "work@example.com (work)" {
		t.Fatalf("ActiveLine = %q, want the bare label", got)
	}
}

func TestTheActiveLineNamesBothProvidersWhenCodexIsServed(t *testing.T) {
	s := Snapshot{ActiveLabel: "work@example.com (work)", CodexServingLabel: "cx@example.com"}
	want := "Claude: work@example.com (work) · Codex: cx@example.com"
	if got := s.ActiveLine(); got != want {
		t.Fatalf("ActiveLine = %q, want %q", got, want)
	}
}

// The free function is what lets `ccdad which` -- which has no Snapshot --
// produce the identical sentence. Two spellings of one line is how the two
// commands come to disagree about a machine neither of them measured twice.
func TestActiveLineIsOneSpellingForEveryCaller(t *testing.T) {
	if got := ActiveLine("a", "b"); got != "Claude: a · Codex: b" {
		t.Fatalf("ActiveLine = %q", got)
	}
	if got := ActiveLine("a", ""); got != "a" {
		t.Fatalf("ActiveLine with no codex = %q, want the bare Claude label", got)
	}
}
