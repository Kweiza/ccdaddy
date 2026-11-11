package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// Round 7.

// A failed adoption records only the naming; whether the stored token is spent
// is decided against whatever the account stores when it is asked. Here the
// adoption of G1 waits on the store lock while another process stores G2, a
// later generation, and then fails. A hold fixed to the login the verdict
// compared would hold the account on G2 -- a live login -- until the daemon
// restarted.
func TestAFailedAdoptionRacingALaterStoredLoginHoldsNothing(t *testing.T) {
	isolateEngine(t)
	seedAccountExpiring(t, "u-1", "org-1", tickEpoch.Add(time.Hour)) // G0
	writeLiveLoginRT(t, "RT-u-1-g1", tickEpoch.Add(8*time.Hour))
	var g1 cclink.Blob
	mustUnmarshal(t, blobExpiring("RT-u-1-g1", tickEpoch.Add(8*time.Hour)), &g1)

	saved := adoptInto
	t.Cleanup(func() { adoptInto = saved })
	adoptInto = func(s *store.Store, a store.Account, _ cclink.Blob) error {
		// Another process stores G2 while this adoption waits, and this one
		// fails.
		other := openEngineStore(t)
		cur, _ := other.Get(a.UUID)
		var g2 cclink.Blob
		mustUnmarshal(t, blobExpiring("RT-u-1-g2", tickEpoch.Add(16*time.Hour)), &g2)
		if err := other.Add(cur, g2); err != nil {
			t.Fatal(err)
		}
		return errors.New("locking the ccdad store: timed out")
	}

	e := engineFor(t, tokensAreFine, noFetch)
	e.ResolveOwner = func(context.Context, string) (string, error) { return "u-1", nil }
	if _, verdict := e.resolveStore(context.Background(), openEngineStore(t), g1); verdict != liveAdoptFailed {
		t.Fatalf("verdict = %v, want adopt-failed", verdict)
	}
	st, err := strategy.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if uuid, ok := st.NamedLogin(store.LoginHash("refresh:RT-u-1-g1")); !ok || uuid != "u-1" {
		t.Fatalf("precondition: the failed adoption did not record the naming: %q %v", uuid, ok)
	}
	passSees(t, e)
	if err := e.holdSpentToken("u-1"); err != nil {
		t.Fatalf("the account is held although G2, issued after G1, is stored: %v", err)
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
	// u-1 in the keychain item, and in the file behind it a newer login of
	// u-2 that the identity oracle named and ccdad could not store.
	withItem(t, "u-1")
	writeLiveLoginRT(t, "RT-u-2-newer", tickEpoch.Add(9*time.Hour))
	nameLogin(t, "refresh:RT-u-2-newer", "u-2")
	recordActive(t, "u-1")
	readings(t, map[string]float64{"u-1": 99, "u-2": 1, "u-3": 30})

	e := engineFor(t, tokensAreFine, noFetch)
	tick(t, e)

	if got := activeOnDisk(t); got != "u-3" {
		t.Fatalf("active = %q, want u-3: the held u-2 was planned for, or nothing moved", got)
	}
}

// The oracle's record of a login ccdad could not store is kept only while a
// login store holds that login: it can hold an account's stored token only
// then. A pass forgets the rest, and a pass whose file could not be read
// forgets nothing.
func TestAPassForgetsTheNamingOfALoginNoStoreHolds(t *testing.T) {
	isolateEngine(t)
	writeLiveLoginRT(t, "RT-u-1-newer", tickEpoch.Add(9*time.Hour))
	nameLogin(t, "refresh:RT-u-1-newer", "u-1")
	nameLogin(t, "refresh:RT-u-2-gone", "u-2")
	e := engineFor(t, tokensAreFine, noFetch)

	named := func(id string) bool {
		st, err := strategy.LoadState()
		if err != nil {
			t.Fatal(err)
		}
		_, ok := st.NamedLogin(store.LoginHash(id))
		return ok
	}
	stores, err := cclink.ReadLoginStores()
	if err != nil {
		t.Fatal(err)
	}
	unread := stores
	unread.FileErr = errors.New("the credentials file could not be read")
	e.forgetNamedLogins(unread)
	if !named("refresh:RT-u-2-gone") {
		t.Fatal("a pass that could not read the file forgot a naming")
	}
	e.forgetNamedLogins(stores)
	if named("refresh:RT-u-2-gone") {
		t.Error("the naming of a login no store holds was kept")
	}
	if !named("refresh:RT-u-1-newer") {
		t.Error("the naming of a login the credentials file holds was forgotten")
	}
}
