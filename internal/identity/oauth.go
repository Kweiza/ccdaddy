package identity

import (
	"os"
	"strings"

	"github.com/Kweiza/ccdaddy/internal/oauth"
)

// This file models how Claude Code decides which OAuth-shaped credential a
// session will authenticate with. It is a SECOND resolver, independent of the
// API-key one in apikey.go, and like that one it is a MODEL of another
// program's behaviour: every rule cites the function it came from in the
// 2.1.238 bundle. Guessing here is worse than declining to answer.
//
// CITATIONS CARRY A BODY FRAGMENT, NOT ONLY A NAME. Minified names are
// re-generated every release -- the config root is An() in 2.1.238 and Hn() in
// 2.1.241 -- so a name on its own is dead the moment Claude Code ships again.
// Each citation below gives a string that can be grepped out of any build.
//
// The resolver is BT() (grep: `return{source:"CCR_OAUTH_TOKEN_FILE"`), in
// priority order, with THE STORED LOGIN LAST:
//
//	1. bare mode                -> the apiKeyHelper if configured, else none
//	2. ANTHROPIC_AUTH_TOKEN     -- unless XMn() suppresses it on a session host
//	3. CLAUDE_CODE_OAUTH_TOKEN
//	4. mbe()                    -> the descriptor variable if set, otherwise the
//	                               host's well-known token file
//	5. the apiKeyHelper          -- unless the session is hosted
//	6. an Anthropic CLI profile  (IP(), ~/.config/anthropic)
//	7. the claude.ai login       -- ONLY when its scopes carry user:inference
//	                               AND it has an access token
//	8. none
//
// TWO CONSEQUENCES THAT CONTRADICT WHAT THIS TREE USED TO ASSUME. The first is
// that ANTHROPIC_AUTH_TOKEN outranks CLAUDE_CODE_OAUTH_TOKEN rather than the
// other way round. The second is branch 7: a login object in the credentials
// file is NOT a credential unless its scopes carry user:inference, so a Console
// login -- which stores org:create_api_key and user:profile and no inference
// scope -- leaves Claude Code with no OAuth credential at all while a
// perfectly well-formed claudeAiOauth record sits in the file.
//
// "CCR_OAUTH_TOKEN_FILE" IS NOT AN ENVIRONMENT VARIABLE. Every occurrence in
// the bundle is the source NAME compared against BT().source, or a case in the
// remedy table; there is no process.env read of it anywhere. Its subject is a
// path compiled into Claude Code as a literal. Telling a user to unset it
// would send them after a variable that does not exist, which is why
// SourceName and String are two different methods.

// OAuthSource names where Claude Code would take a session's OAuth-shaped
// credential from. The order is BT()'s own.
type OAuthSource uint8

const (
	// OAuthNone means no OAuth credential resolves. Claude Code then has
	// whatever the API-key axis produced, which may be nothing.
	OAuthNone OAuthSource = iota
	// OAuthAuthTokenEnv is ANTHROPIC_AUTH_TOKEN. It outranks
	// CLAUDE_CODE_OAUTH_TOKEN.
	OAuthAuthTokenEnv
	// OAuthTokenEnv is CLAUDE_CODE_OAUTH_TOKEN -- what `claude setup-token`
	// prints and what `ccdad run` exports for a setup-token account.
	OAuthTokenEnv
	// OAuthTokenDescriptor is CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR: a
	// session host passing a token through an open file descriptor.
	OAuthTokenDescriptor
	// OAuthHostTokenFile is the well-known path Claude Code reads whenever the
	// descriptor variable is unset. Claude Code calls this source
	// CCR_OAUTH_TOKEN_FILE. The path is a compiled-in literal: it is NOT
	// derived from the home directory and no CLAUDE_CONFIG_DIR moves it.
	OAuthHostTokenFile
	// OAuthHelper is the apiKeyHelper setting reached on THIS axis. The same
	// setting also wins on the api-key axis; which name it is reported under
	// does not change what a user has to do about it.
	OAuthHelper
	// OAuthProfile is an Anthropic CLI profile under ~/.config/anthropic. It is
	// NOT identity.Profile, which is this package's name for the
	// /api/oauth/profile HTTP response and has nothing to do with it.
	OAuthProfile
	// OAuthLogin is the claudeAiOauth record in the credentials file -- the
	// login ccdad manages, and the only source on this axis a switch changes.
	OAuthLogin
)

