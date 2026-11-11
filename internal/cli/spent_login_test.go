package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink/cclinktest"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// A stored login a newer one has already superseded is not presented by any
// ccdad command. Claude Code refreshed chan's login, and with its keychain
// write failing wrote the newer login to the credentials file; the daemon
// named it chan's through the profile endpoint and could not store it. chan's
// STORED refresh token is the one Claude Code just spent, and presenting it
// again -- installed by a switch, seeded into a `ccdad run` session -- can
// revoke the newer login with it. The daemon used to be the only process that
// knew: the hold lived in its memory.
func spentMachine(t *testing.T) *cclinktest.Keychain {
	t.Helper()
	seedAccount(t, "uuid-chan", "chan@example.com")
	seedAccount(t, "uuid-support", "support@example.com")
	writeLiveFile(t, liveLoginJSON("RT-uuid-chan-newer", ""))
	kc := cclinktest.NewKeychain(liveLoginJSON("RT-uuid-support", ""))
	cclinktest.Install(t, kc)
	recordLive(t, "uuid-support")
	nameUnstoredLogin(t, "refresh:RT-uuid-chan-newer", "uuid-chan")
	return kc
}

// nameUnstoredLogin is the daemon's record of the profile endpoint naming a
// login it could not store.
func nameUnstoredLogin(t *testing.T, loginIdentity, uuid string) {
	t.Helper()
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		st.NameLogin(store.LoginHash(loginIdentity), uuid)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSwitchDoesNotInstallAStoredLoginANewerOneHasSpent(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	kc := spentMachine(t)

	code, _, stderr, _ := runRoot(t, "switch", "chan@example.com")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d; stderr:\n%s", code, ExitBlocked, stderr)
	}
	if !strings.Contains(stderr, "already spent") {
		t.Fatalf("the refusal does not say the stored refresh token is spent:\n%s", stderr)
	}
	if got := liveFileNow(t); !strings.Contains(got, "RT-uuid-chan-newer") {
		t.Fatalf("the newer login was written over with the spent one:\n%s", got)
	}
	if secret, _ := kc.Secret(); !strings.Contains(secret, "RT-uuid-support") {
		t.Fatalf("the item was written: %s", secret)
	}
}

func TestRunDoesNotSeedASessionWithAStoredLoginANewerOneHasSpent(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	spentMachine(t)
	stub := stubClaude(t, ExitOK)

	code, _, stderr, top := runRoot(t, "run", "chan@example.com")
	if code != ExitBlocked {
		t.Fatalf("exit = %d, want %d; stderr:\n%s%s", code, ExitBlocked, top, stderr)
	}
	if stub.started {
		t.Fatal("a session was started with a stored login whose refresh token is spent")
	}
	if !strings.Contains(top+stderr, "already spent") {
		t.Fatalf("the refusal does not say the stored refresh token is spent:\n%s%s", top, stderr)
	}
}

// The daemon's own probe is a separate process -- `ccdad probe --uuid U
// --force` -- and the Claude Code it runs refreshes the stored login it is
// seeded with. It applies the hold itself, forced or not.
func TestAForcedProbeDoesNotSpendAStoredLoginANewerOneHasSpent(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	freezeClock(t, probeNow)
	spentMachine(t)
	stub := stubClaude(t, ExitOK)

	code, _, stderr, top := runRoot(t, "probe", "--uuid", "uuid-chan", "--force")
	if stub.started {
		t.Fatalf("a probe was run with a stored login whose refresh token is spent (exit %d)", code)
	}
	if !strings.Contains(top+stderr, "already spent") {
		t.Fatalf("the refusal does not say the stored refresh token is spent:\n%s%s", top, stderr)
	}
}
