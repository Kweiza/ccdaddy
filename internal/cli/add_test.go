package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/oauth"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
)

func TestDetectTokenType(t *testing.T) {
	cases := []struct {
		token    string
		isAPIKey bool
		wantErr  bool
	}{
		{token: "sk-ant-oat01-abcdef", isAPIKey: false},
		{token: "sk-ant-api03-abcdef", isAPIKey: true},
		{token: "  sk-ant-api03-abcdef  ", isAPIKey: true},
		{token: "not-a-token", wantErr: true},
		{token: "", wantErr: true},
	}
	for _, tc := range cases {
		gotAPIKey, err := detectTokenType(tc.token)
		if tc.wantErr {
			if err == nil {
				t.Errorf("detectTokenType(%q) = nil error, want an error", tc.token)
			}
			continue
		}
		if err != nil {
			t.Errorf("detectTokenType(%q) = %v, want nil", tc.token, err)
			continue
		}
		if gotAPIKey != tc.isAPIKey {
			t.Errorf("detectTokenType(%q) isAPIKey = %v, want %v", tc.token, gotAPIKey, tc.isAPIKey)
		}
	}
}

func TestAddCmdRejectsBothSurfaceFlags(t *testing.T) {
	isolate(t)

	err, _, _ := runCmd(t, newAddCmd(), "--claudeai", "--console")
	if err == nil {
		t.Fatal("Execute() = nil, want a usage error")
	}
	if CodeFor(err) != ExitUsage {
		t.Fatalf("CodeFor = %d, want %d", CodeFor(err), ExitUsage)
	}
}

func TestAddTokenRejectsAliasThatLooksLikeAnIndex(t *testing.T) {
	isolate(t)

	err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-oat01-x", "--alias", "12")
	if err == nil {
		t.Fatal("Execute() = nil, want a usage error for a numeric alias")
	}
	if CodeFor(err) != ExitUsage {
		t.Fatalf("CodeFor = %d, want %d", CodeFor(err), ExitUsage)
	}
}

// Cobra reports an arg-count violation as a plain error, which would exit 1.
// Under this binary's contract a missing or extra argument is a usage error,
// and that is the whole point of keeping code 2 exclusive.
func TestArgCountViolationsAreUsageErrors(t *testing.T) {
	isolate(t)

	for _, args := range [][]string{
		{"add", "one", "two"},
		{"add-token", "one", "two"},
	} {
		code, _, _, _ := runRoot(t, args...)
		if code != ExitUsage {
			t.Errorf("ccdad %s = %d, want %d", strings.Join(args, " "), code, ExitUsage)
		}
	}
}

// Giving the alias twice is a user mistake that must be named, not silently
// resolved in favour of one of them.
func TestAddRejectsTheAliasGivenTwice(t *testing.T) {
	isolate(t)

	err, _, _ := runCmd(t, newAddCmd(), "mine", "--alias", "yours")
	if err == nil {
		t.Fatal("Execute() = nil, want a usage error when the alias is given twice")
	}
	if CodeFor(err) != ExitUsage {
		t.Fatalf("CodeFor = %d, want %d", CodeFor(err), ExitUsage)
	}
	// A single --alias is not a mistake, and must get past validation. It then
	// stops at the environment refusal, which is how this asserts "accepted"
	// without running a real login.
	err, _, _ = runCmd(t, newAddCmd(), "--alias", "same")
	if got := CodeFor(err); got != ExitBlocked {
		t.Fatalf("CodeFor(single --alias) = %d, want %d: it should reach the environment check", got, ExitBlocked)
	}
}

// A machine with neither a browser nor a terminal cannot complete a login. The
// point of the check is that it is decided BEFORE the login starts: moving it
// after oauth.Login would still return ExitBlocked, five minutes later.
func TestAddRefusesUpFrontWithNeitherBrowserNorTTY(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)

	start := time.Now()
	err, _, _ := runCmd(t, newAddCmd())
	if err == nil {
		t.Fatal("Execute() = nil, want a refusal")
	}
	if got := CodeFor(err); got != ExitBlocked {
		t.Fatalf("CodeFor = %d, want %d", got, ExitBlocked)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("refusal took %v; it must be decided before the login begins", elapsed)
	}
	assertNoLiveCredentials(t)
}

// refreshTokenExpiresAt, rateLimitTier and clientId are the three fields
// clauth's typed struct drops on every re-serialize. clientId is what a
// revocation needs.
func TestCredentialBlobCarriesEveryField(t *testing.T) {
	tok := &oauth.TokenResponse{
		AccessToken: "AT", RefreshToken: "RT",
		ExpiresIn: 3600, RefreshTokenExpiresIn: 7200,
		Scope: "user:profile user:inference",
	}

	blob, err := credentialBlob(tok, &identity.Profile{RateLimitTier: "default_claude_max_20x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob["claudeAiOauth"], &got); err != nil {
		t.Fatal(err)
	}
	// Presence alone is too weak an assertion: subscriptionType and
	// rateLimitTier are written as an explicit null when unknown, so a key can
	// exist and still carry nothing.
	for k, want := range map[string]any{
		"accessToken":      "AT",
		"refreshToken":     "RT",
		"scopes":           []any{"user:profile", "user:inference"},
		"rateLimitTier":    "default_claude_max_20x",
		"subscriptionType": nil,
	} {
		if !reflect.DeepEqual(got[k], want) {
			t.Errorf("claudeAiOauth[%q] = %#v, want %#v", k, got[k], want)
		}
	}
	// Values, not presence: refreshTokenExpiresAt is one of the three fields
	// clauth destroyed, and a key that exists carrying the wrong number is no
	// better than a missing one.
	for k, seconds := range map[string]int64{"expiresAt": 3600, "refreshTokenExpiresAt": 7200} {
		ms, ok := got[k].(float64)
		if !ok {
			t.Errorf("claudeAiOauth[%q] = %#v, want a millisecond timestamp", k, got[k])
			continue
		}
		if d := ms - float64(time.Now().UnixMilli()); d < float64(seconds*1000-100_000) || d > float64(seconds*1000) {
			t.Errorf("claudeAiOauth[%q] = %v, want roughly now+%ds in milliseconds", k, ms, seconds)
		}
	}
	// clientId must NOT be synthesized. Claude Code's login writes
	// `clientId: t?.oauthClient?.clientId`, which is undefined for the default
	// public client, so JSON.stringify omits the key — and its ABSENCE is what
	// Claude Code's own refresh tests: `d = Boolean((IZ(f.scopes) ||
	// f.subscriptionType) && !f.clientId)` selects the curated refresh scope
	// set. Writing a clientId flips that to false and makes Claude Code refresh
	// with the raw stored scopes, including org:create_api_key, the exact scope
	// the refresh grant drops.
	if _, bad := got["clientId"]; bad {
		t.Errorf("claudeAiOauth carries a synthesized clientId (%v); a first-party login must omit the key", got["clientId"])
	}
}

// A re-authentication must not wipe fields a previous login or import put in
// the stored record. That is the exact clauth failure mode — a typed decode
// that silently drops every field it does not know — and it would be
// reintroduced one layer above cclink.
func TestCredentialBlobPreservesPriorFields(t *testing.T) {
	prior := cclink.Blob{"claudeAiOauth": json.RawMessage(
		`{"accessToken":"OLD","subscriptionType":"max","somethingAnthropicAddedLater":"keep me"}`)}
	tok := &oauth.TokenResponse{AccessToken: "NEW", RefreshToken: "RT", ExpiresIn: 3600}

	blob, err := credentialBlob(tok, nil, prior)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob["claudeAiOauth"], &got); err != nil {
		t.Fatal(err)
	}
	if got["accessToken"] != "NEW" {
		t.Fatalf("accessToken = %v, want the fresh one to win", got["accessToken"])
	}
	if got["subscriptionType"] != "max" {
		t.Errorf("subscriptionType = %v, want the stored value preserved", got["subscriptionType"])
	}
	if got["somethingAnthropicAddedLater"] != "keep me" {
		t.Errorf("an unrecognized stored field was dropped: %v", got)
	}
}

