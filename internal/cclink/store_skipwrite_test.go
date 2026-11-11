package cclink_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/cclink/cclinktest"
)

// These run against cclinktest's in-memory item, from outside the package, the
// way every other package's split-login tests do: what is under test is the
// decision ABOVE the exec layer -- which store a switch writes, and how a write
// that ran out of time is read back -- not the spawn.

const (
	oldLogin = `{"claudeAiOauth":{"accessToken":"AT-old","refreshToken":"RT-old"}}`
	newLogin = `{"accessToken":"AT-new","refreshToken":"RT-new"}`
)

// credentialsFileHolding sandboxes Claude Code's home and puts body in its
// credentials file, returning the file's path.
func credentialsFileHolding(t *testing.T, body string) string {
	t.Helper()
	file := cclink.SandboxClaudeHomeForTest(t)
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func fileNow(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A store that already holds the login is not written. Repairing a split
// installs an account the item already has; on the slow keychain that needs
// the repair, a rewrite of the item spends two bounded spawns to change
// nothing, and an unconfirmable one would fail the whole repair.
func TestAStoreThatAlreadyHoldsTheLoginIsNotWritten(t *testing.T) {
	file := credentialsFileHolding(t, oldLogin)
	kc := cclinktest.NewKeychain(`{"claudeAiOauth":` + newLogin + `}`)
	kc.WriteLandsThenTimesOut, kc.ReadsFailAfterWrite = true, true
	cclinktest.Install(t, kc)

	if err := cclink.Activate(cclink.Blob{"claudeAiOauth": json.RawMessage(newLogin)}); err != nil {
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
	credentialsFileHolding(t, oldLogin)
	kc := cclinktest.NewKeychain(`{"claudeAiOauth":{"accessToken":"AT-before","refreshToken":"RT-new"}}`)
	cclinktest.Install(t, kc)

	if err := cclink.Activate(cclink.Blob{"claudeAiOauth": json.RawMessage(newLogin)}); err != nil {
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
	before := `{"claudeAiOauth":{"accessToken":"AT-before","refreshToken":"RT-new"}}`
	file := credentialsFileHolding(t, before)
	kc := cclinktest.NewKeychain(before)
	kc.WriteTimesOutUnlanded = true
	cclinktest.Install(t, kc)

	err := cclink.Activate(cclink.Blob{"claudeAiOauth": json.RawMessage(newLogin)})
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
	file := credentialsFileHolding(t, oldLogin)
	kc := cclinktest.NewKeychain(oldLogin)
	kc.WriteLandsThenTimesOut = true
	cclinktest.Install(t, kc)

	if err := cclink.ClearLogin(); err != nil {
		t.Fatalf("ClearLogin = %v; the item holds no login, so the logout landed", err)
	}
	if got := fileNow(t, file); strings.Contains(got, "claudeAiOauth") {
		t.Fatalf("the file still holds the login after a confirmed logout:\n%s", got)
	}
}
