package tokens

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/oauth"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// isolate points ccdad's store and every Claude Code path at temp directories.
// It is not optional: this package writes credential snapshots and reads the
// live credentials file, so without it a test edits the developer's real login.
func isolate(t *testing.T) {
	t.Helper()
	claude := t.TempDir()
	t.Setenv("CCDAD_HOME", filepath.Join(t.TempDir(), "ccdad"))
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", claude)
	// The store lock's default wait is five seconds. Every test here that
	// contends for it is asserting a refusal, not waiting for a real holder.
	saved := store.LockTimeout
	store.LockTimeout = 300 * time.Millisecond
	t.Cleanup(func() { store.LockTimeout = saved })
}

// oauthRecord renders a stored claudeAiOauth object. expiresAt is in
// MILLISECONDS, which is what the credential writer produces and what every
// comparison here is against.
func oauthRecord(access, refresh string, expiresAt time.Time, extra string) json.RawMessage {
	ms := ""
	if !expiresAt.IsZero() {
		ms = fmt.Sprintf(`,"expiresAt":%d`, expiresAt.UnixMilli())
	}
	return json.RawMessage(fmt.Sprintf(`{"accessToken":%q,"refreshToken":%q%s%s}`, access, refresh, ms, extra))
}

func seed(t *testing.T, uuid string, rec json.RawMessage) {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{UUID: uuid, Email: uuid + "@example.com"},
		cclink.Blob{"claudeAiOauth": rec}); err != nil {
		t.Fatal(err)
	}
}

func writeLive(t *testing.T, rec json.RawMessage) {
	t.Helper()
	body, err := json.Marshal(map[string]json.RawMessage{"claudeAiOauth": rec})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func storedRecord(t *testing.T, uuid string) record {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	creds, err := s.Credentials(uuid)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := recordOf(creds)
	if !ok {
		t.Fatalf("the stored snapshot for %s is not an OAuth record: %v", uuid, creds)
	}
	return rec
}

// sourceFor builds a Source pointed at a local token endpoint.
func sourceFor(t *testing.T, h http.HandlerFunc) *Source {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Source{
		Client:      &oauth.Client{HTTP: srv.Client(), TokenEndpoint: srv.URL},
		Now:         time.Now,
		Skew:        DefaultSkew,
		LockTimeout: 300 * time.Millisecond,
	}
}

// refusingEndpoint fails the test if it is reached at all.
func refusingEndpoint(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(http.ResponseWriter, *http.Request) {
		t.Error("the token endpoint was called; this path must not spend a request")
	}
}

func tokenResponse(access, refresh string, expiresIn int64) string {
	return fmt.Sprintf(`{"access_token":%q,"refresh_token":%q,"expires_in":%d,"scope":"user:inference user:profile"}`,
		access, refresh, expiresIn)
}

// A stored token that is still good is served without a request. The stored
// expiry is in milliseconds; reading it as seconds puts it either fifty years
// in the past — a refresh on every tick — or fifty thousand years in the
// future, and this pins the direction that costs requests.
func TestAFreshTokenSpendsNoRequest(t *testing.T) {
	isolate(t)
	seed(t, "u-1", oauthRecord("AT", "RT", time.Now().Add(8*time.Hour), ""))

	src := sourceFor(t, refusingEndpoint(t))
	got, err := src.AccessToken(context.Background(), "u-1")
	if err != nil {
		t.Fatalf("AccessToken() = %v, want nil", err)
	}
	if got != "AT" {
		t.Errorf("AccessToken() = %q, want the stored one", got)
	}
}

// The refreshed expiry is written back in MILLISECONDS from an expires_in given
// in SECONDS. If the units were confused the second call would refresh again,
// which is the failure that burns the endpoint.
func TestRefreshWritesMillisecondsFromSeconds(t *testing.T) {
	isolate(t)
	seed(t, "u-1", oauthRecord("OLD-AT", "OLD-RT", time.Now().Add(-time.Hour),
		`,"clientId":"CID","rateLimitTier":"default_claude_max_20x","refreshTokenExpiresAt":123`))

	calls := 0
	src := sourceFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "OLD-RT" {
			t.Errorf("request = %v, want a refresh of the stored token", body)
		}
		fmt.Fprint(w, tokenResponse("NEW-AT", "NEW-RT", 28800))
	})

	got, err := src.AccessToken(context.Background(), "u-1")
	if err != nil {
		t.Fatalf("AccessToken() = %v, want nil", err)
	}
	if got != "NEW-AT" {
		t.Fatalf("AccessToken() = %q, want the refreshed one", got)
	}

	rec := storedRecord(t, "u-1")
	if rec.RefreshToken != "NEW-RT" {
		t.Errorf("stored refresh token = %q, want the rotated one", rec.RefreshToken)
	}
	want := time.Now().Add(8 * time.Hour)
	if d := rec.ExpiresAt.Sub(want); d > time.Minute || d < -time.Minute {
		t.Errorf("stored expiry = %s, want about %s — seconds were written where milliseconds belong",
			rec.ExpiresAt, want)
	}
	// §4.2 rule 3: fields the response did not speak to survive.
	if rec.ClientID != "CID" {
		t.Errorf("clientId was dropped; a revocation request needs it")
	}
	if !hasKey(t, "u-1", "rateLimitTier") {
		t.Error("rateLimitTier was dropped from the stored record")
	}

	// The second call must be free. It is not if the expiry was written in the
	// wrong unit.
	if _, err := src.AccessToken(context.Background(), "u-1"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("the endpoint was called %d times, want 1 — the written expiry is not being read back", calls)
	}
}

