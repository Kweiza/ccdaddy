package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Kweiza/ccdaddy/internal/store"
)

// OutcomeKind is what one refresh attempt learned.
//
// Four values and not two, because "it failed" is three different facts with
// three different consequences: the grant is dead and only a new login helps;
// somebody else already rotated it and this caller should use theirs; or
// nothing was learned at all and the account must not be marked.
type OutcomeKind int

const (
	// Rotated: this call exchanged the grant and stored the new tokens.
	Rotated OutcomeKind = iota
	// Adopted: another refresher had already rotated it, so this call spent
	// nothing and returns the stored pair.
	Adopted
	// Terminal: the grant is dead. Code names why.
	Terminal
	// Transient: nothing was learned about the grant. Code names the shape.
	// This is NEVER a reason to mark an account.
	Transient
)

func (k OutcomeKind) String() string {
	switch k {
	case Rotated:
		return "rotated"
	case Adopted:
		return "adopted"
	case Terminal:
		return "terminal"
	case Transient:
		return "transient"
	}
	return "unknown"
}

// Outcome is one refresh attempt's result. Credential is the pair the caller
// should use: the rotated one after Rotated, the stored one after Adopted, and
// whatever was on disk after either failure — a caller with a live access token
// can still serve a request while the grant behind it is being sorted out.
//
// It is the ZERO Credential in exactly one case: a Transient whose Code is
// "lock_busy" raised on the READ, where nothing could be read off disk to hand
// back. A "lock_busy" raised after a successful rotation carries the rotated
// pair, because that pair exists and is the only copy of it anywhere.
type Outcome struct {
	Kind       OutcomeKind
	Code       string
	Credential Credential
}

// terminalCodes are the three the issuer uses to say the grant itself is gone.
// Refresh tokens are single-use with server-side reuse detection, so a token
// presented twice is not a retryable failure — it is a token that has been
// burned and an account that needs a new login.
var terminalCodes = map[string]bool{
	"refresh_token_expired":     true,
	"refresh_token_reused":      true,
	"refresh_token_invalidated": true,
}

// DefaultRefreshCooldown is how long a transiently-failed account is left
// alone.
//
// It exists because of what the CLIENT does with a 401: codex retries six
// times over six seconds. Without a cooldown each of those six reaches the
// token endpoint, so one bad minute upstream becomes six exchanges against a
// single-use grant.
const DefaultRefreshCooldown = 30 * time.Second

// Classify maps a token-endpoint answer to Terminal or Transient.
//
// It is Codex-specific and deliberately NOT ccdad's Claude-side wire
// classifier. That one reports a reused refresh token as transient, which is
// correct for Claude Code's rotation model and exactly wrong here: a Codex
// grant is single-use with reuse detection, so retrying a reused token is a
// loop that can only ever fail, once per client retry, forever.
//
// TERMINAL iff the endpoint answered 401, or the body's error code is one of
// the three, or a 400 said invalid_grant. RFC 6749 reports an unusable refresh
// token as a bare invalid_grant without the expired/reused/revoked subtype, so
// that arm is what keeps a modern answer terminal.
//
// Everything else — transport failure, 5xx, a 429 from the token endpoint, a
// cancelled context — is TRANSIENT and does not count as an attempt.
func Classify(status int, body []byte, err error) (OutcomeKind, string) {
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return Transient, "cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			return Transient, "timeout"
		}
		return Transient, "transport"
	}

	code := strings.ToLower(errorCodeOf(body))
	if terminalCodes[code] {
		return Terminal, code
	}
	if status == http.StatusBadRequest && code == "invalid_grant" {
		return Terminal, "invalid_grant"
	}
	if status == http.StatusUnauthorized {
		if code != "" {
			return Terminal, code
		}
		return Terminal, "unauthorized"
	}
	return Transient, fmt.Sprintf("http_%d", status)
}