// A corrupt prior record must not make the whole login fail; it is simply
// replaced by the one we just obtained.
func TestCredentialBlobSurvivesACorruptPriorRecord(t *testing.T) {
	prior := cclink.Blob{"claudeAiOauth": json.RawMessage(`not json at all`)}
	tok := &oauth.TokenResponse{AccessToken: "NEW", RefreshToken: "RT", ExpiresIn: 3600}

	blob, err := credentialBlob(tok, nil, prior)
	if err != nil {
		t.Fatalf("credentialBlob() = %v, want a corrupt prior to be replaced, not fatal", err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob["claudeAiOauth"], &got); err != nil {
		t.Fatal(err)
	}
	if got["accessToken"] != "NEW" {
		t.Fatalf("accessToken = %v, want NEW", got["accessToken"])
	}
}

// The 2.1.238 bundle stores an API key in ~/.claude.json as primaryApiKey and a
// setup token only in CLAUDE_CODE_OAUTH_TOKEN. Neither is a claudeAiOauth
// record — that object has exactly eight keys and none of them is an API key —
// so ccdad must not put one there, and must not hand one to cclink.Activate.
func TestAddTokenStoresTokensOutsideTheOAuthRecord(t *testing.T) {
	for _, tc := range []struct{ name, token string }{
		{"api key", "sk-ant-api03-TESTKEY"},
		{"setup token", "sk-ant-oat01-TESTTOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			stubEnvironment(t, false, false)
			// A setup token is a bearer, so this path does resolve it.
			stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, profileJSON("acct-1", "a@example.com"))
			})

			if err, _, _ := runCmd(t, newAddTokenCmd(), tc.token); err != nil {
				t.Fatalf("Execute() = %v, want nil", err)
			}
			assertNoLiveCredentials(t)

			s, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			accts := s.Accounts()
			if len(accts) != 1 {
				t.Fatalf("Accounts() = %d, want the account stored", len(accts))
			}
			creds, err := s.Credentials(accts[0].UUID)
			if err != nil {
				t.Fatal(err)
			}
			if _, bad := creds["claudeAiOauth"]; bad {
				t.Fatalf("stored a claudeAiOauth record for a %s: %s", tc.name, creds["claudeAiOauth"])
			}
			if _, ok := creds[cclink.TokenKey]; !ok {
				t.Fatalf("stored credentials = %v, want the ccdad token record", creds)
			}
		})
	}
}

// A setup token has nowhere to be installed. Claude Code reads one from
// CLAUDE_CODE_OAUTH_TOKEN and from nothing else — `claude setup-token` prints
// it and deliberately skips saving it — so refuse and name the mechanism that
// does work, rather than producing a live credentials file Claude Code cannot
// use.
//
// This is deliberately no longer a table over both token kinds. An API key HAS
// somewhere to go and is activated; see TestAddTokenActivatesAnAPIKey.
func TestAddTokenRefusesToActivateASetupToken(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)

	err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-oat01-TESTTOKEN", "--activate")
	if err == nil {
		t.Fatal("Execute() = nil, want --activate refused for a setup token")
	}
	if got := CodeFor(err); got != ExitUsage {
		t.Fatalf("CodeFor = %d, want %d", got, ExitUsage)
	}
	if !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("error %q should name CLAUDE_CODE_OAUTH_TOKEN", err)
	}
	assertNoLiveCredentials(t)
}

