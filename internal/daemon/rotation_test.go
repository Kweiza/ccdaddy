package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/usage"
	"time"
)

// liveAsRotated writes the credentials file Claude Code leaves behind when it
// refreshes a login on its own: the same account, carrying the pair the server
// rotated to, which matches no stored snapshot.
func liveAsRotated(t *testing.T, uuid string) {
	t.Helper()
	body := `{"claudeAiOauth":{"accessToken":"AT-rotated-` + uuid +
		`","refreshToken":"RT-` + uuid + `-rotated"}}`
	if err := os.WriteFile(mustPath(ccpath.CredentialsPath()), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readLiveFile(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// usageByToken answers 5% for the account named, 95% for everyone else, so the
// ranking has one obvious winner.
func usageByToken(best string) (*httptest.Server, func(context.Context, string) (*usage.Snapshot, error)) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		used := 95.0
		if r.Header.Get("Authorization") == "Bearer AT-"+best {
			used = 5
		}
		fmt.Fprintf(w, `{"five_hour":{"utilization":%v,"resets_at":null}}`, used)
	}))
	client := usage.NewClient()
	client.BaseURL = srv.URL
	return srv, client.FetchUsage
}

// THE REGRESSION, at the level it actually happened.
//
// Observed 2026-08-25: the live account's access token expired at 23:55, Claude
// Code refreshed it, and from 00:07 the daemon logged "switched to <that same
// account>" eighteen times in four hours. Each pass wrote the store's
// pre-rotation snapshot over Claude Code's fresh one, which forced Claude Code
// to refresh again, which broke attribution again. The server eventually
// rejected the re-presented grant and logged the user out of Claude Code
// entirely.
//
// Every step of that was one read: a credentials file matching no stored
// snapshot was taken to mean nobody was live.
func TestATickDoesNotOverwriteALoginItCannotName(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	// u-1 is live AND the account with room, which is what made the engine
	// choose it over and over.
	liveAsRotated(t, "u-1")

	srv, fetch := usageByToken("u-1")
	defer srv.Close()
	e := engineFor(t, tokensAreFine, fetch)

	before := readLiveFile(t)
	tick(t, e) // polls; nothing to rank on yet
	tick(t, e) // ranks, and used to swap here

	if !bytes.Equal(before, readLiveFile(t)) {
		t.Fatalf("the tick overwrote a login it could not name: %s", readLiveFile(t))
	}
	if at, _ := lastSwitchOnDisk(t); !at.IsZero() {
		t.Fatal("a refused switch stamped the anti-flap cooldown")
	}
}

// The repair, not just the refusal. With an oracle that can name the login, the
// rotated pair goes back into its account's stored snapshot and the engine has
// a baseline again — which is what stops the stand-down from being permanent.
func TestATickAdoptsTheLoginClaudeCodeRotated(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAsRotated(t, "u-1")

	srv, fetch := usageByToken("u-1")
	defer srv.Close()
	e := engineFor(t, tokensAreFine, fetch)
	e.ResolveOwner = func(_ context.Context, accessToken string) (string, error) {
		if accessToken != "AT-rotated-u-1" {
			return "", fmt.Errorf("the oracle was handed %q, not the live access token", accessToken)
		}
		return "u-1", nil
	}

	before := readLiveFile(t)
	tick(t, e)
	tick(t, e)

	if !bytes.Equal(before, readLiveFile(t)) {
		t.Fatalf("adoption rewrote the credentials file: %s", readLiveFile(t))
	}
	if got := liveUUID(t); got != "u-1" {
		t.Fatalf("attribution after adoption = %q, want u-1 — the store did not take the rotated pair", got)
	}
}

// An account the oracle names as somebody else's is the machine ccdad exists to
// move off. Standing down on it would make one manual /login stop the engine
// for good.
func TestATickSwitchesOffALoginEstablishedAsForeign(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	liveAsRotated(t, "someone-else")

	srv, fetch := usageByToken("u-1")
	defer srv.Close()
	e := engineFor(t, tokensAreFine, fetch)
	e.ResolveOwner = func(context.Context, string) (string, error) {
		return "an-account-this-store-does-not-hold", nil
	}

	tick(t, e)
	tick(t, e)

	if got := liveUUID(t); got != "u-1" {
		t.Fatalf("live = %q, want u-1 — the engine did not move off an unmanaged login", got)
	}
}

