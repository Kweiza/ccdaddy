// Package oauth implements the Claude Code OAuth login: PKCE, the dual-path
// authorization race, and the token endpoint.
//
// Every constant here was read out of the Claude Code 2.1.238 binary. They ship
// INSIDE Claude Code and can change between its releases, so they live in one
// file: a drift check has exactly one place to look.
package oauth

import (
	"slices"
	"strings"
)

// Endpoints, verbatim from Claude Code 2.1.238.
const (
	// ClientID is Claude Code's own public OAuth client.
	ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	// ClaudeAIAuthorizeURL is the subscription surface: Pro, Max, Team,
	// Enterprise. This is the one that mints claude.ai credentials.
	ClaudeAIAuthorizeURL = "https://claude.com/cai/oauth/authorize"

	// ConsoleAuthorizeURL is the Console/API-billing surface. It does NOT mint
	// claude.ai credentials, so it is only right for a credit-billed account.
	ConsoleAuthorizeURL = "https://platform.claude.com/oauth/authorize"

	// TokenURL exchanges an authorization code and refreshes a token.
	TokenURL = "https://platform.claude.com/v1/oauth/token"

	// ManualRedirectURL is where the browser lands when the user is going to
	// copy the code by hand. The page shows `code#state` for pasting.
	ManualRedirectURL = "https://platform.claude.com/oauth/code/callback"

	// APIBaseURL serves the profile and usage endpoints.
	APIBaseURL = "https://api.anthropic.com"
)

// ScopeString is the scope parameter exactly as Claude Code sends it, in Claude
// Code's own order. Order is cosmetic to the authorization server but kept
// identical so a stored scope string compares equal to one Claude Code wrote.
//
// The authorize builder reads this const and not the Scopes slice: a caller who
// reorders or truncates an exported slice must not be able to corrupt every
// subsequent request process-wide.
const ScopeString = "org:create_api_key user:profile user:inference " +
	"user:sessions:claude_code user:mcp_servers user:file_upload"

// Scopes is the same set split out, for callers that need the individual tokens
// rather than the wire string.
var Scopes = strings.Split(ScopeString, " ")

// RefreshScopeString is the scope parameter Claude Code sends on the REFRESH
// grant, and it is deliberately not ScopeString: it drops org:create_api_key.
//
// In the 2.1.238 bundle the authorize builder joins `Jjs`, the deduped union of
// both scope lists, while the refresh call joins `UYe` alone. Omitting the
// parameter is not equivalent — RFC 6749 §6 reads an absent scope as "the same
// as originally granted", which would keep all six. A live
// ~/.claude/.credentials.json holds exactly these five, so sending anything else
// would write a scope set Claude Code never produces into the file ccdad swaps.
const RefreshScopeString = "user:profile user:inference " +
	"user:sessions:claude_code user:mcp_servers user:file_upload"

// RefreshScopes is RefreshScopeString split out, mirroring Scopes.
var RefreshScopes = strings.Split(RefreshScopeString, " ")

// ScopeInference is the scope that marks a credential as a claude.ai login.
// Claude Code tests for it by name in several places, so it is named here once.
const ScopeInference = "user:inference"

// preservableExpansionScopes is the set a refresh carries forward from the
// stored credential instead of dropping — the bundle's
// PRESERVABLE_EXPANSION_SCOPES, filtered against in preservableScopesFrom.
//
// They are not in RefreshScopeString because a refresh does not ASK for them:
// it only keeps them if the credential already has them. An account that never
// expanded never sees these.
//
// It is a const string, and unexported, for the same reason ScopeString is: a
// caller who can reach the value can silently change every subsequent request.
// Emptying the exported slice used to drop user:plugins from the wire.
const preservableExpansionScopes = "user:projects:read user:projects:write user:plugins"

// PreservableExpansionScopes lists the same set for callers that need to
// inspect it. Mutating it cannot affect a request.
var PreservableExpansionScopes = strings.Split(preservableExpansionScopes, " ")

// PreservableScopesFrom returns the members of stored that survive a refresh,
// in the order stored lists them. It is Claude Code's preservableScopesFrom:
// `e.filter(r => PRESERVABLE_EXPANSION_SCOPES.includes(r))`.
func PreservableScopesFrom(stored []string) []string {
	preservable := strings.Split(preservableExpansionScopes, " ")
	var kept []string
	for _, s := range stored {
		if slices.Contains(preservable, s) {
			kept = append(kept, s)
		}
	}
	return kept
}