// --activate on an API key is BOTH writes or it is nothing.
//
// Asserting only the config half would pass against the bug this path exists to
// avoid: Claude Code prefers a claudeAiOauth login over its stored
// primaryApiKey in every configuration, so a key written while a login sits in
// the credentials file is read and then ignored. The account would report as
// switched and the session would go on billing the old one.
func TestAddTokenActivatesAnAPIKey(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	// A login already in place, so the removal half is observable at all: with
	// an empty credentials file both a correct and a broken implementation leave
	// nothing behind.
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"live","refreshToken":"live-r"},"mcpOAuth":{"srv":1}}`)

	const key = "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV"
	if err, _, _ := runCmd(t, newAddTokenCmd(), key, "--activate"); err != nil {
		t.Fatalf("Execute() = %v", err)
	}

	cfg, err := cclink.LoadGlobalConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := cclink.PrimaryAPIKey(cfg); !ok || got != key {
		t.Fatalf("primaryApiKey = %q (present %v), want the key just added", got, ok)
	}
	if approved := cclink.ApprovedAPIKeys(cfg); len(approved) != 1 || approved[0] != cclink.APIKeyApproval(key) {
		t.Fatalf("approved = %v, want exactly %q", approved, cclink.APIKeyApproval(key))
	}

	live, err := cclink.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, still := live["claudeAiOauth"]; still {
		t.Fatal("the OAuth login is still in the credentials file, so the stored key is inert and nothing was activated")
	}
	if _, gone := live["mcpOAuth"]; !gone {
		t.Fatal("clearing the login destroyed the machine-scoped mcpOAuth key")
	}
}

// The API-key path must make no network call at all: there is no endpoint that
// resolves an API key to an account.
func TestAddTokenAPIKeyMakesNoNetworkCall(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)

	// isolate already fails the test if the profile endpoint is touched, which
	// is exactly the assertion this test wants.

	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-api03-TESTKEY"); err != nil {
		t.Fatal(err)
	}
}

// A token Anthropic has already rejected must not become a managed account
// under a fabricated uuid that can never be reconciled with a real one.
func TestAddTokenRefusesACredentialTheAPIRejected(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)

	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-oat01-DEAD")
	if err == nil {
		t.Fatal("Execute() = nil, want a rejected token refused")
	}
	if got := CodeFor(err); got != ExitUsage {
		t.Fatalf("CodeFor = %d, want %d", got, ExitUsage)
	}
	s, _ := store.Open()
	if n := len(s.Accounts()); n != 0 {
		t.Fatalf("Accounts() = %d, want a rejected token not to be stored", n)
	}
}

// A 403 from the profile endpoint is the ORDINARY answer for a setup token,
// not a broken one. `claude setup-token` mints a credential that does not carry
// user:profile, so the lookup is refused on scope for every token this command
// exists to register -- and folding that into "rejected" made add-token unable
// to store a single one of them. The token still authenticates a session, which
// is what `ccdad run` needs, so it is stored under a synthetic label with the
// reason named.
func TestAddTokenStoresASetupTokenRefusedOnScope(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)

	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	err, _, stderr := runCmd(t, newAddTokenCmd(), "sk-ant-oat01-SCOPED")
	if err != nil {
		t.Fatalf("Execute() = %v, want a scope refusal to still store the account", err)
	}
	s, _ := store.Open()
	if n := len(s.Accounts()); n != 1 {
		t.Fatalf("Accounts() = %d, want the account stored under a synthetic label", n)
	}
	if !strings.Contains(stderr, "user:profile") {
		t.Fatalf("stderr = %q, want it to name the scope the lookup needs", stderr)
	}
	if strings.Contains(stderr, "check it and try again") {
		t.Fatalf("stderr = %q, want it not to blame a token that works", stderr)
	}
}

// A 5xx is not a dead token: the account is still stored, under a synthetic
// label, so a headless machine with a flaky network is not blocked.
func TestAddTokenStoresOnATransientProfileFailure(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)

	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-oat01-FLAKY"); err != nil {
		t.Fatalf("Execute() = %v, want a transient failure to still store the account", err)
	}
	s, _ := store.Open()
	if n := len(s.Accounts()); n != 1 {
		t.Fatalf("Accounts() = %d, want the account stored anyway", n)
	}
}

// A synthetic label is derived from the token's own fingerprint, so it does not
// churn when an unrelated account is removed and the store recompacts.
func TestSyntheticLabelIsStableAcrossReAdd(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)

	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-api03-ONE"); err != nil {
		t.Fatal(err)
	}
	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-api03-TWO"); err != nil {
		t.Fatal(err)
	}
	s, _ := store.Open()
	first, _ := s.Get("apikey-" + shortHash("sk-ant-api03-ONE"))
	before := first.Email

	if err := s.Remove("apikey-" + shortHash("sk-ant-api03-TWO")); err != nil {
		t.Fatal(err)
	}
	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-api03-ONE"); err != nil {
		t.Fatal(err)
	}
	s2, _ := store.Open()
	after, _ := s2.Get("apikey-" + shortHash("sk-ant-api03-ONE"))
	if after.Email != before {
		t.Fatalf("label churned from %q to %q on an unchanged account", before, after.Email)
	}
}

// The stored Kind is what the auto-switch engine ranks on, so it has to be
// asserted through the command that stores it — calling identity.Classify
// directly only duplicates a table case in that package and leaves the CLI's
// own call site free to be replaced by a constant.
func TestAddStoresTheClassifiedKind(t *testing.T) {
	cases := []struct {
		name        string
		billingType string
		want        identity.Kind
	}{
		{"a max org with overage on is a subscription", "subscription", identity.KindSubscription},
		{"a metered billing type is credit", "usage_based", identity.KindCredit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			stubEnvironment(t, true, false)
			stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, `{"account":{"uuid":"acct-1","email":"a@example.com"},`+
					`"organization":{"uuid":"org-1","organization_type":"claude_max",`+
					`"has_extra_usage_enabled":true,"billing_type":"`+tc.billingType+`"}}`)
			})
			stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))

			if err, _, _ := runCmd(t, newAddCmd()); err != nil {
				t.Fatal(err)
			}
			s, _ := store.Open()
			got, ok := s.Get("acct-1")
			if !ok {
				t.Fatal("the account was not stored")
			}
			if got.Kind != tc.want {
				t.Fatalf("stored Kind = %v, want %v", got.Kind, tc.want)
			}
		})
	}
}

// The api-key twin: add-token classifies without a profile at all.
func TestAddTokenStoresTheAPIKeyKind(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)

	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-api03-KINDTEST"); err != nil {
		t.Fatal(err)
	}
	s, _ := store.Open()
	if got := s.Accounts()[0].Kind; got != identity.KindAPIKey {
		t.Fatalf("stored Kind = %v, want api-key", got)
	}
}

// A setup token IS a bearer, and resolving it is the only way `add-token`
// learns the real account uuid rather than filing it under a fabricated one.
// Which token it sends was unpinned: hard-coding some other value in the
// FetchProfile call left the whole suite green.
func TestAddTokenResolvesTheTokenItWasGiven(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)
	const token = "sk-ant-oat01-BEARERTEST"
	var gotAuth string
	stubProfile(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})

	if err, _, _ := runCmd(t, newAddTokenCmd(), token); err != nil {
		t.Fatal(err)
	}
	if want := "Bearer " + token; gotAuth != want {
		t.Fatalf("Authorization = %q, want %q — the account is only the token's if the token is what was asked about", gotAuth, want)
	}
}

// --email is the only label an API-key account can have: no endpoint resolves
// one to an account, so the alternative is the synthetic api-key-<hash> stand-in.
// The flag appeared in no test at all.
func TestAddTokenLabelsAnAPIKeyWithTheGivenEmail(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)

	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-api03-EMAILTEST", "--email", "me@example.com"); err != nil {
		t.Fatal(err)
	}
	s, _ := store.Open()
	if got := s.Accounts()[0].Email; got != "me@example.com" {
		t.Fatalf("stored Email = %q, want the label --email gave it", got)
	}
}

// --alias on add-token was only ever given the invalid value "12", which the
// validator refuses before anything stores it — so the flag's actual job was
// unpinned.
func TestAddTokenStoresAValidAlias(t *testing.T) {
	isolate(t)
	stubEnvironment(t, false, false)

	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-api03-ALIASTEST", "--alias", "work"); err != nil {
		t.Fatal(err)
	}
	s, _ := store.Open()
	if got := s.Accounts()[0].Alias; got != "work" {
		t.Fatalf("stored Alias = %q, want the handle --alias gave it", got)
	}
}

// A cancelled context must surface as SIGINT's code, not as a generic failure:
// a supervisor keys on 130 to tell "the operator stopped it" from "it broke".
// Without the root carrying a signal context this arm is unreachable, and
// Ctrl-C during a five-minute login would abandon the loopback listener instead
// of unwinding it.
func TestAddOnACancelledContextExitsInterrupted(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	root := NewRootCmd()
	root.SetContext(ctx)
	root.SetArgs([]string{"add"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	var top bytes.Buffer
	if code := ExecuteWith(root, &top); code != ExitInterrupted {
		t.Fatalf("exit = %d (%s), want %d", code, top.String(), ExitInterrupted)
	}
}

// announcedURL stands in for the manual authorize URL the real Login builds and
// hands to Announce.
const announcedURL = "https://claude.com/cai/oauth/authorize?state=STATE"

// loginCapture records what `add` asked the login for.
//
// Those options are the command's ENTIRE instruction to oauth.Login, and a
// wrong one is invisible downstream: the login succeeds either way and the
// account is stored either way. A stub that discards them lets `--console`
// authenticate against the subscription surface, `--no-browser` open a browser,
// and the paste half of the race vanish, all with the whole suite green.
type loginCapture struct{ calls []oauth.LoginOptions }

// last is the options of the most recent login the command drove.
func (c *loginCapture) last(t *testing.T) oauth.LoginOptions {
	t.Helper()
	if len(c.calls) == 0 {
		t.Fatal("the login was never called")
	}
	return c.calls[len(c.calls)-1]
}

// stubLogin makes the post-login half of `add` reachable and records the
// options it was given. Everything that decides what happens to a live
// credential lives after the login.
func stubLogin(t *testing.T, tok *oauth.TokenResponse) *loginCapture {
	t.Helper()
	restore := login
	t.Cleanup(func() { login = restore })
	rec := &loginCapture{}
	login = func(_ context.Context, opts oauth.LoginOptions) (*oauth.LoginResult, error) {
		rec.calls = append(rec.calls, opts)
		// The real Login always announces before it waits, and Announce is the
		// whole user-facing instruction block: the URL, and the line that says
		// which of the two paths can still finish. A stub that skips it leaves
		// every one of those lines unexecuted by any test.
		opts.Announce(announcedURL)
		return &oauth.LoginResult{Token: tok, ViaLoopback: true}, nil
	}
	return rec
}

func loginToken(uuid, email, refresh string) *oauth.TokenResponse {
	tok := &oauth.TokenResponse{
		AccessToken: "AT-" + refresh, RefreshToken: refresh,
		ExpiresIn: 3600, Scope: "user:profile user:inference",
	}
	tok.Account.UUID = uuid
	tok.Account.EmailAddress = email
	return tok
}

// Every flag `add` takes ends up in these five fields and nowhere else, so this
// is the only place a wrong answer is observable. `--console` reaching the
// subscription surface would mint a credential the user did not ask for and
// report success; a dropped paste source would silently delete half of the race
// this command's own --help calls its headline.
func TestAddAsksTheLoginForWhatTheFlagsSay(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		tty, browser bool
		wantSurface  oauth.Surface
		wantOpen     bool
		wantPaste    bool
		wantTimeout  time.Duration
	}{
		{
			name: "the default is the subscription surface with both paths running",
			tty:  true, browser: true,
			wantSurface: oauth.SurfaceClaudeAI, wantOpen: true, wantPaste: true,
			wantTimeout: oauth.DefaultLoginTimeout,
		},
		{
			name: "--console authenticates against the Console surface",
			args: []string{"--console"}, tty: true, browser: true,
			wantSurface: oauth.SurfaceConsole, wantOpen: true, wantPaste: true,
			wantTimeout: oauth.DefaultLoginTimeout,
		},
		{
			name: "--no-browser leaves the browser shut on a machine that has one",
			args: []string{"--no-browser"}, tty: true, browser: true,
			wantSurface: oauth.SurfaceClaudeAI, wantOpen: false, wantPaste: true,
			wantTimeout: oauth.DefaultLoginTimeout,
		},
		{
			name: "a machine with no browser opens none",
			tty:  true, browser: false,
			wantSurface: oauth.SurfaceClaudeAI, wantOpen: false, wantPaste: true,
			wantTimeout: oauth.DefaultLoginTimeout,
		},
		{
			name: "stdin that is not a terminal removes the paste path",
			tty:  false, browser: true,
			wantSurface: oauth.SurfaceClaudeAI, wantOpen: true, wantPaste: false,
			wantTimeout: oauth.DefaultLoginTimeout,
		},
		{
			name: "--timeout is the deadline the login is given",
			args: []string{"--timeout", "42s"}, tty: true, browser: true,
			wantSurface: oauth.SurfaceClaudeAI, wantOpen: true, wantPaste: true,
			wantTimeout: 42 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			stubEnvironment(t, tc.tty, tc.browser)
			stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, profileJSON("acct-1", "a@example.com"))
			})
			rec := stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))

			if err, _, _ := runCmd(t, newAddCmd(), tc.args...); err != nil {
				t.Fatal(err)
			}

			opts := rec.last(t)
			if opts.Surface != tc.wantSurface {
				t.Errorf("Surface = %v, want %v", opts.Surface, tc.wantSurface)
			}
			if opts.OpenBrowser != tc.wantOpen {
				t.Errorf("OpenBrowser = %v, want %v", opts.OpenBrowser, tc.wantOpen)
			}
			if got := opts.Paste != nil; got != tc.wantPaste {
				t.Errorf("a paste source was supplied = %v, want %v", got, tc.wantPaste)
			}
			if opts.Timeout != tc.wantTimeout {
				t.Errorf("Timeout = %v, want %v", opts.Timeout, tc.wantTimeout)
			}
		})
	}
}

// Announce is the whole user-facing instruction block, and which half of it
// runs is decided by the machine rather than by a flag: a terminal is prompted
// to paste, a pipe is told that only the browser callback can still finish. The
// second line is what the isTTY helper was written for, and neither branch had
// ever been executed by a test.
func TestAddAnnouncesTheURLAndWhichPathCanFinish(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tty, browser bool
		want         []string
		notWant      []string
	}{
		{
			name: "a terminal is told where to paste",
			tty:  true, browser: false,
			want:    []string{announcedURL, "Visit this URL to sign in", pastePrompt},
			notWant: []string{"Opening your browser"},
		},
		{
			name: "a pipe is told the browser callback is the only path left",
			tty:  false, browser: true,
			want: []string{announcedURL, "Opening your browser to sign in.", "If it does not open",
				"stdin is not a terminal, so ccdad is waiting on the browser callback only."},
			notWant: []string{pastePrompt},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			stubEnvironment(t, tc.tty, tc.browser)
			stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, profileJSON("acct-1", "a@example.com"))
			})
			stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))

			err, _, stderr := runCmd(t, newAddCmd())
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want it to carry %q", stderr, want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(stderr, notWant) {
					t.Errorf("stderr = %q, want it NOT to carry %q on this machine", stderr, notWant)
				}
			}
		})
	}
}

// add never switches the live login unless --activate. The two halves are a
// matched pair — the first alone is satisfied by deleting the Activate call,
// the second alone by always activating.
func TestAddDoesNotSwitchWithoutActivate(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})

	if err, _, _ := runCmd(t, newAddCmd()); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	assertNoLiveCredentials(t)

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s.Accounts()); n != 1 {
		t.Fatalf("Accounts() = %d, want the account stored anyway", n)
	}
}

func TestAddWithActivateWritesTheLiveFile(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})

	if err, _, _ := runCmd(t, newAddCmd(), "--activate"); err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}

	raw, err := os.ReadFile(mustPath(ccpath.CredentialsPath()))
	if err != nil {
		t.Fatalf("--activate did not write the live credentials file: %v", err)
	}
	var live map[string]json.RawMessage
	if err := json.Unmarshal(raw, &live); err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(live["claudeAiOauth"], &rec); err != nil {
		t.Fatal(err)
	}
	if rec["refreshToken"] != "RT-1" {
		t.Fatalf("live refreshToken = %v, want the one just obtained", rec["refreshToken"])
	}
	// rateLimitTier must carry the profile's value, not merely exist: it is one
	// of the three fields clauth destroyed.
	if rec["rateLimitTier"] != "default_claude_max_20x" {
		t.Fatalf("rateLimitTier = %v, want the profile's value", rec["rateLimitTier"])
	}
	// subscriptionType is the other half of the same rule, and Claude Code's own
	// normalizer writes it from the profile's organization_type.
	if rec["subscriptionType"] != "max" {
		t.Fatalf("subscriptionType = %v, want Claude Code's mapped short name", rec["subscriptionType"])
	}
	s, _ := store.Open()
	if got := s.ActiveUUID(); got != "acct-1" {
		t.Fatalf("ActiveUUID() = %q, want acct-1", got)
	}
}

// add doubles as re-authentication. Re-authenticating the account that is
// currently live must keep the other account-scoped keys that are sitting in
// the live file: losing trustedDeviceToken costs a device-cap slot and losing
// enterpriseGateway costs a re-trust.
func TestReAuthenticatingTheLiveAccountKeepsItsOtherKeys(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	if err, _, _ := runCmd(t, newAddCmd(), "--activate"); err != nil {
		t.Fatal(err)
	}
	// Claude Code earns these during use; they are not part of a fresh login.
	addLiveKey(t, "trustedDeviceToken", `"device-token-value"`)

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-2"))
	if err, _, _ := runCmd(t, newAddCmd()); err != nil {
		t.Fatal(err)
	}

	s, _ := store.Open()
	stored, err := s.Credentials("acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored["trustedDeviceToken"]; !ok {
		t.Fatalf("re-authentication dropped trustedDeviceToken: %v", stored)
	}
}

// The mirror: capturing unconditionally would file the PREVIOUS account's
// device token under the account being added, which is the same leak in the
// other direction.
func TestAddingASecondAccountDoesNotInheritTheFirstsKeys(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer AT-RT-2" {
			io.WriteString(w, profileJSON("acct-2", "b@example.com"))
			return
		}
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	if err, _, _ := runCmd(t, newAddCmd(), "--activate"); err != nil {
		t.Fatal(err)
	}
	addLiveKey(t, "trustedDeviceToken", `"first-accounts-device"`)

	stubLogin(t, loginToken("acct-2", "b@example.com", "RT-2"))
	if err, _, _ := runCmd(t, newAddCmd()); err != nil {
		t.Fatal(err)
	}

	s, _ := store.Open()
	stored, err := s.Credentials("acct-2")
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := stored["trustedDeviceToken"]; leaked {
		t.Fatalf("the new account inherited the previous account's device token: %v", stored)
	}
}

// An alias is unique across accounts. store.Add writes Account.Alias straight
// through with no uniqueness check, so routing it through SetAlias is the only
// thing that catches a collision — and it must surface as exit 2.
func TestAddRejectsAnAliasAnotherAccountHolds(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer AT-RT-2" {
			io.WriteString(w, profileJSON("acct-2", "b@example.com"))
			return
		}
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	if err, _, _ := runCmd(t, newAddCmd(), "--alias", "work"); err != nil {
		t.Fatal(err)
	}

	stubLogin(t, loginToken("acct-2", "b@example.com", "RT-2"))
	err, _, _ := runCmd(t, newAddCmd(), "--alias", "work")
	if err == nil {
		t.Fatal("Execute() = nil, want the duplicate alias refused")
	}
	if got := CodeFor(err); got != ExitUsage {
		t.Fatalf("CodeFor = %d, want %d", got, ExitUsage)
	}
}

// An explicit --alias must re-label an account being re-authenticated: store.Add
// deliberately preserves the stored alias over the incoming one, so assigning
// Account.Alias would silently discard the flag.
func TestReAuthenticationHonoursANewAlias(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	if err, _, _ := runCmd(t, newAddCmd(), "--alias", "old"); err != nil {
		t.Fatal(err)
	}
	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-2"))
	if err, _, _ := runCmd(t, newAddCmd(), "--alias", "new"); err != nil {
		t.Fatal(err)
	}

	s, _ := store.Open()
	got, _ := s.Get("acct-1")
	if got.Alias != "new" {
		t.Fatalf("Alias = %q, want the re-authentication to have applied it", got.Alias)
	}
}

// Nothing on stdin is a caller mistake, which the exit contract reserves 2 for.
func TestAddTokenWithEmptyStdinIsUsageError(t *testing.T) {
	isolate(t)

	cmd := newAddTokenCmd()
	cmd.SetIn(strings.NewReader(""))
	err, _, _ := runCmd(t, cmd, "-")
	if err == nil {
		t.Fatal("Execute() = nil, want a usage error for empty stdin")
	}
	if got := CodeFor(err); got != ExitUsage {
		t.Fatalf("CodeFor = %d, want %d", got, ExitUsage)
	}
}

// With no argument and no terminal there is nowhere to read a token from, and
// that is a caller mistake rather than a runtime failure. This pins the branch
// immediately above the no-echo prompt.
func TestAddTokenWithNoArgumentAndNoTTYIsUsageError(t *testing.T) {
	isolate(t)

	err, _, _ := runCmd(t, newAddTokenCmd())
	if err == nil {
		t.Fatal("Execute() = nil, want a usage error")
	}
	if got := CodeFor(err); got != ExitUsage {
		t.Fatalf("CodeFor = %d, want %d", got, ExitUsage)
	}
}

// The prompt goes to stderr, never stdout: `ccdad add-token > file` must not
// capture it, and a token typed at a prompt on stdout would be one redirect away
// from a log.
func TestAddTokenPromptsOnStderr(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	restore := readPassword
	t.Cleanup(func() { readPassword = restore })
	readPassword = func() ([]byte, error) { return []byte("sk-ant-api03-TYPED"), nil }

	err, out, errOut := runCmd(t, newAddTokenCmd())
	if err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if strings.Contains(out, "Token:") {
		t.Fatalf("the token prompt went to stdout: %q", out)
	}
	if !strings.Contains(errOut, "Token:") {
		t.Fatalf("stderr = %q, want the prompt", errOut)
	}
	s, _ := store.Open()
	if n := len(s.Accounts()); n != 1 {
		t.Fatalf("Accounts() = %d, want the typed token stored", n)
	}
}

// The live credentials file can change without ccdad being told: the user runs
// `/login` inside Claude Code. ccdad's own active_uuid is a display hint (the
// store says so), so gating the key carry on it means a re-authentication can
// absorb whichever account happens to be live — filing that account's device
// and design tokens under this one. That is a cross-account credential leak,
// which is worse than the loss it was added to prevent.
func TestReAuthenticationDoesNotAbsorbAnotherAccountsKeys(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	// Token-aware, and it has to be: ccdad now RESOLVES the live login rather
	// than giving up on it, so a stub that answers acct-1 for every bearer
	// would report someone else's token as acct-1's and the test would pass
	// while the code absorbed the keys.
	stubProfile(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer OTHER" {
			io.WriteString(w, profileJSON("acct-someone-else", "else@example.com"))
			return
		}
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	if err, _, _ := runCmd(t, newAddCmd(), "--activate"); err != nil {
		t.Fatal(err)
	}

	// Someone else's login lands in the live file, out of band. ccdad's
	// active_uuid still names acct-1.
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"OTHER","refreshToken":"RT-SOMEONE-ELSE"},`+
		`"trustedDeviceToken":"SOMEONE-ELSES-DEVICE","designOauth":{"refreshToken":"SOMEONE-ELSES-DESIGN"}}`)

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-2"))
	if err, _, _ := runCmd(t, newAddCmd()); err != nil {
		t.Fatal(err)
	}

	s, _ := store.Open()
	stored, err := s.Credentials("acct-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"trustedDeviceToken", "designOauth"} {
		if _, bad := stored[leaked]; bad {
			t.Fatalf("re-authentication absorbed another account's %s: %s", leaked, stored[leaked])
		}
	}
}

