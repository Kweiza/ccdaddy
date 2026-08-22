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
//	4. CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR
//	5. apiKeyHelper
//	6. primaryApiKey from ~/.claude.json  (source "/login managed key")
//	7. none
//
// The OAuth gate is `BE()`, which the main client binds as
// `anthropicAuthEnabled`. It turns OFF -- meaning the key wins and the login is
// ignored -- for sources 1 to 5, and NOT for 6. A stored primaryApiKey is
// therefore inert whenever a claudeAiOauth record exists.

// APIKeySource names where Claude Code would take a session's API key from.
type APIKeySource uint8

const (
	// APIKeyNone means no key resolves and the session is on its OAuth login,
	// or on nothing.
	APIKeyNone APIKeySource = iota
	// APIKeyEnv is the ANTHROPIC_API_KEY environment variable.
	APIKeyEnv
	// APIKeyFileDescriptor is CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR, which a
	// sandbox host uses to pass a key without putting it in the environment.
	// Claude Code reports it under the same source NAME as the environment
	// variable, so it displaces an OAuth login the same way.
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
		return "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR"
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
// This is `BE()`'s test, which reads `source === "ANTHROPIC_API_KEY" ||
// source === "apiKeyHelper"` -- and the file-descriptor branch returns the
// former name, which is why it is on this side of the line too.
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
	// FileDescriptorKey reports whether CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR is
	// set. The key itself lives behind a descriptor ccdad cannot read, so this
	// is a presence flag: enough to know a key WILL resolve and displace the
	// login, not enough to say whose it is.
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
// rule and TestAPIKeyApprovalMatchesCclink pins that they agree.
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
