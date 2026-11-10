package cclink

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
)

// The read-back has the WRITE bound, not the lookup bound: it asks the same
// slow keychain that just failed to answer a write in time. Every spawn here
// takes 1.5 s, a lookup is allowed 0.3 s and the write path 6 s, so a read-back
// held to the lookup bound cannot confirm the write that landed.
func TestTheReadBackIsHeldToTheWriteBound(t *testing.T) {
	withClaudeHome(t)
	file := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(file, []byte(oldLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeSecurity{item: statefulItem(t, oldLogin), hang: 1500 * time.Millisecond, writeHang: time.Minute}.install(t)
	keychainReadTimeout = 300 * time.Millisecond
	keychainWriteTimeout = 6 * time.Second

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)}); err != nil {
		t.Fatalf("Activate = %v: the read-back did not get the write bound", err)
	}
	if got := fileNow(t, file); !strings.Contains(got, "RT-new") {
		t.Fatalf("the file was not written after a confirmed write:\n%s", got)
	}
}

// A store that already holds the login is not written. Repairing a split
// installs an account the item already has; on the slow keychain that needs
// the repair, a rewrite of the item spends two bounded spawns to change
// nothing, and an unconfirmable one would fail the whole repair.
func TestAStoreThatAlreadyHoldsTheLoginIsNotWritten(t *testing.T) {
	withClaudeHome(t)
	file := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(file, []byte(oldLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	kc := NewFakeKeychain(`{"claudeAiOauth":` + newLogin + `}`)
	kc.WriteLandsThenTimesOut, kc.ReadsFailAfterWrite = true, true
	t.Cleanup(SetKeychainForTest(kc))

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)}); err != nil {
		t.Fatalf("Activate = %v; the item already held the login and needed no write", err)
	}
	if n := kc.Writes(); n != 0 {
		t.Fatalf("the item was written %d times although it already held the login", n)
	}
	if got := fileNow(t, file); !strings.Contains(got, "RT-new") {
		t.Fatalf("the file, which did differ, was not written:\n%s", got)
	}
}

// The same login IDENTITY is not the same credential. A token endpoint may
// rotate the access token and keep the refresh token; skipping a store that
// holds the old access token under the same refresh token would leave the live
// login on the old one while ccdad's copy moved on.
func TestAStoreWithTheSameRefreshTokenButAnOlderAccessTokenIsWritten(t *testing.T) {
	withClaudeHome(t)
	file := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(file, []byte(oldLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	kc := NewFakeKeychain(`{"claudeAiOauth":{"accessToken":"AT-before","refreshToken":"RT-new"}}`)
	t.Cleanup(SetKeychainForTest(kc))

	if err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if n := kc.Writes(); n != 1 {
		t.Fatalf("the item holding an older access token was written %d times, want 1", n)
	}
	if secret, _ := kc.Secret(); !strings.Contains(secret, `"AT-new"`) {
		t.Fatalf("the item still holds the older access token: %s", secret)
	}
}

// S-E. The read-back compares CREDENTIALS, not identity. A write that keeps the
// refresh token and changes the access token, ran out of time and did NOT
// land, leaves the item on the old access token under the same identity;
// calling that "landed" would write the file and leave the two stores holding
// different credentials that no split check can see.
func TestAnUnlandedWriteUnderTheSameRefreshTokenIsNotReadBackAsLanded(t *testing.T) {
	withClaudeHome(t)
	file := mustPath(ccpath.CredentialsPath())
	before := `{"claudeAiOauth":{"accessToken":"AT-before","refreshToken":"RT-new"}}`
	if err := os.WriteFile(file, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	kc := NewFakeKeychain(before)
	kc.WriteTimesOutUnlanded = true
	t.Cleanup(SetKeychainForTest(kc))

	err := Activate(Blob{"claudeAiOauth": json.RawMessage(newLogin)})
	if err == nil {
		t.Fatal("a write that did not land under the same refresh token was read back as landed")
	}
	if got := fileNow(t, file); strings.Contains(got, `"AT-new"`) {
		t.Fatalf("the file was written past a keychain write that did not land:\n%s", got)
	}
}

// A LOGOUT that ran out of time is confirmed by the item holding no login.
// Comparing identities there compares "" with "", which never matches, so every
// slow logout read as unknown and left the file holding the login.
func TestALogoutThatLandedAndTimedOutCompletes(t *testing.T) {
	withClaudeHome(t)
	file := mustPath(ccpath.CredentialsPath())
	if err := os.WriteFile(file, []byte(oldLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	kc := NewFakeKeychain(oldLogin)
	kc.WriteLandsThenTimesOut = true
	t.Cleanup(SetKeychainForTest(kc))

	if err := ClearLogin(); err != nil {
		t.Fatalf("ClearLogin = %v; the item holds no login, so the logout landed", err)
	}
	if got := fileNow(t, file); strings.Contains(got, "claudeAiOauth") {
		t.Fatalf("the file still holds the login after a confirmed logout:\n%s", got)
	}
}
