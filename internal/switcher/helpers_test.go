package switcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

// isolate points ccdad's store and every Claude Code path this package can
// reach at temp directories.
//
// It asserts its own sandboxing rather than trusting it: this package WRITES
// Claude Code's credentials file, so an unsandboxed run logs the developer out.
//
// CLAUDE_SECURESTORAGE_CONFIG_DIR must be set and non-empty —
// ccpath.CredentialHome() prefers it over CLAUDE_CONFIG_DIR whenever it is
// DEFINED, and defined-but-empty falls back to the real ~/.claude.
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	claude := filepath.Join(home, ".claude")
	if err := os.MkdirAll(claude, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCDAD_HOME", filepath.Join(t.TempDir(), "ccdad"))
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", claude)
	for _, v := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		t.Setenv(v, "")
	}
	if got := mustPath(ccpath.CredentialHome()); got != claude {
		t.Fatalf("isolate: ccpath.CredentialHome() = %q, want %q — refusing to run unsandboxed", got, claude)
	}
}

func credsPath(t *testing.T) string {
	t.Helper()
	return mustPath(ccpath.CredentialsPath())
}

// oauthBlob is a stored snapshot anchored on a refresh token, which is what
// attribution compares.
func oauthBlob(refresh string) cclink.Blob {
	return cclink.Blob{"claudeAiOauth": json.RawMessage(
		`{"accessToken":"AT-` + refresh + `","refreshToken":"` + refresh + `"}`)}
}

// seed adds an account holding an ordinary browser login.
func seed(t *testing.T, uuid, email string) store.Account {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	a := store.Account{UUID: uuid, Email: email}
	if err := s.Add(a, oauthBlob("RT-"+uuid)); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(uuid)
	if !ok {
		t.Fatalf("seed: %s did not land in the store", uuid)
	}
	return got
}

// seedToken adds an account whose credential Claude Code reads from somewhere
// other than the credentials file.
func seedToken(t *testing.T, uuid, email, kind, token string) store.Account {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cclink.TokenRecord{Kind: kind, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	a := store.Account{UUID: uuid, Email: email}
	if err := s.Add(a, cclink.Blob{cclink.TokenKey: encoded}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(uuid)
	if !ok {
		t.Fatalf("seedToken: %s did not land in the store", uuid)
	}
	return got
}

// writeLive replaces Claude Code's credentials file wholesale.
func writeLive(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(credsPath(t), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// liveAs writes a credentials file that attributes to the account seeded under
// uuid.
func liveAs(t *testing.T, uuid string) {
	t.Helper()
	writeLive(t, `{"claudeAiOauth":{"accessToken":"AT-RT-`+uuid+`","refreshToken":"RT-`+uuid+`"}}`)
}

func readLive(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(credsPath(t))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// lastSwitch reports the cooldown stamp on disk.
func lastSwitch(t *testing.T) (time.Time, string) {
	t.Helper()
	st, err := strategy.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	return st.LastSwitch()
}
