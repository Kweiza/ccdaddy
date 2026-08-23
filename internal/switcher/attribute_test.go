package switcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// Attribution matches on the refresh token, which survives an access-token
// rotation, so a running Claude Code that has refreshed since the switch is
// still attributed correctly.
func TestAttributeMatchesOnRefreshToken(t *testing.T) {
	accounts := []store.Account{
		{UUID: "u-1", Email: "a@example.com", Idx: 1},
		{UUID: "u-2", Email: "b@example.com", Idx: 2},
	}
	stored := map[string]cclink.Blob{
		"u-1": oauthBlob("RT-ONE"),
		"u-2": oauthBlob("RT-TWO"),
	}
	live := cclink.Blob{"claudeAiOauth": json.RawMessage(
		`{"accessToken":"A-DIFFERENT-ACCESS-TOKEN","refreshToken":"RT-TWO"}`)}

	got, ok := AttributeFile(live, accounts, func(uuid string) (cclink.Blob, error) {
		return stored[uuid], nil
	})
	if !ok {
		t.Fatal("attribute() = false, want a match")
	}
	if got.UUID != "u-2" {
		t.Fatalf("attributed to %q, want u-2", got.UUID)
	}
}

func TestAttributeReportsNoMatch(t *testing.T) {
	accounts := []store.Account{{UUID: "u-1", Idx: 1}}
	stored := map[string]cclink.Blob{"u-1": oauthBlob("RT-ONE")}
	live := cclink.Blob{"claudeAiOauth": json.RawMessage(`{"refreshToken":"SOMETHING-ELSE"}`)}

	if _, ok := AttributeFile(live, accounts, func(uuid string) (cclink.Blob, error) {
		return stored[uuid], nil
	}); ok {
		t.Fatal("attribute() = true, want no match for an unmanaged login")
	}
}

// A token account's stored blob carries no OAuth record, so its identity is ""
// — and so is an empty live file's. Without the empty-identity guard those two
// compare equal, and a machine where Claude Code has never logged in gets
// confidently attributed to a token account it is not using.
func TestAttributeEmptyLiveIsNoMatch(t *testing.T) {
	accounts := []store.Account{{UUID: "apikey-abc", Idx: 1}}
	tokenAccount := cclink.Blob{cclink.TokenKey: json.RawMessage(
		`{"kind":"api-key","token":"sk-ant-api03-x"}`)}

	if got, ok := AttributeFile(cclink.Blob{}, accounts, func(string) (cclink.Blob, error) {
		return tokenAccount, nil
	}); ok {
		t.Fatalf("attribute(empty live) = %q, want no match", got.UUID)
	}
}

// The same guard from the other side: a real login in the live file must not be
// attributed to a token account just because that account cannot identify
// itself out of the credentials file.
func TestAttributeDoesNotMatchATokenAccountFromTheLiveFile(t *testing.T) {
	accounts := []store.Account{{UUID: "apikey-abc", Idx: 1}}
	tokenAccount := cclink.Blob{cclink.TokenKey: json.RawMessage(
		`{"kind":"api-key","token":"sk-ant-api03-x"}`)}
	live := cclink.Blob{"claudeAiOauth": json.RawMessage(`{"refreshToken":"RT-SOMEONE"}`)}

	if got, ok := AttributeFile(live, accounts, func(string) (cclink.Blob, error) {
		return tokenAccount, nil
	}); ok {
		t.Fatalf("attribute() = %q, want no match", got.UUID)
	}
}

// The prefixes are what stop an access token stored by one account from
// colliding with a refresh token stored by another.
func TestAttributeDoesNotCrossMatchTokenKinds(t *testing.T) {
	accounts := []store.Account{{UUID: "u-access", Idx: 1}}
	stored := map[string]cclink.Blob{
		"u-access": {"claudeAiOauth": json.RawMessage(`{"accessToken":"SHARED-VALUE"}`)},
	}
	live := cclink.Blob{"claudeAiOauth": json.RawMessage(`{"refreshToken":"SHARED-VALUE"}`)}

	if got, ok := AttributeFile(live, accounts, func(u string) (cclink.Blob, error) { return stored[u], nil }); ok {
		t.Fatalf("attribute() = %q, want no match: a refresh token is not an access token", got.UUID)
	}
}

// apiKeyCreds builds the stored blob an `add-token` API key produces.
func apiKeyCreds(key string) cclink.Blob {
	return cclink.Blob{cclink.TokenKey: json.RawMessage(
		`{"kind":"api-key","token":"` + key + `"}`)}
}