func hasKey(t *testing.T, uuid, key string) bool {
	t.Helper()
	rec := storedRecord(t, uuid)
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec.raw, &payload); err != nil {
		t.Fatal(err)
	}
	_, ok := payload[key]
	return ok
}

// An INACTIVE account is nothing to do with ~/.claude, so no Claude Code lock is
// taken for it. Taking one would stall Claude Code over a file that is not
// involved.
func TestInactiveAccountTakesNoClaudeCodeLock(t *testing.T) {
	isolate(t)
	seed(t, "u-1", oauthRecord("OLD-AT", "OLD-RT", time.Now().Add(-time.Hour), ""))
	writeLive(t, oauthRecord("OTHER-AT", "OTHER-RT", time.Now().Add(time.Hour), ""))

	src := sourceFor(t, func(w http.ResponseWriter, _ *http.Request) {
		// The lock must be absent DURING the call, not merely released after.
		if _, err := os.Stat(cclock.OAuthRefreshLockDir()); err == nil {
			t.Error("Claude Code's refresh lock is held while refreshing an inactive account")
		}
		fmt.Fprint(w, tokenResponse("NEW-AT", "NEW-RT", 28800))
	})

	if _, err := src.AccessToken(context.Background(), "u-1"); err != nil {
		t.Fatalf("AccessToken() = %v, want nil", err)
	}
	if _, err := os.Stat(cclock.OAuthRefreshLockDir()); err == nil {
		t.Error("Claude Code's refresh lock was left behind by an inactive refresh")
	}
	// The live login is untouched: this package never writes ~/.claude.
	live, err := cclink.Load()
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := recordOf(live); rec.RefreshToken != "OTHER-RT" {
		t.Errorf("the live credentials file changed: %+v", rec)
	}
}

