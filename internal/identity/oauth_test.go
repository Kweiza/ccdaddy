package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/oauth"
)

// login is a credential Claude Code would authenticate with.
func login() Login { return UsableLogin }

// consoleLogin is the shape a Console sign-in leaves: a perfectly well-formed
// claudeAiOauth record with an access token and no user:inference scope.
func consoleLogin() Login {
	return Login{HasAccessToken: true, Scopes: []string{"org:create_api_key", "user:profile"}}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// THE ORDER THIS TREE HAD BACKWARDS. Three places assumed
// CLAUDE_CODE_OAUTH_TOKEN was the top of the OAuth axis; BT() reads
// ANTHROPIC_AUTH_TOKEN first. On a machine with both set, a report naming the
// second sends someone to unset the variable that is not deciding anything.
func TestTheAuthTokenOutranksTheOAuthToken(t *testing.T) {
	env := OAuthEnvironment{AuthToken: true, TokenEnv: "sk-ant-oat-1"}

	got, ok := env.Resolve(login())
	if !ok {
		t.Fatal("Resolve declined on a machine with two plain variables set")
	}
	if got != OAuthAuthTokenEnv {
		t.Errorf("Resolve = %v, want OAuthAuthTokenEnv — BT() reads ANTHROPIC_AUTH_TOKEN before "+
			"CLAUDE_CODE_OAUTH_TOKEN", got)
	}
}

// The one place on this axis where SETTING a credential variable makes less
// difference rather than more: inside a session host Claude Code skips
// ANTHROPIC_AUTH_TOKEN entirely, and the login it was assumed to displace wins.
func TestAHostedSessionSkipsTheAuthToken(t *testing.T) {
	hosted := OAuthEnvironment{AuthToken: true, Host: HostContext{Remote: true}}
	if got, _ := hosted.Resolve(login()); got != OAuthLogin {
		t.Errorf("Resolve = %v, want OAuthLogin — XMn() suppresses the auth token on a host", got)
	}

	// CLAUDE_CODE_HOST_AUTH_ENV_VAR puts it back...
	withVar := hosted
	withVar.Host.HostAuthEnvVar = true
	if got, _ := withVar.Resolve(login()); got != OAuthAuthTokenEnv {
		t.Errorf("Resolve = %v, want OAuthAuthTokenEnv with CLAUDE_CODE_HOST_AUTH_ENV_VAR set", got)
	}

	// ...and so does the one entrypoint carved out of the suppression.
	thirdParty := OAuthEnvironment{AuthToken: true, Host: HostContext{Entrypoint: "claude-desktop-3p"}}
	if got, _ := thirdParty.Resolve(login()); got != OAuthAuthTokenEnv {
		t.Errorf("Resolve = %v, want OAuthAuthTokenEnv — claude-desktop-3p is carved out of XMn()", got)
	}
}

// A hosted entrypoint is a host even with no CLAUDE_CODE_REMOTE. The set is
// three members and BF() is where it comes from.
func TestTheHostedEntrypointsAreASetAndNotAPrefix(t *testing.T) {
	for _, name := range []string{"claude-desktop", "claude-desktop-3p", "local-agent"} {
		if !(HostContext{Entrypoint: name}).IsHosted() {
			t.Errorf("%q is not treated as a session host", name)
		}
	}
	for _, name := range []string{"", "cli", "sdk-ts", "claude-desktop-4p", "local"} {
		if (HostContext{Entrypoint: name}).IsHosted() {
			t.Errorf("%q was treated as a session host", name)
		}
	}
}

// The descriptor outranks the login, which is the whole reason the queue item
// was filed: without it a switch writes a login nothing reads.
func TestTheOAuthDescriptorOutranksTheLogin(t *testing.T) {
	env := OAuthEnvironment{TokenDescriptor: true}
	if got, _ := env.Resolve(login()); got != OAuthTokenDescriptor {
		t.Errorf("Resolve = %v, want OAuthTokenDescriptor", got)
	}
}

// The half the queue item got wrong: this source has NO environment variable.
// BT() reaches it when the descriptor variable is unset and the well-known file
// has bytes — so a model built on a variable named CCR_OAUTH_TOKEN_FILE would
// never fire at all.
func TestTheHostTokenFileIsASourceWithNoVariableBehindIt(t *testing.T) {
	env := OAuthEnvironment{HostTokenFile: true}
	got, ok := env.Resolve(login())
	if !ok || got != OAuthHostTokenFile {
		t.Fatalf("Resolve = %v (ok=%v), want OAuthHostTokenFile", got, ok)
	}
	if !strings.Contains(got.String(), HostOAuthTokenFile) {
		t.Errorf("String() does not name the path, which is the only thing a user can act on:\n%s", got.String())
	}
	if strings.Contains(got.String(), "CCR_OAUTH_TOKEN_FILE") {
		t.Errorf("String() printed the source NAME, which reads as a variable to unset:\n%s", got.String())
	}
	if got.SourceName() != "CCR_OAUTH_TOKEN_FILE" {
		t.Errorf("SourceName = %q, want the literal Claude Code prints", got.SourceName())
	}
	if strings.Contains(got.Remedy(), "Unset") {
		t.Errorf("Remedy tells the user to unset something; Claude Code's own remedy points at the host:\n%s",
			got.Remedy())
	}
}

// The descriptor variable NAMES the source even when the file is what answered.
// BT() tests the variable, not the outcome, so a machine with both reports the
// descriptor — and ccdad must report what Claude Code would say, not what it
// would prefer to say.
func TestTheDescriptorNamesTheSourceEvenWhenTheFileIsAlsoThere(t *testing.T) {
	env := OAuthEnvironment{TokenDescriptor: true, HostTokenFile: true}
	if got, _ := env.Resolve(login()); got != OAuthTokenDescriptor {
		t.Errorf("Resolve = %v, want OAuthTokenDescriptor", got)
	}
}

// THE RULE FOUR PLACES IN THIS TREE APPROXIMATED WITH "IS THERE A CLAUDEAIOAUTH
// OBJECT". A Console-flow login carries org:create_api_key and user:profile and
// no user:inference, and BT() falls straight past it: Claude Code has no OAuth
// credential at all while a well-formed login sits in the file.
func TestALoginWithoutInferenceScopeIsNotACredential(t *testing.T) {
	if got, _ := (OAuthEnvironment{}).Resolve(consoleLogin()); got != OAuthNone {
		t.Errorf("Resolve = %v, want OAuthNone for a login with no user:inference scope", got)
	}
	if got, _ := (OAuthEnvironment{}).Resolve(login()); got != OAuthLogin {
		t.Errorf("Resolve = %v, want OAuthLogin for a login that carries it", got)
	}
	// And an inference scope with no token is not one either.
	scopeOnly := Login{Scopes: []string{oauth.ScopeInference}}
	if got, _ := (OAuthEnvironment{}).Resolve(scopeOnly); got != OAuthNone {
		t.Errorf("Resolve = %v, want OAuthNone for scopes with no access token", got)
	}
}

// The bg-auth snapshot is the one state ccdad DECLINES on. Claude Code consumes
// that file before it looks at anything else and the deciding fact is inside
// it, so any source named here would be a guess — and the file is gone by the
// time anyone could check.
func TestABgSnapshotMakesTheAnswerADecline(t *testing.T) {
	env := OAuthEnvironment{BgSnapshot: true}
	got, ok := env.Resolve(login())
	if ok {
		t.Fatalf("Resolve answered %v instead of declining", got)
	}
	if got != OAuthNone {
		t.Errorf("a declined resolve returned %v; the zero value is the only honest source", got)
	}
	// It sits BELOW the two plain variables, because BT() reaches the snapshot
	// inside mbe() and mbe() is branch 4.
	withToken := OAuthEnvironment{BgSnapshot: true, TokenEnv: "sk-ant-oat-1"}
	if _, ok := withToken.Resolve(login()); !ok {
		t.Error("a snapshot made the answer a decline on a machine where CLAUDE_CODE_OAUTH_TOKEN decides it")
	}
}

// Bare mode reads the helper and NOTHING else — not the login, not the token
// variables. It is the one branch that can turn a working machine into "none".
func TestBareModeReadsOnlyTheHelper(t *testing.T) {
	bare := OAuthEnvironment{Bare: true, TokenEnv: "sk-ant-oat-1", AuthToken: true}
	if got, _ := bare.Resolve(login()); got != OAuthNone {
		t.Errorf("Resolve = %v, want OAuthNone — bare mode reads neither variable nor the login", got)
	}
	withHelper := OAuthEnvironment{Bare: true, Helper: true}
	if got, _ := withHelper.Resolve(login()); got != OAuthHelper {
		t.Errorf("Resolve = %v, want OAuthHelper", got)
	}
}

// The helper is skipped on a session host, which is the second inversion on
// this axis and the mirror of the auth-token one.
func TestAHostedSessionSkipsTheHelper(t *testing.T) {
	hosted := OAuthEnvironment{Helper: true, Host: HostContext{Remote: true}}
	if got, _ := hosted.Resolve(login()); got != OAuthLogin {
		t.Errorf("Resolve = %v, want OAuthLogin — the helper branch carries a !Wer() gate", got)
	}
	local := OAuthEnvironment{Helper: true}
	if got, _ := local.Resolve(login()); got != OAuthHelper {
		t.Errorf("Resolve = %v, want OAuthHelper off a host", got)
	}
}

// THE ONE PLACE THE TWO BRANCHES INTERACT. An IMPLICIT profile whose auth type
// is user_oauth gives way to a real login; every other profile shape beats it.
// Getting this backwards would either hide a profile that is deciding the
// session or invent one that is not.
func TestAnImplicitUserOAuthProfileLosesToALogin(t *testing.T) {
	for _, c := range []struct {
		name    string
		profile AntProfile
		want    OAuthSource
	}{
		{"implicit user_oauth loses", AntProfile{"profile-implicit", "user_oauth"}, OAuthLogin},
		{"implicit oidc_federation wins", AntProfile{"profile-implicit", "oidc_federation"}, OAuthProfile},
		{"explicit user_oauth wins", AntProfile{"profile-explicit", "user_oauth"}, OAuthProfile},
		{"the federation variables win", AntProfile{"env-quad", "oidc_federation"}, OAuthProfile},
		{"no profile", AntProfile{}, OAuthLogin},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := OAuthEnvironment{Profile: c.profile}
			if got, _ := env.Resolve(login()); got != c.want {
				t.Errorf("Resolve = %v, want %v", got, c.want)
			}
		})
	}

	// The carve-out is about a USABLE login. With no login to lose to, even the
	// implicit user_oauth profile is what the session authenticates with.
	implicit := OAuthEnvironment{Profile: AntProfile{"profile-implicit", "user_oauth"}}
	if got, _ := implicit.Resolve(consoleLogin()); got != OAuthProfile {
		t.Errorf("Resolve = %v, want OAuthProfile — there is no usable login for it to lose to", got)
	}
}

