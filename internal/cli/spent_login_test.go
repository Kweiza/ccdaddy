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
	if err := store.WithSpentRecord(time.Second, func(st *store.SpentRecord) error {
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

// orphanedMachine is the hold nothing lifts on its own: the record says chan's
// stored login is spent, and no login store holds a login nobody can name --
// the item and the file both hold support's stored login. Whatever the newer
// login was, it is gone from this machine, and only logging chan in again
// stores another login.
func orphanedMachine(t *testing.T) {
	t.Helper()
	seedAccount(t, "uuid-chan", "chan@example.com")
	seedAccount(t, "uuid-support", "support@example.com")
	writeLiveFile(t, liveLoginJSON("RT-uuid-support", ""))
	cclinktest.Install(t, cclinktest.NewKeychain(liveLoginJSON("RT-uuid-support", "")))
	recordLive(t, "uuid-support")
	recordUnstoredLogin(t, "uuid-chan", "refresh:RT-uuid-chan-newer", "refresh:RT-uuid-chan")
}

// A HOLD NOTHING WILL LIFT IS SAID, AND SO IS WHAT DOES. Refusals used to say
// the daemon would store the newer login, which it never will once no store
// holds it; doctor and status said nothing at all, and the account sat out of
// every plan and every poll.
func TestAHoldOnlyALoginCanLiftSaysSoEverywhere(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	orphanedMachine(t)

	code, _, stderr, _ := runRoot(t, "switch", "chan@example.com")
	if code != ExitBlocked || !strings.Contains(stderr, "ccdad add claude") {
		t.Errorf("switch: exit = %d, want %d and the way out, `ccdad add claude`:\n%s", code, ExitBlocked, stderr)
	}
	if strings.Contains(stderr, "as soon as it can") || strings.Contains(stderr, "profile endpoint") {
		t.Errorf("switch suggests the daemon may yet store a login no store holds:\n%s", stderr)
	}

	_, r, _ := runDoctor(t)
	if got := r.level(t, "spent-logins"); got != "warn" {
		t.Errorf("spent-logins = %q, want warn: %s", got, r.detail(t, "spent-logins"))
	}
	if d := r.detail(t, "spent-logins"); !strings.Contains(d, "chan@example.com") ||
		!strings.Contains(d, "ccdad add claude") || strings.Contains(d, "profile endpoint") {
		t.Errorf("spent-logins does not name the account and the one way out: %s", d)
	}

	_, _, stderr, _ = runRoot(t, "status")
	if !strings.Contains(stderr, "chan@example.com") || !strings.Contains(stderr, "ccdad add claude") {
		t.Errorf("status does not say chan is held, or what lifts it:\n%s", stderr)
	}
}

// While a store holds a login nobody can name yet, it may be the newer login
// rotated again, and the daemon may still store it: the refusal says both
// that and what to do if it is not.
func TestAHoldBehindAnUnnamedLoginSaysTheDaemonMayStillStoreIt(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	spentMachine(t)
	writeLiveFile(t, liveLoginJSON("RT-uuid-chan-rotated-again", ""))

	_, _, stderr, _ := runRoot(t, "switch", "chan@example.com")
	if !strings.Contains(stderr, "profile endpoint") || !strings.Contains(stderr, "ccdad add claude") {
		t.Errorf("the refusal does not say the daemon may still name the login, and what to do if not:\n%s", stderr)
	}
}

func TestDoctorPassesWhenNoStoredLoginIsHeld(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	seedAccount(t, "uuid-support", "support@example.com")
	writeLiveFile(t, liveLoginJSON("RT-uuid-support", ""))

	_, r, _ := runDoctor(t)
	if got := r.level(t, "spent-logins"); got != "ok" {
		t.Errorf("spent-logins = %q, want ok: %s", got, r.detail(t, "spent-logins"))
	}
}

// ONE TABLE OF REASONS. What a refusal, doctor or status says about a hold is
// the sentence a SpentError prints in daemon.log, never a second copy of it.
func TestARefusalSaysWhatTheSpentErrorSays(t *testing.T) {
	a := store.Account{UUID: "uuid-chan", Email: "chan@example.com"}
	for _, hold := range []store.Hold{store.HeldByNewerLogin, store.HeldByRecord, store.HeldUnreadableRecord} {
		err := &store.SpentError{UUID: a.UUID, Label: a.Label(), Hold: hold}
		if got := spentLoginReason(a, hold, nil); got != err.Error() {
			t.Errorf("hold %d: the refusal says\n%q\nand the error\n%q", hold, got, err.Error())
		}
	}
}

// unreadableRecordMachine has a record of spent stored logins nobody can read,
// support on the login Claude Code holds, and chan stored in no store.
func unreadableRecordMachine(t *testing.T) {
	t.Helper()
	seedAccount(t, "uuid-chan", "chan@example.com")
	seedAccount(t, "uuid-support", "support@example.com")
	writeLiveFile(t, liveLoginJSON("RT-uuid-support", ""))
	recordLive(t, "uuid-support")
	if err := os.WriteFile(mustPath(store.SpentRecordPath()), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// RECOVERY MUST NOT UNDO THE PROTECTION. Removing an unreadable record releases
// every hold it kept, and a spent stored login is then presented again. So the
// advice is: restore the file, or log in again every account it may hold --
// named -- and only then remove it.
func TestTheAdviceForAnUnreadableRecordKeepsItsHolds(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	unreadableRecordMachine(t)

	_, r, _ := runDoctor(t)
	if got := r.level(t, "spent-logins"); got != "fail" {
		t.Errorf("spent-logins = %q, want fail: %s", got, r.detail(t, "spent-logins"))
	}
	d := r.detail(t, "spent-logins")
	for _, want := range []string{"chan@example.com", "ccdad add claude", "and only then remove it", "Restore ",
		"stays on `ccdad doctor`'s list after it is logged in again"} {
		if !strings.Contains(d, want) {
			t.Errorf("spent-logins does not say %q: %s", want, d)
		}
	}
	// support is on the login Claude Code holds, and is not held.
	if strings.Contains(d, "support@example.com") {
		t.Errorf("spent-logins lists an account the record cannot hold: %s", d)
	}

	code, _, stderr, _ := runRoot(t, "switch", "chan@example.com")
	if code != ExitBlocked {
		t.Fatalf("switch: exit = %d, want %d:\n%s", code, ExitBlocked, stderr)
	}
	for _, want := range []string{"ccdad add claude", "and only then remove it", "Restore "} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, stderr)
		}
	}
}

// An account whose stored login cannot be read either is on the list. The
// record may hold it; once its file reads again, removing the record would let
// that login be presented, so it has to be logged in again like the rest.
func TestTheRecoveryListKeepsAnAccountWhoseStoredLoginCannotBeRead(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	unreadableRecordMachine(t)
	creds := filepath.Join(mustPath(ccpath.StoreHome()), "credentials", "uuid-chan.json")
	if err := os.WriteFile(creds, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	if d := r.detail(t, "spent-logins"); !strings.Contains(d, "chan@example.com") {
		t.Fatalf("spent-logins leaves out an account whose stored login cannot be read: %s", d)
	}
}

// Doctor names owners the way the hold does. With the record unreadable and
// E2's stores -- support's login in the item, chan's in the file -- each login
// is the other account's, Claude Code is on support, and neither account's
// stored login can be shown spent by the other's: neither is listed.
func TestDoctorDoesNotListAnAccountForAnotherAccountsLogin(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	seedAccount(t, "uuid-chan", "chan@example.com")
	seedAccount(t, "uuid-support", "support@example.com")
	writeLiveFile(t, liveLoginJSON("RT-uuid-chan", ""))
	cclinktest.Install(t, cclinktest.NewKeychain(liveLoginJSON("RT-uuid-support", "")))
	recordLive(t, "uuid-chan")
	if err := os.WriteFile(mustPath(store.SpentRecordPath()), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, r, _ := runDoctor(t)
	d := r.detail(t, "spent-logins")
	if strings.Contains(d, "support@example.com") || strings.Contains(d, "chan@example.com") {
		t.Fatalf("spent-logins lists an account for another account's login: %s", d)
	}
}
