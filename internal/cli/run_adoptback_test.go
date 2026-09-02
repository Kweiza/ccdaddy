package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"strings"
)

// stubSessionKeychain replaces the two windows onto a session's scoped item.
// read is what the item answers; deleted collects every item Delete was asked
// for, so a test can assert the teardown named the right one.
func stubSessionKeychain(t *testing.T, read func(cclink.KeychainItem) (string, bool, error)) *[]cclink.KeychainItem {
	t.Helper()
	savedRead, savedDelete := sessionKeychainRead, sessionKeychainDelete
	t.Cleanup(func() { sessionKeychainRead, sessionKeychainDelete = savedRead, savedDelete })
	deleted := &[]cclink.KeychainItem{}
	sessionKeychainRead = read
	sessionKeychainDelete = func(it cclink.KeychainItem) error {
		*deleted = append(*deleted, it)
		return nil
	}
	return deleted
}

func noItem(cclink.KeychainItem) (string, bool, error) { return "", false, nil }

// sessionFor builds a runSession over a temp home, scoped the way newSession
// scopes one, without touching the real store home.
func sessionFor(t *testing.T) runSession {
	t.Helper()
	home := t.TempDir()
	return runSession{home: home, env: []string{"USER=tester", "CLAUDE_SECURESTORAGE_CONFIG_DIR=" + home}, ephemeral: true}
}

// seedLogin is seedAccount with the stored login spelled out, because every
// assertion here is about which exact blob ended up in the snapshot.
func seedLogin(t *testing.T, uuid string, oauth string) {
	t.Helper()
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Add(store.Account{Provider: provider.Claude, UUID: uuid, Email: uuid + "@example.com"},
		cclink.Blob{"claudeAiOauth": json.RawMessage(oauth)}); err != nil {
		t.Fatal(err)
	}
}

