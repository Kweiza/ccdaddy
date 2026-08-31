// Package tokens hands out a valid access token for a managed account,
// refreshing the stored credential snapshot when one has expired.
//
// It exists because polling N accounts means holding N valid bearers, and every
// stored access token expires in about eight hours. Without it, `oauth.Refresh`
// has no production caller at all: every inactive account answers "unknown"
// after the first day, and the unknown-is-not-zero rule then leaves the engine
// with nothing it may rank.
//
// It is its own package rather than a corner of internal/usage or
// internal/store. It is a TOKEN-endpoint concern, not a usage-endpoint one, and
// internal/store must never make a network call — an account database that can
// block on api.anthropic.com is one every CLI command inherits a timeout from.
package tokens

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/cclock"
	"github.com/Kweiza/ccdaddy/internal/oauth"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// DefaultSkew is how long before its stated expiry a token is treated as
// expired. A poll that starts with four minutes left can still be answering
// when the token dies, and a 401 costs a retry against a 28-30 requests per
// rolling hour budget.
const DefaultSkew = 5 * time.Minute

// DefaultLockTimeout bounds the wait for Claude Code's refresh lock. Claude
// Code holds it for one token-endpoint round trip, so this outlasts a normal
// hold without stalling a tick.
const DefaultLockTimeout = 9 * time.Second

var (
	// ErrRejected means the token endpoint refused the stored refresh token
	// itself. This is the ONLY failure that says anything about the account,
	// and the only one the anti-flap quarantine may trigger on.
	ErrRejected = errors.New("the stored refresh token was rejected")

	// ErrUnavailable means the endpoint could not be reached or would not
	// answer — a laptop on a plane, or Anthropic returning 503. It says
	// nothing about the account, and quarantining on it would take the whole
	// fleet out over one outage.
	ErrUnavailable = errors.New("the token endpoint could not be reached")

	// ErrNoOAuthCredential means the account has no claudeAiOauth to refresh.
	// A `ccdad add-token` account is the ordinary case: Claude Code reads those
	// from an environment variable or ~/.claude.json, and there is no refresh
	// grant behind them.
	ErrNoOAuthCredential = errors.New("that account has no OAuth login to refresh")

	// ErrLivenessUnknown means the live credential store could not be READ, so
	// this account cannot be shown to be — or not to be — the login Claude Code
	// is authenticating with. On macOS a locked login keychain answers
	// errSecInteractionNotAllowed and cclink.Load reports it rather than
	// falling back to the file, which is the ordinary way to reach this.
	//
	// It says NOTHING about the account, so it must never quarantine one: it is
	// a fact about this machine's ability to look, not about the grant. See
	// AccessToken for why the answer is a refusal rather than a refresh.
	ErrLivenessUnknown = errors.New("the live credential store could not be read, so whether this account is the live login cannot be established")

	// ErrLiveTokenStale means the account is the LIVE Claude Code login and its
	// token has reached Claude Code's own refresh window. ccdad rotates the
	// live login only ABOVE that window; inside it the grant is Claude Code's
	// to spend, and a second spender there revokes one of the two. See
	// AccessToken.
	ErrLiveTokenStale = errors.New("the live Claude Code login's token has expired, and Claude Code is the one that rotates it")
)

// Source hands out access tokens.
//
// The zero value is not usable; call New.
type Source struct {
	Client *oauth.Client
	// Now is the clock, injectable because every decision here is an expiry
	// comparison.
	Now func() time.Time
	// Skew is how long before expiry a token counts as expired.
	Skew time.Duration
	// LockTimeout bounds the wait for Claude Code's refresh lock.
	LockTimeout time.Duration

	// Log records every refresh grant this Source SPENDS. Nil is silent.
	//
	// A refresh token is single-use: minting a new pair revokes the old one. So
	// a mint nobody recorded is a credential destroyed leaving nothing behind
	// but a file mtime, and that is how the 2026-09-01 logout read as causeless
	// -- five grants were spent between 22:45 and 03:28 while daemon.log's
	// entire account of those eight hours was "tick still failing". See logMint
	// for what one line has to say.
	Log func(format string, a ...any)
}

// New returns a Source with the production client and clock.
func New() *Source {
	return &Source{
		Client:      oauth.NewClient(),
		Now:         time.Now,
		Skew:        DefaultSkew,
		LockTimeout: DefaultLockTimeout,
	}
}

