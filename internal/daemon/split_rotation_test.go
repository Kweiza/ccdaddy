package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// THE HOLD OUTLIVES A SECOND ROTATION. The oracle names G1 as u-1's newer
// login and storing it fails, so u-1's stored refresh token -- the one G1 was
// minted from -- is spent. Claude Code then refreshes G1 into G2 before anybody
// has named G2. G1 is gone from every store, and nothing on this machine can
// say G2 is u-1's; the stored token is no less spent for it, and presenting it
// now revokes G2 -- the rotation-stomp class. So the hold lasts until the
// stored login CHANGES from the spent one, and that fact is persisted, because
// it cannot be derived once G1 has left the stores.
func TestTheHoldOutlivesASecondRotationBeforeItIsNamed(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	withItem(t, "u-2")
	writeLiveLoginRT(t, "RT-u-1-g1", tickEpoch.Add(9*time.Hour))
	recordActive(t, "u-2")

	saved := adoptInto
	t.Cleanup(func() { adoptInto = saved })
	adoptInto = func(*store.Store, store.Account, cclink.Blob) error {
		return errors.New("locking the ccdad store: timed out")
	}

	now := tickEpoch
	e := engineFor(t, tokensAreFine, noFetch)
	e.Now = func() time.Time { return now }
	fetches := 0
	e.AccessToken = func(_ context.Context, uuid string) (string, error) {
		if uuid == "u-1" {
			fetches++
		}
		return "AT-" + uuid, nil
	}
	e.ResolveOwner = func(_ context.Context, token string) (string, error) {
		if token == "AT-RT-u-1-g1" {
			return "u-1", nil
		}
		return "", errors.New("the profile endpoint did not answer")
	}

	tick(t, e)
	if e.holdSpentToken("u-1") == nil {
		t.Fatal("precondition: naming G1 and failing to store it held nothing")
	}

	// Claude Code refreshes G1 into G2, into the same store, before G2 is named.
	writeLiveLoginRT(t, "RT-u-1-g2", tickEpoch.Add(10*time.Hour))
	now = now.Add(resolveMinInterval)
	fetches = 0
	tick(t, e)
	if _, err := e.accessToken(context.Background(), "u-1"); !errors.Is(err, store.ErrStoredLoginSpent) {
		t.Errorf("u-1's token read = %v, want store.ErrStoredLoginSpent: its stored refresh token is spent", err)
	}
	if fetches != 0 {
		t.Errorf("u-1's spent stored token was read %d times after G1 was rotated away", fetches)
	}
}