// String names the source inside a sentence, for a human. It reads correctly
// after the word "from", which is the shape doctor's api-key row already uses.
//
// It deliberately never returns "CCR_OAUTH_TOKEN_FILE": see SourceName.
func (s OAuthSource) String() string {
	switch s {
	case OAuthAuthTokenEnv:
		return "ANTHROPIC_AUTH_TOKEN"
	case OAuthTokenEnv:
		return "CLAUDE_CODE_OAUTH_TOKEN"
	case OAuthTokenDescriptor:
		return "the file descriptor CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR names"
	case OAuthHostTokenFile:
		return HostOAuthTokenFile + ", the file a session host injects a token into"
	case OAuthHelper:
		return "the apiKeyHelper command"
	case OAuthProfile:
		return "the Anthropic CLI profile under ~/.config/anthropic"
	case OAuthLogin:
		return "the login in the credentials file"
	default:
		return "nothing"
	}
}

// SourceName is the literal Claude Code compares against and prints in its own
// diagnostics. It exists for two callers: Remedy's dispatch, and a user who has
// seen one of these strings come out of `claude` and needs ccdad to use the
// same word. It is never the whole of a sentence ccdad writes.
func (s OAuthSource) SourceName() string {
	switch s {
	case OAuthAuthTokenEnv:
		return "ANTHROPIC_AUTH_TOKEN"
	case OAuthTokenEnv:
		return "CLAUDE_CODE_OAUTH_TOKEN"
	case OAuthTokenDescriptor:
		return "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR"
	case OAuthHostTokenFile:
		return "CCR_OAUTH_TOKEN_FILE"
	case OAuthHelper:
		return "apiKeyHelper"
	case OAuthProfile:
		return "profile"
	case OAuthLogin:
		return "claude.ai"
	default:
		return "none"
	}
}

// Remedy is Claude Code's own remedy for this source, reproduced from its
// per-source table (grep: `injected by the CCR host`) including that table's
// default arm.
//
// Reproduced rather than invented, because Claude Code prints these sentences
// to the same user: two programs prescribing two different fixes for one state
// is worse than either fix on its own. Note how little of the table is "unset
// the variable" -- which is what the single template ccdad used to print said
// for every source.
func (s OAuthSource) Remedy() string {
	switch s {
	case OAuthNone:
		return ""
	case OAuthLogin:
		return "claude /logout to sign out of claude.ai."
	case OAuthProfile:
		return "Run `ant auth logout`, or remove the active profile under ~/.config/anthropic/configs/."
	case OAuthHelper:
		return "Unset the apiKeyHelper setting."
	case OAuthHostTokenFile:
		return "This token is injected by the CCR host; check the host session."
	default:
		return "Unset the " + s.SourceName() + " environment variable."
	}
}

// Login is the two fields BT()'s last branch tests, and nothing else.
//
// The access token is a PRESENCE flag: BT() only asks whether one exists, and
// holding a live bearer token to answer that is what this package's credentials
// rule forbids.
type Login struct {
	HasAccessToken bool
	Scopes         []string
}

// SignsInForInference is fPn (grep: `IZ(e?.scopes)&&!!e?.accessToken`), whose
// IZ is `Array.isArray(e)&&e.includes(qte)` with qte the literal
// "user:inference".
//
// It is the rule this tree used to approximate with "is there a claudeAiOauth
// object". A Console-flow login stores org:create_api_key and user:profile and
// no user:inference, so BT() falls straight past it -- Claude Code has no OAuth
// credential while a login object sits in the file.
func (l Login) SignsInForInference() bool {
	if !l.HasAccessToken {
		return false
	}
	for _, s := range l.Scopes {
		if s == oauth.ScopeInference {
			return true
		}
	}
	return false
}

