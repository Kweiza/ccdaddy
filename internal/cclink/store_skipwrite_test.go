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