// Offline is not evidence that the login belongs to somebody else. A resolver
// that cannot answer must leave the file alone, because "cannot reach the
// endpoint" and "a managed account just rotated" look identical from here.
func TestATickStandsDownWhenTheOracleCannotAnswer(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAsRotated(t, "u-1")

	srv, fetch := usageByToken("u-1")
	defer srv.Close()
	e := engineFor(t, tokensAreFine, fetch)
	e.ResolveOwner = func(context.Context, string) (string, error) {
		return "", errors.New("the profile endpoint could not be reached")
	}

	before := readLiveFile(t)
	tick(t, e)
	tick(t, e)

	if !bytes.Equal(before, readLiveFile(t)) {
		t.Fatalf("an unresolved login was overwritten anyway: %s", readLiveFile(t))
	}
}

// The oracle may only write the account it NAMED. Trusting anything else —
// whoever ccdad last switched to, say — is how a user's own /login gets stored
// over a managed account's only refresh token.
func TestAdoptionWritesOnlyTheAccountTheOracleNamed(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAsRotated(t, "u-2")

	srv, fetch := usageByToken("u-1")
	defer srv.Close()
	e := engineFor(t, tokensAreFine, fetch)
	e.ResolveOwner = func(context.Context, string) (string, error) { return "u-2", nil }

	tick(t, e)
	tick(t, e)

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	kept, err := s.Credentials("u-1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(kept["claudeAiOauth"], []byte("RT-u-1")) ||
		bytes.Contains(kept["claudeAiOauth"], []byte("rotated")) {
		t.Fatalf("adoption wrote u-2's rotated login into u-1's snapshot: %s", kept["claudeAiOauth"])
	}
}

// store.Add replaces the credential file wholesale, so adoption has to overlay
// rather than write in place. `ccdad add` carries the same warning: without it,
// taking a refreshed token costs the account every other key it had stored.
func TestAdoptionKeepsWhatTheAccountAlreadyHeld(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	held, err := s.Credentials("u-1")
	if err != nil {
		t.Fatal(err)
	}
	held[cclink.TokenKey] = json.RawMessage(`{"kind":"api-key","token":"sk-ant-api-KEEP"}`)
	acct, _ := s.Get("u-1")
	if err := s.Add(acct, held); err != nil {
		t.Fatal(err)
	}

	liveAsRotated(t, "u-1")
	srv, fetch := usageByToken("u-1")
	defer srv.Close()
	e := engineFor(t, tokensAreFine, fetch)
	e.ResolveOwner = func(context.Context, string) (string, error) { return "u-1", nil }

	tick(t, e)
	tick(t, e)

	after, err := mustStore(t).Credentials("u-1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(after["claudeAiOauth"], []byte("RT-u-1-rotated")) {
		t.Fatalf("the rotated pair was not adopted: %s", after["claudeAiOauth"])
	}
	if !bytes.Contains(after[cclink.TokenKey], []byte("sk-ant-api-KEEP")) {
		t.Fatalf("adoption dropped the account's stored api-key record: %v", after)
	}
}

func mustStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// THE ORACLE IS A NETWORK CALL ON A 1 Hz PATH. act() reaches resolveLive on
// every tick that wants to switch while the live login cannot be named, and
// only the LOG line was latched -- so the request behind it went out once a
// second, carrying the live login's own bearer, for as long as the state
// lasted. After a rotation that lasts until a human intervenes.
func TestTheIdentityOracleIsNotAskedEverySecond(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAsRotated(t, "u-2")

	srv, fetch := usageByToken("u-1")
	defer srv.Close()
	asked := 0
	e := engineFor(t, tokensAreFine, fetch)
	e.ResolveOwner = func(context.Context, string) (string, error) {
		asked++
		return "", errors.New("the profile endpoint is having a bad day")
	}

	for i := 0; i < 10; i++ {
		tick(t, e)
	}
	if asked > 1 {
		t.Fatalf("the oracle was asked %d times across ten ticks in the same second, want 1", asked)
	}
}

// Bounded is not disabled: once the interval has passed the oracle is asked
// again, or a rotation ccdad could have adopted stays unadopted forever.
func TestTheIdentityOracleIsAskedAgainAfterTheInterval(t *testing.T) {
	isolateEngine(t)
	seedAccount(t, "u-1", "org-1")
	seedAccount(t, "u-2", "org-2")
	liveAsRotated(t, "u-2")

	srv, fetch := usageByToken("u-1")
	defer srv.Close()
	asked := 0
	now := tickEpoch
	e := engineFor(t, tokensAreFine, fetch)
	e.Now = func() time.Time { return now }
	e.ResolveOwner = func(context.Context, string) (string, error) {
		asked++
		return "", errors.New("still a bad day")
	}

	tick(t, e)
	tick(t, e)
	if asked != 1 {
		t.Fatalf("the oracle was asked %d times before the interval, want 1", asked)
	}
	now = now.Add(resolveMinInterval + time.Second)
	tick(t, e)
	if asked != 2 {
		t.Fatalf("the oracle was asked %d times after the interval passed, want 2", asked)
	}
}
