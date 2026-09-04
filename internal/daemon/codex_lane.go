package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/codexswitch"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// codexTick is the Codex lane: one evaluation, one act, one dispatch.
//
// It is a SEPARATE path rather than a loop around the Claude one, and the order
// mirrors it exactly: rank on the cache as it stands, act on that ranking, then
// dispatch the polls whose answers the NEXT tick will rank. A lane that polled
// first would make every tick wait on a network round trip.
//
// What it never does, and each is a decision rather than an omission:
//
//   - store.ApplyUsage. That function re-files an account's kind from a
//     reading, and a Codex reading has nothing to say about Anthropic metering.
//   - refreshProfile. There is no Anthropic profile behind a Codex account.
//   - probeDue / probeNow. A probe runs a real Claude Code out of a scratch
//     credential home; there is nothing to seed one with here.
//   - warmClamp. It aims the next look at a Claude window's rollover.
//   - switcher.Execute, store.SetActive, cclink.ActivateWith. The act is a
//     pointer file and nothing else.
func (e *Engine) codexTick(ctx context.Context, s *store.Store, cfg config.Config, now time.Time) (switcher.Evaluation, error) {
	var ev switcher.Evaluation
	accounts := s.CodexAccounts()
	if len(accounts) == 0 {
		// Nothing is read, no lock is taken and no file is opened. A machine
		// with only Claude accounts must not pay for this lane at 1 Hz.
		return ev, nil
	}
	root, err := ccpath.StoreHome()
	if err != nil {
		return ev, err
	}

	ev, err = switcher.EvaluateCodex(s, root, switcher.EvalOptions{
		Now:      now,
		Provider: provider.Codex,
		Config:   func() (config.Config, error) { return cfg, nil },
	}, e.CodexBook)
	if err != nil {
		return ev, err
	}

	if ev.HasTarget {
		switch serr := codexswitch.Execute(root, ev.Target.UUID); {
		case serr == nil:
			e.logf("codex: serving %s from the next new thread (%s)",
				ev.Target.Label(), ev.Plan.Reason)
			// publish reads THIS evaluation, and it was taken before the
			// pointer moved. Carry the move into it, or the document names the
			// account that WAS serving and every reader of `status`, the
			// dashboard and doctor is a tick behind the machine. The Claude
			// lane does the same through activeAfter, which folds the switch's
			// result back into what dispatch is told.
			ev.Live, ev.LiveKnown = ev.Target, true
			ev.LastSwitchAt, ev.LastSwitchTo = now, ev.Target.UUID

		case errors.Is(serr, codexswitch.ErrPointerMovedUnstamped):
			// The pointer MOVED -- Execute writes it before it stamps the
			// cooldown -- but the stamp itself failed, so the anti-flap
			// cooldown for this switch is not in force. Fold the move into ev
			// for exactly the reason the success case above does: a caller
			// that skipped this would publish the account that WAS serving
			// while the machine actually serves the new one. Logged distinctly
			// from the success case because the missing stamp means the next
			// tick can repoint again immediately, with nothing holding it back.
			e.logf("codex: serving %s from the next new thread, but its switch cooldown was not recorded: %v",
				ev.Target.Label(), serr)
			ev.Live, ev.LiveKnown = ev.Target, true
			ev.LastSwitchAt, ev.LastSwitchTo = now, ev.Target.UUID

		default:
			// The pointer did NOT move: the machine goes on serving whatever
			// it was serving, and the next tick tries again. Returning would
			// put a filesystem hiccup into the tick loop's wedged-daemon
			// streak.
			e.logf("repointing codex at %s failed: %v", ev.Target.UUID, serr)
		}
	}

	serving, _ := codexswitch.ReadServing(root)
	e.codexDispatch(ctx, s, accounts, cfg, now, serving)
	return ev, nil
}

