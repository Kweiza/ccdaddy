// Package oauth implements the Claude Code OAuth login: PKCE, the dual-path
// authorization race, and the token endpoint.
//
// Every constant here was read out of the Claude Code 2.1.238 binary. They ship
// INSIDE Claude Code and can change between its releases, so they live in one
// file: a drift check has exactly one place to look.
package oauth

import "strings"

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