// AccessToken returns a usable bearer for one managed account.
//
// THE LIVE ACCOUNT IS THE WHOLE DESIGN HERE, and what it turns on is WHEN.
// Rotating a refresh token revokes the old one, so two rotations of the same
// login race, and the loser's copy is dead. That left three ways out: write the
// live file too, hold Claude Code's lock across the network call (cclock
// forbids that — Claude Code refreshes under those same locks, so holding one
// stalls it for a full round trip), or leave the live login to Claude Code.
//
// This used to take the third, and the third is what broke. Claude Code rotates
// correctly and under its own locks, but it leaves ccdad's stored snapshot a
// generation behind — and a snapshot that no longer matches the file is a
// snapshot nothing can attribute, which the engine then read as "nobody is
// live" and installed straight back over the running session.
//
// So it takes the first, on a schedule that makes the race impossible rather
// than merely unlikely. Claude Code refreshes only inside its own five-minute
// window (cclink.SelfRefreshThreshold); ccdad refreshes only in the band ABOVE
// that window and below LiveRefreshLead, where Claude Code's own gate answers
// "not_needed" every time it looks. One rotation, performed by whichever side
// owns that stretch of the token's life, written to both copies. See
// refreshLive for the ordering, which still never puts a network call under a
// Claude Code lock.
//
// Below the band the login is Claude Code's: this reads what Claude Code has,
// adopts it into the stored snapshot so a later `ccdad switch` back is not
// carrying a dead token, and returns it. Claude Code's `.oauth_refresh.lock` IS
// taken for that read — it is the only thing that stops the read landing
// between Claude Code's write of a new access token and its write of the
// matching refresh token. An INACTIVE account takes no Claude Code lock at all:
// nothing in ~/.claude is being read or written for it, and taking a lock over
// a file that is not involved would stall Claude Code for no reason.
//
// A live login already inside Claude Code's window answers ErrLiveTokenStale
// rather than being refreshed. Claude Code refreshes it the next time it runs,
// and this Source adopts the result on the next tick — and reaching that window
// at all means Claude Code has not run since the band opened, so there is no
// session whose rotation is urgent.
func (s *Source) AccessToken(ctx context.Context, uuid string) (string, error) {
	st, err := store.Open()
	if err != nil {
		return "", err
	}
	stored, err := st.Credentials(uuid)
	if err != nil {
		return "", err
	}
	rec, ok := recordOf(stored)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNoOAuthCredential, uuid)
	}

	// An unreadable live store does not make this account INACTIVE. It makes
	// its liveness UNKNOWN, and those are opposite answers, because the
	// inactive branch below SPENDS the account's refresh token. The grant is
	// rotating and single-use, so spending one that somebody else is still
	// holding — Claude Code here, a `ccdad run` session in its own credential
	// home, another machine — revokes their copy, and the next thing they do
	// with it is told the refresh token expired.
	//
	// This is rotation-stomp invariant 1, "cannot attribute the live file" is
	// not "nobody is live". The switcher honours it and this Source did not:
	// on 2026-09-01 a login keychain that had locked answered
	// errSecInteractionNotAllowed for eight hours, cclink.Load reported that
	// rather than falling back to the file, isLiveLogin then answered false for
	// every account, and every managed account's grant was rotated from here
	// with no log line and no live-store write.
	//
	// Serving a stored token that is still good stays allowed: it spends
	// nothing and takes no lock. Only the rotation is refused, which is also
	// what makes refresh's own precondition true rather than merely assumed.
	//
	// The live branch needs no guard of its own: a failed Load answers a nil
	// blob, isLiveLogin refuses that, and liveToken re-reads under the lock and
	// reports the same failure if it is ever reached another way.
	live, liveErr := cclink.Load()
	if isLiveLogin(rec, live) {
		return s.liveToken(ctx, uuid, rec)
	}

	if !s.expired(rec) {
		return rec.AccessToken, nil
	}
	if liveErr != nil {
		return "", fmt.Errorf("%w: %q: %v", ErrLivenessUnknown, uuid, liveErr)
	}
	return s.refresh(ctx, uuid, rec)
}

