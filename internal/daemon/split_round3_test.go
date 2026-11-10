package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// The third review round of the split-login repair.

func openEngineStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustUnmarshal(t *testing.T, raw string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		t.Fatal(err)
	}
}

// S-D. The oracle's answer is remembered only once its adoption SUCCEEDED.
// Remembered before, a failed adoption would name the login from memory on
// every later tick, so it would never be asked about or adopted again -- and
// it may be the account's only live grant.
func TestAnOracleAnswerWhoseAdoptionFailedIsNotRemembered(t *testing.T) {
	isolateEngine(t)
	seedAccountExpiring(t, "u-1", "org-1", tickEpoch.Add(time.Hour))
	root, err := ccpath.StoreHome()
	if err != nil {
		t.Fatal(err)
	}
	// The stored login cannot be read, so the adoption cannot happen.
	if err := os.WriteFile(filepath.Join(root, "credentials", "u-1.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openEngineStore(t)
	var newer cclink.Blob
	mustUnmarshal(t, blobExpiring("RT-u-1-newer", tickEpoch.Add(8*time.Hour)), &newer)

	e := engineFor(t, tokensAreFine, noFetch)
	e.ResolveOwner = func(context.Context, string) (string, error) { return "u-1", nil }
	if _, verdict := e.resolveStore(context.Background(), s, newer); verdict != liveUnresolved {
		t.Fatalf("verdict = %v, want unresolved: the adoption could not happen", verdict)
	}
	if _, ok := e.ownerOf(cclink.LoginIdentity(newer)); ok {
		t.Fatal("an answer whose adoption failed was remembered; the login would never be adopted")
	}
}

// S-F. The tick whose identification ADOPTED a login does not act on its plan:
// that plan was ranked from the stores as they were before one of them was
// named. Here the plan, made from the item's spent u-2, would move to u-3;
// the file turns out to hold u-1's newer login, so the next tick's
// evaluation, not this one, decides.
func TestTheTickThatAdoptsALoginDoesNotActOnItsPlan(t *testing.T) {
	isolateEngine(t)
	seedAccountExpiring(t, "u-1", "org-1", tickEpoch.Add(time.Hour))
	seedAccount(t, "u-2", "org-2")
	seedAccount(t, "u-3", "org-3")
	writeLiveLoginRT(t, "RT-u-1-refreshed", tickEpoch.Add(8*time.Hour))
	kc := cclink.NewFakeKeychain(loginExpiring("u-2", tickEpoch.Add(time.Hour)))
	t.Cleanup(cclink.SetKeychainForTest(kc))
	recordActive(t, "u-2")
	readings(t, map[string]float64{"u-1": 36, "u-2": 99, "u-3": 5})

	e := engineFor(t, tokensAreFine, noFetch)
	e.ResolveOwner = func(_ context.Context, token string) (string, error) { return "u-1", nil }
	var log logLines
	log.attach(e)
	tick(t, e)

	if !log.contains("newer login of") {
		t.Fatalf("the precondition did not hold: nothing was adopted: %q", log.lines)
	}
	if got := activeOnDisk(t); got != "u-2" {
		t.Fatalf("active = %q: the tick that adopted a login acted on a plan made before it", got)
	}
	if n := kc.Writes(); n != 0 {
		t.Fatalf("the item was written %d times in the tick that adopted a login", n)
	}
}
