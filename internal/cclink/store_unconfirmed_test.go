package cclink

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// The 2026-11-10 incident, as fixtures: a keychain write that LANDS and then
// runs out of time. Before the fix ccdad called that "nothing was written",
// left the credentials file on the old account, and recorded nothing -- while
// the item held the new one.

const (
	oldLogin = `{"claudeAiOauth":{"accessToken":"AT-old","refreshToken":"RT-old"}}`
	newLogin = `{"accessToken":"AT-new","refreshToken":"RT-new"}`
)

// slowKeychain installs a stateful item holding the old login, with the file
// holding the same, and a write bound far shorter than the write takes.
func slowKeychain(t *testing.T, f fakeSecurity) (item, file string) {
	t.Helper()
	withClaudeHome(t)
	file = mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(file, []byte(oldLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	item = statefulItem(t, oldLogin)
	f.item = item
	f.install(t)
	// The write sleeps a minute after landing; the bound is 6 s, so the write is
	// killed with its outcome already decided. 6 s and not less: each READ is a
	// re-exec of this test binary, and under -race on a loaded machine one took
	// over a second, which failed the reads at a 1 s bound.
	keychainWriteTimeout = 6 * time.Second
	return item, file
}

func fileNow(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// THE CORE CASE. The write landed and timed out; reading the item back finds
// the new login, so the switch is complete: the file follows the item, and the
// caller is told it succeeded.
func TestAWriteThatLandedAndTimedOutCompletesTheSwitch(t *testing.T) {
	item, file := slowKeychain(t, fakeSecurity{writeHang: time.Minute})

	err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)})
	if err != nil {
		t.Fatalf("Activate = %v, want nil: the item holds the new login", err)
	}
	if got := itemNow(t, item); !strings.Contains(got, "RT-new") {
		t.Fatalf("the item does not hold the new login: %s", got)
	}
	if got := fileNow(t, file); !strings.Contains(got, "RT-new") {
		t.Fatalf("the item holds the new login and the file was left on the old one:\n%s", got)
	}
}

// The write ran out of time and did NOT land: the item still holds the login it
// held. That is the failure it looked like, and it is reported as one.
func TestAWriteThatTimedOutWithoutLandingMovesNothing(t *testing.T) {
	item, file := slowKeychain(t, fakeSecurity{writeHang: time.Minute, writeLost: true})

	err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)})
	if err == nil {
		t.Fatal("Activate succeeded although the item still holds the old login")
	}
	if errors.Is(err, ErrLoginOutcomeUnknown) {
		t.Fatalf("the item was read back and holds the old login, so the outcome is known: %v", err)
	}
	if !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("the error does not say the login did not change: %v", err)
	}
	if got := itemNow(t, item); strings.Contains(got, "RT-new") {
		t.Fatalf("the fixture stored a write it was told to lose: %s", got)
	}
	if got := fileNow(t, file); strings.Contains(got, "RT-new") {
		t.Fatalf("the file was moved although the login was not:\n%s", got)
	}
}

// The write ran out of time and the item cannot be read back. Neither answer is
// claimed: the error is ErrLoginOutcomeUnknown, nothing further is written, and
// the message says the next pass repairs it.
func TestAWriteThatCannotBeReadBackClaimsNeitherOutcome(t *testing.T) {
	_, file := slowKeychain(t, fakeSecurity{writeHang: time.Minute, readFailsAfterWrite: true})

	err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)})
	if !errors.Is(err, ErrLoginOutcomeUnknown) {
		t.Fatalf("Activate = %v, want ErrLoginOutcomeUnknown", err)
	}
	if strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("an unconfirmed write was reported as nothing written: %v", err)
	}
	if !strings.Contains(err.Error(), "next pass") {
		t.Fatalf("the error does not say what happens next: %v", err)
	}
	if got := fileNow(t, file); strings.Contains(got, "RT-new") {
		t.Fatalf("the file was written past a keychain write nobody could confirm:\n%s", got)
	}
}