// codexDispatch starts one poll per due account.
//
// The Elsewhere rule is the mirror of the Claude lane's: an account another
// machine drives is not this machine's to read on a cadence, EXCEPT when it is
// the one serving -- going blind on the account the proxy is spending is how
// the lane loses the baseline its own cooldown is measured against.
func (e *Engine) codexDispatch(ctx context.Context, s *store.Store, accounts []store.Account,
	cfg config.Config, now time.Time, serving string) {

	if e.CodexAccessToken == nil || e.CodexFetchUsage == nil {
		return
	}
	cache, err := usage.LoadCache()
	if err != nil {
		e.logf("the codex lane could not read the usage cache: %v", err)
		return
	}
	thr := codexThresholds(cfg)
	for _, a := range accounts {
		// This function is only ever handed s.CodexAccounts() today, but the
		// guard is not written on trust of the caller: codexReloginSet checks
		// the same field for the same reason, and the Claude lane's own
		// pollable() checks its own provider before anything else. A credential
		// SHAPE check alone (creds[codexauth.Key] below) is not a provider
		// check -- this project has already shipped a gate that held on shape
		// rather than on provider, and a Claude row is never eligible here no
		// matter what a caller passes in.
		if a.Provider != provider.Codex {
			continue
		}
		if a.Elsewhere && a.UUID != serving {
			continue
		}
		creds, cerr := s.Credentials(a.UUID)
		if cerr != nil {
			continue
		}
		if _, ok := creds[codexauth.Key]; !ok {
			continue
		}
		if codexauth.NeedsRelogin(a, creds) {
			// Every request would spend a round trip to be told the same thing.
			// The account's row says needs-relogin, which is the answer.
			continue
		}
		// A reading the proxy already took off a real turn is committed instead
		// of being paid for again with a poll. It was taken by a request the
		// user actually made, so it is both free and more current than anything
		// this lane's fifteen-minute floor can produce.
		if snap, ok := e.CodexSample(a.UUID); ok {
			e.codexCommitHarvested(a, snap, thr, now, a.UUID == serving)
			continue
		}
		entry, has := cache.Get(a.UUID)
		if !codexDue(entry, has, now) {
			continue
		}
		if !e.claim(a.UUID) {
			continue
		}
		e.wg.Add(1)
		go func(a store.Account) {
			defer e.wg.Done()
			defer e.release(a.UUID)
			// The error is deliberately dropped here and returned to
			// Engine.Refresh instead: a failed poll is already recorded in the
			// cache row the next tick reads, and a tick has nobody to report to.
			_ = e.codexPoll(ctx, a, thr, a.UUID == serving)
		}(a)
	}
}

// codexDue is the Claude lane's `due` without the stand-down argument: nothing
// writes a stand-down on this side, because the Codex allowance is per account
// rather than per organization and there is no shared budget to yield.
func codexDue(e usage.Entry, has bool, now time.Time) bool {
	if !has {
		return true
	}
	if e.FreshWithin(now, e.ScheduledTTL()) {
		return false
	}
	at := e.PollAt(false)
	if at.IsZero() {
		return true
	}
	return !now.Before(at)
}

// codexThresholds is the table a Codex reading is judged against. There is no
// per-window key: the Codex response carries two windows and neither has a
// configuration entry of its own.
func codexThresholds(cfg config.Config) strategy.Thresholds {
	d := cfg.Codex.Threshold
	if d <= 0 {
		d = cfg.Threshold
	}
	return strategy.Thresholds{Default: d, Credit: cfg.CreditThreshold}
}

// codexPoll takes one reading. It returns the error so that
// Engine.Refresh -- the `ccdad list --refresh` path, which reports per account
// -- can say what happened; the tick's own dispatch ignores it, because a
// failed poll has already been recorded where the next tick will read it.
func (e *Engine) codexPoll(ctx context.Context, a store.Account, thr strategy.Thresholds, serving bool) error {
	ctx, cancel := context.WithTimeout(ctx, e.pollTimeout())
	defer cancel()

	token, accountID, err := e.CodexAccessToken(ctx, a.UUID)
	if err == nil && e.CodexRefresher != nil {
		// THE PROACTIVE TRIGGER, and it lives here because here is the only
		// place a refresher exists: a token inside an hour of its expiry is
		// rotated by the daemon lane and by nothing else, so that a codex turn
		// never has to eat the 401 that would otherwise be what triggers a
		// rotation. A grant is single-use, so the window is deliberately narrow
		// -- rotating a token with hours left spends the fleet's grants for
		// nothing -- and an expiry that cannot be read at all is not a reason to
		// rotate, which is why AccessExpiry answers unknown rather than zero.
		if exp, known := codexauth.AccessExpiry(codexauth.Credential{AccessToken: token}); known &&
			exp.Sub(e.now()) < time.Hour {
			if out, rerr := e.CodexRefresher.Refresh(ctx, a.UUID, token); rerr != nil {
				e.logf("refreshing %s's codex grant ahead of its expiry failed: %v", a.UUID, rerr)
			} else if out.Kind == codexauth.Rotated || out.Kind == codexauth.Adopted {
				token = out.Credential.AccessToken
				if out.Credential.AccountID != "" {
					accountID = out.Credential.AccountID
				}
			}
		}
	}
	var snap *usage.Snapshot
	if err == nil {
		snap, _, err = e.CodexFetchUsage(ctx, token, accountID)
	}

	now := e.now()
	e.record(a.UUID, now, err)
	if err != nil {
		e.codexFailure(ctx, a, token, err, thr, now, serving)
		return err
	}
	e.codexCommit(a, snap, thr, now, serving, nil)
	return nil
}