// UsableLogin is what a caller passes when the question is "would anything
// outrank a WORKING login" rather than "what does this machine's login resolve
// to". A switch about to WRITE a login asks the first: whether that write
// survives cannot depend on the login already in the file.
var UsableLogin = Login{HasAccessToken: true, Scopes: []string{oauth.ScopeInference}}

// HostContext is the three environment variables that decide whether Claude
// Code believes it is running inside a session host.
//
// Filled from ccdad's OWN environment, and that is an assumption stated rather
// than hidden: a launcher that injects CLAUDE_CODE_ENTRYPOINT into the child it
// starts is invisible from here. When ccdad's environment carries none of
// these, the child it would start inherits none either, which is the case on
// every machine a user runs `ccdad which` on by hand.
type HostContext struct {
	// Remote is CLAUDE_CODE_REMOTE parsed by TruthyEnv, NOT tested for presence.
	// Claude Code reads its environment through a typed accessor and declares
	// this variable bool there, so `CLAUDE_CODE_REMOTE=0` is not a host.
	Remote bool
	// Entrypoint is CLAUDE_CODE_ENTRYPOINT, TRIMMED: the accessor's string
	// schema trims and turns a blank into nothing, and the membership test runs
	// on that value.
	Entrypoint string
	// HostAuthEnvVar is CLAUDE_CODE_HOST_AUTH_ENV_VAR, non-empty.
	HostAuthEnvVar bool
}

// hostEntrypoints is the set BF() tests (grep: `claude-desktop-3p`) -- the
// entrypoints Claude Code treats as a session host even without
// CLAUDE_CODE_REMOTE.
var hostEntrypoints = []string{"claude-desktop", "claude-desktop-3p", "local-agent"}

// IsHosted is Wer(): `V.CLAUDE_CODE_REMOTE||BF()`.
func (h HostContext) IsHosted() bool {
	if h.Remote {
		return true
	}
	for _, e := range hostEntrypoints {
		if h.Entrypoint == e {
			return true
		}
	}
	return false
}

// SuppressesAuthToken is XMn() (grep: `CLAUDE_CODE_HOST_AUTH_ENV_VAR`):
// hosted, no CLAUDE_CODE_HOST_AUTH_ENV_VAR, and the entrypoint is not
// "claude-desktop-3p". When it holds, Claude Code SKIPS ANTHROPIC_AUTH_TOKEN
// entirely -- the one place on this axis where setting a credential variable
// makes less difference rather than more.
func (h HostContext) SuppressesAuthToken() bool {
	return h.IsHosted() && !h.HostAuthEnvVar && h.Entrypoint != "claude-desktop-3p"
}

// AntProfile is what Claude Code learned about the Anthropic CLI's
// configuration. Both fields are plain strings rather than Go enums, because
// they are another program's vocabulary and inventing names for them would hide
// which string came from where.
type AntProfile struct {
	// Precedence is HY(): "", "profile-explicit", "env-quad" or
	// "profile-implicit".
	Precedence string
	// AuthType is the profile's authentication.type: "", "oidc_federation" or
	// "user_oauth".
	AuthType string
}

// Configured reports whether the Anthropic CLI has a profile that applies at
// all -- wVo(), which is `HY() !== null`.
func (p AntProfile) Configured() bool { return p.Precedence != "" }

// LosesToLogin is MLn(): an IMPLICIT profile whose auth type is user_oauth
// gives way to a real claude.ai login. It is the one place the two branches
// genuinely interact, and it is why an explicit ANTHROPIC_PROFILE, the
// federation variable pair, and an implicit oidc_federation profile all beat a
// login while an implicit user_oauth one does not.
func (p AntProfile) LosesToLogin() bool {
	return p.Precedence == "profile-implicit" && p.AuthType == "user_oauth"
}