// errorCodeOf reads the code out of a token-endpoint error body, in the three
// shapes the endpoint uses: `error` as an object with a `code`, `error` as a
// bare string, or a top-level `code`.
//
// It returns "" for anything else, including a body that is not JSON. The BODY
// never reaches a caller or a log — it can echo a value ccdad sent.
func errorCodeOf(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	if e, ok := raw["error"]; ok {
		var obj struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(e, &obj); err == nil && obj.Code != "" {
			return obj.Code
		}
		var s string
		if err := json.Unmarshal(e, &s); err == nil && s != "" {
			return s
		}
	}
	var s string
	if c, ok := raw["code"]; ok {
		if err := json.Unmarshal(c, &s); err == nil {
			return s
		}
	}
	return ""
}

// RefresherConfig builds a Refresher. Every field has a working default, so a
// caller that only has an *http.Client passes only that.
type RefresherConfig struct {
	Client   *http.Client
	Now      func() time.Time
	Log      func(format string, a ...any)
	Cooldown time.Duration
}

// Refresher is the ONE thing in a ccdad process that exchanges a Codex refresh
// token.
//
// It exists as a type rather than a function because the single-use grant needs
// a place to keep two facts: which account is being exchanged right now, and
// which account is cooling down. Both are per-account, both outlive one call,
// and neither may be a package-level global — a test would then leak state into
// the next one.
//
// The lock discipline, which is the whole of its correctness:
//
//   - a per-uuid in-process mutex is held across re-read + POST + save, so two
//     goroutines cannot both read the same grant and both exchange it;
//   - the store's cross-process flock is taken INSIDE that, twice — once for
//     the read and once for the write — and NEVER across the POST. Holding a
//     file lock across a network call parks every other ccdad process on the
//     machine for as long as the endpoint takes to answer.
//
// Waiters block on the mutex, re-read, find an access token that is no longer
// the one they saw, and adopt.
//
// The cost of taking the file lock twice rather than once is that the WRITE can
// fail on contention after the POST has already spent the grant. That rotated
// pair is then the only copy there is, so it is held in refreshState.pending
// and landed at the top of the next call. Dropping it is not an option: the
// stored refresh token is burned the moment the exchange succeeds, so a
// discarded rotation leaves the account one refresh away from reuse detection.
type Refresher struct {
	client   *http.Client
	now      func() time.Time
	log      func(format string, a ...any)
	cooldown time.Duration

	mu       sync.Mutex
	accounts map[string]*refreshState
}

// refreshState is one account's. Its mutex is what serializes the exchange;
// cooldownUntil is only ever EXTENDED, so a lucky read cannot shorten a
// backoff another failure earned.
type refreshState struct {
	mu            sync.Mutex
	cooldownUntil time.Time
	// pending is a rotated pair the store could not take because another
	// process held its lock. The grant behind it is already spent, so it is
	// landed ahead of anything else on the next call.
	pending *Credential
}