// Adopting the account Claude Code is already logged in as DOES carry its other
// account-scoped keys, at the cost of one profile call.
//
// Nothing in the credentials file names an account, so the cheap comparison —
// this login's record against the one this account last stored — has nothing to
// compare on a first adoption. The account endpoint does name one, so resolving
// the live login's own access token settles whose those keys are. What it buys
// is concrete: trustedDeviceToken is a device-cap slot and enterpriseGateway is
// a gateway re-trust, both lost on the very first switch away.
func TestAdoptingTheLiveLoginCarriesItsKeysWhenTheProfileProvesTheAccount(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	var probedLive bool
	stubProfile(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer OLD" {
			probedLive = true
		}
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"OLD","refreshToken":"RT-OLD"},`+
		`"trustedDeviceToken":"DEVICE","enterpriseGateway":{"url":"https://gw"}}`)

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	if err, _, _ := runCmd(t, newAddCmd()); err != nil {
		t.Fatal(err)
	}
	if !probedLive {
		t.Fatal("the live login was never resolved, so the keys can only have been carried on a guess")
	}

	s, _ := store.Open()
	stored, err := s.Credentials("acct-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"trustedDeviceToken", "enterpriseGateway"} {
		if _, ok := stored[want]; !ok {
			t.Fatalf("adoption dropped %s even though the profile proved the account: %v", want, stored)
		}
	}
}

// When the probe cannot answer, the old behaviour stands: carry nothing and say
// so. A live access token that has expired is the ordinary way to get here, and
// it must not turn a successful login into a failure — nor into a silent drop.
func TestAdoptingTheLiveLoginSaysWhatItCannotCarryWhenTheProbeFails(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer EXPIRED" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"EXPIRED","refreshToken":"RT-OLD"},`+
		`"trustedDeviceToken":"DEVICE","enterpriseGateway":{"url":"https://gw"}}`)

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	err, _, errOut := runCmd(t, newAddCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "trustedDeviceToken") || !strings.Contains(errOut, "enterpriseGateway") {
		t.Fatalf("stderr = %q, want it to name the account-scoped keys it is not carrying", errOut)
	}
	s, _ := store.Open()
	stored, err := s.Credentials("acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, bad := stored["trustedDeviceToken"]; bad {
		t.Fatal("an unresolved live login had its keys carried anyway")
	}
}

