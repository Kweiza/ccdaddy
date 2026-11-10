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