// A session host disqualifies the profile branch outright.
func TestAHostedSessionDisqualifiesTheProfile(t *testing.T) {
	env := OAuthEnvironment{
		Profile: AntProfile{"profile-explicit", "oidc_federation"},
		Host:    HostContext{Remote: true},
	}
	if got, _ := env.Resolve(login()); got != OAuthLogin {
		t.Errorf("Resolve = %v, want OAuthLogin — IP() disqualifies itself on a host", got)
	}
}

// CLAUDE_CODE_SIMPLE=0 is NOT bare mode. Claude Code parses it with a
// truthiness test that accepts four spellings; treating any non-empty string as
// true would put ccdad in bare mode — where the answer is "none" — while Claude
// Code is authenticating with the login.
func TestTruthyEnvIsClaudeCodesAndNotJustNonEmpty(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", " yes ", "on", "On"} {
		if !TruthyEnv(v) {
			t.Errorf("TruthyEnv(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "2", "y", "please"} {
		if TruthyEnv(v) {
			t.Errorf("TruthyEnv(%q) = true, want false", v)
		}
	}
}

// Every source but the login displaces it, and the enum's order is the
// resolver's own. A gap here means a source that resolves ahead of the login
// and is reported as harmless.
func TestEverySourceButTheLoginOutranksIt(t *testing.T) {
	envs := map[OAuthSource]OAuthEnvironment{
		OAuthAuthTokenEnv:    {AuthToken: true},
		OAuthTokenEnv:        {TokenEnv: "sk-ant-oat-1"},
		OAuthTokenDescriptor: {TokenDescriptor: true},
		OAuthHostTokenFile:   {HostTokenFile: true},
		OAuthHelper:          {Helper: true},
		OAuthProfile:         {Profile: AntProfile{"profile-explicit", "user_oauth"}},
	}
	for want, env := range envs {
		got, ok := env.Resolve(login())
		if !ok || got != want {
			t.Errorf("Resolve = %v (ok=%v), want %v — it must outrank a working login", got, ok, want)
		}
		if got.Remedy() == "" {
			t.Errorf("%v has no remedy, so doctor would name a source and offer nothing to do about it", got)
		}
		if got.String() == "" || got.SourceName() == "" {
			t.Errorf("%v has an empty name", got)
		}
	}
}

// A source with no name is a report that says nothing, and OAuthNone's remedy
// must stay empty so callers can test for "nothing to say".
func TestTheEmptySourceHasNothingToSay(t *testing.T) {
	if OAuthNone.Remedy() != "" {
		t.Errorf("OAuthNone.Remedy() = %q, want empty", OAuthNone.Remedy())
	}
	if OAuthNone.SourceName() != "none" {
		t.Errorf("OAuthNone.SourceName() = %q, want %q", OAuthNone.SourceName(), "none")
	}
}

// ---- the probe ----

// sandbox points the two compiled-in paths at a directory a test owns, and
// clears every variable the probe reads. Without it the answer depends on
// whether the machine running the suite happens to have a /home/claude.
func sandbox(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	savedToken, savedKey := HostOAuthTokenFile, HostAPIKeyFile
	t.Cleanup(func() { HostOAuthTokenFile, HostAPIKeyFile = savedToken, savedKey })
	HostOAuthTokenFile = filepath.Join(root, "remote", ".oauth_token")
	HostAPIKeyFile = filepath.Join(root, "remote", ".api_key")

	for _, v := range []string{
		"CLAUDE_CODE_SIMPLE", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_BG_AUTH_SNAPSHOT_PATH",
		"CLAUDE_CODE_REMOTE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_HOST_AUTH_ENV_VAR",
		"ANTHROPIC_PROFILE", "ANTHROPIC_CONFIG_DIR", "XDG_CONFIG_HOME",
		"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID",
	} {
		t.Setenv(v, "")
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	return root
}

// A zero-byte well-known file is not a credential. Claude Code reads and trims,
// so an empty file gives it nothing — and the installer-shaped hazard of
// reporting an in-progress write as a live token is the same one this tree just
// fixed in internal/ccver.
func TestAnEmptyHostTokenFileIsNotASource(t *testing.T) {
	sandbox(t)
	write(t, HostOAuthTokenFile, "")
	if ProbeOAuthEnvironment().HostTokenFile {
		t.Error("an empty host token file was reported as a credential")
	}
	write(t, HostOAuthTokenFile, "sk-ant-oat-x")
	if !ProbeOAuthEnvironment().HostTokenFile {
		t.Error("a host token file with bytes in it was not reported")
	}
}

// The API-key half of the same rule, and the gap that was already shipped:
// ccdad modelled the variable and not the file.
func TestTheHostAPIKeyFileIsSeenWithNoVariableSet(t *testing.T) {
	sandbox(t)
	if HostAPIKeyFilePresent() {
		t.Fatal("a clean sandbox reported a host-injected API key")
	}
	write(t, HostAPIKeyFile, "sk-ant-api-x")
	if !HostAPIKeyFilePresent() {
		t.Error("the well-known API key file was not seen, so a key that displaces the login is invisible")
	}
}

// The probe reads and stats. It must not create the directory Claude Code's own
// writer would.
func TestTheProbeCreatesNothing(t *testing.T) {
	root := sandbox(t)
	ProbeOAuthEnvironment()
	HostAPIKeyFilePresent()
	for _, path := range []string{HostOAuthTokenFile, HostAPIKeyFile, filepath.Join(root, "remote")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s exists after a probe (err=%v)", path, err)
		}
	}
}

// The Anthropic CLI walk, end to end through the filesystem, because the
// precedence rules are what decide whether a profile is reported at all.
func TestTheAntProfileWalk(t *testing.T) {
	t.Run("an implicit default profile", func(t *testing.T) {
		root := sandbox(t)
		dir := filepath.Join(root, "cfg")
		t.Setenv("ANTHROPIC_CONFIG_DIR", dir)
		write(t, filepath.Join(dir, "configs", "default.json"), `{"authentication":{"type":"oidc_federation"}}`)

		got := ProbeOAuthEnvironment().Profile
		if got != (AntProfile{"profile-implicit", "oidc_federation"}) {
			t.Fatalf("Profile = %+v", got)
		}
	})

	t.Run("active_config names the profile", func(t *testing.T) {
		root := sandbox(t)
		dir := filepath.Join(root, "cfg")
		t.Setenv("ANTHROPIC_CONFIG_DIR", dir)
		write(t, filepath.Join(dir, "active_config"), "work\n")
		write(t, filepath.Join(dir, "configs", "work.json"), `{"authentication":{"type":"oidc_federation"}}`)

		if got := ProbeOAuthEnvironment().Profile; !got.Configured() {
			t.Fatalf("Profile = %+v, want the profile active_config names", got)
		}
	})

	t.Run("a user_oauth profile needs its credentials file", func(t *testing.T) {
		root := sandbox(t)
		dir := filepath.Join(root, "cfg")
		t.Setenv("ANTHROPIC_CONFIG_DIR", dir)
		write(t, filepath.Join(dir, "configs", "default.json"), `{"authentication":{"type":"user_oauth"}}`)

		if got := ProbeOAuthEnvironment().Profile; got.Configured() {
			t.Fatalf("Profile = %+v — a user_oauth profile with no credentials file is not one", got)
		}
		write(t, filepath.Join(dir, "credentials", "default.json"), `{"access_token":"x"}`)
		if got := ProbeOAuthEnvironment().Profile; !got.Configured() {
			t.Fatalf("Profile = %+v, want it once the credentials file is there", got)
		}
	})

	t.Run("credentials_path moves the file the profile needs", func(t *testing.T) {
		root := sandbox(t)
		dir := filepath.Join(root, "cfg")
		elsewhere := filepath.Join(root, "elsewhere", "creds.json")
		t.Setenv("ANTHROPIC_CONFIG_DIR", dir)
		write(t, filepath.Join(dir, "configs", "default.json"),
			`{"authentication":{"type":"user_oauth","credentials_path":`+quote(elsewhere)+`}}`)
		write(t, filepath.Join(dir, "credentials", "default.json"), `{"access_token":"x"}`)

		if got := ProbeOAuthEnvironment().Profile; got.Configured() {
			t.Fatalf("Profile = %+v — the default path is not the one this profile named", got)
		}
		write(t, elsewhere, `{"access_token":"x"}`)
		if got := ProbeOAuthEnvironment().Profile; !got.Configured() {
			t.Fatal("the profile's own credentials_path was not honoured")
		}
	})

	t.Run("an explicit profile that does not resolve stops rather than falling through", func(t *testing.T) {
		root := sandbox(t)
		dir := filepath.Join(root, "cfg")
		t.Setenv("ANTHROPIC_CONFIG_DIR", dir)
		t.Setenv("ANTHROPIC_PROFILE", "missing")
		t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "rule-1")
		t.Setenv("ANTHROPIC_ORGANIZATION_ID", "org-1")

		if got := ProbeOAuthEnvironment().Profile; got.Configured() {
			t.Fatalf("Profile = %+v — a named profile that does not resolve is the END of the walk, "+
				"not a fall-through to the federation variables", got)
		}
	})

	t.Run("the federation pair is both or neither", func(t *testing.T) {
		sandbox(t)
		t.Setenv("ANTHROPIC_FEDERATION_RULE_ID", "rule-1")
		if got := ProbeOAuthEnvironment().Profile; got.Configured() {
			t.Fatalf("Profile = %+v with only one of the two variables set", got)
		}
		t.Setenv("ANTHROPIC_ORGANIZATION_ID", "org-1")
		if got := ProbeOAuthEnvironment().Profile; got != (AntProfile{"env-quad", "oidc_federation"}) {
			t.Fatalf("Profile = %+v, want env-quad", got)
		}
	})

	t.Run("an unrecognised auth type is no profile", func(t *testing.T) {
		root := sandbox(t)
		dir := filepath.Join(root, "cfg")
		t.Setenv("ANTHROPIC_CONFIG_DIR", dir)
		write(t, filepath.Join(dir, "configs", "default.json"), `{"authentication":{"type":"something_else"}}`)

		if got := ProbeOAuthEnvironment().Profile; got.Configured() {
			t.Fatalf("Profile = %+v for an auth type Claude Code does not recognise", got)
		}
	})

	// THE ONLY BRANCH A DEFAULT MACHINE TAKES, and it had no test: every subtest
	// above sets ANTHROPIC_CONFIG_DIR, so the walk never went through $HOME.
	t.Run("the default is $HOME/.config/anthropic", func(t *testing.T) {
		root := sandbox(t)
		home := filepath.Join(root, "home")
		write(t, filepath.Join(home, ".config", "anthropic", "configs", "default.json"),
			`{"authentication":{"type":"oidc_federation"}}`)

		got := ProbeOAuthEnvironment().Profile
		if got != (AntProfile{"profile-implicit", "oidc_federation"}) {
			t.Fatalf("Profile = %+v — a profile in the default location was not found", got)
		}
	})

	// The middle branch, likewise unexercised, and it must WIN over $HOME
	// rather than merely be reachable.
	t.Run("XDG_CONFIG_HOME wins over HOME", func(t *testing.T) {
		root := sandbox(t)
		xdg := filepath.Join(root, "xdg")
		t.Setenv("XDG_CONFIG_HOME", xdg)
		write(t, filepath.Join(xdg, "anthropic", "configs", "default.json"),
			`{"authentication":{"type":"oidc_federation"}}`)
		// A DIFFERENT profile under HOME, which must not be the one found.
		write(t, filepath.Join(root, "home", ".config", "anthropic", "configs", "default.json"),
			`{"authentication":{"type":"user_oauth"}}`)

		if got := ProbeOAuthEnvironment().Profile; got.AuthType != "oidc_federation" {
			t.Fatalf("Profile = %+v, want the one under XDG_CONFIG_HOME", got)
		}
	})

	// With a real profile sitting under HOME, clearing HOME must lose it —
	// which is what makes this test able to fail for the property it names.
	// Asserting the negative on an empty sandbox passed either way.
	t.Run("no HOME and no override is no config directory", func(t *testing.T) {
		root := sandbox(t)
		write(t, filepath.Join(root, "home", ".config", "anthropic", "configs", "default.json"),
			`{"authentication":{"type":"oidc_federation"}}`)
		if got := ProbeOAuthEnvironment().Profile; !got.Configured() {
			t.Fatal("the fixture does not describe a machine with a profile, so clearing HOME proves nothing")
		}
		t.Setenv("HOME", "")
		if got := ProbeOAuthEnvironment().Profile; got.Configured() {
			t.Fatalf("Profile = %+v — Kra() reads $HOME with no fallback, so there is no directory to walk", got)
		}
	})

	// Claude Code keeps a user_oauth profile whose credentials file is ZERO
	// BYTES: its reader returns the contents and only a read ERROR is a miss.
	// Requiring bytes would drop a profile Claude Code is using and report the
	// login as the winner — the one direction this package must not err in.
	t.Run("a zero-byte credentials file still counts", func(t *testing.T) {
		root := sandbox(t)
		dir := filepath.Join(root, "cfg")
		t.Setenv("ANTHROPIC_CONFIG_DIR", dir)
		write(t, filepath.Join(dir, "configs", "default.json"), `{"authentication":{"type":"user_oauth"}}`)
		write(t, filepath.Join(dir, "credentials", "default.json"), "")

		if got := ProbeOAuthEnvironment().Profile; !got.Configured() {
			t.Fatalf("Profile = %+v — an empty credentials file is not a missing one to Claude Code", got)
		}
	})
}