// liveToken answers for the account Claude Code is logged in as.
func (s *Source) liveToken(ctx context.Context, uuid string, stored record) (string, error) {
	lockDir, err := cclock.OAuthRefreshLockDir()
	if err != nil {
		return "", err
	}
	lock, err := cclock.Acquire(lockDir, cclock.Options{
		Stale:   cclock.RefreshStale,
		Timeout: s.LockTimeout,
	})
	if err != nil {
		return "", fmt.Errorf("waiting for Claude Code's refresh lock: %w", err)
	}
	live, rerr := cclink.Load()
	// Release's own error is part of the answer — it performs the synchronous
	// re-stat that is the only check able to see a takeover since the touch
	// goroutine's last tick — so it is joined rather than dropped. Nothing is
	// written under this lock, so a takeover only means the read may be torn;
	// reporting it is what lets the caller retry instead of believing it.
	err = errors.Join(rerr, lock.Release())
	if err != nil {
		return "", fmt.Errorf("reading the live login: %w", err)
	}

	current, ok := recordOf(live)
	if !ok {
		// The file changed under us between the identity check and this read.
		return "", fmt.Errorf("%w: the live credentials file no longer holds an OAuth login", ErrUnavailable)
	}

	// Adopt whatever Claude Code has rotated to, so a later `ccdad switch` back
	// to this account does not install a token the server has already revoked.
	if current.AccessToken != stored.AccessToken || current.RefreshToken != stored.RefreshToken {
		if err := s.save(uuid, current.raw, stored.RefreshToken); err != nil {
			return "", err
		}
	}
	if s.expired(current) {
		return "", fmt.Errorf("%w: %q", ErrLiveTokenStale, uuid)
	}
	// Ahead of Claude Code, or not at all. See refreshLive.
	if s.dueForLiveRefresh(current) {
		return s.refreshLive(ctx, uuid, current)
	}
	return current.AccessToken, nil
}

// LiveRefreshLead is how far ahead of expiry ccdad rotates the LIVE login
// itself, and it is the whole preventive half of this package.
//
// The band it opens is (cclink.SelfRefreshThreshold, LiveRefreshLead]. Below
// the floor the login belongs to Claude Code: it is inside Claude Code's own
// refresh window, Claude Code is about to spend the grant under its own locks,
// and a second spender there is the double-refresh hazard. Above the ceiling
// there is nothing to do. Between them Claude Code has no reason to refresh at
// all — its gate returns "not_needed" on every look — so the rotation can be
// performed once, written to both copies, and never race anything.
//
// That is what makes the difference between guarding against the loop and
// preventing it. The guards elsewhere stop a rotation ccdad did not perform
// from destroying an account; this stops the rotation from being one ccdad did
// not perform.
//
// Thirty minutes, against a five-minute floor, leaves a twenty-five minute band
// on an eight-hour token. The live account is polled far more often than that,
// so the band is entered and acted on long before it closes, and a machine
// asleep across the whole band simply wakes to Claude Code's own refresh and
// the adoption path — which is the behaviour this had before.
const LiveRefreshLead = 30 * time.Minute

// dueForLiveRefresh reports whether the live login sits in that band.
//
// A record with no expiry answers false, for expired's reason: there is no
// deadline to get ahead of, and rotating on every poll because a field is
// missing would burn the token endpoint.
func (s *Source) dueForLiveRefresh(rec record) bool {
	if rec.ExpiresAt.IsZero() {
		return false
	}
	return !s.now().Add(LiveRefreshLead).Before(rec.ExpiresAt)
}

