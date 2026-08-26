package identity

import "strings"

// This file models how Claude Code decides which API key a session will use,
// and whether that key displaces an OAuth login. It is a MODEL of another
// program's behaviour, so every rule below cites the function it came from in
// the 2.1.238 bundle. Guessing here is worse than declining to answer: `ccdad
// which` reports which account Claude Code is really using, and a confident
// wrong answer sends someone to debug the wrong account.
//
// The resolver is `eB()`. Reproduced, with the branches that are dead in a
// shipped build already removed (`bG()` returns false and `Un(!1)` is false, so
// the two branches that look like they gate the environment variable never
// run):
//
//	1. bare mode          -> ANTHROPIC_API_KEY, else apiKeyHelper, else none
//	2. non-interactive    -> ANTHROPIC_API_KEY outright, no approval needed
//	3. ANTHROPIC_API_KEY  -> only if approved (see APIKeyApproval)
//	4. a HOST-INJECTED key: CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR, or -- when that
//	   variable is unset -- the well-known file identity.HostAPIKeyFile. Both
//	   reach eB() through one call (grep: `wellKnownPath:`) and both are
//	   reported under the source name ANTHROPIC_API_KEY.
//	5. apiKeyHelper
//	6. primaryApiKey from ~/.claude.json  (source "/login managed key")
//	7. none
//
// The OAuth gate is `BE()`, and it is BIGGER than the clause ccdad models. The
// main client binds it as `anthropicAuthEnabled`. Read out of the bundle, it
// opens with three early returns before it reaches any API key at all:
//
//	if (bare mode)             -> false
//	if (ANTHROPIC_UNIX_SOCKET) -> !!CLAUDE_CODE_OAUTH_TOKEN
//	if (an Anthropic CLI profile applies) -> false
//
// Note the middle one: with the socket set the gate is not simply off, it is on
// exactly when CLAUDE_CODE_OAUTH_TOKEN is set. And the gate carries a
// first-party term as well, so any third-party provider turns it off too.
//
// What DisplacesOAuth models is the API-key clause that survives all of that:
// `source === "ANTHROPIC_API_KEY" || source === "apiKeyHelper"`. THAT CLAUSE IS
// ITSELF HOST-GATED in the bundle -- the environment-key and helper halves carry
// `!XMn()` and `!Wer()` respectively -- so on a session host a resolved key does
// NOT turn the gate off. DisplacesOAuth takes no host input and does not model
// it; oauth.go's HostContext is where that shape is modelled, on the axis where
// it decides the answer rather than only softening it.
//
// Within the clause ccdad does model, the gate turns OFF -- meaning the key wins
// and the login is ignored -- for sources 1 to 5, and NOT for 6. A stored
// primaryApiKey is therefore inert whenever a usable claudeAiOauth record
// exists.
//
// There is a SECOND resolver over a different axis, in oauth.go. It decides
// which OAuth-shaped credential a session gets, and the login this one calls
// inert is the LAST thing it looks at. Neither file is the whole answer.

// APIKeySource names where Claude Code would take a session's API key from.
type APIKeySource uint8

const (
	// APIKeyNone means no key resolves and the session is on its OAuth login,
	// or on nothing.
	APIKeyNone APIKeySource = iota
	// APIKeyEnv is the ANTHROPIC_API_KEY environment variable.
	APIKeyEnv
	// APIKeyFileDescriptor is a key a SESSION HOST injected: either through
	// CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR, or -- when that variable is unset --
	// through the well-known file identity.HostAPIKeyFile. One branch of eB()
	// covers both, and Claude Code reports both under the same source NAME as
	// the environment variable, so both displace an OAuth login the same way.
	//
	// ONE constant for two routes, deliberately: a second one would be two
	// names for a rule that is identical on every axis a caller can ask about.
	APIKeyFileDescriptor
	// APIKeyHelper is the apiKeyHelper setting, a command Claude Code runs.
	APIKeyHelper
	// APIKeyManaged is primaryApiKey in ~/.claude.json -- what Claude Code's
	// own `/login` writes when the user picks an API key, and what `ccdad
	// switch` writes for an api-key account. It is the ONLY source that does
	// not displace an OAuth login.
	APIKeyManaged
)

func (s APIKeySource) String() string {
	switch s {
	case APIKeyEnv:
		return "ANTHROPIC_API_KEY"
	case APIKeyFileDescriptor:
		// Names both routes, because this string is printed on machines where
		// the variable is NOT the one that resolved -- and telling a user to
		// look at a variable that is not set sends them nowhere.
		return "the key the session host injected (CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR, or " +
			HostAPIKeyFile + " when that is unset)"
	case APIKeyHelper:
		return "apiKeyHelper"
	case APIKeyManaged:
		return "the key stored in ~/.claude.json"
	default:
		return "none"
	}
}

// DisplacesOAuth reports whether a key from this source makes Claude Code
// ignore the credentials file entirely.
//
// This is one term of `BE()` -- `source === "ANTHROPIC_API_KEY" || source ===
// "apiKeyHelper"` -- and the host-injected branch returns the former name,
// which is why it is on this side of the line too. The file header lists the
// other three terms, all of which can turn the gate off for reasons no API key
// controls; an Anthropic CLI profile is one of them, and oauth.go's
// OAuthProfile is where that is modelled.
func (s APIKeySource) DisplacesOAuth() bool {
	switch s {
	case APIKeyEnv, APIKeyFileDescriptor, APIKeyHelper:
		return true
	default:
		return false
	}
}

