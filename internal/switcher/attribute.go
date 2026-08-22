// Package switcher performs the credential swap, and answers the question the
// swap turns on: which managed account is live right now.
//
// It exists because the swap used to be ~90 lines of Cobra RunE. The daemon
// needs the same sequence, and a second copy of it diverges from the one path
// anybody has hand-tested — so the sequence lives here and both callers reach
// it. The CLI keeps the words; this package keeps the behaviour.
package switcher

import (
	"encoding/json"
	"os"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// Lookup reads an account's stored credential snapshot. It is a parameter
// rather than a *store.Store so attribution can be exercised without one, and
// so a caller that has already opened the store does not open a second.
type Lookup func(uuid string) (cclink.Blob, error)

// There are TWO attribution questions, not one, and the task-15 brief's
// Interfaces block named a single `cli.attribute(live, accounts)` that was never
// written. The divergence is deliberate and is settled here:
//
//   - AttributeFile asks about the credentials FILE. It is what a switch
//     rewrites, so it is the only sound already-on check and the only sound
//     anti-flap baseline.
//   - AttributeLogin asks what Claude Code will actually USE, which an
//     environment key or an environment token can win without the file changing
//     at all.
//
// Both take a Lookup, which the named signature had no room for: matching a
// live credential against a managed account means reading every account's
// stored snapshot. AttributeLogin additionally takes the api-key environment,
// because the axis that displaces a login is not in the file either.

// CredentialIdentity is the value attribution compares, for accounts whose
// credential really does live in the credentials file.
//
// The refresh token leads because it survives an access-token rotation: a
// Claude Code that has refreshed since the switch still carries it. An access
// token is the fallback, for a record that has been through Claude Code's
// dead-token clear or was written without one.
//
// There is deliberately no API-key case: the claudeAiOauth object has exactly
// eight keys and none of them is an API key, so a branch for one would be dead
// code pretending to be defensive. Token accounts are matched by AttributeLogin
// instead, where Claude Code actually reads them from.
//
// The kind prefix is load-bearing: without it an access token stored by one
// account could match a refresh token stored by another. "" means "cannot
// identify" and never matches anything, including another "" — which is the
// case for every token account, whose stored blob has no OAuth record at all.
func CredentialIdentity(b cclink.Blob) string {
	raw, ok := b["claudeAiOauth"]
	if !ok {
		return ""
	}
	var payload struct {
		RefreshToken string `json:"refreshToken"`
		AccessToken  string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	switch {
	case payload.RefreshToken != "":
		return "refresh:" + payload.RefreshToken
	case payload.AccessToken != "":
		return "access:" + payload.AccessToken
	}
	return ""
}

// AttributeFile matches the live credentials file against the managed accounts.
func AttributeFile(live cclink.Blob, accounts []store.Account, lookup Lookup) (store.Account, bool) {
	liveID := CredentialIdentity(live)
	if liveID == "" {
		return store.Account{}, false
	}
	for _, a := range accounts {
		stored, err := lookup(a.UUID)
		if err != nil {
			continue
		}
		if CredentialIdentity(stored) == liveID {
			return a, true
		}
	}
	return store.Account{}, false
}

// Attribution is what AttributeLogin learned: the account, and how Claude Code
// gets to it. The how is reported even when the account is not one ccdad
// manages, because "not managed" plus "because apiKeyHelper is set" is
// actionable where "not managed" alone is not.
type Attribution struct {
	Account store.Account
	OK      bool
	// Via names the mechanism, in the caller's words.
	Via string
}

// AttributeLogin answers "which managed account is Claude Code actually using".
//
// It models Claude Code's two competing axes in the order Claude Code resolves
// them, which is documented rule by rule in identity/apikey.go:
//
//   - An API key from the ENVIRONMENT, a file descriptor or an apiKeyHelper
//     turns the OAuth path off entirely (`BE()`), so it is asked first. The
//     credentials file must not even be consulted then: reporting its account
//     would name one Claude Code is not using.
//   - Otherwise the OAuth axis answers, in `ua()`'s own order —
//     CLAUDE_CODE_OAUTH_TOKEN first, the credentials file second.
//   - Only when the OAuth axis has nothing does a STORED primaryApiKey become
//     the credential. It is Claude Code's lowest-priority source, and treating
//     it as the answer while a login exists is the single mistake this whole
//     model exists to avoid.
func AttributeLogin(live cclink.Blob, accounts []store.Account,
	lookup Lookup, env identity.APIKeyEnvironment) Attribution {

	key, source := env.Resolve()

	if source.DisplacesOAuth() {
		if key == "" {
			// A file descriptor or a helper: something WILL resolve and it will
			// displace the login, but reading it means reading another
			// process's descriptor or running a user's command. Neither belongs
			// in a read-only question, so the honest answer is the mechanism
			// without the account.
			return Attribution{Via: source.String()}
		}
		if acct, ok := APIKeyOwner(accounts, lookup, key); ok {
			return Attribution{Account: acct, OK: true, Via: source.String()}
		}
		return Attribution{Via: source.String()}
	}

	if envToken := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); envToken != "" {
		for _, a := range accounts {
			stored, err := lookup(a.UUID)
			if err != nil {
				continue
			}
			if rec, ok := cclink.TokenRecordOf(stored); ok && rec.Token == envToken {
				return Attribution{Account: a, OK: true, Via: "CLAUDE_CODE_OAUTH_TOKEN"}
			}
		}
		return Attribution{Via: "CLAUDE_CODE_OAUTH_TOKEN"}
	}

	if _, hasOAuth := live["claudeAiOauth"]; hasOAuth {
		acct, ok := AttributeFile(live, accounts, lookup)
		return Attribution{Account: acct, OK: ok, Via: "the Claude Code credentials file"}
	}

	// No login anywhere, so Claude Code falls through to its stored key.
	if source == identity.APIKeyManaged {
		if acct, ok := APIKeyOwner(accounts, lookup, key); ok {
			return Attribution{Account: acct, OK: true, Via: source.String()}
		}
		return Attribution{Via: source.String()}
	}
	return Attribution{Via: "none"}
}

// APIKeyOwner finds the managed account whose stored credential is key.
func APIKeyOwner(accounts []store.Account, lookup Lookup, key string) (store.Account, bool) {
	if key == "" {
		return store.Account{}, false
	}
	for _, a := range accounts {
		creds, err := lookup(a.UUID)
		if err != nil {
			continue
		}
		if rec, ok := cclink.TokenRecordOf(creds); ok && rec.Kind == cclink.APIKeyKind && rec.Token == key {
			return a, true
		}
	}
	return store.Account{}, false
}
