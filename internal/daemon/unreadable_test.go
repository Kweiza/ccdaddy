package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// breakLiveStore makes cclink.Load fail rather than answer empty, which is what
// a locked login keychain does. The symlink is the portable stand-in:
// openCredentialsFile opens O_NOFOLLOW and refuses it, and unlike
// errSecInteractionNotAllowed it is reachable off macOS.
func breakLiveStore(t *testing.T) {
	t.Helper()
	path := mustPath(ccpath.CredentialsPath())
	_ = os.Remove(path)
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere.json"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := cclink.Load(); !errors.Is(err, cclink.ErrSymlink) {
		t.Fatalf("precondition: cclink.Load must fail, got %v", err)
	}
}

// THE REGRESSION, and it is the one that cost eight hours. An unreadable login
// store used to reach the swap, which failed at its write under the credential
// locks and returned the error — so EVERY tick failed, at 1 Hz, and the daemon
// spent its whole replacement budget on a fault no fresh process could clear.
// The stand-down belongs before the swap, where the reason can be named.
func TestATickWithAnUnreadableLoginStoreStandsDownInsteadOfFailing(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	breakLiveStore(t)

	var said []string
	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(90), nil
	})
	e.Log = func(format string, a ...any) { said = append(said, fmt.Sprintf(format, a...)) }

	for i := 0; i < 3; i++ {
		if err := e.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d failed on an unreadable store: %v", i, err)
		}
		e.Wait()
	}

	n := 0
	for _, line := range said {
		if strings.Contains(line, "cannot be read") {
			n++
		}
	}
	if n == 0 {
		t.Fatalf("the stand-down was not reported:\n%s", strings.Join(said, "\n"))
	}
	if n != 1 {
		t.Fatalf("the stand-down was logged %d times over three ticks; it must latch:\n%s",
			n, strings.Join(said, "\n"))
	}
}

// Standing down means writing NOTHING: not the live store, and not an account's
// snapshot. A grant spent here is a grant revoked under whatever session Claude
// Code is still serving out of its own fallback.
func TestAnUnreadableLoginStoreSpendsAndWritesNothing(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	before := map[string][]byte{}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"u-1", "u-2"} {
		creds, err := s.Credentials(uuid)
		if err != nil {
			t.Fatal(err)
		}
		before[uuid] = append([]byte(nil), creds["claudeAiOauth"]...)
	}
	breakLiveStore(t)

	e := engineFor(t, tokensAreFine, func(context.Context, string) (*usage.Snapshot, error) {
		return snapshotWith(95), nil
	})
	// Nil already, and named here because it is the assertion: reaching either
	// would be ccdad spending a grant it cannot show is idle.
	e.Freshen = nil
	e.ResolveOwner = nil

	for i := 0; i < 3; i++ {
		if err := e.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		e.Wait()
	}

	s2, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for uuid, was := range before {
		creds, err := s2.Credentials(uuid)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(creds["claudeAiOauth"], was) {
			t.Fatalf("%s's stored login moved while the store was unreadable:\n was %s\n now %s",
				uuid, was, creds["claudeAiOauth"])
		}
	}
	// The symlink is still a symlink: nothing tried to write through it.
	if fi, err := os.Lstat(mustPath(ccpath.CredentialsPath())); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the live path was written through: mode %v, err %v", fi.Mode(), err)
	}
}
