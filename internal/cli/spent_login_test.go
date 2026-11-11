package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/cclink/cclinktest"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
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
	recordUnstoredLogin(t, "uuid-chan", "refresh:RT-uuid-chan-newer", "refresh:RT-uuid-chan")
	return kc
}

// recordUnstoredLogin is the daemon's record of the profile endpoint naming
// newer as uuid's and storing it failing, with spent the stored login it
// replaced (daemon.Engine.nameUnstored).
func recordUnstoredLogin(t *testing.T, uuid, newer, spent string) {
	t.Helper()
	if err := strategy.WithState(time.Second, func(st *strategy.State) error {
		st.NameLogin(store.LoginHash(newer), uuid)
		st.RecordSpentStoredLogin(uuid, store.LoginHash(spent), store.LoginHash(newer))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Claude Code then refreshes the newer login AGAIN, into the same file, before
// the profile endpoint has named what replaced it. Nothing in the stores
// connects chan's stored login to the login there now; its refresh token is no
// less spent, and presenting it revokes the whole family. Neither a switch nor
// a session presents it.
func TestASpentStoredLoginStaysHeldWhenTheNewerLoginIsRotatedAgainBeforeItIsNamed(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	spentMachine(t)
	writeLiveFile(t, liveLoginJSON("RT-uuid-chan-rotated-again", ""))

	code, _, stderr, _ := runRoot(t, "switch", "chan@example.com")
	if code != ExitBlocked || !strings.Contains(stderr, "already spent") {
		t.Fatalf("switch: exit = %d, want %d and the spent-login refusal; stderr:\n%s", code, ExitBlocked, stderr)
	}
	if got := liveFileNow(t); !strings.Contains(got, "RT-uuid-chan-rotated-again") {
		t.Fatalf("the login Claude Code is on was written over with the spent one:\n%s", got)
	}

	stub := stubClaude(t, ExitOK)
	code, _, stderr, top := runRoot(t, "run", "chan@example.com")
	if code != ExitBlocked || stub.started {
		t.Fatalf("run: exit = %d, started = %v; want %d and no session:\n%s%s", code, stub.started, ExitBlocked, top, stderr)
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

// A keychain that cannot be read -- locked over SSH, or past the lookup bound
// on the loaded Mac -- is no evidence that the hold has lifted. The record
// says the stored login is spent, and it says so without the stores.
func TestARecordedSpentLoginStaysHeldWhenTheKeychainCannotBeRead(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	freezeClock(t, probeNow)
	kc := spentMachine(t)
	writeLiveFile(t, liveLoginJSON("RT-uuid-chan-rotated-again", ""))
	kc.FailReads(true)
	stub := stubClaude(t, ExitOK)

	code, _, stderr, top := runRoot(t, "run", "chan@example.com")
	if stub.started || code != ExitBlocked {
		t.Fatalf("run: exit = %d, started = %v; want %d and no session:\n%s%s", code, stub.started, ExitBlocked, top, stderr)
	}
	code, _, stderr, top = runRoot(t, "probe", "--uuid", "uuid-chan", "--force")
	if stub.started {
		t.Fatalf("probe --force spent a stored login recorded as spent (exit %d):\n%s%s", code, top, stderr)
	}
}

// THE CHECK AND THE USE ARE ONE READ. The daemon stores chan's newer login
// while `ccdad run` is reading the login stores, after it read the spent one.
// A check that read the store again would find nothing held and seed the
// session with the spent copy it had in hand.
func TestRunSeedsTheStoredLoginTheHoldWasDecidedOn(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	kc := spentMachine(t)
	adopted := false
	kc.OnRead = func() {
		if !adopted {
			adopted = true
			s, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			a, _ := s.Get("uuid-chan")
			if err := s.Add(a, credsFor("RT-uuid-chan-newer")); err != nil {
				t.Fatal(err)
			}
		}
	}
	var seeded []byte
	stubClaudeDuring(t, ExitOK, func(spec launchSpec) {
		home, _ := envOf(spec.Env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
		seeded, _ = os.ReadFile(filepath.Join(home, ccpath.CredentialsFile))
	})

	runRoot(t, "run", "chan@example.com")
	if strings.Contains(string(seeded), `"RT-uuid-chan"`) {
		t.Fatalf("the session was seeded with the spent stored login:\n%s", seeded)
	}
}

// A setup token beside a spent OAuth login is run with the token: Claude Code
// takes it from the environment, and no file is seeded, so the spent login is
// never presented. The hold is for the path that seeds the login, and only
// that one.
func TestASetupTokenRunsBesideASpentOAuthLogin(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	spentMachine(t)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Get("uuid-chan")
	creds, err := s.Credentials("uuid-chan")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := json.Marshal(cclink.TokenRecord{Kind: "setup-token", Token: "sk-ant-oat-chan"})
	if err != nil {
		t.Fatal(err)
	}
	creds[cclink.TokenKey] = rec
	if err := s.Add(a, creds); err != nil {
		t.Fatal(err)
	}
	stub := stubClaude(t, ExitOK)

	code, _, stderr, top := runRoot(t, "run", "chan@example.com")
	if code != ExitOK || !stub.started {
		t.Fatalf("exit = %d, started = %v; want the setup token run:\n%s%s", code, stub.started, top, stderr)
	}
	if got, _ := envOf(stub.spec.Env, "CLAUDE_CODE_OAUTH_TOKEN"); got != "sk-ant-oat-chan" {
		t.Fatalf("CLAUDE_CODE_OAUTH_TOKEN = %q, want the setup token", got)
	}
}
