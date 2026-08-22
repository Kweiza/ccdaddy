// Package cclink classifies and merges the top-level keys of Claude Code's
// .credentials.json, and reads and writes the file itself — Load, Activate,
// ActivateWith, ClearLogin, and the atomic writer.
//
// The file holds twelve top-level keys, not one. Five travel with the account
// and seven belong to the machine; getting the split wrong destroys either the
// user's MCP server logins and machine device identity (too much swapped) or
// leaks one account's device token into another (too little).
package cclink

import "encoding/json"

// Blob is the decoded credentials file. Values stay as RawMessage so unknown
// fields survive a round trip unchanged in value — decoding into typed
// structs silently drops keys instead: clauth's typed token struct loses
// refreshTokenExpiresAt, rateLimitTier and clientId on every rewrite, and
// clientId is required to revoke a token. (The one exception is
// coworkRemoteDevice's sub-object during Merge, which is decoded and
// re-encoded to union it per organization; see merge.go.)
type Blob map[string]json.RawMessage

// AccountScopedKeys are the keys that travel with the login.
//
// This list mirrors, exactly, the set Claude Code deletes in its own re-login
// prune. Expressing the rule as a deny-list rather than allow-listing the
// machine-scoped keys is deliberate: when Anthropic adds a new machine-scoped
// key, a deny-list preserves it automatically, whereas an allow-list destroys
// it on every switch. The cost is that a new ACCOUNT-scoped key would leak
// until ccdad is updated — rare, and caught by UnknownKeys.
var AccountScopedKeys = []string{
	"claudeAiOauth",      // the OAuth login itself
	"organizationUuid",   // legacy; still pruned, so still ours
	"trustedDeviceToken", // per device AND per account
	"enterpriseGateway",  // url/jwt/expiresAt/idpRefreshToken — a live session
	"designOauth",        // a second full OAuth login, revoked alongside the first
}

// KnownMachineKeys are the machine-scoped keys observed in Claude Code 2.1.238.
// This list is documentation and drift detection only: Merge preserves anything
// not in AccountScopedKeys whether or not it appears here.
var KnownMachineKeys = []string{
	"mcpOAuth",             // MCP server logins
	"mcpOAuthClientConfig", // the client-secret half of mcpOAuth
	"mcpXaaIdp",
	"mcpXaaIdpConfig",
	"pluginSecrets",
	"gatewayTrust",       // gateway hostname -> pinned TLS fingerprint256
	"coworkRemoteDevice", // this machine's P-256 device key, keyed by org uuid
}

// IsAccountScoped reports whether key travels with the login.
func IsAccountScoped(key string) bool {
	for _, k := range AccountScopedKeys {
		if k == key {
			return true
		}
	}
	return false
}

// UnknownKeys returns the top-level keys ccdad does not recognize. Six machine
// keys appeared after clauth's one-key carry list was written, so drift here is
// demonstrated rather than hypothetical: warn on it and surface it in --json
// and in exports.
func UnknownKeys(b Blob) []string {
	known := make(map[string]bool, len(AccountScopedKeys)+len(KnownMachineKeys))
	for _, k := range AccountScopedKeys {
		known[k] = true
	}
	for _, k := range KnownMachineKeys {
		known[k] = true
	}
	var out []string
	for k := range b {
		if !known[k] {
			out = append(out, k)
		}
	}
	return out
}