// A write `security` ANSWERED as failed is not read back: it is a failure, and
// "nothing was written" is true of it. writeOutcomeUnknown is what tells the
// two apart.
func TestOnlyAWriteCcdadGaveUpOnHasAnUnknownOutcome(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"timed out", &KeychainError{Op: "add-generic-password", failure: failTimedOut}, true},
		{"output pipe outlived it", &KeychainError{Op: "add-generic-password", failure: failLingering}, true},
		{"refused", &KeychainError{Op: "add-generic-password", failure: failNoInteraction, code: 36}, false},
		{"binary missing", &KeychainError{Op: "add-generic-password", failure: failSecurityMissing}, false},
		{"not a keychain error", errors.New("encoding"), false},
		{"ended by a signal", &KeychainError{Op: "add-generic-password", failure: failSignaled}, true},
		{"spawn failed with no status", &KeychainError{Op: "add-generic-password", failure: failOther}, true},
		{"exited with a status and unrecognised stderr", &KeychainError{Op: "add-generic-password", failure: failOther, code: 1, stderr: "?"}, false},
	}
	for _, tc := range cases {
		if got := writeOutcomeUnknown(tc.err); got != tc.want {
			t.Errorf("%s: writeOutcomeUnknown = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ONE PLACE DEFINES EACH BOUND, and the two are not the same. A lookup is held
// to the short one; everything on the write path -- the merge-base read, the
// write -- to the long one. A keychain that takes 1.5 s per call fails a
// lookup and completes a switch.
func TestALookupAndASwitchRunUnderDifferentBounds(t *testing.T) {
	withClaudeHome(t)
	file := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(file, []byte(oldLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeSecurity{item: statefulItem(t, oldLogin), hang: 1500 * time.Millisecond}.install(t)
	keychainReadTimeout = 300 * time.Millisecond
	keychainWriteTimeout = 20 * time.Second

	if _, _, err := testItem.Read(context.Background()); err == nil {
		t.Fatal("a lookup outlived the read bound")
	}
	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)}); err != nil {
		t.Fatalf("a switch on a slow keychain failed under the write bound: %v", err)
	}
	if got := fileNow(t, file); !strings.Contains(got, "RT-new") {
		t.Fatalf("the switch did not reach the file:\n%s", got)
	}
}

// The write bound is the one the incident needed: longer than Claude Code's own
// 10 s and longer than the 9.4 s measured on the loaded Mac. A bound brought
// back under that measurement is the regression this pins.
func TestTheWriteBoundOutlastsTheMeasuredSlowKeychain(t *testing.T) {
	const measured = 9400 * time.Millisecond
	if keychainWriteTimeout <= measured {
		t.Fatalf("keychainWriteTimeout = %s, which a measured loaded-Mac call (%s) exceeds", keychainWriteTimeout, measured)
	}
	if keychainReadTimeout >= keychainWriteTimeout {
		t.Fatalf("the read bound (%s) is not shorter than the write bound (%s)", keychainReadTimeout, keychainWriteTimeout)
	}
}

// ReadLoginStores reports BOTH stores, which Load cannot: an item on one login
// and a file on another is the split this exists to see.
func TestReadLoginStoresReportsEachStore(t *testing.T) {
	withClaudeHome(t)
	if err := os.WriteFile(mustPath(ccpath.CredentialsPath()), []byte(oldLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeSecurity{item: statefulItem(t, `{"claudeAiOauth":`+newLogin+`}`)}.install(t)

	st, err := ReadLoginStores()
	if err != nil {
		t.Fatalf("ReadLoginStores: %v", err)
	}
	if !st.HasItem || LoginIdentity(st.Item) != "refresh:RT-new" {
		t.Fatalf("item = %v (has %v), want the new login", st.Item, st.HasItem)
	}
	if LoginIdentity(st.File) != "refresh:RT-old" {
		t.Fatalf("file = %v, want the old login", st.File)
	}
	live, src, err := st.Live()
	if err != nil || src != SourceKeychainItem || LoginIdentity(live) != "refresh:RT-new" {
		t.Fatalf("Live = %v, %v, %v; want the item's login", live, src, err)
	}
}