// clientId is one of the three fields clauth destroyed. Not synthesizing one
// is not the same as dropping one that is already stored: a credential that
// really did come from a non-default client keeps it.
func TestCredentialBlobPreservesAStoredClientID(t *testing.T) {
	prior := cclink.Blob{"claudeAiOauth": json.RawMessage(
		`{"accessToken":"OLD","clientId":"a-non-default-client"}`)}
	tok := &oauth.TokenResponse{AccessToken: "NEW", RefreshToken: "RT", ExpiresIn: 3600}

	blob, err := credentialBlob(tok, nil, prior)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob["claudeAiOauth"], &got); err != nil {
		t.Fatal(err)
	}
	if got["clientId"] != "a-non-default-client" {
		t.Fatalf("clientId = %v, want the stored one preserved", got["clientId"])
	}
}

// Claude Code maps organization_type through a four-entry table before storing
// it, and every one of its tier predicates compares against the SHORT name:
// HXe() is `subscriptionType === "max"`. Writing the raw "claude_max" makes a
// Max subscriber's entitlement invisible to the running Claude Code.
func TestCredentialBlobMapsSubscriptionType(t *testing.T) {
	for orgType, want := range map[string]any{
		"claude_max":        "max",
		"claude_pro":        "pro",
		"claude_enterprise": "enterprise",
		"claude_team":       "team",
		"something_new":     nil, // unmapped must be null, not the raw string
	} {
		tok := &oauth.TokenResponse{AccessToken: "AT", RefreshToken: "RT", ExpiresIn: 3600}
		blob, err := credentialBlob(tok, &identity.Profile{OrganizationType: orgType}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(blob["claudeAiOauth"], &got); err != nil {
			t.Fatal(err)
		}
		if got["subscriptionType"] != want {
			t.Errorf("organization_type %q -> subscriptionType %#v, want %#v", orgType, got["subscriptionType"], want)
		}
	}
}

// A setup token resolves to a real account uuid, which may already be managed
// through a browser login. store.Add replaces the credential file wholesale, so
// writing only the token record there destroys that account's claudeAiOauth —
// including the refresh token, which nothing else has a copy of.
func TestAddTokenKeepsAnExistingOAuthRecord(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	if err, _, _ := runCmd(t, newAddCmd()); err != nil {
		t.Fatal(err)
	}
	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-oat01-SAMEACCOUNT"); err != nil {
		t.Fatal(err)
	}

	s, _ := store.Open()
	stored, err := s.Credentials("acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored["claudeAiOauth"]; !ok {
		t.Fatalf("add-token destroyed the account's OAuth login: %v", stored)
	}
	if _, ok := stored[cclink.TokenKey]; !ok {
		t.Fatalf("add-token did not record the token: %v", stored)
	}
}

