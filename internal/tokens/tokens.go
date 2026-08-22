// Package tokens hands out a valid access token for a managed account,
// refreshing the stored credential snapshot when one has expired.
//
// It exists because polling N accounts means holding N valid bearers, and every
// stored access token expires in about eight hours. Without it, `oauth.Refresh`
// has no production caller at all: every inactive account answers "unknown"
// after the first day, and §7.2's unknown-is-not-zero rule then leaves the
// engine with nothing it may rank.
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
	// and the only one §7.2 may quarantine on.
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

	// ErrLiveTokenStale means the account is the LIVE Claude Code login and its
	// token has expired. ccdad deliberately does not refresh that one; see
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
// THE ACTIVE ACCOUNT IS NOT REFRESHED HERE, and that is the central decision.
// Rotating a refresh token revokes the old one, so refreshing the account
// Claude Code is logged in as — while leaving the live credentials file holding
// the pre-rotation token, which this package never writes — would log the user
// out at Claude Code's very next refresh. The three ways out are: write the
// live file too (this package exists precisely so polling does not have to),
// hold Claude Code's lock across the network call (§12 forbids it), or leave
// the live login to Claude Code, which already rotates it correctly and under
// its own locks.
//
// So for the live login this reads what Claude Code has, adopts it into the
// stored snapshot so a later `ccdad switch` back is not carrying a dead token,
// and returns it. Claude Code's `.oauth_refresh.lock` IS taken for that read —
// it is the only thing that stops the read landing between Claude Code's write
// of a new access token and its write of the matching refresh token. An
// INACTIVE account takes no Claude Code lock at all: nothing in ~/.claude is
// being read or written for it, and taking a lock over a file that is not
// involved would stall Claude Code for no reason.
//
// A live login whose token has already expired answers ErrLiveTokenStale rather
// than being refreshed. Claude Code refreshes it the next time it runs, and
// this Source adopts the result on the next tick — and an eight-hour-old live
// token means Claude Code has not run in eight hours, so there is no session
// whose rotation is urgent.
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

	// An unreadable live file is not an error here: it only means this account
	// cannot be shown to be the live login, and every account is then treated
	// as inactive — which is the branch that touches nothing in ~/.claude.
	live, _ := cclink.Load()
	if isLiveLogin(rec, live) {
		return s.liveToken(uuid, rec)
	}

	if !s.expired(rec) {
		return rec.AccessToken, nil
	}
	return s.refresh(ctx, uuid, rec)
}

// liveToken answers for the account Claude Code is logged in as.
func (s *Source) liveToken(uuid string, stored record) (string, error) {
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
	return current.AccessToken, nil
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
	if err := s.save(uuid, updated, rec.RefreshToken); err != nil {
		return "", err
	}
	return fresh.AccessToken, nil
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
// §4.2 rule 3: decode as map[string]json.RawMessage and replace only what is
// being changed. clauth's typed struct drops refreshTokenExpiresAt,
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
