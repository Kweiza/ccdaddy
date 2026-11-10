package cli

import (
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// The CLI's half of the 2026-11-10 incident: `ccdad switch support` answered
// "Already on support" because it asked the keychain item alone, and doctor
// had no row that compared the item with the file.

// splitMachine is the incident's machine: chan in the credentials file and in
// ccdad's record, support in the keychain item Claude Code reads first.
func splitMachine(t *testing.T) *cclink.FakeKeychain {
	t.Helper()
	seedAccount(t, "uuid-chan", "chan@example.com")
	seedAccount(t, "uuid-support", "support@example.com")
	writeLiveFile(t, liveLoginJSON("RT-uuid-chan", ""))
	kc := cclink.NewFakeKeychain(liveLoginJSON("RT-uuid-support", ""))
	t.Cleanup(cclink.SetKeychainForTest(kc))
	recordLive(t, "uuid-chan")
	return kc
}

func recordLive(t *testing.T, uuid string) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive(uuid); err != nil {
		t.Fatal(err)
	}
}

func liveFileNow(t *testing.T) string {
	t.Helper()
	return string(readFileForTest(t, mustPath(ccpath.CredentialsPath())))
}

func TestSwitchRepairsASplitLoginInsteadOfSayingAlreadyOn(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	kc := splitMachine(t)

	code, _, stderr, _ := runRoot(t, "switch", "support@example.com")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if strings.Contains(stderr, "Already on") {
		t.Fatalf("a split login was reported as already on:\n%s", stderr)
	}
	if !strings.Contains(stderr, "The login was split") || !strings.Contains(stderr, "chan@example.com") {
		t.Fatalf("the switch does not say the login was split, or which store held which account:\n%s", stderr)
	}
	if got := liveFileNow(t); !strings.Contains(got, "RT-uuid-support") {
		t.Fatalf("the credentials file still holds the other account:\n%s", got)
	}
	if secret, _ := kc.Secret(); !strings.Contains(secret, "RT-uuid-support") {
		t.Fatalf("the item no longer holds support: %s", secret)
	}
}

func TestDoctorFailsASplitLoginAndNamesEachStore(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	splitMachine(t)

	code, r, _ := runDoctor(t)
	if got := r.level(t, "login-split"); got != "fail" {
		t.Fatalf("login-split = %q, want fail: %s", got, r.detail(t, "login-split"))
	}
	if code == ExitOK {
		t.Fatal("doctor exited 0 over a split login")
	}
	detail := r.detail(t, "login-split")
	for _, want := range []string{
		"keychain item Claude Code reads first holds support@example.com (uuid-support)",
		"credentials file holds chan@example.com (uuid-chan)",
		"records chan@example.com (uuid-chan) as live",
		"ccdad switch",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("login-split detail is missing %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "RT-") {
		t.Fatalf("the detail prints a token:\n%s", detail)
	}
}

func TestDoctorPassesALoginWhoseStoresAndRecordAgree(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	seedAccount(t, "uuid-support", "support@example.com")
	writeLiveFile(t, liveLoginJSON("RT-uuid-support", ""))
	t.Cleanup(cclink.SetKeychainForTest(cclink.NewFakeKeychain(liveLoginJSON("RT-uuid-support", ""))))
	recordLive(t, "uuid-support")

	_, r, _ := runDoctor(t)
	if got := r.level(t, "login-split"); got != "ok" {
		t.Fatalf("login-split = %q, want ok: %s", got, r.detail(t, "login-split"))
	}
}

// Both stores on one account and only the record behind: Claude Code spends
// nothing it should not, so it is a warning, not a failure.
func TestDoctorWarnsWhenOnlyTheRecordIsBehind(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	seedAccount(t, "uuid-chan", "chan@example.com")
	seedAccount(t, "uuid-support", "support@example.com")
	writeLiveFile(t, liveLoginJSON("RT-uuid-support", ""))
	t.Cleanup(cclink.SetKeychainForTest(cclink.NewFakeKeychain(liveLoginJSON("RT-uuid-support", ""))))
	recordLive(t, "uuid-chan")

	_, r, _ := runDoctor(t)
	if got := r.level(t, "login-split"); got != "warn" {
		t.Fatalf("login-split = %q, want warn: %s", got, r.detail(t, "login-split"))
	}
}

// A file holding a login ccdad cannot name, beside a different login in the
// item, is not "fine": it may be Claude Code's newer login, written there
// because its keychain write failed. doctor warns.
func TestDoctorWarnsAboutAFileItCannotNameBesideADifferentItem(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	seedAccount(t, "uuid-support", "support@example.com")
	writeLiveFile(t, liveLoginJSON("RT-stranger", ""))
	t.Cleanup(cclink.SetKeychainForTest(cclink.NewFakeKeychain(liveLoginJSON("RT-uuid-support", ""))))
	recordLive(t, "uuid-support")

	_, r, _ := runDoctor(t)
	if got := r.level(t, "login-split"); got != "warn" {
		t.Fatalf("login-split = %q, want warn: %s", got, r.detail(t, "login-split"))
	}
	if !strings.Contains(r.detail(t, "login-split"), "cannot name") {
		t.Fatalf("the detail does not say a login cannot be named: %s", r.detail(t, "login-split"))
	}
}

// A switch to the account both stores already hold, with only the record
// behind, corrects the record and says so -- it does not call the login split.
func TestSwitchDoesNotCallARecordOnlyCorrectionSplit(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	seedAccount(t, "uuid-chan", "chan@example.com")
	seedAccount(t, "uuid-support", "support@example.com")
	writeLiveFile(t, liveLoginJSON("RT-uuid-support", ""))
	t.Cleanup(cclink.SetKeychainForTest(cclink.NewFakeKeychain(liveLoginJSON("RT-uuid-support", ""))))
	recordLive(t, "uuid-chan")

	_, _, stderr, _ := runRoot(t, "switch", "support@example.com")
	if strings.Contains(stderr, "split") {
		t.Fatalf("a record-only correction was called a split:\n%s", stderr)
	}
	if !strings.Contains(stderr, "Already on") {
		t.Fatalf("stderr = %q, want the already-on answer with the record corrected", stderr)
	}
}

// "Already on" an api-key account means no OAuth login in front of the key in
// EITHER store. A login left in the credentials file -- behind an item that
// holds none -- is one Claude Code falls back to whenever its keychain read
// fails.
func TestAnAPIKeySwitchIsNotAlreadyOnWhileTheFileHoldsALogin(t *testing.T) {
	isolate(t)
	suppressAutoStart(t)
	seedAPIKeyAccount(t, "uuid-key", "key@example.com")
	if code, _, _, top := runRoot(t, "switch", "key@example.com"); code != ExitOK {
		t.Fatalf("first switch = %d (%s)", code, top)
	}
	t.Cleanup(cclink.SetKeychainForTest(cclink.NewFakeKeychain(`{}`)))
	writeLiveFile(t, liveLoginJSON("RT-left-behind", ""))

	code, _, stderr, _ := runRoot(t, "switch", "key@example.com")
	if code == ExitNothingToDo {
		t.Fatalf("an api-key switch said already on with a login in the file:\n%s", stderr)
	}
	if strings.Contains(liveFileNow(t), "claudeAiOauth") {
		t.Fatalf("the login in the file is still in front of the key:\n%s", liveFileNow(t))
	}
}
