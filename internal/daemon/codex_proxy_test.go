package daemon

import (
	"testing"

	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

func TestTheRankingTheLaneRecordedIsWhatTheProxyReads(t *testing.T) {
	e := NewEngine()
	if got := e.codexRanked(); len(got) != 0 {
		t.Fatalf("codexRanked() = %v before any lane tick, want nothing", got)
	}
	e.SetCodexRanked([]string{"uuid-a", "uuid-b"})
	got := e.codexRanked()
	if len(got) != 2 || got[0] != "uuid-a" || got[1] != "uuid-b" {
		t.Fatalf("codexRanked() = %v, want the recorded order", got)
	}
	// The proxy reads this from a request goroutine while the lane writes it
	// from the tick, so it must hand out a copy.
	got[0] = "clobbered"
	if again := e.codexRanked(); again[0] != "uuid-a" {
		t.Fatalf("codexRanked() = %v after a caller wrote into the last result", again)
	}
}

func TestAHarvestedReadingIsHandedToTheLaneExactlyOnce(t *testing.T) {
	e := NewEngine()
	if _, ok := e.CodexSample("uuid-a"); ok {
		t.Fatal("CodexSample() reported a reading nothing harvested")
	}
	snap := &usage.Snapshot{}
	e.harvestCodexSample("uuid-a", snap)

	got, ok := e.CodexSample("uuid-a")
	if !ok || got != snap {
		t.Fatalf("CodexSample() = (%v, %v), want the harvested reading", got, ok)
	}
	// Once committed it must not be committed again, or one reading would be
	// written into the cache on every tick until another replaced it.
	if _, ok := e.CodexSample("uuid-a"); ok {
		t.Fatal("the same reading was handed out twice")
	}
}

// The lane's ranking is what the proxy serves a new thread from, and the two
// live in different packages joined by exactly this function.
func TestTheLanesRankingReachesTheProxy(t *testing.T) {
	ev := switcher.Evaluation{Plan: strategy.Plan{Result: strategy.Result{
		Order: []strategy.Ranked{{UUID: "uuid-a"}, {UUID: "uuid-b"}},
	}}}
	got := rankedUUIDs(ev)
	if len(got) != 2 || got[0] != "uuid-a" || got[1] != "uuid-b" {
		t.Fatalf("rankedUUIDs() = %v, want [uuid-a uuid-b] in that order", got)
	}
	if got := rankedUUIDs(switcher.Evaluation{}); len(got) != 0 {
		t.Fatalf("rankedUUIDs() = %v for an evaluation that never ranked, want nothing", got)
	}
}

func TestAHarvestWithNothingInItIsIgnored(t *testing.T) {
	e := NewEngine()
	e.harvestCodexSample("", &usage.Snapshot{})
	e.harvestCodexSample("uuid-a", nil)
	if _, ok := e.CodexSample("uuid-a"); ok {
		t.Fatal("a nil reading was recorded")
	}
	if _, ok := e.CodexSample(""); ok {
		t.Fatal("a reading with no account was recorded")
	}
}