// NewRefresher builds one. A process has exactly one, shared by everything that
// can see a 401 for a Codex account.
func NewRefresher(cfg RefresherConfig) *Refresher {
	r := &Refresher{
		client:   cfg.Client,
		now:      cfg.Now,
		log:      cfg.Log,
		cooldown: cfg.Cooldown,
		accounts: map[string]*refreshState{},
	}
	if r.client == nil {
		r.client = &http.Client{
			Timeout: 30 * time.Second,
			// The POSTed body IS the refresh grant, and it is single-use: a
			// 307 or 308 from the token host would make Go's default client
			// replay that body — the refresh token itself — to whatever
			// target the redirect names. Returning http.ErrUseLastResponse
			// stops the client from following it and hands the redirect
			// response back unchanged, which Classify then judges on its own
			// terms instead of on the followed target's.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.log == nil {
		r.log = func(string, ...any) {}
	}
	if r.cooldown <= 0 {
		r.cooldown = DefaultRefreshCooldown
	}
	return r
}

// state is uuid's slot, created on first use.
func (r *Refresher) state(uuid string) *refreshState {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.accounts[uuid]
	if !ok {
		st = &refreshState{}
		r.accounts[uuid] = st
	}
	return st
}

type refreshRequest struct {
	ClientID     string `json:"client_id"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
}

type refreshResponse struct {
	IDToken      *string `json:"id_token"`
	AccessToken  *string `json:"access_token"`
	RefreshToken *string `json:"refresh_token"`
}

// Refresh exchanges uuid's grant, or explains why it did not.
//
// triggeredBy is the access token the caller SAW. If the stored one differs,
// another refresher has already rotated and this call adopts rather than
// spending a second grant. An empty triggeredBy means "the caller has no
// opinion" and always attempts.
//
// The error return is for the things that are not outcomes: the store cannot be
// opened, the account is not there, its credential is not a Codex one. Every
// refusal ABOUT THE GRANT is an Outcome with a nil error — and so is store
// CONTENTION, which is Transient with the code "lock_busy": another ccdad
// process holding the file lock says nothing about the grant, and it is not
// even an attempt, so it earns no cooldown either.
func (r *Refresher) Refresh(ctx context.Context, uuid, triggeredBy string) (Outcome, error) {
	st := r.state(uuid)
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.pending != nil {
		// A previous rotation spent the grant and could not be written. Land it
		// before reading anything, or the read below hands back a refresh token
		// the issuer has already burned.
		if err := store.WithStore(func(s *store.Store) error {
			return s.SetCredentials(uuid, st.pending.ToBlob())
		}); err != nil {
			if errors.Is(err, store.ErrLockBusy) {
				return Outcome{Kind: Transient, Code: "lock_busy", Credential: *st.pending}, nil
			}
			return Outcome{}, fmt.Errorf("storing the refreshed Codex credential: %w", err)
		}
		st.pending = nil
	}

	acct, cred, err := readCodexCredential(uuid)
	if err != nil {
		if errors.Is(err, store.ErrLockBusy) {
			// Another process holds the store. Nothing was learned about the
			// grant and the token endpoint was never reached, so this is a
			// pause and not a verdict -- and not a cooldown either, because no
			// attempt was made.
			return Outcome{Kind: Transient, Code: "lock_busy"}, nil
		}
		return Outcome{}, err
	}

	// Elsewhere first, ahead of every other arm. The account belongs to another
	// machine's ccdad, so rotating it here burns a grant that one is about to
	// present.
	if acct.Elsewhere {
		return Outcome{Kind: Transient, Code: "elsewhere", Credential: cred}, nil
	}
	if triggeredBy != "" && cred.AccessToken != triggeredBy {
		return Outcome{Kind: Adopted, Credential: cred}, nil
	}
	// After the adopt check, never before it: an adopt costs nothing and must
	// not be held back by a cooldown another failure earned.
	if now := r.now(); now.Before(st.cooldownUntil) {
		return Outcome{Kind: Transient, Code: "cooldown", Credential: cred}, nil
	}

	body, err := json.Marshal(refreshRequest{
		ClientID:     ClientID,
		GrantType:    "refresh_token",
		RefreshToken: cred.RefreshToken,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("building the refresh request: %w", err)
	}

	// The store lock is NOT held here. See the note on Refresher.
	raw, status, postErr := post(ctx, r.client, authBase+tokenPath, "application/json", bytes.NewReader(body))
	if postErr != nil || status != http.StatusOK {
		return r.failed(uuid, st, cred, status, raw, postErr), nil
	}

	var res refreshResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		// A 200 whose body cannot be read tells nothing about the grant.
		st.extendCooldown(r.now().Add(r.cooldown))
		return Outcome{Kind: Transient, Code: "unreadable", Credential: cred}, nil
	}

	next := cred
	// Each field is overwritten ONLY when present. The response's three fields
	// are all optional, and writing an absent one blanks a token that is still
	// good.
	if res.IDToken != nil {
		next.IDToken = *res.IDToken
	}
	if res.AccessToken != nil {
		next.AccessToken = *res.AccessToken
	}
	if res.RefreshToken != nil {
		next.RefreshToken = *res.RefreshToken
	}
	next.LastRefresh = r.now().UTC()

	if err := store.WithStore(func(s *store.Store) error {
		return s.SetCredentials(uuid, next.ToBlob())
	}); err != nil {
		if errors.Is(err, store.ErrLockBusy) {
			// The grant is spent and the new pair exists only here. Keep it and
			// hand it out: a caller can serve with it now, and the next call
			// lands it before it reads the stale pair off disk.
			//
			// Dropping it instead is the failure this arm exists for. The
			// stored refresh token has already been burned by the POST above,
			// so the account's next exchange would present a spent grant, be
			// reuse-detected, and go Terminal -- an account that needs a new
			// login because another process held a file lock for a moment.
			pending := next
			st.pending = &pending
			r.log("codex refresh: %s rotated and the store was busy; the new pair is held until the next call", uuid)
			return Outcome{Kind: Rotated, Credential: next}, nil
		}
		return Outcome{}, fmt.Errorf("storing the refreshed Codex credential: %w", err)
	}
	return Outcome{Kind: Rotated, Credential: next}, nil
}

// failed turns a refused exchange into an Outcome, marking the account only
// when the grant itself is dead.
//
// st is the caller's own refreshState, already locked by Refresh for the
// whole call -- failed takes it as a parameter rather than looking it up
// again so a caller cannot pass a state it does not hold the lock on.
func (r *Refresher) failed(uuid string, st *refreshState, cred Credential, status int, body []byte, postErr error) Outcome {
	kind, code := Classify(status, body, postErr)
	// Every attempt against the endpoint earns the cooldown, Terminal
	// included: DefaultRefreshCooldown exists because codex's client retries
	// six times in six seconds, and a dead grant is exactly the case that
	// backoff has to cover — without it, a token the issuer has already
	// flagged as reused would be re-presented on all six of those retries,
	// and again on every poll after, until a human logs in again.
	st.extendCooldown(r.now().Add(r.cooldown))
	if kind != Terminal {
		return Outcome{Kind: Transient, Code: code, Credential: cred}
	}

	// Compare-and-swap on the rejected token. Written unconditionally, a slow
	// terminal answer would quarantine an account whose grant a concurrent
	// `ccdad codex add` had already replaced — and the mark would then match
	// nothing and never clear.
	hash := RefreshTokenHash(cred.RefreshToken)
	if err := store.WithStore(func(s *store.Store) error {
		return s.SetCodexReloginFor(uuid, hash, hash)
	}); err != nil {
		r.log("codex refresh: %s is terminal (%s) and the relogin mark could not be written: %v", uuid, code, err)
		return Outcome{Kind: Terminal, Code: code, Credential: cred}
	}
	// Once, with the uuid and the code. Never a token, never a body.
	r.log("codex refresh: %s needs a new login (%s)", uuid, code)
	return Outcome{Kind: Terminal, Code: code, Credential: cred}
}

// extendCooldown is monotonic: a cooldown can be lengthened and never cut
// short, so two failures a second apart leave the LATER deadline standing.
//
// Its caller must already hold st.mu: this mutates st.cooldownUntil with no
// locking of its own.
func (st *refreshState) extendCooldown(until time.Time) {
	if until.After(st.cooldownUntil) {
		st.cooldownUntil = until
	}
}

// readCodexCredential re-reads the account and its credential under the store
// lock, so what is exchanged is what is on disk right now rather than what the
// caller read some time ago.
func readCodexCredential(uuid string) (store.Account, Credential, error) {
	var acct store.Account
	var cred Credential
	err := store.WithStore(func(s *store.Store) error {
		a, ok := s.Get(uuid)
		if !ok {
			return fmt.Errorf("no stored account %q", uuid)
		}
		acct = a
		blob, err := s.Credentials(uuid)
		if err != nil {
			return err
		}
		c, ok, err := FromBlob(blob)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%q has no Codex credential", uuid)
		}
		cred = c
		return nil
	})
	if err != nil {
		return store.Account{}, Credential{}, err
	}
	return acct, cred, nil
}