func storedOAuth(t *testing.T, uuid string) string {
	t.Helper()
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	creds, err := st.Credentials(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return string(creds["claudeAiOauth"])
}

func writeSessionFile(t *testing.T, session runSession, oauth string) {
	t.Helper()
	body := []byte(`{"claudeAiOauth":` + oauth + `}`)
	if err := os.WriteFile(filepath.Join(session.home, ccpath.CredentialsFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// THE REGRESSION. On macOS the ordinary shape of a session that refreshed is a
// scoped keychain item holding the rotated pair and NO file: the combinator
// deletes the fallback when the primary write succeeds. Reading only the file
// scored that as "never refreshed" and threw the rotation away, leaving the
// account's snapshot holding a grant the server had already revoked.
func TestAdoptBackCarriesARotationOutOfTheSessionKeychainItem(t *testing.T) {
	isolate(t)
	seedLogin(t, "u-1", `{"accessToken":"a1","refreshToken":"r1"}`)
	session := sessionFor(t)
	rotated := `{"accessToken":"a2","refreshToken":"r2"}`
	stubSessionKeychain(t, func(cclink.KeychainItem) (string, bool, error) {
		return `{"claudeAiOauth":` + rotated + `}`, true, nil
	})

	if err := adoptBack("u-1", session); err != nil {
		t.Fatalf("adoptBack: %v", err)
	}
	if got := storedOAuth(t, "u-1"); got != rotated {
		t.Fatalf("stored login = %s, want the rotated pair %s", got, rotated)
	}
}

// The item is Claude Code's PRIMARY store, so where both answer it is the one
// that is current. A stale file left beside a written item must not win.
func TestAdoptBackPrefersTheKeychainItemOverTheFile(t *testing.T) {
	isolate(t)
	seedLogin(t, "u-1", `{"accessToken":"a1","refreshToken":"r1"}`)
	session := sessionFor(t)
	writeSessionFile(t, session, `{"accessToken":"aOLD","refreshToken":"rOLD"}`)
	fromItem := `{"accessToken":"a2","refreshToken":"r2"}`
	stubSessionKeychain(t, func(cclink.KeychainItem) (string, bool, error) {
		return `{"claudeAiOauth":` + fromItem + `}`, true, nil
	})

	if err := adoptBack("u-1", session); err != nil {
		t.Fatalf("adoptBack: %v", err)
	}
	if got := storedOAuth(t, "u-1"); got != fromItem {
		t.Fatalf("stored login = %s, want the item's %s", got, fromItem)
	}
}

// A read that FAILS is not "no login". The caller keeps the session directory
// on an error, so reporting one is what keeps a rotated credential recoverable;
// answering nil would let removeSession delete the only copy.
func TestAdoptBackReportsAKeychainItItCannotRead(t *testing.T) {
	isolate(t)
	seedLogin(t, "u-1", `{"accessToken":"a1","refreshToken":"r1"}`)
	session := sessionFor(t)
	locked := errors.New("security find-generic-password: interaction-not-allowed (exit 36)")
	stubSessionKeychain(t, func(cclink.KeychainItem) (string, bool, error) { return "", false, locked })

	err := adoptBack("u-1", session)
	if err == nil {
		t.Fatal("adoptBack succeeded; an unreadable item must be reported so the session is kept")
	}
	if !errors.Is(err, locked) {
		t.Fatalf("adoptBack error = %v, want it to carry the keychain failure", err)
	}
	if got := storedOAuth(t, "u-1"); got != `{"accessToken":"a1","refreshToken":"r1"}` {
		t.Fatalf("the snapshot was written from a failed read: %s", got)
	}
}

// Neither store holding anything is the ordinary case for a session that never
// refreshed, and for one that never started.
func TestAdoptBackIsSilentWhenNeitherStoreHasALogin(t *testing.T) {
	isolate(t)
	seedLogin(t, "u-1", `{"accessToken":"a1","refreshToken":"r1"}`)
	stubSessionKeychain(t, noItem)

	if err := adoptBack("u-1", sessionFor(t)); err != nil {
		t.Fatalf("adoptBack: %v", err)
	}
	if got := storedOAuth(t, "u-1"); got != `{"accessToken":"a1","refreshToken":"r1"}` {
		t.Fatalf("the snapshot moved with nothing to move it to: %s", got)
	}
}

// The file half still works, and a file that cannot be read for a reason other
// than absence is now an error rather than a silent "never refreshed".
func TestAdoptBackStillReadsTheFileAndReportsAnUnreadableOne(t *testing.T) {
	isolate(t)
	seedLogin(t, "u-1", `{"accessToken":"a1","refreshToken":"r1"}`)
	stubSessionKeychain(t, noItem)

	session := sessionFor(t)
	fromFile := `{"accessToken":"a2","refreshToken":"r2"}`
	writeSessionFile(t, session, fromFile)
	if err := adoptBack("u-1", session); err != nil {
		t.Fatalf("adoptBack: %v", err)
	}
	if got := storedOAuth(t, "u-1"); got != fromFile {
		t.Fatalf("stored login = %s, want the file's %s", got, fromFile)
	}

	// A directory where the credentials file should be is an EISDIR, not an
	// ENOENT: the one shape that separates "absent" from "unreadable".
	broken := sessionFor(t)
	if err := os.Mkdir(filepath.Join(broken.home, ccpath.CredentialsFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := adoptBack("u-1", broken); err == nil {
		t.Fatal("adoptBack succeeded on an unreadable credentials file")
	}
}

// Tearing a session down means both of its stores. Without this every session
// that ever refreshed leaves a live refresh token in the login keychain under a
// name derived from a directory that no longer exists, which nothing can name
// again.
func TestRemoveSessionDeletesTheSessionsKeychainItem(t *testing.T) {
	isolate(t)
	session := sessionFor(t)
	deleted := stubSessionKeychain(t, noItem)

	if err := removeSession(session); err != nil {
		t.Fatalf("removeSession: %v", err)
	}
	if len(*deleted) != 1 {
		t.Fatalf("Delete was called %d times, want exactly one", len(*deleted))
	}
	want := cclink.LiveKeychainItem(session.env)
	if (*deleted)[0] != want {
		t.Fatalf("deleted %+v, want the session's own item %+v", (*deleted)[0], want)
	}
	if want.Service == "Claude Code-credentials" {
		t.Fatal("the session's item name is the machine's; a scoped session must never name the live item")
	}
	if _, err := os.Stat(session.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the session home survived removeSession: %v", err)
	}
}

// Claude Code does not degrade a rejected refresh, it ZEROES the credential in
// place: on invalid_grant 2.1.251 rewrites it as
// {...d,refreshToken:"",accessToken:"",expiresAt:0}. That is still a valid
// claudeAiOauth and still different from the stored one, so the adopt-back
// would have written it -- overwriting the grant the server still honours with
// one that cannot be switched to, polled with, or refreshed back into life.
func TestAdoptBackRefusesACredentialClaudeCodeZeroed(t *testing.T) {
	isolate(t)
	alive := `{"accessToken":"a1","refreshToken":"r1"}`
	seedLogin(t, "u-1", alive)
	session := sessionFor(t)
	zeroed := `{"accessToken":"","refreshToken":"","expiresAt":0,"scopes":["user:inference"]}`
	stubSessionKeychain(t, func(cclink.KeychainItem) (string, bool, error) {
		return `{"claudeAiOauth":` + zeroed + `}`, true, nil
	})

	if err := adoptBack("u-1", session); err != nil {
		t.Fatalf("adoptBack: %v", err)
	}
	if got := storedOAuth(t, "u-1"); got != alive {
		t.Fatalf("the zeroed record was carried into the store: %s", got)
	}
}

// The refusal is about an ABSENT grant, not about anything else the record may
// have lost, so a rotation that still carries a refresh token still lands.
func TestAdoptBackStillTakesARecordThatCarriesAGrant(t *testing.T) {
	isolate(t)
	seedLogin(t, "u-1", `{"accessToken":"a1","refreshToken":"r1"}`)
	session := sessionFor(t)
	rotated := `{"accessToken":"","refreshToken":"r2"}`
	stubSessionKeychain(t, func(cclink.KeychainItem) (string, bool, error) {
		return `{"claudeAiOauth":` + rotated + `}`, true, nil
	})

	if err := adoptBack("u-1", session); err != nil {
		t.Fatalf("adoptBack: %v", err)
	}
	if got := storedOAuth(t, "u-1"); got != rotated {
		t.Fatalf("stored login = %s, want the rotated pair %s", got, rotated)
	}
}

// A session's grant can be a GENERATION BEHIND by the time it ends: a second
// `ccdad run` on the account, a probe of it, or the poller's own rotation all
// mint a new pair, and the server revokes the one this session was seeded with.
// Writing that back is not a lost refresh -- it is the next `ccdad switch`
// handing Claude Code a dead token.
func TestAdoptBackRefusesAGrantTheStoreHasAlreadyMovedPast(t *testing.T) {
	isolate(t)
	newer := `{"accessToken":"a2","refreshToken":"r2","expiresAt":2000000000000}`
	seedLogin(t, "u-1", newer)
	session := sessionFor(t)
	older := `{"accessToken":"a1","refreshToken":"r1","expiresAt":1000000000000}`
	stubSessionKeychain(t, func(cclink.KeychainItem) (string, bool, error) {
		return `{"claudeAiOauth":` + older + `}`, true, nil
	})

	err := adoptBack("u-1", session)
	if err == nil {
		t.Fatal("adoptBack succeeded with a grant older than the stored one")
	}
	if !strings.Contains(err.Error(), "generation behind") {
		t.Fatalf("the error does not say why it refused: %v", err)
	}
	if got := storedOAuth(t, "u-1"); got != newer {
		t.Fatalf("the older grant was written over the newer one: %s", got)
	}
}

// The ordinary rotation this function exists for still lands: a session grant
// NEWER than the stored one is exactly what adopt-back is for.
func TestAdoptBackStillTakesAGrantNewerThanTheStoredOne(t *testing.T) {
	isolate(t)
	seedLogin(t, "u-1", `{"accessToken":"a1","refreshToken":"r1","expiresAt":1000000000000}`)
	session := sessionFor(t)
	newer := `{"accessToken":"a2","refreshToken":"r2","expiresAt":2000000000000}`
	stubSessionKeychain(t, func(cclink.KeychainItem) (string, bool, error) {
		return `{"claudeAiOauth":` + newer + `}`, true, nil
	})

	if err := adoptBack("u-1", session); err != nil {
		t.Fatalf("adoptBack: %v", err)
	}
	if got := storedOAuth(t, "u-1"); got != newer {
		t.Fatalf("stored login = %s, want the session's newer grant", got)
	}
}