// codexFailure decides what a failed Codex poll means. Three classes, and only
// two of them say anything.
func (e *Engine) codexFailure(ctx context.Context, a store.Account, token string,
	err error, thr strategy.Thresholds, now time.Time, serving bool) {

	switch {
	case errors.Is(err, usage.ErrRateLimited):
		var retry time.Duration
		var hasRetry bool
		var se *usage.StatusError
		if errors.As(err, &se) {
			retry, hasRetry = se.RetryAfter()
		}
		e.codexCommit(a, nil, thr, now, serving, func(st pollpolicy.State) pollpolicy.State {
			return pollpolicy.Codex.RateLimited(st, now, retry, hasRetry)
		})
		// And into the book the proxy reads, so that the two halves of ccdad
		// agree about which account is throttled without either asking the
		// other. The entry the commit above just wrote is where the instant
		// comes from, so the poll cadence and the book cannot disagree.
		if entry, ok := e.codexEntry(a.UUID); ok {
			if until, limited := pollpolicy.Codex.RateLimitedUntil(pollStateOf(entry), now); limited {
				e.CodexBook.MarkLimited(a.UUID, until)
			}
		}

	case errors.Is(err, usage.ErrUnauthorized):
		// The token is stale. Only the daemon refreshes a Codex grant, and only
		// through the one shared refresher: a per-poll refresh would spend N
		// grants for one rotation and the endpoint kills a reused one.
		if e.CodexRefresher != nil {
			if out, rerr := e.CodexRefresher.Refresh(ctx, a.UUID, token); rerr != nil {
				e.logf("refreshing %s's codex grant failed: %v", a.UUID, rerr)
			} else if out.Kind == codexauth.Terminal {
				e.logf("%s's codex grant is dead (%s); it needs `ccdad codex add`", a.UUID, out.Code)
			}
		}
		// No reading either way. The next tick polls with whatever the refresher
		// left in the store.
		e.codexCommit(a, nil, thr, now, serving, nil)

	default:
		// A transport failure, a 5xx, a body ccdad could not parse. None of them
		// is evidence about the account, and NOTHING is marked: the Claude
		// lane's quarantine has no twin here, because the only thing that puts a
		// Codex account out of rotation is a grant the token endpoint rejected,
		// and that arrives through the refresher above.
		e.codexCommit(a, nil, thr, now, serving, nil)
	}
}

// codexEntry re-reads one cache row. It is a read with no lock for the reason
// usage.LoadCache takes none: every write to that document is a rename, so a
// reader sees one whole version or another.
func (e *Engine) codexEntry(uuid string) (usage.Entry, bool) {
	c, err := usage.LoadCache()
	if err != nil {
		return usage.Entry{}, false
	}
	return c.Get(uuid)
}

// codexCommit writes what a POLL read: the reading, the next schedule, and the
// sample.
//
// It is the Claude commit with four things removed: ApplyUsage, the profile
// re-read, the probe verdict and warmClamp. Each is named in codexTick's own
// comment with the reason it cannot apply here.
func (e *Engine) codexCommit(a store.Account, snap *usage.Snapshot, thr strategy.Thresholds,
	now time.Time, serving bool, adjust func(pollpolicy.State) pollpolicy.State) {

	e.codexWrite(a, snap, thr, now, serving, adjust, false)
}

// codexCommitHarvested writes what the PROXY read off a turn it was forwarding
// anyway. It differs from the poll's commit in one way, and that way is about
// where the reading came from rather than about what it says.
//
// The reading can be partial, so it is merged into the cached one rather than
// written over it. A poll asks /wham/usage and is answered about every
// window the account has; a harvest reads whatever the answer to one turn
// happened to carry, and internal/codexproxy publishes a sample when EITHER
// window family is present -- deliberately, because the family on a 429 is the
// most informative reading there is and it is often primary-only. Committed
// wholesale, such a sample ERASED the other window: a 96%-spent weekly vanished
// from the row, strategy.HeadroomOf re-read the same account as 90% of room on
// its five-hour window, and the lane went on ranking it first and serving it
// into a wall of 429s.
func (e *Engine) codexCommitHarvested(a store.Account, snap *usage.Snapshot,
	thr strategy.Thresholds, now time.Time, serving bool) {

	e.codexWrite(a, snap, thr, now, serving, nil, true)
}