// A stored primaryApiKey does NOT displace a live OAuth login, and `which` has
// to say so.
//
// This is the single most important rule in the model and the easiest to get
// backwards. Claude Code's client binds `anthropicAuthEnabled: BE()`, and BE()
// is unaffected by primaryApiKey -- only an ENVIRONMENT key, a file descriptor
// or an apiKeyHelper turns it off. So with both present the session is on the
// login, and an implementation that answered "the api-key account" would send
// someone to look at the wrong account's quota.
func TestAttributePrefersTheLoginOverAStoredAPIKey(t *testing.T) {
	accounts := []store.Account{
		{UUID: "u-login", Email: "login@example.com", Idx: 1},
		{UUID: "u-key", Email: "key@example.com", Idx: 2},
	}
	stored := map[string]cclink.Blob{
		"u-login": oauthBlob("RT-ONE"),
		"u-key":   apiKeyCreds("sk-ant-api03-STORED"),
	}
	live := liveLogin("RT-ONE")
	env := identity.APIKeyEnvironment{Interactive: true, ManagedKey: "sk-ant-api03-STORED"}

	res := AttributeLogin(live, accounts, lookupFrom(stored), env, oauthEnv())
	if !res.OK || res.Account.UUID != "u-login" {
		t.Fatalf("attributed to %+v via %q, want the login account", res.Account, res.Via)
	}
}

// With no login left, the stored key IS the credential -- which is exactly the
// state `ccdad switch <api-key account>` creates.
func TestAttributeUsesTheStoredAPIKeyWhenThereIsNoLogin(t *testing.T) {
	accounts := []store.Account{{UUID: "u-key", Email: "key@example.com", Idx: 1}}
	stored := map[string]cclink.Blob{"u-key": apiKeyCreds("sk-ant-api03-STORED")}
	env := identity.APIKeyEnvironment{Interactive: true, ManagedKey: "sk-ant-api03-STORED"}

	res := AttributeLogin(cclink.Blob{}, accounts, lookupFrom(stored), env, oauthEnv())
	if !res.OK || res.Account.UUID != "u-key" {
		t.Fatalf("attributed to %+v via %q, want the api-key account", res.Account, res.Via)
	}
}

// An APPROVED ANTHROPIC_API_KEY displaces the login, and the credentials file
// must not be consulted as a fallback -- naming its account would name one
// Claude Code is not using.
func TestAttributeEnvAPIKeyDisplacesTheLogin(t *testing.T) {
	const key = "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV"
	accounts := []store.Account{
		{UUID: "u-login", Email: "login@example.com", Idx: 1},
		{UUID: "u-key", Email: "key@example.com", Idx: 2},
	}
	stored := map[string]cclink.Blob{
		"u-login": oauthBlob("RT-ONE"),
		"u-key":   apiKeyCreds(key),
	}
	live := liveLogin("RT-ONE")

	env := identity.APIKeyEnvironment{
		Interactive: true,
		EnvKey:      key,
		Approved:    []string{identity.APIKeyApproval(key)},
	}
	res := AttributeLogin(live, accounts, lookupFrom(stored), env, oauthEnv())
	if !res.OK || res.Account.UUID != "u-key" {
		t.Fatalf("attributed to %+v via %q, want the api-key account", res.Account, res.Via)
	}

	// The same key with no approval entry is refused by an interactive Claude
	// Code, so the login is the answer again. This is the pair that proves the
	// approval list is actually consulted: without it, both halves would
	// attribute to the key.
	env.Approved = nil
	res = AttributeLogin(live, accounts, lookupFrom(stored), env, oauthEnv())
	if !res.OK || res.Account.UUID != "u-login" {
		t.Fatalf("unapproved key attributed to %+v via %q, want the login account", res.Account, res.Via)
	}
	if !env.EnvKeyNeedsApproval() {
		t.Fatal("EnvKeyNeedsApproval() = false; the caller has no way to report the ambiguity")
	}
}

// A configured apiKeyHelper resolves a key ccdad cannot see and that displaces
// the login. The answer is "not managed", and it names the mechanism -- because
// "not managed" alone would send someone looking at their accounts when the
// cause is a settings key.
func TestAttributeReportsAnApiKeyHelperRatherThanGuessing(t *testing.T) {
	accounts := []store.Account{{UUID: "u-login", Email: "login@example.com", Idx: 1}}
	stored := map[string]cclink.Blob{"u-login": oauthBlob("RT-ONE")}
	live := liveLogin("RT-ONE")

	res := AttributeLogin(live, accounts, lookupFrom(stored), identity.APIKeyEnvironment{Interactive: true, Helper: true}, oauthEnv())
	if res.OK {
		t.Fatalf("attributed to %+v; the helper's key is one ccdad cannot see", res.Account)
	}
	if !strings.Contains(res.Via, "apiKeyHelper") {
		t.Fatalf("via = %q, want it to name apiKeyHelper", res.Via)
	}
}

