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

// A machine with no codex accounts gets exactly what it got before this field
// existed. `ccdad which` is read by shells, so a clause that appeared
// unconditionally would change what every one of them parses out of a machine
// that has no second provider on it.
func TestTheActiveLineIsUnchangedWithNoCodexAccount(t *testing.T) {
	if got := ActiveLine("work@example.com (work)", ""); got != "work@example.com (work)" {
		t.Fatalf("ActiveLine = %q, want the bare label", got)
	}
}

func TestTheActiveLineNamesBothProvidersWhenCodexIsServed(t *testing.T) {
	want := "Claude: work@example.com (work) · Codex: cx@example.com"
	if got := ActiveLine("work@example.com (work)", "cx@example.com"); got != want {
		t.Fatalf("ActiveLine = %q, want %q", got, want)
	}
}

// The joined line is `ccdad which`'s and belongs to no Snapshot. A surface with
// a Snapshot has room for a block and draws SummaryLines instead, one line per
// provider, so this must not grow a second entry point that would put a joined
// line back on a page that has already chosen the other shape.
func TestNoSnapshotCanProduceTheJoinedLine(t *testing.T) {
	if _, ok := any(Snapshot{}).(interface{ ActiveLine() string }); ok {
		t.Fatal("Snapshot has an ActiveLine method again; the joined line is `ccdad which`'s alone")
	}
}