// An account that has both a browser login and a token is switchable: the OAuth
// record is what goes in the credentials file, and the token record sitting
// beside it must not make the account look uninstallable.
func TestSwitchPrefersTheOAuthRecordOverAToken(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})
	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	if err, _, _ := runCmd(t, newAddCmd()); err != nil {
		t.Fatal(err)
	}
	if err, _, _ := runCmd(t, newAddTokenCmd(), "sk-ant-oat01-SAMEACCOUNT"); err != nil {
		t.Fatal(err)
	}

	code, _, _, top := runRoot(t, "switch", "1")
	if code != ExitOK {
		t.Fatalf("switch = %d (%s), want it to activate the OAuth record", code, top)
	}
}

// A cancelled profile lookup is not a transient network failure. Filing it as
// one stores the account under a fabricated token-<hash> uuid and reports
// success, so the next run stores the SAME account a second time under its real
// uuid.
func TestAddTokenOnACancelledContextDoesNotStoreTheAccount(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := newAddTokenCmd()
	cmd.SetContext(ctx)
	err, _, _ := runCmd(t, cmd, "sk-ant-oat01-INTERRUPTED")
	if err == nil {
		t.Fatal("Execute() = nil, want a cancelled lookup to fail rather than be filed as transient")
	}
	if got := CodeFor(err); got != ExitInterrupted {
		t.Fatalf("CodeFor = %d, want %d", got, ExitInterrupted)
	}
	s, _ := store.Open()
	if n := len(s.Accounts()); n != 0 {
		t.Fatalf("Accounts() = %d, want nothing stored for an interrupted run", n)
	}
}