// refreshLive rotates the live login and writes the result to BOTH copies.
//
// Writing both is the point, and it is the option the old comment on
// AccessToken listed and declined. Declining it left Claude Code to rotate the
// live login alone, which it does correctly — and which leaves ccdad's stored
// snapshot a generation behind, unable to name the file it is looking at. The
// cost of that showed up as the engine installing the stale snapshot back over
// the fresh one.
//
// The ordering rule is unchanged and is what makes this safe: the POST happens
// with NO Claude Code lock held. The lock is taken twice around it instead,
// once for the read that decides and once for the write that lands, which is
// the same shape cclink.ActivateWith already has.
//
// The write is conditional on the live file still holding the exact grant that
// was spent. If it does not, something else rotated in the meantime and this
// call cannot tell whose login is in the file now — writing there could install
// one account's pair over another's. The minted pair still goes to the STORE,
// so nothing is lost: the next tick finds a file it cannot name, resolves it,
// and adopts whatever is really there.
//
// A failed rotation is not an error to the caller. The access token in hand is
// still valid — that is what being above the floor means — so the poll this was
// called for proceeds, and Claude Code refreshes at its own threshold as it
// always did.
func (s *Source) refreshLive(ctx context.Context, uuid string, current record) (string, error) {
	fresh, err := s.Client.Refresh(ctx, oauth.RefreshParams{
		RefreshToken:     current.RefreshToken,
		StoredScopes:     current.Scopes,
		SubscriptionType: current.SubscriptionType,
		ClientID:         current.ClientID,
	})
	if err != nil {
		return current.AccessToken, nil
	}
	updated, err := current.apply(fresh, s.now())
	if err != nil {
		return current.AccessToken, nil
	}

	// The live file first, under the lock, conditional on the grant that was
	// spent still being the one in it. cclink.ActivateWith is the locked
	// read-decide-write; ErrNoChange from the decision leaves the file alone.
	werr := activateWith(func(live cclink.Blob) (cclink.Blob, error) {
		at, ok := recordOf(live)
		if !ok || at.RefreshToken != current.RefreshToken {
			return nil, cclink.ErrNoChange
		}
		next := cclink.Extract(live)
		next["claudeAiOauth"] = updated
		return next, nil
	})
	// The store second, and unconditionally on the live write: a pair that was
	// minted and not recorded is a pair nothing can ever use again.
	serr := s.save(uuid, updated, current.RefreshToken)
	// Before either refusal returns, because both of them return NIL: the two
	// branches below hand the caller a token and no error, so this line is the
	// only thing that can say a grant was spent and one copy did not take it.
	s.logMint(uuid, true, serr, werr)
	if serr != nil {
		return current.AccessToken, nil
	}
	if werr != nil && !errors.Is(werr, cclink.ErrNoChange) {
		return current.AccessToken, nil
	}
	return fresh.AccessToken, nil
}

// refresh trades an inactive account's stored refresh token for a new pair.
//
// The store lock is NOT held across the network call. It is a flock, so it has
// no staleness to lose, but a thirty-second token round trip would stall every
// CLI command that wants to write for its whole duration. The read is taken
// before the call and the write after it, and the write re-reads under the lock
// — see save.
//
// No Claude Code lock is taken at any point: this account is not the live
// login, so nothing in ~/.claude is being read or written for it.
func (s *Source) refresh(ctx context.Context, uuid string, rec record) (string, error) {
	fresh, err := s.Client.Refresh(ctx, oauth.RefreshParams{
		RefreshToken:     rec.RefreshToken,
		StoredScopes:     rec.Scopes,
		SubscriptionType: rec.SubscriptionType,
		ClientID:         rec.ClientID,
	})
	if err != nil {
		return "", classify(err)
	}

	updated, err := rec.apply(fresh, s.now())
	if err != nil {
		return "", err
	}
	serr := s.save(uuid, updated, rec.RefreshToken)
	// After the save and before its error returns. The grant is already spent
	// either way, and the error the caller gets names a store that would not
	// write -- never the rotation that made writing necessary.
	s.logMint(uuid, false, serr, nil)
	if serr != nil {
		return "", serr
	}
	return fresh.AccessToken, nil
}

// logMint records one spent grant, and names which copies took the replacement.
//
// It is written per OUTCOME rather than as one line with fields because the
// outcomes want different words: a store that refused the new pair has lost it
// outright, and a live store that refused it has left Claude Code holding a
// grant this rotation just revoked. Those are the two that end in a logout, and
// neither returns an error to anybody.
//
// No token text is ever formatted here. The uuid is the account's primary key
// and says everything a reader needs; the secret says nothing a reader needs
// and would be in a file that rotates.
func (s *Source) logMint(uuid string, live bool, storeErr, liveErr error) {
	if s.Log == nil {
		return
	}
	who := "not the live login"
	if live {
		who = "the live Claude Code login"
	}
	switch {
	case storeErr != nil:
		s.Log("refreshed the grant for %s (%s), but the ccdad store refused the new pair: %v; "+
			"the grant it replaced is revoked and the replacement is held nowhere", uuid, who, storeErr)
	case errors.Is(liveErr, cclink.ErrNoChange):
		s.Log("refreshed the grant for %s (%s): the live credentials no longer held the grant that "+
			"was spent, so the new pair is in the ccdad store alone", uuid, who)
	case liveErr != nil:
		s.Log("refreshed the grant for %s (%s), but Claude Code's credential store refused the new "+
			"pair: %v; Claude Code is still holding the grant this rotation revoked", uuid, who, liveErr)
	case live:
		s.Log("refreshed the grant for %s (%s): the new pair is in Claude Code's credential store "+
			"and in the ccdad store", uuid, who)
	default:
		s.Log("refreshed the grant for %s (%s): the new pair is in the ccdad store", uuid, who)
	}
}