// APIKeyEnvironment is what the resolution reads. ccdad fills it from the
// process environment, Claude Code's settings, and ~/.claude.json.
//
// Two things Claude Code consults are deliberately NOT modelled, and both are
// absent rather than guessed: CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST and the
// third-party provider switches (Bedrock, Vertex, Foundry), which mean the
// session is not talking to Anthropic's first-party API at all. On a machine
// with any of those set, no answer ccdad gives about accounts is meaningful,
// so the caller checks for them separately rather than this returning a key
// that will not be used.
type APIKeyEnvironment struct {
	// Bare is CLAUDE_CODE_SIMPLE or the --bare flag: a stripped-down mode that
	// reads only the environment variable and the helper.
	Bare bool
	// Interactive is whether the session is a terminal session rather than
	// `claude -p`. It changes the answer -- see Resolve.
	Interactive bool
	// EnvKey is ANTHROPIC_API_KEY.
	EnvKey string
	// FileDescriptorKey reports that a key WILL come from the session host:
	// CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR is set, or -- when it is not --
	// identity.HostAPIKeyFile stats non-empty. Both reach the same branch of
	// eB() and both are reported by Claude Code under the source name
	// ANTHROPIC_API_KEY.
	//
	// The key itself lives behind a descriptor ccdad cannot read, or in a file
	// it must not read, so this is a presence flag: enough to know a key WILL
	// resolve and displace the login, not enough to say whose it is.
	FileDescriptorKey bool
	// Helper reports whether the apiKeyHelper setting is configured. As with
	// the descriptor, the value would come from running a command, which a
	// read-only question must not do.
	Helper bool
	// Approved is customApiKeyResponses.approved from ~/.claude.json.
	Approved []string
	// ManagedKey is primaryApiKey from ~/.claude.json.
	ManagedKey string
}

// APIKeyApproval is the value Claude Code stores in, and looks up from,
// customApiKeyResponses.approved: the last twenty characters of the trimmed
// key (`Cbe(e){return e.trim().slice(-20)}`).
//
// It is duplicated from cclink rather than imported because identity must not
// depend on the package that writes Claude Code's files; both spell the same
// rule and TestAPIKeyApprovalAgreesWithCclink pins that they agree.
func APIKeyApproval(key string) string {
	trimmed := strings.TrimSpace(key)
	if len(trimmed) <= 20 {
		return trimmed
	}
	return trimmed[len(trimmed)-20:]
}

// Resolve answers which key Claude Code would use, and from where.
//
// A key is returned only when ccdad can actually see it. The file-descriptor
// and helper sources resolve to a source with an EMPTY key: something will
// resolve and it will displace the login, but reading it would mean reading
// another process's descriptor or running a user-configured command, and this
// question is read-only.
func (e APIKeyEnvironment) Resolve() (key string, source APIKeySource) {
	env := strings.TrimSpace(e.EnvKey)

	if e.Bare {
		if env != "" {
			return env, APIKeyEnv
		}
		if e.Helper {
			return "", APIKeyHelper
		}
		return "", APIKeyNone
	}

	// The approval list is an INTERACTIVE-mode gate and nothing more. In
	// `claude -p` the environment variable wins outright, which is why this is
	// two branches and not one -- a model that always demanded approval would
	// report "not managed" for a scripted session that is very much using the
	// key.
	if env != "" {
		if !e.Interactive || e.approved(env) {
			return env, APIKeyEnv
		}
	}
	if e.FileDescriptorKey {
		return "", APIKeyFileDescriptor
	}
	if e.Helper {
		return "", APIKeyHelper
	}
	if managed := strings.TrimSpace(e.ManagedKey); managed != "" {
		return managed, APIKeyManaged
	}
	return "", APIKeyNone
}

func (e APIKeyEnvironment) approved(key string) bool {
	want := APIKeyApproval(key)
	for _, a := range e.Approved {
		if a == want {
			return true
		}
	}
	return false
}

// Remedy is what to do about a key from this source. It is ccdad's own wording
// and NOT the table oauth.go's OAuthSource.Remedy reproduces, and the
// difference is the point: Claude Code's table is keyed by the name eB()
// reports, which collapses the variable, the descriptor and the well-known file
// into one "ANTHROPIC_API_KEY" arm whose remedy is "unset the ANTHROPIC_API_KEY
// environment variable". On a host-injected machine that variable is not set,
// so mirroring the table there would send a user after nothing.
func (s APIKeySource) Remedy() string {
	switch s {
	case APIKeyEnv:
		return "Unset ANTHROPIC_API_KEY."
	case APIKeyFileDescriptor:
		return "Unset CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR, or remove " + HostAPIKeyFile +
			" on the host that injected it."
	case APIKeyHelper:
		return "Unset the apiKeyHelper setting."
	default:
		return ""
	}
}

// EnvKeyNeedsApproval reports the one case where the answer depends on how
// Claude Code is started: an ANTHROPIC_API_KEY that a scripted session would
// use and an interactive one would refuse.
//
// It exists so a caller can say so instead of picking one of the two answers
// and being wrong half the time.
func (e APIKeyEnvironment) EnvKeyNeedsApproval() bool {
	env := strings.TrimSpace(e.EnvKey)
	return !e.Bare && env != "" && !e.approved(env)
}
