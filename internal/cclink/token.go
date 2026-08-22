package cclink

import "encoding/json"

// TokenKey is ccdad's own record for a credential Claude Code does NOT read out
// of the credentials file.
//
// The 2.1.238 bundle is explicit about both kinds. An API key is persisted to
// ~/.claude.json as the top-level string primaryApiKey (function Saa), and the
// claudeAiOauth object has exactly eight keys — accessToken, refreshToken,
// expiresAt, refreshTokenExpiresAt, scopes, subscriptionType, rateLimitTier,
// clientId (function hjd) — none of which is an API key. A setup token is not
// persisted at all: `claude setup-token` prints it and tells the user to export
// CLAUDE_CODE_OAUTH_TOKEN, and the OAuth flow's setup-token branch deliberately
// skips the credential save.
//
// So neither belongs under claudeAiOauth. Putting one there would write a
// record Claude Code never writes and cannot read as a login. This key is
// ccdad's own, plainly named as such; Activate refuses a snapshot with no
// claudeAiOauth, which makes the separation fail-closed.
//
// It is deliberately absent from both AccountScopedKeys and KnownMachineKeys.
// Merge copies only AccountScopedKeys out of an incoming snapshot, so this key
// can never reach the live file however a caller mishandles it — the same
// fail-closed direction, enforced by the merge rather than by care.
const TokenKey = "ccdadToken"

// APIKeyKind is the TokenRecord.Kind an API key is stored under. The other is
// "setup-token", which Claude Code reads from the environment only and which
// therefore has no activation path at all.
const APIKeyKind = "api-key"

// TokenRecord is what TokenKey holds.
type TokenRecord struct {
	// Kind is "api-key" or "setup-token".
	Kind  string `json:"kind"`
	Token string `json:"token"`
}

// TokenRecordOf reports the ccdad token record a blob carries, if any.
//
// A malformed record reads as absent rather than as an error. The callers are
// all asking a classification question — is this account a token account? —
// and there is no answer to give a corrupt record other than "not one this can
// install", which is what absent already means.
func TokenRecordOf(b Blob) (TokenRecord, bool) {
	raw, ok := b[TokenKey]
	if !ok {
		return TokenRecord{}, false
	}
	var rec TokenRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return TokenRecord{}, false
	}
	return rec, true
}
