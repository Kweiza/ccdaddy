package store

import (
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
)

// A Codex row's history names nobody, for the reason its stored blob does
// not: a Claude-shaped login a Codex row once carried, and replaced, proves
// nothing about whose Claude login it is.
func TestACodexRowsHistoryNamesNobody(t *testing.T) {
	accounts := []Account{{UUID: "u-x", Provider: provider.Codex, SupersededLogins: LoginHash("refresh:RT-x")}}
	lookup := func(string) (cclink.Blob, error) { return cclink.Blob{}, nil }
	if a, ev := OwnerByEvidence("refresh:RT-x", accounts, lookup); ev != NoEvidence {
		t.Fatalf("OwnerByEvidence = %s, %v; a Codex row's history named a Claude login", a.UUID, ev)
	}
}
