package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// Round 7.

// The spent identity is the stored login the verdict COMPARED, never one read
// again after the adoption failed. Here the adoption of G1 waits on the store
// lock while another process stores G2, then fails. A re-read would record G2
// -- a live login -- as spent, and the hold would never lift.
func TestTheSpentLoginIsTheOneTheVerdictCompared(t *testing.T) {
	isolateEngine(t)
	seedAccountExpiring(t, "u-1", "org-1", tickEpoch.Add(time.Hour)) // G0
	var g1 cclink.Blob
	mustUnmarshal(t, blobExpiring("RT-u-1-g1", tickEpoch.Add(8*time.Hour)), &g1)

	saved := adoptInto
	t.Cleanup(func() { adoptInto = saved })
	adoptInto = func(s *store.Store, a store.Account, _ cclink.Blob) error {
		// Another process stores G2 while this adoption waits, and this one
		// fails.
		other := openEngineStore(t)
		cur, _ := other.Get(a.UUID)
		if err := other.Add(cur, oauthBlob("RT-u-1-g2")); err != nil {
			t.Fatal(err)
		}
		return errors.New("locking the ccdad store: timed out")
	}

	e := engineFor(t, tokensAreFine, noFetch)
	e.ResolveOwner = func(context.Context, string) (string, error) { return "u-1", nil }
	if _, verdict := e.resolveStore(context.Background(), openEngineStore(t), g1); verdict != liveAdoptFailed {
		t.Fatalf("verdict = %v, want adopt-failed", verdict)
	}
	if e.holdSpentToken("u-1") == nil {
		t.Fatal("precondition: a failed adoption placed no hold")
	}
	e.releaseStored(openEngineStore(t))
	if err := e.holdSpentToken("u-1"); err != nil {
		t.Fatalf("the hold stayed although G2, not the spent G0, is stored: %v", err)
	}
}

// A HELD account is not a plan target: its stored login would be refused by
// every path that installs it, so the engine picks the next candidate instead
// of standing down every tick.
func TestAHeldAccountIsNotAPlanTarget(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	seedAccount(t, "u-3", "org-3")
	liveAs(t, "u-1")
	recordActive(t, "u-1")
	readings(t, map[string]float64{"u-1": 99, "u-2": 1, "u-3": 30})

	e := engineFor(t, tokensAreFine, noFetch)
	e.holdUntilStored("u-2", "refresh:RT-u-2-newer", "refresh:RT-u-2")
	tick(t, e)

	if got := activeOnDisk(t); got != "u-3" {
		t.Fatalf("active = %q, want u-3: the held u-2 was planned for, or nothing moved", got)
	}
}