// save writes a claudeAiOauth record back into the account's stored snapshot,
// under the store lock, re-reading inside it.
//
// expect is the refresh token that was on disk when this operation started. The
// re-read compares against it, and that is what makes two processes refreshing
// the same account survivable rather than merely unlikely. Refresh is not
// idempotent: if something else rotated this account while the request was in
// flight, one of the two tokens has already been revoked and there is no way
// from here to tell which. The stored one wins, because whoever wrote it is
// already using it — writing ours over the top would put a token in the file
// that its own holder does not know about. The real mitigation is upstream: the
// daemon singleton means only one process polls.
func (s *Source) save(uuid string, updated json.RawMessage, expect string) error {
	return store.WithStore(func(st *store.Store) error {
		current, err := st.Credentials(uuid)
		if err != nil {
			return err
		}
		if theirs, ok := recordOf(current); ok && theirs.RefreshToken != expect {
			// Something rotated this account while the request was in flight.
			return nil
		}
		next := cclink.Blob{}
		for k, v := range current {
			next[k] = v
		}
		next["claudeAiOauth"] = updated

		acct, ok := st.Get(uuid)
		if !ok {
			return fmt.Errorf("%w: %q", store.ErrNotFound, uuid)
		}
		return st.Add(acct, next)
	})
}

func (s *Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Source) skew() time.Duration {
	if s.Skew > 0 {
		return s.Skew
	}
	return DefaultSkew
}

// expired reports whether a record's access token is past use.
//
// A record with no expiry answers false: it is what a hand-written or very old
// credential looks like, and refreshing on every single tick because a field is
// missing would burn the token endpoint for nothing. The 401 that follows is
// recoverable; the request storm is not.
func (s *Source) expired(rec record) bool {
	if rec.ExpiresAt.IsZero() {
		return false
	}
	return !s.now().Add(s.skew()).Before(rec.ExpiresAt)
}