// OAuthEnvironment is what BT() reads. ccdad fills it from the process
// environment, two file stats, and Claude Code's settings.
//
// The refusal APIKeyEnvironment carries applies here too, and is repeated
// rather than cross-referenced because a reader arrives at one or the other:
// CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST, ANTHROPIC_UNIX_SOCKET and the
// third-party provider switches (Bedrock, Vertex, Foundry, the AWS and Google
// variants, Mantle) are deliberately NOT modelled. On a machine with any of
// them set the session is not talking to Anthropic's first-party API at all, no
// answer ccdad gives about accounts is meaningful, and the caller checks for
// them separately rather than this returning a source that will not be used.
// They are also most of IP()'s own disqualifier list, which is why
// profileApplies below has only one gate left to test.
type OAuthEnvironment struct {
	// Bare is CLAUDE_CODE_SIMPLE, parsed by TruthyEnv. Only the variable half
	// of cg() is filled -- the other half reads process.argv of a `claude` that
	// has not started. See the refusal at the bottom of this file.
	Bare bool

	// AuthToken reports that ANTHROPIC_AUTH_TOKEN is set. A PRESENCE flag and
	// not the value: ccdad stores nothing under that name, so a value could
	// never be attributed to an account, and holding a live bearer token to
	// answer a presence question is what the credentials rule forbids.
	AuthToken bool

	// TokenEnv is CLAUDE_CODE_OAUTH_TOKEN's VALUE, and the asymmetry with
	// AuthToken is deliberate: ccdad DOES store credentials under this name --
	// every setup-token account -- so the value is what lets attribution name
	// the account rather than only the mechanism. APIKeyEnvironment.EnvKey is
	// the same trade for the same reason.
	TokenEnv string

	// TokenDescriptor reports that CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR is
	// set. Whether the descriptor yields a token would need a read of
	// /proc/self/fd/<n> in a process that has not started, so this over-reports
	// for a descriptor that would fail -- in the safe direction.
	TokenDescriptor bool

	// HostTokenFile reports that HostOAuthTokenFile exists and is non-empty.
	//
	// Claude Code reads that path on EVERY machine when the descriptor variable
	// is unset. Only a CLAUDE_CODE_REMOTE session ever WRITES it -- the writer
	// opens `if(!V.CLAUDE_CODE_REMOTE)return` while the reader has no such
	// gate -- so a file found on a laptop was put there by something other than
	// a local `claude`, and ccdad must not offer to remove it.
	//
	// A non-empty stat, never a read. The one imprecision it costs: Claude
	// Code's reader trims, so a whitespace-only file is nothing to it and
	// present here. That over-reports, which is the safe direction.
	HostTokenFile bool

	// BgSnapshot reports that CLAUDE_BG_AUTH_SNAPSHOT_PATH names a non-empty
	// file. Gwb() runs INSIDE mbe(), parses that file's JSON, seeds the very
	// cache mbe() returns, and UNLINKS the file. Presence alone cannot decide
	// the branch -- it fires only if the JSON carries an accessToken -- so this
	// makes Resolve DECLINE rather than guess.
	BgSnapshot bool

	// Helper reports that an apiKeyHelper is configured, as on the api-key axis.
	Helper bool

	// Host and Profile are the two gates that are not single variables.
	Host    HostContext
	Profile AntProfile
}

// Resolve answers which source Claude Code would take a session's OAuth-shaped
// credential from, given the login sitting in the credentials file.
//
// ok is false when ccdad DECLINES: the state is real, it is decidable only by
// reading a credential, and naming a source anyway would be a confident wrong
// answer. There is exactly one such state, and it is BgSnapshot.
func (e OAuthEnvironment) Resolve(login Login) (source OAuthSource, ok bool) {
	if e.Bare {
		if e.Helper {
			return OAuthHelper, true
		}
		return OAuthNone, true
	}
	if e.AuthToken && !e.Host.SuppressesAuthToken() {
		return OAuthAuthTokenEnv, true
	}
	if e.TokenEnv != "" {
		return OAuthTokenEnv, true
	}
	// mbe(), in its own order: the snapshot is consumed first, then the
	// descriptor, then the well-known file. BT() reports the DESCRIPTOR name
	// whenever that variable is set, even when the descriptor read failed and
	// the well-known file is what actually answered.
	if e.BgSnapshot {
		return OAuthNone, false
	}
	if e.TokenDescriptor {
		return OAuthTokenDescriptor, true
	}
	if e.HostTokenFile {
		return OAuthHostTokenFile, true
	}
	if e.Helper && !e.Host.IsHosted() {
		return OAuthHelper, true
	}
	if e.profileApplies(login) {
		return OAuthProfile, true
	}
	if login.SignsInForInference() {
		return OAuthLogin, true
	}
	return OAuthNone, true
}