func lookupFrom(stored map[string]cclink.Blob) func(string) (cclink.Blob, error) {
	return func(uuid string) (cclink.Blob, error) { return stored[uuid], nil }
}

// THE TWO ARMS THAT STOP `which` ANSWERING WITH THE WRONG ACCOUNT, and neither
// had a test: every other call in this file probes an empty sandbox, so they all
// take the login or none arm.
//
// A host-injected token outranks the credentials file, and ccdad cannot say
// WHOSE it is — the credential is in a file it must not read. Naming the file's
// account there would name an account Claude Code is not using, which is the one
// mistake this whole model exists to avoid.
func TestAttributeNamesTheMechanismWhenItCannotNameTheAccount(t *testing.T) {
	isolate(t)
	accounts := []store.Account{{UUID: "u-login", Email: "login@example.com", Idx: 1}}
	stored := map[string]cclink.Blob{"u-login": oauthBlob("RT-ONE")}
	live := liveLogin("RT-ONE")
	writeHostToken(t, "sk-ant-oat-INJECTED")

	res := AttributeLogin(live, accounts, lookupFrom(stored),
		identity.APIKeyEnvironment{Interactive: true}, oauthEnv())
	if res.OK {
		t.Fatalf("attributed to %+v — the credential is a file ccdad must not read", res.Account)
	}
	if !strings.Contains(res.Via, identity.HostOAuthTokenFile) {
		t.Errorf("Via = %q, want the path that outranks the file", res.Via)
	}
}

// The decline arm. A bg-auth snapshot is consumed by Claude Code before it looks
// at anything else, and the fact that decides it is inside a credential — so
// "which account is this" has no honest answer, and a guess is worst here.
func TestAttributeDeclinesOnABgAuthSnapshot(t *testing.T) {
	isolate(t)
	accounts := []store.Account{{UUID: "u-login", Email: "login@example.com", Idx: 1}}
	stored := map[string]cclink.Blob{"u-login": oauthBlob("RT-ONE")}
	snapshot := filepath.Join(t.TempDir(), "snap.json")
	if err := os.WriteFile(snapshot, []byte(`{"accessToken":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_BG_AUTH_SNAPSHOT_PATH", snapshot)

	res := AttributeLogin(liveLogin("RT-ONE"), accounts, lookupFrom(stored),
		identity.APIKeyEnvironment{Interactive: true}, oauthEnv())
	if res.OK {
		t.Fatalf("attributed to %+v on a machine ccdad cannot resolve", res.Account)
	}
	if !strings.Contains(res.Via, "cannot resolve") {
		t.Errorf("Via = %q, want the decline", res.Via)
	}
}

// The stand-down note is built from the SOURCE, not from a variable name. Three
// of the sources it fires for have no variable, and the note used to tell every
// one of them to unset CLAUDE_CODE_OAUTH_TOKEN.
func TestDisplacementNoteNamesTheSourceAndNotAVariable(t *testing.T) {
	hostFile := Result{EnvTokenWins: true, DisplacedBy: identity.OAuthHostTokenFile}
	note := DisplacementNote("Not switching: ", hostFile)
	if strings.Contains(note, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("the note names a variable that is not set:\n%s", note)
	}
	for _, want := range []string{identity.HostOAuthTokenFile, "check the host session"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not carry %q:\n%s", want, note)
		}
	}

	// And the variable that IS one is still named.
	envToken := Result{EnvTokenWins: true, DisplacedBy: identity.OAuthTokenEnv}
	if got := DisplacementNote("Note: ", envToken); !strings.Contains(got, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("the note drops the variable's name when there IS one:\n%s", got)
	}

	// The decline says it cannot tell rather than naming a source.
	declined := Result{EnvTokenWins: true, DisplacedUnresolved: true}
	if got := DisplacementNote("Not switching: ", declined); !strings.Contains(got, "cannot tell") {
		t.Errorf("the decline does not say so:\n%s", got)
	}
}

// The gate itself: a source with no variable behind it must stand the engine
// down, because the swap succeeds and changes nothing.
func TestUnattendedSwitchStandsDownForAHostInjectedToken(t *testing.T) {
	isolate(t)
	writeHostToken(t, "sk-ant-oat-INJECTED")

	target := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")
	seed(t, "u-1", "one@example.com")

	res, err := Execute(openStore(t), Request{Target: target, LiveUUID: "u-1", Unattended: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Overridden {
		t.Fatalf("Outcome = %v, want Overridden — the engine would switch into the void", res.Outcome)
	}
	if res.DisplacedBy != identity.OAuthHostTokenFile {
		t.Errorf("DisplacedBy = %v, want the host token file", res.DisplacedBy)
	}
}