// The store lock is released across the network call. Held, a thirty-second
// token round trip would stall every CLI command that wants to write.
func TestStoreLockIsReleasedAcrossTheNetworkCall(t *testing.T) {
	isolate(t)
	seed(t, "u-1", oauthRecord("OLD-AT", "OLD-RT", time.Now().Add(-time.Hour), ""))

	src := sourceFor(t, func(w http.ResponseWriter, _ *http.Request) {
		if err := store.WithStore(func(*store.Store) error { return nil }); err != nil {
			t.Errorf("the store lock is held across the token request: %v", err)
		}
		fmt.Fprint(w, tokenResponse("NEW-AT", "NEW-RT", 28800))
	})

	if _, err := src.AccessToken(context.Background(), "u-1"); err != nil {
		t.Fatalf("AccessToken() = %v, want nil", err)
	}
}

// Rotating a refresh token revokes the old one. Refreshing the account Claude
// Code is logged in as, while never writing the live credentials file, would
// leave Claude Code holding a revoked token — so the live login is adopted, not
// refreshed.
func TestLiveLoginIsAdoptedNotRefreshed(t *testing.T) {
	isolate(t)
	stored := oauthRecord("OLD-AT", "SHARED-RT", time.Now().Add(-time.Hour), "")
	seed(t, "u-1", stored)
	// Claude Code refreshed it itself: same refresh token identity, newer pair.
	writeLive(t, oauthRecord("LIVE-AT", "SHARED-RT", time.Now().Add(7*time.Hour), ""))

	src := sourceFor(t, refusingEndpoint(t))
	got, err := src.AccessToken(context.Background(), "u-1")
	if err != nil {
		t.Fatalf("AccessToken() = %v, want nil", err)
	}
	if got != "LIVE-AT" {
		t.Errorf("AccessToken() = %q, want what Claude Code has", got)
	}
	if rec := storedRecord(t, "u-1"); rec.AccessToken != "LIVE-AT" {
		t.Errorf("the stored snapshot was not brought up to date: %q", rec.AccessToken)
	}
}

