package store

import (
	"encoding/json"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/provider"
)

func codexCreds(refresh string) cclink.Blob {
	return cclink.Blob{"codexOAuth": json.RawMessage(
		`{"access_token":"AT","refresh_token":"` + refresh + `","account_id":"acct","user_id":"u"}`)}
}

func seedBothProviders(t *testing.T) *Store {
	t.Helper()
	withStore(t)
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{UUID: "u-claude", Provider: provider.Claude}, sampleCreds("AT")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Account{UUID: "u-codex", Provider: provider.Codex}, codexCreds("RT-1")); err != nil {
		t.Fatal(err)
	}
	return s
}

// The two filters are what every Codex path starts from and what keeps the
// Claude paths from ever seeing a Codex row. They are methods on the store
// rather than a predicate each caller writes, because a caller that wrote its
// own would be one that could forget.
func TestTheProviderFilters(t *testing.T) {
	s := seedBothProviders(t)

	codex := s.CodexAccounts()
	if len(codex) != 1 || codex[0].UUID != "u-codex" {
		t.Fatalf("CodexAccounts() = %+v, want only u-codex", codex)
	}
	claude := s.ClaudeAccounts()
	if len(claude) != 1 || claude[0].UUID != "u-claude" {
		t.Fatalf("ClaudeAccounts() = %+v, want only u-claude", claude)
	}
	if len(s.Accounts()) != 2 {
		t.Fatal("the unfiltered listing lost a row")
	}
}

// SetCredentials replaces one account's credential file without touching the
// row. The Codex refresher needs it: a rotated token is a new credential file
// and nothing else, and going through Add would re-run the whole re-add path.
func TestSetCredentialsReplacesTheFileAndLeavesTheRow(t *testing.T) {
	s := seedBothProviders(t)
	before, ok := s.Get("u-codex")
	if !ok {
		t.Fatal("fixture: u-codex is not in the store")
	}

	if err := s.SetCredentials("u-codex", codexCreds("RT-2")); err != nil {
		t.Fatalf("SetCredentials() = %v, want nil", err)
	}
	got, err := s.Credentials("u-codex")
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(got["codexOAuth"], &rec); err != nil {
		t.Fatal(err)
	}
	if rec.RefreshToken != "RT-2" {
		t.Fatalf("stored refresh token = %q, want RT-2", rec.RefreshToken)
	}
	after, _ := s.Get("u-codex")
	if after != before {
		t.Fatalf("SetCredentials changed the row: %+v -> %+v", before, after)
	}
}

func TestSetCredentialsRefusesAnAccountThatIsNotThere(t *testing.T) {
	s := seedBothProviders(t)
	if err := s.SetCredentials("u-nope", codexCreds("RT-9")); err == nil {
		t.Fatal("SetCredentials() = nil for an account the store does not name")
	}
}

// The mark is written under a compare-and-set against the token that was
// actually rejected.
//
// Without it the sequence that loses a working login is two lines long: the
// refresher gets a terminal refusal for token A, a concurrent `ccdad codex
// add` stores token B, and the refresher then marks the account -- which now
// holds a perfectly good grant -- as needing a relogin.
func TestSetCodexReloginForIsACompareAndSet(t *testing.T) {
	s := seedBothProviders(t)
	stale := storedRefreshTokenHash(t, s, "u-codex")

	// The token moves on, exactly as a concurrent re-login would move it.
	if err := s.SetCredentials("u-codex", codexCreds("RT-2")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCodexReloginFor("u-codex", stale, stale); err != nil {
		t.Fatalf("SetCodexReloginFor() = %v, want nil for a token the store moved past", err)
	}
	got, _ := s.Get("u-codex")
	if got.CodexReloginFor != "" {
		t.Fatalf("CodexReloginFor = %q; the store had moved past the rejected token", got.CodexReloginFor)
	}

	// The same call against the token the store still holds does write.
	current := storedRefreshTokenHash(t, s, "u-codex")
	if err := s.SetCodexReloginFor("u-codex", current, current); err != nil {
		t.Fatalf("SetCodexReloginFor() = %v, want nil", err)
	}
	got, _ = s.Get("u-codex")
	if got.CodexReloginFor != current {
		t.Fatalf("CodexReloginFor = %q, want %q", got.CodexReloginFor, current)
	}
	if got.NeedsRelogin(current) != true {
		t.Error("the account this mark was written for does not report needing a relogin")
	}
}

// The mark survives a reopen, because it lives in accounts.toml and the daemon
// that wrote it may not be the process that reads it.
func TestTheReloginMarkSurvivesAReopen(t *testing.T) {
	s := seedBothProviders(t)
	current := storedRefreshTokenHash(t, s, "u-codex")
	if err := s.SetCodexReloginFor("u-codex", current, current); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := reopened.Get("u-codex")
	if got.CodexReloginFor != current {
		t.Fatalf("CodexReloginFor = %q after a reopen, want %q", got.CodexReloginFor, current)
	}
}

func storedRefreshTokenHash(t *testing.T, s *Store, uuid string) string {
	t.Helper()
	blob, err := s.Credentials(uuid)
	if err != nil {
		t.Fatal(err)
	}
	h, err := codexRefreshTokenHashOf(blob)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