// codexWrite is the body both commits share. `carry` is the whole difference
// between them, and codexCommitHarvested above says what it buys.
func (e *Engine) codexWrite(a store.Account, snap *usage.Snapshot, thr strategy.Thresholds,
	now time.Time, serving bool, adjust func(pollpolicy.State) pollpolicy.State, carry bool) {

	err := usage.WithCache(cacheTimeout, func(c *usage.Cache) error {
		entry, had := c.Get(a.UUID)
		state := pollStateOf(entry)
		if adjust != nil {
			state = adjust(state)
		}
		if snap != nil {
			stored := snap
			if carry {
				// Merged INSIDE the transaction, against the row this write is
				// about to replace rather than against a copy read before the
				// lock was taken: usage.json is a file several processes write,
				// and a merge computed outside the lock would carry a window
				// forward over one somebody else had just made newer.
				stored = codexCarry(snap, entry.Snapshot, now)
			}
			entry.Snapshot, entry.FetchedAt = stored, now
		} else if !had {
			// A failed first attempt still stamps, or the entry is pruned and
			// the backoff it just earned does not survive to the next tick.
			entry.FetchedAt = now
		}

		h := strategy.HeadroomOf(entry.Snapshot, thr)
		reading := pollpolicy.Reading{Exhausted: exhausted(h)}
		if snap != nil && h.Known {
			reading.BindingPct, reading.Known = 100-h.Pct, true
		}
		in := pollpolicy.Input{
			Now:       now,
			Active:    serving,
			Reading:   reading,
			Threshold: thr.For(h.Binding),
		}
		at, next := pollpolicy.Codex.Next(state, in, e.rand())
		// No Share: the Codex allowance is per account rather than per
		// organization, so there is no identity to divide a cadence among.
		entry.NextPollAt = at
		entry.ServeTTL = pollpolicy.Codex.ServeTTLFor(in)
		entry.Poll = usage.PollState{Interval: next.Interval, LastRateLimited: next.LastRateLimited}
		entry.Poll.LastBindingPct, entry.Poll.HasLastBinding = next.LastBindingPct, next.HasLastBinding
		c.Put(a.UUID, entry)
		e.scheduled(a.UUID, entry.NextPollAt)
		return nil
	})
	if err != nil {
		e.logf("recording %s's codex reading failed: %v", a.UUID, err)
	}

	// Outside the cache lock, for the reason the Claude lane records its series
	// outside it: both are cclock mkdir mutexes on the same store, and nesting
	// them would hold the cache shut against every reader for as long as the
	// series lock happened to be contended.
	if snap != nil {
		// The reading as it ARRIVED, never the merged one written above: the
		// series records what was observed, and a window carried forward from
		// the cache was not read at this instant. A point that claimed it was
		// would put an unchanged percentage at a time nothing measured, and a
		// burn rate taken across that segment reads flat.
		if herr := history.Record(historyTimeout, a.UUID, historySample(snap, now), now); herr != nil {
			e.logf("appending %s's codex sample to the usage history failed: %v", a.UUID, herr)
		}
	}
}

// codexCarry is a harvested reading with the windows it did not carry taken
// from the row it is about to replace.
//
// The two Codex windows are separately optional in a harvest and not in a poll,
// which is why this is not codexCommit's business: absent from a poll's answer
// means the account does not have that window, and absent from a harvest means
// only that this one response did not mention it.
func codexCarry(snap, prev *usage.Snapshot, now time.Time) *usage.Snapshot {
	if snap == nil || prev == nil {
		return snap
	}
	if snap.CodexPrimary.Present && snap.CodexSecondary.Present {
		return snap
	}
	// A COPY of the caller's reading rather than the reading itself. The same
	// pointer is handed to history.Record after the cache closes, and merging in
	// place would append a window carried forward out of the cache to the usage
	// series as though this turn had read it.
	out := *snap
	if !out.CodexPrimary.Present {
		out.CodexPrimary = codexCarriedWindow(prev.CodexPrimary, now)
	}
	if !out.CodexSecondary.Present {
		out.CodexSecondary = codexCarriedWindow(prev.CodexSecondary, now)
	}
	return &out
}