// Reading the live login takes Claude Code's refresh lock, so the read cannot
// land between Claude Code's write of a new access token and its write of the
// matching refresh token.
func TestLiveLoginTakesClaudeCodesRefreshLock(t *testing.T) {
	isolate(t)
	rec := oauthRecord("AT", "SHARED-RT", time.Now().Add(7*time.Hour), "")
	seed(t, "u-1", rec)
	writeLive(t, rec)

	held, err := cclock.Acquire(cclock.OAuthRefreshLockDir(), cclock.Options{
		Stale:   cclock.RefreshStale,
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	src := sourceFor(t, refusingEndpoint(t))
	_, err = src.AccessToken(context.Background(), "u-1")
	if !errors.Is(err, cclock.ErrTimeout) {
		t.Fatalf("AccessToken() while Claude Code holds its refresh lock = %v, want a lock timeout", err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}

	// And it gives the lock back: a second call succeeds.
	if _, err := src.AccessToken(context.Background(), "u-1"); err != nil {
		t.Fatalf("AccessToken() after the lock was freed = %v, want nil", err)
	}
	if _, err := os.Stat(cclock.OAuthRefreshLockDir()); err == nil {
		t.Error("the refresh lock was left held")
	}
}

// An expired live login is Claude Code's to rotate. Eight hours of expiry means
// Claude Code has not run in eight hours, so no session's rotation is urgent.
func TestExpiredLiveLoginIsNotRefreshed(t *testing.T) {
	isolate(t)
	rec := oauthRecord("AT", "SHARED-RT", time.Now().Add(-time.Hour), "")
	seed(t, "u-1", rec)
	writeLive(t, rec)

	src := sourceFor(t, refusingEndpoint(t))
	_, err := src.AccessToken(context.Background(), "u-1")
	if !errors.Is(err, ErrLiveTokenStale) {
		t.Fatalf("AccessToken() for an expired live login = %v, want ErrLiveTokenStale", err)
	}
}

// The three outcomes §7.2 branches on. Only a rejected credential says anything
// about the ACCOUNT; a plane and a 503 must not quarantine the fleet.
func TestFailureOutcomesAreDistinct(t *testing.T) {
	isolate(t)

	t.Run("rejected", func(t *testing.T) {
		isolate(t)
		seed(t, "u-1", oauthRecord("AT", "RT", time.Now().Add(-time.Hour), ""))
		src := sourceFor(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
		})
		_, err := src.AccessToken(context.Background(), "u-1")
		if !errors.Is(err, ErrRejected) {
			t.Fatalf("a 401 = %v, want ErrRejected", err)
		}
		if errors.Is(err, ErrUnavailable) {
			t.Error("a rejected credential also reads as unavailable")
		}
	})

	t.Run("status", func(t *testing.T) {
		isolate(t)
		seed(t, "u-1", oauthRecord("AT", "RT", time.Now().Add(-time.Hour), ""))
		src := sourceFor(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		_, err := src.AccessToken(context.Background(), "u-1")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("a 503 = %v, want ErrUnavailable", err)
		}
		if errors.Is(err, ErrRejected) {
			t.Fatal("a 503 quarantines the account; one bad minute at Anthropic would cost every account a manual re-login")
		}
	})

	t.Run("transport", func(t *testing.T) {
		isolate(t)
		seed(t, "u-1", oauthRecord("AT", "RT", time.Now().Add(-time.Hour), ""))
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		client := srv.Client()
		url := srv.URL
		srv.Close()
		src := &Source{Client: &oauth.Client{HTTP: client, TokenEndpoint: url}, Now: time.Now, Skew: DefaultSkew}

		_, err := src.AccessToken(context.Background(), "u-1")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("an unreachable endpoint = %v, want ErrUnavailable", err)
		}
		if errors.Is(err, ErrRejected) {
			t.Fatal("a plane quarantines the account")
		}
	})
}

// A ccdad token account has no refresh grant behind it: Claude Code reads those
// from an environment variable or ~/.claude.json.
func TestTokenAccountHasNothingToRefresh(t *testing.T) {
	isolate(t)
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{UUID: "u-tok", Email: "t@example.com"},
		cclink.Blob{"ccdadToken": json.RawMessage(`{"kind":"api-key","token":"sk-ant-api-X"}`)}); err != nil {
		t.Fatal(err)
	}

	src := sourceFor(t, refusingEndpoint(t))
	_, err = src.AccessToken(context.Background(), "u-tok")
	if !errors.Is(err, ErrNoOAuthCredential) {
		t.Fatalf("AccessToken() for a token account = %v, want ErrNoOAuthCredential", err)
	}
}

// Refresh is not idempotent. If something else rotated this account while the
// request was in flight, one of the two tokens is already revoked and the
// stored one wins — whoever wrote it is already using it.
func TestARotationUnderUsIsNotOverwritten(t *testing.T) {
	isolate(t)
	seed(t, "u-1", oauthRecord("OLD-AT", "OLD-RT", time.Now().Add(-time.Hour), ""))

	src := sourceFor(t, func(w http.ResponseWriter, _ *http.Request) {
		// Another process finishing its own refresh mid-flight.
		err := store.WithStore(func(st *store.Store) error {
			acct, _ := st.Get("u-1")
			return st.Add(acct, cclink.Blob{
				"claudeAiOauth": oauthRecord("THEIR-AT", "THEIR-RT", time.Now().Add(8*time.Hour), ""),
			})
		})
		if err != nil {
			t.Errorf("staging the concurrent rotation failed: %v", err)
		}
		fmt.Fprint(w, tokenResponse("OUR-AT", "OUR-RT", 28800))
	})

	if _, err := src.AccessToken(context.Background(), "u-1"); err != nil {
		t.Fatalf("AccessToken() = %v, want nil", err)
	}
	if rec := storedRecord(t, "u-1"); rec.RefreshToken != "THEIR-RT" {
		t.Fatalf("stored refresh token = %q, want the one the other writer left", rec.RefreshToken)
	}
}
