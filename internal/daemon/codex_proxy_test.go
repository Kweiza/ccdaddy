package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// plantDeadLaunch writes the two files a codex launcher that was killed leaves
// behind: a record and a lock nothing holds.
func plantDeadLaunch(t *testing.T, root, name string) (lockPath, jsonPath string) {
	t.Helper()
	dir := filepath.Join(root, "codex", "launches")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath = filepath.Join(dir, name+".lock")
	jsonPath = filepath.Join(dir, name+".json")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonPath, []byte(`{"pin":"","startedAt":"2026-09-02T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return lockPath, jsonPath
}

func TestDeadLaunchRecordsAreReapedButNotOnEveryTick(t *testing.T) {
	root := isolate(t)
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	e := NewEngine()
	e.Now = func() time.Time { return now }

	lock, doc := plantDeadLaunch(t, root, "aaaa1111")
	e.reapCodexLaunches()
	for _, p := range []string{lock, doc} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived the sweep", filepath.Base(p))
		}
	}

	// The tick runs about once a second. A sweep that ran every time would stat
	// and try-lock every live codex session's record 86,400 times a day.
	lock, doc = plantDeadLaunch(t, root, "bbbb2222")
	e.reapCodexLaunches()
	if _, err := os.Stat(doc); err != nil {
		t.Fatalf("a second sweep ran inside the interval: %v", err)
	}

	now = now.Add(codexReapInterval + time.Second)
	e.reapCodexLaunches()
	for _, p := range []string{lock, doc} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived the sweep after the interval passed", filepath.Base(p))
		}
	}
}

func TestReapingAStoreWithNoLaunchesIsSilent(t *testing.T) {
	isolate(t)
	e := NewEngine()
	var lines []string
	e.Log = func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) }
	e.reapCodexLaunches()
	if len(lines) != 0 {
		t.Fatalf("a store with no launches logged %v", lines)
	}
}