// profileApplies is what is LEFT of IP() once Resolve has reached branch 6.
//
// IP() disqualifies itself on a long list. Most of it -- bare mode,
// ANTHROPIC_AUTH_TOKEN, CLAUDE_CODE_OAUTH_TOKEN, mbe(), the helper -- is
// already false by construction here, because Resolve tested each one above and
// fell through. The rest are the provider switches, ANTHROPIC_UNIX_SOCKET and
// CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST, which OAuthEnvironment's doc declares
// outside this model's domain. That leaves the hosted test and the one
// carve-out where a login wins.
func (e OAuthEnvironment) profileApplies(login Login) bool {
	if !e.Profile.Configured() || e.Host.IsHosted() {
		return false
	}
	return !(e.Profile.LosesToLogin() && login.SignsInForInference())
}

// TruthyEnv is Un() (grep: `function Un(`): a value counts as true only when it
// trims and lowercases to one of "1", "true", "yes", "on".
//
// Both axes need it. Treating any non-empty string as true -- which is what
// claudeAPIKeyEnvironment did for CLAUDE_CODE_SIMPLE -- puts ccdad in bare mode
// for CLAUDE_CODE_SIMPLE=0 while Claude Code is not, and bare mode changes the
// answer on both axes.
func TruthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// envPresent is "this variable carries something after trimming", which is what
// Claude Code's typed environment accessor does to every STRING variable before
// anything tests it: its schema trims and turns what is left of a blank into
// nothing. A variable set to spaces is set to nothing.
//
// It is NOT the test for CLAUDE_CODE_REMOTE or CLAUDE_CODE_SIMPLE. Those are
// declared bool in that accessor and go through TruthyEnv.
func envPresent(name string) bool { return strings.TrimSpace(os.Getenv(name)) != "" }

// WHAT THIS FILE DELIBERATELY DOES NOT MODEL.
//
// `claude --bare`. cg()'s other half reads process.argv of a `claude` that has
// not started. It is not observable and it is not guessed at: Bare comes from
// CLAUDE_CODE_SIMPLE alone, and a session started with the flag resolves
// differently from what ccdad reports. It is not hedged about either -- bare
// mode resolves to "none" on almost every machine, so a caveat printed for it
// would appear on every report forever and tell nobody anything.
//
// EVERY CREDENTIAL VALUE BEHIND A SOURCE. The descriptor's token lives behind a
// file descriptor of a process that has not started; the helper's key comes
// from running a command; the ANTHROPIC_AUTH_TOKEN and host-file tokens are
// live bearer credentials. Each is a presence flag and nothing more: enough to
// know a credential WILL resolve and displace the login, never enough to say
// whose it is.
//
// THE BG-AUTH SNAPSHOT'S CONTENTS. Its file is a credential, and the only fact
// that decides the branch -- whether its JSON carries a non-empty accessToken
// -- is inside it. Claude Code's own reader consumes it and UNLINKS it, so the
// state ccdad would describe does not survive the first `claude` that starts.
// Declining is cheaper than being right once.
//
// A LAUNCHER THAT INJECTS CLAUDE_CODE_ENTRYPOINT INTO ITS CHILD. HostContext is
// filled from ccdad's own environment, and nothing here can see a variable a
// program has not been started to set.