// classify maps a token-endpoint failure onto the two outcomes a caller may act
// on, and it is the one place the distinction is drawn.
//
// Only TokenErrorInvalidCode says anything about the ACCOUNT: a 401, or any
// status carrying RFC 6749's invalid_grant, means this refresh token is dead.
// TokenErrorTransport is a laptop on a plane and TokenErrorStatus is Anthropic
// having a bad minute; quarantining the fleet on either would turn one outage
// into a manual re-login for every account. TokenErrorInvalidScope has already
// had its one retry inside oauth.Refresh, and surfacing here still means the
// ENDPOINT refused the scope set rather than the credential.
func classify(err error) error {
	var te *oauth.TokenError
	if !errors.As(err, &te) {
		// A context cancellation, or a malformed body. Neither is evidence
		// about the account.
		return err
	}
	if te.Kind == oauth.TokenErrorInvalidCode {
		return fmt.Errorf("%w: %w", ErrRejected, err)
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// record is the parsed shape of a claudeAiOauth object, plus the raw bytes it
// came from so unknown fields can be written back untouched.
type record struct {
	AccessToken      string
	RefreshToken     string
	ExpiresAt        time.Time
	Scopes           []string
	SubscriptionType string
	ClientID         string

	raw json.RawMessage
}

func recordOf(b cclink.Blob) (record, bool) {
	raw, ok := b["claudeAiOauth"]
	if !ok {
		return record{}, false
	}
	return parseRecord(raw)
}

func parseRecord(raw json.RawMessage) (record, bool) {
	var wire struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		// ExpiresAt is in MILLISECONDS. oauth.TokenResponse.ExpiresIn is in
		// SECONDS, and the credential writer bridges them as `now +
		// expires_in*1000`. Comparing the two directly puts every expiry
		// roughly fifty years in the past, which refreshes on every tick and
		// burns the token endpoint — the exact failure this comment exists to
		// stop the next reader reintroducing.
		ExpiresAt        *int64   `json:"expiresAt"`
		Scopes           []string `json:"scopes"`
		SubscriptionType string   `json:"subscriptionType"`
		ClientID         string   `json:"clientId"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return record{}, false
	}
	if wire.AccessToken == "" && wire.RefreshToken == "" {
		return record{}, false
	}
	rec := record{
		AccessToken:      wire.AccessToken,
		RefreshToken:     wire.RefreshToken,
		Scopes:           wire.Scopes,
		SubscriptionType: wire.SubscriptionType,
		ClientID:         wire.ClientID,
		raw:              raw,
	}
	if wire.ExpiresAt != nil {
		rec.ExpiresAt = time.UnixMilli(*wire.ExpiresAt)
	}
	return rec, true
}

// apply folds a token response into the stored record, preserving every field
// the response did not speak to.
//
// The round-trip rule: decode as map[string]json.RawMessage and replace only
// what is being changed. clauth's typed struct drops refreshTokenExpiresAt,
// rateLimitTier and clientId on every re-serialize, and clientId is what a
// revocation request needs.
func (rec record) apply(fresh *oauth.TokenResponse, now time.Time) (json.RawMessage, error) {
	payload := map[string]any{}
	if len(rec.raw) > 0 {
		// A record we cannot parse as an object is replaced rather than
		// merged: there is nothing in it to preserve.
		_ = json.Unmarshal(rec.raw, &payload)
	}
	ms := now.UnixMilli()
	payload["accessToken"] = fresh.AccessToken
	payload["refreshToken"] = fresh.RefreshToken
	// Seconds in, milliseconds out. See parseRecord.
	payload["expiresAt"] = ms + fresh.ExpiresIn*1000
	if fresh.RefreshTokenExpiresIn > 0 {
		payload["refreshTokenExpiresAt"] = ms + fresh.RefreshTokenExpiresIn*1000
	}
	// An empty scope string means the endpoint did not restate the set, which
	// is not the same as restating it as empty.
	if scopes := strings.Fields(fresh.Scope); len(scopes) > 0 {
		payload["scopes"] = scopes
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding the refreshed credential: %w", err)
	}
	return encoded, nil
}

// isLiveLogin reports whether this stored record is the login Claude Code is
// currently using. Both tokens are compared because a record can carry either
// one alone, which is the same anchor `ccdad which` attributes on.
func isLiveLogin(rec record, live cclink.Blob) bool {
	current, ok := recordOf(live)
	if !ok {
		return false
	}
	switch {
	case rec.RefreshToken != "" && current.RefreshToken != "":
		return rec.RefreshToken == current.RefreshToken
	case rec.AccessToken != "" && current.AccessToken != "":
		return rec.AccessToken == current.AccessToken
	}
	return false
}

// Freshen refreshes an account's stored credential and returns the snapshot a
// switch should install in its place.
//
// It exists because a switch and a poll want the same work and stop at
// different places: the poll wants a bearer, the switch wants the RECORD, and
// the record is what AccessToken leaves behind in the store rather than what it
// returns. So this runs the same path and reads the result back, instead of a
// second refresh-and-save that could disagree with the first.
//
// THE LIVE LOGIN IS NOT REFRESHED HERE either, and it does not need its own
// branch to be safe: AccessToken answers ErrLiveTokenStale for a live account
// whose token has expired, which arrives at the caller as a refusal to switch
// rather than as a rotation performed behind a running session. switcher's own
// exemption for the account it believes is live is the first line of that
// defence; this is the second, and the one that holds when the belief is stale.
func (s *Source) Freshen(ctx context.Context, uuid string) (cclink.Blob, error) {
	if _, err := s.AccessToken(ctx, uuid); err != nil {
		return nil, err
	}
	st, err := store.Open()
	if err != nil {
		return nil, err
	}
	return st.Credentials(uuid)
}

// activateWith is cclink's locked read-decide-write, as a var so a test can
// describe a live file that moved under the lock without arranging one.
var activateWith = cclink.ActivateWith