// codexCarriedWindow is one cached window kept, or nothing when it has already
// rolled over.
//
// The bound is the window's own reset. A percentage that described a window
// which has since ended says nothing about the one running now, and carrying a
// spent one forward would hold an account out of rotation through quota it has
// already got back -- the mirror of the erasure this merge exists to prevent,
// and just as invisible.
func codexCarriedWindow(w usage.Window, now time.Time) usage.Window {
	if !w.Present {
		return usage.Window{}
	}
	if at, ok := w.Reset(); ok && !now.Before(at) {
		return usage.Window{}
	}
	return w
}

// codexReloginSet is the accounts whose stored relogin mark still names the
// token they hold, which is the only thing that makes such a mark true.
//
// The mark is checked BEFORE the credential file is opened, so a machine with no
// dead grants -- which is every healthy machine -- reads no files at all at the
// tick loop's 1 Hz.
func codexReloginSet(s *store.Store, accounts []store.Account) map[string]bool {
	var out map[string]bool
	for _, a := range accounts {
		if a.Provider != provider.Codex || a.CodexReloginFor == "" {
			continue
		}
		creds, err := s.Credentials(a.UUID)
		if err != nil {
			continue
		}
		if !codexauth.NeedsRelogin(a, creds) {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[a.UUID] = true
	}
	return out
}

// refreshCodex is Engine.Refresh's Codex arm: the stored access token, the
// Codex table's 429 hold, and NO refresher.
//
// That last is the point rather than an omission. This runs in a CLI process,
// and the token endpoint kills a refresh token that is used twice -- so a
// second process willing to rotate would be spending a grant the daemon is also
// spending. A 401 here therefore leaves the row exactly as it was: the reading
// stays stale and nothing is marked, because a process that never asked the
// token endpoint anything has no verdict about the grant to record.
//
// The gate is the poll policy's and not the tick's cadence: a hand pressing a
// button is not on a cadence, so serveTTL and whatever floor a 429 earned are
// the only two things allowed to hold it. That is the rule the Claude arm above
// already follows, and the reason nextPollAt is not consulted here.
func (e *Engine) refreshCodex(ctx context.Context, s *store.Store, a store.Account,
	res *RefreshResult, cache *usage.Cache, cfg config.Config, now time.Time, wg *sync.WaitGroup) {

	if e.CodexRefresher != nil {
		// Loud, not silent. Every Engine this arm is meant to run on is built
		// without a refresher -- `daemon.NewEngine` never sets one, and
		// `wireCodex` is only ever called for the daemon's own Engine -- so a
		// caller that reaches this line WITH one has wired a mistake, and the
		// grant it holds is single-use: spending it here is what a second
		// process rotating alongside the daemon looks like.
		res.State = RefreshFailed
		res.Err = errors.New("a hand-triggered codex refresh must not hold a refresher")
		return
	}
	if e.CodexAccessToken == nil || e.CodexFetchUsage == nil {
		// No seams: this Engine was built without them, which is every Engine
		// in this binary that did not ask. Unpollable is the same answer an
		// add-token account gets, and `list` says nothing about it.
		res.State = RefreshUnpollable
		return
	}
	creds, err := s.Credentials(a.UUID)
	if err != nil {
		res.State, res.Err = RefreshFailed, err
		return
	}
	if _, ok := creds[codexauth.Key]; !ok || codexauth.NeedsRelogin(a, creds) {
		res.State = RefreshUnpollable
		return
	}
	entry, has := cache.Get(a.UUID)
	if has && entry.Fresh(now) {
		res.State, res.At = RefreshCached, entry.FetchedAt.Add(usage.ServeTTL)
		return
	}
	if at, held := pollpolicy.Codex.RateLimitedUntil(pollStateOf(entry), now); held {
		res.State, res.At = RefreshHeld, at
		return
	}
	root, err := ccpath.StoreHome()
	if err != nil {
		res.State, res.Err = RefreshFailed, err
		return
	}
	// Which account is serving decides the cadence the commit writes back, the
	// same way the Claude arm passes `active`.
	serving, _ := codexswitch.ReadServing(root)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if perr := e.codexPoll(ctx, a, codexThresholds(cfg), a.UUID == serving); perr != nil {
			res.State, res.Err = RefreshFailed, perr
			return
		}
		res.State = RefreshFetched
	}()
}
