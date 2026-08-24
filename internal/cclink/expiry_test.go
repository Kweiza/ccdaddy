package cclink

import (
	"encoding/json"
	"testing"
	"time"
)

func oauthBlob(t *testing.T, expiresAt any) Blob {
	t.Helper()
	payload := map[string]any{"accessToken": "sk-ant-oat01-a", "refreshToken": "sk-ant-ort01-a"}
	if expiresAt != nil {
		payload["expiresAt"] = expiresAt
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return Blob{"claudeAiOauth": json.RawMessage(raw)}
}

func TestWouldSelfRefreshAtClaudeCodesOwnThreshold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	cases := []struct {
		name  string
		until time.Duration
		want  bool
	}{
		// Yfe is `Date.now() + 300000 >= expiresAt`, so the boundary itself
		// refreshes and one millisecond further out does not.
		{"long life", time.Hour, false},
		{"just outside the threshold", SelfRefreshThreshold + time.Millisecond, false},
		{"exactly at the threshold", SelfRefreshThreshold, true},
		{"inside the threshold", SelfRefreshThreshold - time.Second, true},
		{"already expired", -time.Hour, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := oauthBlob(t, now.Add(tc.until).UnixMilli())
			if got := WouldSelfRefresh(b, now); got != tc.want {
				t.Fatalf("WouldSelfRefresh(expires in %s) = %v, want %v", tc.until, got, tc.want)
			}
		})
	}
}

// A credential with no expiry is what a hand-written or very old record looks
// like. Claude Code's Yfe answers false for a null expiresAt, so ccdad must not
// invent an expiry that Claude Code does not read.
func TestWouldSelfRefreshWithoutAnExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		name string
		at   any
	}{
		{"absent", nil},
		{"null", nil},
		{"not a number", "soon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if WouldSelfRefresh(oauthBlob(t, tc.at), now) {
				t.Fatal("a credential with no readable expiry must not read as one Claude Code would refresh")
			}
		})
	}
}

// A blob with no OAuth record is not a login at all. Answering true would make
// the switch refuse an api-key account, which has no expiry to speak of.
func TestWouldSelfRefreshWithoutALogin(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if WouldSelfRefresh(Blob{}, now) {
		t.Fatal("a blob with no claudeAiOauth must not read as one Claude Code would refresh")
	}
	if WouldSelfRefresh(Blob{"claudeAiOauth": json.RawMessage(`"not an object"`)}, now) {
		t.Fatal("an unparseable OAuth record must not read as one Claude Code would refresh")
	}
}
