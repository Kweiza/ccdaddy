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

// LiveState is what the credentials file holds, in the four answers a swap
// needs to tell apart.
//
// AttributeFile used to return two, and collapsing the last two into one bool
// is what let the engine overwrite a running session's login. "No login in the
// file" and "a login this store cannot name" have OPPOSITE answers for an
// unattended swap: the first is a machine with nothing to lose, and the second
// is very often a managed account whose refresh token Claude Code has just
// rotated — installing over it re-presents a superseded grant and takes the
// whole family down with it.
//
// The same distinction is already drawn, correctly, one package over: the
// daemon's probeDue refuses to spend quota against an account when the live one
// "could not be worked out", for exactly this reason. This type is that rule
// given a name so the swap path can hold it too.
type LiveState uint8

const (
	// LiveNone: the file carries no OAuth login at all. Nobody is logged in,
	// and a swap has nothing to overwrite.
	LiveNone LiveState = iota
	// LiveManaged: the file's login is one of this store's accounts.
	LiveManaged
	// LiveUnattributed: the file carries an OAuth login that matches no stored
	// snapshot. It may be an account nothing here manages, or it may be a
	// managed account that has rotated since ccdad last saw it, and the file
	// alone cannot say which.
	LiveUnattributed
	// LiveUnreadable: the login store could not be READ, so what it holds is
	// not known -- not even whether it holds anything.
	//
	// This is the fourth answer, and it was missing for the same reason the
	// third one was. A caller that could not read the store passed a nil blob,
	// which lands on LiveNone above -- "nobody is logged in, and a swap has
	// nothing to overwrite" -- and that is the most dangerous of the four
	// things it could mean. On macOS the ordinary way to get here is a locked
	// login keychain answering errSecInteractionNotAllowed while Claude Code,
	// whose combinator falls back to the credentials file, carries on serving a
	// live session from a store ccdad cannot see.
	//
	// It is appended rather than inserted so the existing values keep their
	// numbers, the same rule Outcome carries.
	LiveUnreadable
)

func (s LiveState) String() string {
	switch s {
	case LiveNone:
		return "no login"
	case LiveManaged:
		return "a managed account"
	case LiveUnattributed:
		return "a login this store cannot name"
	case LiveUnreadable:
		return "a login store this machine cannot read"
	}
	return "unknown"
}

// AttributeFile matches the live credentials file against the managed accounts.
//
// The bool answers "is this a managed account", which is what a caller that
// only needs a name wants. A caller deciding whether to WRITE the file wants
// LiveStateOf instead: false here spans both LiveNone and LiveUnattributed.
func AttributeFile(live cclink.Blob, accounts []store.Account, lookup Lookup) (store.Account, bool) {
	a, state := LiveStateOf(live, accounts, lookup)
	return a, state == LiveManaged
}

// LiveStateOf is AttributeFile without the collapse.
func LiveStateOf(live cclink.Blob, accounts []store.Account, lookup Lookup) (store.Account, LiveState) {
	liveID := CredentialIdentity(live)
	if liveID == "" {
		return store.Account{}, LiveNone
	}
	for _, a := range accounts {
		stored, err := lookup(a.UUID)
		if err != nil {
			continue
		}
		if CredentialIdentity(stored) == liveID {
			return a, LiveManaged
		}
	}
	return store.Account{}, LiveUnattributed
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
	// FromLoginStore says the credential came from the LOGIN STORE -- the
	// keychain item or the credentials file -- rather than from an environment
	// override or a stored API key.
	//
	// It exists because a failure to attribute means different things on the
	// two axes, and only one of them supports the sentence ccdad used to print
	// for both. A login store's refresh token is rotated by Claude Code on
	// every refresh, and attribution matches on that token, so an
	// unattributable login is very often one of the user's OWN accounts seconds
	// after a refresh -- calling it unmanaged asserts ownership nothing
	// established. A token handed over in CLAUDE_CODE_OAUTH_TOKEN is not
	// rotated by anything ccdad can miss, so there the claim can be made.
	FromLoginStore bool
}

