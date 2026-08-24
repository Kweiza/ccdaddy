package tokens

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
)

func readLiveRecord(t *testing.T) record {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var blob cclink.Blob
	if err := json.Unmarshal(b, &blob); err != nil {
		t.Fatal(err)
	}
	rec, ok := recordOf(blob)
	if !ok {
		t.Fatalf("the live credentials file is not an OAuth record: %s", b)
	}
	return rec
}

// The preventive half. Inside the band Claude Code has no reason to refresh --
// its own gate answers "not_needed" -- so ccdad performs the rotation once and
// writes it to both copies. Nothing is racing, and the two never diverge, which
// is what stops attribution from ever losing the account in the first place.
func TestTheLiveLoginIsRotatedAheadOfClaudeCode(t *testing.T) {
	isolate(t)
	rec := oauthRecord("OLD-AT", "OLD-RT", time.Now().Add(20*time.Minute), "")
	seed(t, "u-1", rec)
	writeLive(t, rec)

	src := sourceFor(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err == nil && r.PostFormValue("refresh_token") != "" {
			t.Errorf("the grant was posted as a form; Claude Code's endpoint takes JSON")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokenResponse("NEW-AT", "NEW-RT", 28800)))
	})
	got, err := src.AccessToken(context.Background(), "u-1")
	if err != nil {
		t.Fatalf("AccessToken() = %v, want nil", err)
	}
	if got != "NEW-AT" {
		t.Errorf("AccessToken() = %q, want the pair it just minted", got)
	}
	if live := readLiveRecord(t); live.AccessToken != "NEW-AT" || live.RefreshToken != "NEW-RT" {
		t.Errorf("the live file kept the old pair: %+v", live)
	}
	if stored := storedRecord(t, "u-1"); stored.AccessToken != "NEW-AT" || stored.RefreshToken != "NEW-RT" {
		t.Errorf("the stored snapshot kept the old pair: %+v", stored)
	}
}

// The band's two edges, both of which are somebody else's territory.
//
// Above it there is nothing to get ahead of. Below it the login is inside
// Claude Code's own refresh window, Claude Code is about to spend the grant
// under its own locks, and a second spender there is the double-refresh that
// revokes one of the two.
func TestOutsideTheBandTheLiveLoginIsLeftAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		left  time.Duration
		stale bool
	}{
		{"above the lead", LiveRefreshLead + time.Minute, false},
		{"inside Claude Code's own window", 4 * time.Minute, true},
		{"already expired", -time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			rec := oauthRecord("AT", "RT", time.Now().Add(tc.left), "")
			seed(t, "u-1", rec)
			writeLive(t, rec)

			src := sourceFor(t, refusingEndpoint(t))
			got, err := src.AccessToken(context.Background(), "u-1")
			switch {
			case tc.stale:
				if !errors.Is(err, ErrLiveTokenStale) {
					t.Fatalf("AccessToken() = (%q, %v), want ErrLiveTokenStale", got, err)
				}
			default:
				if err != nil || got != "AT" {
					t.Fatalf("AccessToken() = (%q, %v), want the stored token and no error", got, err)
				}
			}
			if live := readLiveRecord(t); live.RefreshToken != "RT" {
				t.Errorf("the live file was rewritten outside the band: %+v", live)
			}
		})
	}
}

// A rotation that cannot be written to the live file is still a rotation the
// server performed: the grant is spent and the new pair is the only usable one
// left, so it goes to the store whatever the file does. Losing it there would
// leave a pair nothing can ever present again.
func TestAMintedPairIsStoredEvenWhenTheLiveFileMoved(t *testing.T) {
	isolate(t)
	rec := oauthRecord("OLD-AT", "OLD-RT", time.Now().Add(20*time.Minute), "")
	seed(t, "u-1", rec)
	writeLive(t, rec)

	// The live file moved between the read that decided and the write that
	// lands: it now holds a grant this call did not spend, so it cannot be
	// shown to be the same account's any more.
	var decided bool
	saved := activateWith
	t.Cleanup(func() { activateWith = saved })
	activateWith = func(decide func(cclink.Blob) (cclink.Blob, error)) error {
		decided = true
		_, err := decide(cclink.Blob{"claudeAiOauth": oauthRecord("SOMEONE-AT", "SOMEONE-RT", time.Now().Add(time.Hour), "")})
		return err
	}

	src := sourceFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokenResponse("NEW-AT", "NEW-RT", 28800)))
	})
	if _, err := src.AccessToken(context.Background(), "u-1"); err != nil {
		t.Fatalf("AccessToken() = %v, want nil", err)
	}
	if !decided {
		t.Fatal("the live write was never attempted")
	}
	if stored := storedRecord(t, "u-1"); stored.RefreshToken != "NEW-RT" {
		t.Errorf("the minted pair was dropped: %+v", stored)
	}
}

// A rotation that fails is not the caller's problem. The access token in hand
// is still valid -- being above the floor is exactly what that means -- so the
// poll it was fetched for proceeds, and Claude Code refreshes at its own
// threshold as it always did.
func TestAFailedLeadRefreshStillAnswersWithAUsableToken(t *testing.T) {
	isolate(t)
	rec := oauthRecord("AT", "RT", time.Now().Add(20*time.Minute), "")
	seed(t, "u-1", rec)
	writeLive(t, rec)

	src := sourceFor(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	got, err := src.AccessToken(context.Background(), "u-1")
	if err != nil {
		t.Fatalf("AccessToken() = %v, want nil — the token in hand is still good", err)
	}
	if got != "AT" {
		t.Errorf("AccessToken() = %q, want the token that is still valid", got)
	}
	if live := readLiveRecord(t); live.RefreshToken != "RT" {
		t.Errorf("a failed rotation still rewrote the live file: %+v", live)
	}
}
