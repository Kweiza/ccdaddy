package switcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/identity"
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
	// Claude Code's auth environment, cleared for the reason internal/cli's
	// isolate states: this package resolves BOTH axes now, so a developer who
	// exports any of these gets answers out of this suite that CI never sees.
	// The last four put the Anthropic CLI's profile walk inside the sandboxed
	// HOME rather than in the developer's real ~/.config.
	for _, v := range identity.AuthEnvironmentVars() {
		t.Setenv(v, "")
	}

	// The two paths no t.Setenv can reach: Claude Code compiles them in as
	// absolute literals outside the home directory. See internal/cli's isolate
	// for the full reason. The directory is deliberately not created.
	savedHostToken, savedHostKey := identity.HostOAuthTokenFile, identity.HostAPIKeyFile
	t.Cleanup(func() {
		identity.HostOAuthTokenFile, identity.HostAPIKeyFile = savedHostToken, savedHostKey
	})
	hostRemote := filepath.Join(t.TempDir(), "remote")
	identity.HostOAuthTokenFile = filepath.Join(hostRemote, ".oauth_token")
	identity.HostAPIKeyFile = filepath.Join(hostRemote, ".api_key")
	if got := mustPath(ccpath.CredentialHome()); got != claude {
		t.Fatalf("isolate: ccpath.CredentialHome() = %q, want %q — refusing to run unsandboxed", got, claude)
	}
}

func credsPath(t *testing.T) string {
	t.Helper()
	return mustPath(ccpath.CredentialsPath())
}

// liveLogin is the CREDENTIALS FILE holding a login, as distinct from oauthBlob,
// which is a snapshot ccdad stored for an account. Both carry user:inference for
// the same reason: without it Claude Code has no login at all, so a fixture
// without it describes a machine none of these tests mean. Every test that
// needs a live login goes through this rather than spelling the JSON, so the
// next one cannot be written scope-less.
func liveLogin(refresh string) cclink.Blob {
	return cclink.Blob{"claudeAiOauth": json.RawMessage(
		`{"accessToken":"AT","refreshToken":"` + refresh + `","scopes":["user:inference","user:profile"]}`)}
}

// oauthBlob is a stored snapshot anchored on a refresh token, which is what
// attribution compares.
//
// IT CARRIES user:inference, and that is not decoration. Claude Code takes a
// login as a credential only when its scopes contain that one -- a Console
// sign-in leaves a well-formed record with an access token and without it, and
// no session ever authenticates with that record. A fixture without the scope
// describes a machine Claude Code would treat as having no login at all, which
// is not the machine any of these tests mean.
func oauthBlob(refresh string) cclink.Blob {
	return cclink.Blob{"claudeAiOauth": json.RawMessage(
		`{"accessToken":"AT-` + refresh + `","refreshToken":"` + refresh +
			`","scopes":["user:inference","user:profile"]}`)}
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

func globalConfigPath(t *testing.T) string {
	t.Helper()
	return mustPath(ccpath.GlobalConfigPath())
}

// writeGlobalConfig replaces ~/.claude.json wholesale, the sandboxed test
// stand-in for it.
func writeGlobalConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(globalConfigPath(t), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readOAuthAccount reads back the oauthAccount object ~/.claude.json holds
// after a switch, decoded to a plain map so a test can assert on individual
// fields without caring about key order.
func readOAuthAccount(t *testing.T) map[string]any {
	t.Helper()
	g, err := cclink.LoadGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := cclink.OAuthAccountSnapshot(g)
	if !ok {
		t.Fatal("readOAuthAccount: ~/.claude.json has no oauthAccount")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
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

// oauthEnv is the OAuth axis a test starts from: the machine's, sandboxed by
// isolate, with the apiKeyHelper left where Execute leaves it.
//
// It is a function rather than a value because the probe reads the environment
// EACH TIME, and a package-level value would freeze whatever the first test to
// run happened to see.
func oauthEnv() identity.OAuthEnvironment { return identity.ProbeOAuthEnvironment() }

// writeHostToken puts a token at the sandboxed stand-in for the path Claude
// Code compiles in. isolate has already repointed the package var.
func writeHostToken(t *testing.T, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(identity.HostOAuthTokenFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity.HostOAuthTokenFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seedExpiring adds an account whose stored login sits inside Claude Code's own
// refresh threshold, which is what a credential looks like after it has sat in
// the store unpolled for its whole eight-hour life.
func seedExpiring(t *testing.T, uuid, email string, until time.Duration) store.Account {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	a := store.Account{UUID: uuid, Email: email}
	if err := s.Add(a, expiringBlob("RT-"+uuid, until)); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get(uuid)
	if !ok {
		t.Fatalf("seedExpiring: %s did not land in the store", uuid)
	}
	return got
}

// expiringBlob is oauthBlob with an expiry on it, relative to fixedNow.
func expiringBlob(refresh string, until time.Duration) cclink.Blob {
	return cclink.Blob{"claudeAiOauth": json.RawMessage(
		`{"accessToken":"AT-` + refresh + `","refreshToken":"` + refresh +
			`","scopes":["user:inference","user:profile"],"expiresAt":` +
			strconv.FormatInt(fixedNow.Add(until).UnixMilli(), 10) + `}`)}
}

// fixedNow is the instant the staleness tests reckon from, so a fixture's
// expiry is a fact about the test rather than about when it ran.
var fixedNow = time.Unix(1_700_000_000, 0)

func at(now time.Time) func() time.Time { return func() time.Time { return now } }