// LoginOf reads the credentials file as the two fields the OAuth resolver's
// last branch tests, and nothing else.
//
// It lives here rather than in internal/identity because that package must not
// depend on the one that reads and writes Claude Code's files -- the same rule
// that made APIKeyApproval a duplicated three lines rather than an import.
//
// The access token is reduced to a BOOL and never carried further: the question
// is whether one exists, and passing a live bearer token around to answer that
// is what this tree's credentials rule forbids. A record that will not parse is
// the zero Login, which is the right answer for it -- cclink's loader is where
// a malformed credentials file is reported, not here.
func LoginOf(live cclink.Blob) identity.Login {
	raw, ok := live["claudeAiOauth"]
	if !ok {
		return identity.Login{}
	}
	var record struct {
		AccessToken string   `json:"accessToken"`
		Scopes      []string `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return identity.Login{}
	}
	return identity.Login{HasAccessToken: record.AccessToken != "", Scopes: record.Scopes}
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
//   - Otherwise the OAuth axis answers, in ITS OWN resolver's order. That order
//     is not "the environment token, then the file": ANTHROPIC_AUTH_TOKEN
//     outranks CLAUDE_CODE_OAUTH_TOKEN, a session host can inject a token at a
//     path with no variable behind it, an Anthropic CLI profile sits in front
//     of the file, and the file itself counts only when its scopes carry
//     user:inference. identity/oauth.go is that model; this function renders
//     it.
//   - Only when the OAuth axis has nothing does a STORED primaryApiKey become
//     the credential. It is Claude Code's lowest-priority source, and treating
//     it as the answer while a login exists is the single mistake this whole
//     model exists to avoid.
//
// liveSource is WHICH STORE live came from, and it is a parameter rather than
// something this function works out: the read already happened, and re-deriving
// it here would be a second answer that can disagree with the first. Callers
// get it from cclink.LoadWithSource.
func AttributeLogin(live cclink.Blob, accounts []store.Account,
	lookup Lookup, env identity.APIKeyEnvironment, oauthEnv identity.OAuthEnvironment,
	liveSource cclink.CredentialSource) Attribution {

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

	oauthSource, resolved := oauthEnv.Resolve(LoginOf(live))
	switch {
	case !resolved:
		// The one state the OAuth model declines on. Naming a source would be a
		// guess about a credential ccdad refuses to read, and "which account is
		// this" is the question a wrong guess is worst for.
		return Attribution{Via: "a credential ccdad cannot resolve"}
	case oauthSource == identity.OAuthTokenEnv:
		// The one OAuth source ccdad can attribute to an ACCOUNT, because it is
		// the one it stores credentials under.
		for _, a := range accounts {
			stored, err := lookup(a.UUID)
			if err != nil {
				continue
			}
			if rec, ok := cclink.TokenRecordOf(stored); ok && rec.Token == oauthEnv.TokenEnv {
				return Attribution{Account: a, OK: true, Via: oauthSource.SourceName()}
			}
		}
		return Attribution{Via: oauthSource.SourceName()}
	case oauthSource == identity.OAuthLogin:
		acct, ok := AttributeFile(live, accounts, lookup)
		// The store that ANSWERED, not a constant. On macOS the keychain item
		// is read before the file, so naming the file here was wrong on exactly
		// the machines where which store it is mattered.
		return Attribution{Account: acct, OK: ok, Via: liveSource.String(), FromLoginStore: true}
	case oauthSource != identity.OAuthNone:
		// Everything else on this axis is a mechanism ccdad can name and an
		// account it cannot: the credential is behind a descriptor, in a file
		// it must not read, or another program's entirely.
		return Attribution{Via: oauthSource.String()}
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

// DisplacementNote is the ONE sentence every caller prints when a switch is
// outranked, built from the source rather than from a variable name.
//
// It exists because widening the gate without widening its messages is what
// this function was added to repair: the gate had grown from one environment
// variable to six sources, three of which have no variable, while four call
// sites went on telling the user to unset CLAUDE_CODE_OAUTH_TOKEN. A single
// renderer is what stops the fifth from being written that way.
//
// lead is the caller's own opening ("Note: " for an attended switch, "not
// switching: " for the engine), because the two differ in tense and nothing
// else.
func DisplacementNote(lead string, res Result) string {
	if res.DisplacedUnresolved {
		return lead + "CLAUDE_BG_AUTH_SNAPSHOT_PATH names a token snapshot, and Claude Code consumes it " +
			"before it looks at the credentials file — so a switch may change nothing, and ccdad cannot " +
			"tell from here. Check the host session that set it."
	}
	note := lead + "Claude Code takes its OAuth credential from " + res.DisplacedBy.String() +
		", and it reads that before the credentials file — so a switch changes nothing about what a " +
		"session authenticates as."
	if remedy := res.DisplacedBy.Remedy(); remedy != "" {
		note += " " + remedy
	}
	return note
}