// --activate is a switch, so the unknown-key probe has to run on it too.
func TestAddActivateRunsTheUnknownKeyProbe(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"OLD","refreshToken":"RT-OLD"},"somethingNew":{"a":1}}`)
	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))

	err, _, errOut := runCmd(t, newAddCmd(), "--activate")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "somethingNew") {
		t.Fatalf("stderr = %q, want the unrecognized key named", errOut)
	}
}

// Two credentials that cannot identify themselves are not the same credential.
// A live file carrying account-scoped keys but no OAuth record has identity "",
// and so does an account with no prior record — comparing those as equal would
// absorb keys of unknown ownership on the very first add.
func TestCaptureDoesNotTreatTwoUnidentifiableCredentialsAsEqual(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})
	// No claudeAiOauth, so credentialIdentity(live) == "" — same as a brand-new
	// account's prior.
	writeLiveFile(t, `{"trustedDeviceToken":"UNKNOWN-OWNER","designOauth":{"refreshToken":"UNKNOWN"}}`)

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	if err, _, _ := runCmd(t, newAddCmd()); err != nil {
		t.Fatal(err)
	}

	s, _ := store.Open()
	stored, err := s.Credentials("acct-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"trustedDeviceToken", "designOauth"} {
		if _, bad := stored[k]; bad {
			t.Fatalf("absorbed %s from a credential that cannot identify itself: %v", k, stored)
		}
	}
}

// Re-authenticating an account that is NOT the live one must still keep that
// account's OWN account-scoped keys. There is no ambiguity about whose they
// are — they came out of this account's own stored snapshot — and store.Add
// replaces the credential file wholesale, so not carrying them forward deletes
// them.
func TestReAuthenticatingANonLiveAccountKeepsItsOwnKeys(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	if err, _, _ := runCmd(t, newAddCmd(), "--activate"); err != nil {
		t.Fatal(err)
	}
	addLiveKey(t, "trustedDeviceToken", `"acct-1-device"`)
	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-2"))
	if err, _, _ := runCmd(t, newAddCmd()); err != nil {
		t.Fatal(err)
	}

	// Something else becomes the live login, so acct-1 is no longer live.
	writeLiveFile(t, `{"claudeAiOauth":{"accessToken":"OTHER","refreshToken":"RT-OTHER"}}`)

	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-3"))
	if err, _, _ := runCmd(t, newAddCmd()); err != nil {
		t.Fatal(err)
	}

	s, _ := store.Open()
	stored, err := s.Credentials("acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored["trustedDeviceToken"]; !ok {
		t.Fatalf("re-authenticating a non-live account dropped its own device token: %v", stored)
	}
}

// A login timeout is exit 1 — a runtime failure in the exit contract the
// README's "Exit codes" section publishes.
func TestAddOnALoginTimeoutExitsOne(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	restore := login
	t.Cleanup(func() { login = restore })
	login = func(context.Context, oauth.LoginOptions) (*oauth.LoginResult, error) {
		return nil, oauth.ErrLoginTimeout
	}

	err, _, _ := runCmd(t, newAddCmd())
	if got := CodeFor(err); got != ExitFailure {
		t.Fatalf("CodeFor = %d, want %d (a login timeout is exit 1)", got, ExitFailure)
	}
}

// A paste without a '#' is a re-prompt rather than an abort — the loopback
// race may still be about to win. oauth.Login carries that machinery and
// reports each unreadable line through LoginOptions.Rejected, so whether the
// user sees anything at all is decided HERE, by whether `add` supplies the
// callback. Without it a malformed paste is swallowed in silence and the
// terminal just sits there.
func TestAddRepromptsOnAPasteItCouldNotRead(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})
	restore := login
	t.Cleanup(func() { login = restore })
	// Mimics the real Login's order: announce, then report an unreadable line,
	// then let the loopback win — which is exactly the sequence a bounded
	// re-prompt exists to keep open.
	login = func(_ context.Context, opts oauth.LoginOptions) (*oauth.LoginResult, error) {
		if opts.Rejected == nil {
			t.Fatal("add gave the login no Rejected callback, so an unreadable paste tells the user nothing")
		}
		opts.Announce("https://claude.ai/oauth/authorize?state=STATE")
		opts.Rejected("that does not look like a full code")
		return &oauth.LoginResult{Token: loginToken("acct-1", "a@example.com", "RT-1")}, nil
	}

	err, _, stderr := runCmd(t, newAddCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "that does not look like a full code") {
		t.Errorf("stderr = %q, want the reason the paste was not usable", stderr)
	}
	// Announce printed the first prompt; the rejection must re-issue it, or the
	// user is left at a bare cursor with nothing saying ccdad is still waiting.
	if got := strings.Count(stderr, "Paste code here: "); got < 2 {
		t.Errorf("stderr = %q, want the paste prompt re-issued after the rejection (saw it %d time(s))", stderr, got)
	}
}

// The callback's error parameter is parsed into a closed set precisely so a
// caller could log the OAuth error code with none of the browser's bytes
// reaching a message; that is the stated purpose of RejectionError in
// internal/oauth/callback.go. Nothing called LogDetail until `add` did.
//
// The unrecognized row is the one that carries this test: RejectionRefused and
// RejectionUnrecognized share UserMessage's default arm, so "Anthropic refused
// the login" was the whole of what a user saw for either, and the OAuth error
// code is the only thing that says which. The other three rows are cheaper —
// their
// canned messages already differ, as this test's own last assertion shows — and
// they are here so that a LogDetail wired for one arm only cannot pass.
func TestAddReportsTheRejectionDetailBehindTheCannedMessage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rejection oauth.AuthorizeRejection
		want      string
	}{
		{"declined", oauth.RejectionDeclined, "access_denied"},
		{"refused", oauth.RejectionRefused, "invalid_request"},
		{"upstream", oauth.RejectionUpstream, "server_error"},
		{"unrecognized", oauth.RejectionUnrecognized, "outside RFC 6749's set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			stubEnvironment(t, true, false)
			restore := login
			t.Cleanup(func() { login = restore })
			login = func(context.Context, oauth.LoginOptions) (*oauth.LoginResult, error) {
				return nil, &oauth.RejectionError{Rejection: tc.rejection}
			}

			err, _, stderr := runCmd(t, newAddCmd())
			if err == nil {
				t.Fatal("Execute() = nil, want the rejected login reported")
			}
			// An OAuth error in the callback is exit 1, the same
			// runtime-failure code as the login timeout beside it.
			if got := CodeFor(err); got != ExitFailure {
				t.Errorf("CodeFor = %d, want %d (a rejected callback is exit 1)", got, ExitFailure)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want the OAuth error code %q an operator can act on", stderr, tc.want)
			}
			// The canned user message is still what the command fails with; the
			// detail is a note beside it, not a replacement for it.
			if !strings.Contains(err.Error(), tc.rejection.UserMessage()) {
				t.Errorf("error = %q, want the canned message %q", err, tc.rejection.UserMessage())
			}
		})
	}
}

// The engine's quarantine fires on a dead refresh token, and re-authenticating
// is the only thing that fixes one. store.Add updates an existing uuid IN
// PLACE, so without an explicit lift the user logs in again, is told it worked,
// and the engine goes on refusing to use the account with nothing anywhere
// saying why.
func TestAddLiftsTheEngineQuarantine(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) (error, string, string)
	}{
		{"browser login", func(t *testing.T) (error, string, string) {
			stubEnvironment(t, true, false)
			stubLogin(t, loginToken("acct-1", "a@example.com", "RT-2"))
			stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, profileJSON("acct-1", "a@example.com"))
			})
			return runCmd(t, newAddCmd())
		}},
		{"setup token", func(t *testing.T) (error, string, string) {
			stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, profileJSON("acct-1", "a@example.com"))
			})
			return runCmd(t, newAddTokenCmd(), "sk-ant-oat01-TESTTOKEN")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			seedAccount(t, "acct-1", "a@example.com")
			quarantined := time.Now().Add(-time.Minute)
			if err := strategy.WithState(time.Second, func(st *strategy.State) error {
				st.Quarantine("acct-1", quarantined, time.Hour, "dead refresh token")
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			err, _, stderr := tc.run(t)
			if err != nil {
				t.Fatalf("Execute() = %v, want nil", err)
			}

			st, lerr := strategy.LoadState()
			if lerr != nil {
				t.Fatal(lerr)
			}
			if _, held := st.Quarantined("acct-1", time.Now()); held {
				t.Error("the account is still quarantined after being re-authenticated")
			}
			if !strings.Contains(stderr, "quarantine") {
				t.Errorf("stderr = %q, want the lift said out loud", stderr)
			}
		})
	}
}

// An account that was never quarantined must not be told one was lifted.
func TestAddSaysNothingWhenThereWasNoQuarantine(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})

	err, _, stderr := runCmd(t, newAddCmd())
	if err != nil {
		t.Fatalf("Execute() = %v, want nil", err)
	}
	if strings.Contains(stderr, "quarantine") {
		t.Errorf("stderr = %q, want no mention of a quarantine that never existed", stderr)
	}
}

// The account IS added by the time the lift runs, so a state file that cannot
// be written is a note on stderr and never a failed command: reporting a
// successful login as an error is worse than a quarantine that outlives it.
func TestAddSurvivesAnUnwritableEngineState(t *testing.T) {
	isolate(t)
	stubEnvironment(t, true, false)
	stubLogin(t, loginToken("acct-1", "a@example.com", "RT-1"))
	stubProfile(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, profileJSON("acct-1", "a@example.com"))
	})
	// A directory where the state document belongs cannot be replaced by a
	// file, so the write fails while everything before it succeeds.
	if err := os.MkdirAll(filepath.Join(mustPath(ccpath.StoreHome()), "strategy.json", "held"), 0o700); err != nil {
		t.Fatal(err)
	}

	err, _, stderr := runCmd(t, newAddCmd())
	if err != nil {
		t.Fatalf("Execute() = %v, want the add to succeed anyway", err)
	}
	if !strings.Contains(stderr, "auto-switch state could not be updated") {
		t.Errorf("stderr = %q, want the failure named", stderr)
	}
	s, serr := store.Open()
	if serr != nil {
		t.Fatal(serr)
	}
	if len(s.Accounts()) != 1 {
		t.Error("the account was not stored")
	}
}