// CLAUDE_CODE_REMOTE IS A BOOL IN CLAUDE CODE, not a presence flag, and reading
// it as presence errs in this axis's one unsafe direction: believing the session
// is hosted SUPPRESSES ANTHROPIC_AUTH_TOKEN and the helper and disqualifies the
// profile, so ccdad would report the login as the winner while one of them
// decides the session. Claude Code's own environment writer stores a false
// boolean as the string "0", so the value is not hypothetical.
func TestCLAUDECODEREMOTEZeroIsNotAHost(t *testing.T) {
	for _, c := range []struct {
		value  string
		hosted bool
	}{
		{"1", true}, {"true", true}, {"on", true},
		{"0", false}, {"false", false}, {"", false}, {"no", false},
	} {
		t.Run("CLAUDE_CODE_REMOTE="+c.value, func(t *testing.T) {
			sandbox(t)
			t.Setenv("CLAUDE_CODE_REMOTE", c.value)
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-live")

			env := ProbeOAuthEnvironment()
			if env.Host.IsHosted() != c.hosted {
				t.Fatalf("IsHosted() = %v for %q, want %v", env.Host.IsHosted(), c.value, c.hosted)
			}
			got, _ := env.Resolve(login())
			want := OAuthAuthTokenEnv
			if c.hosted {
				want = OAuthLogin
			}
			if got != want {
				t.Errorf("Resolve = %v, want %v — a wrong host verdict hides the token that is deciding "+
					"the session", got, want)
			}
		})
	}
}

