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
	"io"
	"strings"
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

// writeGlobalConfig puts an oauthAccount naming uuid into ~/.claude.json, which
// is the only thing left that can name a login Claude Code has BLANKED.
func writeGlobalConfig(t *testing.T, uuid string) {
	t.Helper()
	path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json")
	body := `{"oauthAccount":{"accountUuid":"` + uuid + `","emailAddress":"x@example.com"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Claude Code does not delete a rejected credential, it BLANKS it in place:
// {...d,refreshToken:"",accessToken:"",expiresAt:0}. The window is the POST
// itself -- refreshLive reads and decides under the lock, RELEASES it to reach
// the network, and re-takes it to write. A Claude Code whose own refresh is
// rejected in that gap leaves the record blanked, recordOf refuses it, and the
// CAS read it as somebody else's file and stood down -- leaving the user logged
// out while ccdad held the very pair that repairs them.
func TestADeadTokenClearIsWrittenBackOverWhenTheConfigStillNamesUs(t *testing.T) {
	isolate(t)
	now := time.Now()
	// Inside the band ccdad rotates in: above Claude Code's five-minute floor,
	// at or below LiveRefreshLead.
	rec := oauthRecord("a1", "r1", now.Add(20*time.Minute), "")
	seed(t, "u-1", rec)
	writeLive(t, rec)
	writeGlobalConfig(t, "u-1")

	src := sourceFor(t, func(w http.ResponseWriter, _ *http.Request) {
		// The blanking happens WHILE the POST is in flight, which is the only
		// moment the CAS can meet it.
		writeLive(t, json.RawMessage(`{"accessToken":"","refreshToken":"","expiresAt":0}`))
		_, _ = io.WriteString(w, tokenResponse("a2", "r2", 28800))
	})
	src.Now = func() time.Time { return now }

	if _, err := src.AccessToken(context.Background(), "u-1"); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	live, err := cclink.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live["claudeAiOauth"]), `"a2"`) {
		t.Fatalf("the blanked live record was left blanked, so the user stays logged out: %s",
			live["claudeAiOauth"])
	}
}

// The repair is identity-GUARDED. A blanked record the config does not name is
// somebody else's logout, and writing this account's pair over it would attach
// one account's grant to another's session.
func TestADeadTokenClearIsLeftAloneWhenTheConfigNamesSomebodyElse(t *testing.T) {
	isolate(t)
	now := time.Now()
	rec := oauthRecord("a1", "r1", now.Add(20*time.Minute), "")
	seed(t, "u-1", rec)
	writeLive(t, rec)
	writeGlobalConfig(t, "somebody-else")

	src := sourceFor(t, func(w http.ResponseWriter, _ *http.Request) {
		writeLive(t, json.RawMessage(`{"accessToken":"","refreshToken":"","expiresAt":0}`))
		_, _ = io.WriteString(w, tokenResponse("a2", "r2", 28800))
	})
	src.Now = func() time.Time { return now }

	if _, err := src.AccessToken(context.Background(), "u-1"); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	live, err := cclink.Load()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(live["claudeAiOauth"]), `"a2"`) {
		t.Fatalf("this account's pair was written over somebody else's blanked login: %s",
			live["claudeAiOauth"])
	}
	// The store still records the mint, because the grant was spent either way.
	if r := storedRecord(t, "u-1"); r.RefreshToken != "r2" {
		t.Fatalf("the minted pair was not recorded anywhere: %q", r.RefreshToken)
	}
}