// The typed accessor Claude Code reads its environment through TRIMS every
// string variable and turns a blank into nothing, so a variable set to spaces
// is set to nothing — and for the two HOST variables reading it raw inverts a
// gate rather than merely over-reporting.
func TestABlankVariableIsNotSet(t *testing.T) {
	sandbox(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "   ")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "  ")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "  claude-desktop  ")

	env := ProbeOAuthEnvironment()
	if env.AuthToken {
		t.Error("a whitespace-only ANTHROPIC_AUTH_TOKEN was read as set")
	}
	if env.TokenEnv != "" {
		t.Errorf("TokenEnv = %q for a whitespace-only value", env.TokenEnv)
	}
	if !env.Host.IsHosted() {
		t.Error("a padded CLAUDE_CODE_ENTRYPOINT was not recognised — the membership test runs on the " +
			"trimmed value, so reading it raw UNDER-reports a host")
	}
}

// An unreadable well-known path is ABSENT, not present. Claude Code's reader is
// a read in a try/catch that returns null for the whole error class, so a
// /home/claude it cannot read gives it no credential either — and guessing the
// other way would stop the daemon switching forever on a machine with an
// unrelated local user of that name.
func TestAnUnreadableHostTokenPathIsNotACredential(t *testing.T) {
	root := sandbox(t)
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(locked, ".oauth_token"), "sk-ant-oat-x")
	HostOAuthTokenFile = filepath.Join(locked, ".oauth_token")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("this host will not make a directory unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if _, err := os.Stat(HostOAuthTokenFile); err == nil {
		t.Skip("this host stats through a mode-000 directory (running as root?)")
	}

	if ProbeOAuthEnvironment().HostTokenFile {
		t.Error("a path ccdad cannot stat was reported as a live credential")
	}
}

func quote(s string) string {
	out := make([]rune, 0, len(s)+2)
	out = append(out, '"')
	for _, r := range s {
		if r == '\\' || r == '"' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(append(out, '"'))
}
