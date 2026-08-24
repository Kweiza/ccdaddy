package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/tokens"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// defaultPollTimeout bounds one poll end to end: a token round trip plus the
// usage call. It is generous because the alternative to waiting is an account
// that reads as UNKNOWN — unknown headroom is never read as zero, so the
// account falls into a tier of its own with no figure to compare it on.
const defaultPollTimeout = 45 * time.Second

// cacheTimeout bounds the wait for the usage cache's own lock. Every writer is
// a sub-second read-modify-write; the contention is between poller goroutines
// of this process and a `ccdad list --refresh` beside it.
const cacheTimeout = 5 * time.Second

// stateTimeout bounds the wait for the engine state lock.
const stateTimeout = 5 * time.Second

// Engine is the tick loop's body: the poller fleet, the scheduler, and the
// unattended switch.
//
// One rule shapes the whole type. The tick NEVER waits on the network. It
// dispatches polls as goroutines and moves straight on to the decision, because
// Loop waits for the body to return — a tick that awaited a poll would stop
// publishing status and stop executing switches for as long as the endpoint
// took to answer, which on a laptop that just lost its connection is the
// request timeout, every tick, forever.
//
// The second rule is the one cclock states in prose for the CLI and that this
// is the place it will actually be broken: no Claude Code lock is ever held
// across a usage fetch or a token refresh. The poller and the swap executor
// share this process, so the two are only separated by where the calls are
// made — internal/tokens releases Claude Code's refresh lock before it saves,
// and nothing here takes one at all.
type Engine struct {
	// AccessToken hands out an access token for one account, and FetchUsage
	// spends it. They are funcs rather than the concrete types so a test can
	// describe an endpoint's behaviour without one.
	AccessToken func(ctx context.Context, uuid string) (string, error)
	FetchUsage  func(ctx context.Context, accessToken string) (*usage.Snapshot, error)
	// Now is the clock, and Rand the jitter source the poll policy wants.
	Now  func() time.Time
	Rand func() float64
	// PollTimeout bounds one poll. Zero means defaultPollTimeout.
	PollTimeout time.Duration
	// Log records what a tick decided. Nil is silent.
	Log func(format string, a ...any)

	reloader *config.Reloader

	mu       sync.Mutex
	cfg      config.Config
	cfgErr   error
	inFlight map[string]struct{}
	polls    map[string]pollRecord
	status   Status
	// saidOverridden, saidContended and saidClaimNotice are touched only from
	// Tick, which Loop runs one at a time, so they are deliberately outside the
	// mutex above: the fields under it are the ones a poller goroutine or
	// Snapshot also reaches.
	//
	// They suppress the repeat of the notices that would otherwise be logged on
	// every evaluation for as long as the machine stays as it is. The tick loop
	// runs about once a second; a warning it re-emits every second is a warning
	// nobody reads.
	//
	// saidClaimNotice holds the TEXT rather than a flag, because that notice can
	// change while remaining a notice — an unreadable owner document becoming an
	// unlockable filesystem is a different fact about the same claim, and
	// latching on a bool would print only whichever came first.
	saidOverridden  bool
	saidContended   bool
	saidClaimNotice string

	wg sync.WaitGroup
}

// pollRecord is what the last poll attempt of an account produced. It lives
// here rather than in the cache because it is engine state, and the rule that
// every field has exactly one authoritative file gives engine state to
// status.json alone.
type pollRecord struct {
	at   time.Time
	err  string
	next time.Time
}

// NewEngine wires the real token source and usage client.
func NewEngine() *Engine {
	src := tokens.New()
	client := usage.NewClient()
	return &Engine{
		AccessToken: src.AccessToken,
		FetchUsage:  client.FetchUsage,
		reloader:    config.NewReloader(),
		inFlight:    map[string]struct{}{},
		polls:       map[string]pollRecord{},
		cfg:         config.Defaults(),
	}
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) rand() float64 {
	if e.Rand != nil {
		return e.Rand()
	}
	// A midpoint sample is no jitter at all, which is worse than none: it looks
	// like it is working. A caller that wants jitter supplies a source.
	return 0.5
}

func (e *Engine) pollTimeout() time.Duration {
	if e.PollTimeout > 0 {
		return e.PollTimeout
	}
	return defaultPollTimeout
}

func (e *Engine) logf(format string, a ...any) {
	if e.Log != nil {
		e.Log(format, a...)
	}
}

// Config is the auto-switch engine's knobs currently in force, and ConfigError
// whatever went wrong last reading them. Both are for the operator's benefit;
// the engine has already used the config either way.
func (e *Engine) Config() config.Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

func (e *Engine) ConfigError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfgErr
}

// Wait blocks until every dispatched poll has finished. Shutdown calls it so a
// poll's cache write cannot land after the final status document was published.
func (e *Engine) Wait() { e.wg.Wait() }

// Snapshot is the engine state daemon.Run publishes.
//
// The poll times are overlaid HERE rather than baked in when the tick
// published, and that is not tidiness: the tick does not wait for the polls it
// dispatched, so a document frozen at publish time would report every account's
// last poll one tick late, forever. What the tick decided is a tick-scoped
// fact; when an account was last reached is not.
func (e *Engine) Snapshot() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.status
	out.Accounts = append([]AccountStatus(nil), e.status.Accounts...)
	for i := range out.Accounts {
		rec, ok := e.polls[out.Accounts[i].UUID]
		if !ok {
			continue
		}
		out.Accounts[i].LastPollAt, out.Accounts[i].LastPollError = rec.at, rec.err
		if !rec.next.IsZero() {
			out.Accounts[i].NextPollAt = rec.next
		}
	}
	return out
}

// Tick is one iteration of the tick loop.
func (e *Engine) Tick(ctx context.Context) error {
	now := e.now()
	s, err := store.Open()
	if err != nil {
		return err
	}
	accounts := s.Accounts()

	// External config changes, picked up once per tick and shared by the
	// scheduler and the decision. Reload never fails usefully: it returns the
	// config to run on plus a warning, and the last-good-config rule makes the
	// warning the whole point — a broken hand-edit leaves the engine on the
	// last config that PARSED rather than reverting a tuned threshold to stock.
	cfg, cfgErr := e.reloader.Reload()
	e.mu.Lock()
	e.cfg, e.cfgErr = cfg, cfgErr
	e.mu.Unlock()
	if cfgErr != nil {
		e.logf("config: %v (running on the last one that parsed)", cfgErr)
	}

	cache, err := usage.LoadCache()
	if err != nil {
		return err
	}

	ev, evErr := switcher.Evaluate(s, switcher.EvalOptions{
		Now:    now,
		Config: func() (config.Config, error) { return cfg, cfgErr },
	})
	if evErr != nil {
		e.publish(accounts, cache, ev, cfg)
		return evErr
	}

	res, swapErr := e.act(s, ev)
	// Dispatched AFTER the decision, and handed the live account rather than
	// looking it up. The poll cadence branches on which account is active, and a
	// poller that read that from the engine's published state would be racing
	// the publish this same tick is about to do — the account's very first poll
	// would take the active cadence or the candidate one depending on which
	// goroutine got there first.
	//
	// Taking it from the evaluation also means a swap performed a moment ago is
	// already reflected: the account that just became live is polled as the
	// live one, this tick rather than next.
	e.dispatch(ctx, s, accounts, cache, cfg, now, activeAfter(ev, res))
	e.publish(accounts, cache, ev, cfg)

	if swapErr != nil {
		return swapErr
	}
	if res.Outcome == switcher.Switched {
		e.logf("switched to %s", res.Target.Label())
	}
	return nil
}

// activeAfter is the account Claude Code is logged in as once this tick's swap
// has been accounted for.
func activeAfter(ev switcher.Evaluation, res switcher.Result) string {
	if res.Outcome == switcher.Switched {
		return res.Target.UUID
	}
	if ev.LiveKnown {
		return ev.Live.UUID
	}
	return ""
}

// act executes the plan, when the plan is to move.
func (e *Engine) act(s *store.Store, ev switcher.Evaluation) (switcher.Result, error) {
	if ev.NoReadings || ev.Plan.Action != strategy.ActionSwitch || !ev.HasTarget {
		return switcher.Result{}, nil
	}
	live := ""
	if ev.LiveKnown {
		live = ev.Live.UUID
	}
	res, err := switcher.Execute(s, switcher.Request{
		Target: ev.Target, LiveUUID: live, Unattended: true,
	})
	if err != nil {
		return res, err
	}
	switch res.Outcome {
	case switcher.Overridden:
		// Detected once and then held. Unattended, this is the state where
		// every switch succeeds and changes nothing at all, and repeating the
		// sentence on every evaluation is how it stops being read.
		if !e.saidOverridden {
			e.saidOverridden = true
			e.logf("%s", switcher.DisplacementNote("not switching: ", res))
		}
		return res, nil
	case switcher.Contended:
		// Latched for the same reason Overridden is: this state persists until
		// somebody changes a machine, and it is reached at 1 Hz.
		//
		// A daemon that HOLDS the claim never gets here — its own claim answers
		// "mine" — so reaching this at all means this daemon is running
		// degraded, without the claim, while another store's engine has it.
		// That is worth one loud line.
		if !e.saidContended {
			e.saidContended = true
			e.logf("stood down: the ccdad store at %s (pid %d) is driving this Claude Code login, "+
				"and this daemon does not hold the claim on it. Two stores on one login undo each "+
				"other's switches; point CLAUDE_CONFIG_DIR at a directory of this store's own, or "+
				"stop that engine", res.Claim.Owner.Store, res.Claim.Owner.PID)
		}
		return res, nil
	case switcher.Raced:
		e.logf("stood down: the live login changed while the switch was being decided")
		return res, nil
	}
	if n := res.Claim.Notice; n != "" && n != e.saidClaimNotice {
		e.saidClaimNotice = n
		e.logf("%s", n)
	} else if n == "" {
		e.saidClaimNotice = ""
	}
	e.saidContended = false
	e.saidOverridden = false
	if res.CooldownErr != nil {
		e.logf("%v", res.CooldownErr)
	}
	if res.KeyErr != nil {
		e.logf("the API key in Claude Code's config could not be cleared: %v", res.KeyErr)
	}
	return res, nil
}

// dispatch starts a poll for every account that is due, and returns without
// waiting for any of them.
func (e *Engine) dispatch(ctx context.Context, s *store.Store, accounts []store.Account,
	cache *usage.Cache, cfg config.Config, now time.Time, active string) {

	sizes := identitySizes(accounts)
	for _, a := range accounts {
		if !pollable(s, a) {
			continue
		}
		entry, has := cache.Get(a.UUID)
		if !due(entry, has, now) {
			continue
		}
		if !e.claim(a.UUID) {
			// A poll from an earlier tick is still running. Starting a second
			// one would spend the identity's budget twice for one reading and
			// race two writers into the same cache row.
			continue
		}
		e.wg.Add(1)
		go func(a store.Account) {
			defer e.wg.Done()
			defer e.release(a.UUID)
			// Discarded: a tick reports through the status document, and
			// the failure is already in this account's pollRecord.
			_ = e.poll(ctx, a, cfg, sizes[identityOf(a)], a.UUID == active)
		}(a)
	}
}

// identityOf is the budget an account draws on. The allowance is per
// organization, so accounts sharing one share it; an account whose profile
// reported no organization cannot be SHOWN to share with anyone, so it is its
// own identity rather than being lumped in with every other unknown.
func identityOf(a store.Account) string {
	if a.OrganizationUUID == "" {
		return "uuid:" + a.UUID
	}
	return "org:" + a.OrganizationUUID
}

func identitySizes(accounts []store.Account) map[string]int {
	out := make(map[string]int, len(accounts))
	for _, a := range accounts {
		out[identityOf(a)]++
	}
	return out
}

// pollable reports whether there is anything to poll WITH. An account with no
// claudeAiOauth has no refresh grant behind it — `ccdad add-token` accounts are
// the ordinary case — so asking the token source for one produces
// tokens.ErrNoOAuthCredential every cadence forever. Skipping it here keeps a
// permanent non-answer out of the status document.
func pollable(s *store.Store, a store.Account) bool {
	creds, err := s.Credentials(a.UUID)
	if err != nil {
		return false
	}
	_, hasOAuth := creds["claudeAiOauth"]
	return hasOAuth
}

// due applies both gates: serveTTL, which is the same rule the read path
// enforces and the only thing standing between a busy fleet and the endpoint's
// hourly allowance, and the schedule the last poll set.
func due(e usage.Entry, has bool, now time.Time) bool {
	if !has {
		return true
	}
	if e.Fresh(now) {
		return false
	}
	if e.NextPollAt.IsZero() {
		return true
	}
	return !now.Before(e.NextPollAt)
}

func (e *Engine) claim(uuid string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, running := e.inFlight[uuid]; running {
		return false
	}
	e.inFlight[uuid] = struct{}{}
	return true
}

func (e *Engine) release(uuid string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.inFlight, uuid)
}

// poll takes one reading. Dispatched by a tick it runs on its own goroutine and
// its only output is the cache, the engine state, and this account's
// pollRecord; the returned error is for Refresh, which has a caller waiting to
// be told what happened rather than a status document to publish into.
func (e *Engine) poll(ctx context.Context, a store.Account, cfg config.Config, identitySize int, active bool) error {
	ctx, cancel := context.WithTimeout(ctx, e.pollTimeout())
	defer cancel()

	token, err := e.AccessToken(ctx, a.UUID)
	var snap *usage.Snapshot
	if err == nil {
		// No Claude Code lock is held here. internal/tokens takes the refresh
		// lock only around its own read of the live file and releases it before
		// returning, and this call reaches the network.
		snap, err = e.FetchUsage(ctx, token)
	}

	now := e.now()
	e.record(a.UUID, now, err)
	if err != nil {
		e.handleFailure(a, cfg, err, now, identitySize, active)
		return err
	}
	e.commit(a, snap, now, identitySize, cfg, active, nil)
	return nil
}

// handleFailure decides what a failed poll means. Only ONE of the failures says
// anything about the account.
func (e *Engine) handleFailure(a store.Account, cfg config.Config, err error,
	now time.Time, identitySize int, active bool) {
	switch {
	case errors.Is(err, tokens.ErrNoOAuthCredential):
		// Nothing to poll with, and pollable() should already have caught it.
		// Nothing is written: an account that cannot be polled has no schedule.
		return

	case errors.Is(err, tokens.ErrLiveTokenStale):
		// This is UNKNOWN, and NOT an endpoint failure. Claude Code is the one
		// that rotates the live login, and an eight-hour-old token means Claude
		// Code has not run in eight hours — so there is no session whose
		// rotation is urgent, and this must not feed AIMD or quarantine
		// anything. It reschedules at the ordinary cadence and waits.
		e.commit(a, nil, now, identitySize, cfg, active, nil)
		return

	case errors.Is(err, usage.ErrRateLimited):
		var se *usage.StatusError
		var retry time.Duration
		var hasRetry bool
		if errors.As(err, &se) {
			retry, hasRetry = se.RetryAfter()
		}
		e.commit(a, nil, now, identitySize, cfg, active, func(st pollpolicy.State) pollpolicy.State {
			return pollpolicy.RateLimited(st, now, retry, hasRetry)
		})
		return
	}

	// Everything else: a transport failure, a 503, a 401 from the usage
	// endpoint. None of them is evidence about the account, and quarantining on
	// one turns a single outage into a manual re-login for every account. Only
	// a REJECTED refresh token qualifies, and ClassifyRefresh is the only thing
	// allowed to say so.
	if strategy.ClassifyRefresh(err).Quarantines() {
		e.quarantine(a.UUID, cfg, now)
	}
	e.commit(a, nil, now, identitySize, cfg, active, nil)
}

func (e *Engine) quarantine(uuid string, cfg config.Config, now time.Time) {
	d := cfg.StrategyConfig().QuarantineFor
	if d <= 0 {
		d = strategy.DefaultQuarantine
	}
	if err := strategy.WithState(stateTimeout, func(st *strategy.State) error {
		st.Quarantine(uuid, now, d, strategy.RefreshDead.String())
		return nil
	}); err != nil {
		e.logf("quarantining %s failed: %v", uuid, err)
	}
}

// commit writes the reading and the next schedule. snap is nil for a poll that
// produced no reading, and adjust is the rate-limit rule when there was one.
//
// A failed poll never erases the last good reading: an account that could not
// be read is UNKNOWN, and an unknown one that used to have evidence still has
// that evidence — throwing it away would make one bad minute look like a fresh
// account with no history.
func (e *Engine) commit(a store.Account, snap *usage.Snapshot, now time.Time,
	identitySize int, cfg config.Config, active bool,
	adjust func(pollpolicy.State) pollpolicy.State) {

	// Resolved once, before the cache callback: it is read twice inside and the
	// value is the same for the whole call.
	thr := cfg.Thresholds()
	err := usage.WithCache(cacheTimeout, func(c *usage.Cache) error {
		entry, had := c.Get(a.UUID)
		state := pollStateOf(entry)
		if adjust != nil {
			state = adjust(state)
		}

		if snap != nil {
			entry.Snapshot, entry.FetchedAt = snap, now
		} else if !had {
			// There is no prior reading to keep, and an entry the cache prunes
			// is a backoff that does not survive the next evaluation: Prune
			// drops anything dated before the account was added. Stamping the
			// attempt keeps the schedule alive; Snapshot stays nil, which is
			// what every reader takes as "no reading".
			entry.FetchedAt = now
		}

		h := strategy.HeadroomOf(entry.Snapshot, thr)
		reading := pollpolicy.Reading{Exhausted: exhausted(h)}
		if snap != nil && h.Known {
			reading.BindingPct, reading.Known = 100-h.Pct, true
		}
		in := pollpolicy.Input{
			Now:     now,
			Active:  active,
			Reading: reading,
			// The BINDING window's own floor, and not the fallback. BindingPct
			// is measured on the binding window, and the urgent band is "within
			// 15 points of the threshold": comparing that reading against a
			// different window's number puts the band 20 points out of place for
			// an account whose weekly floor is 60 while the fallback is 80, and
			// the account is then polled at the idle cadence through the exact
			// span the band exists to cover.
			//
			// An unreadable account has an empty Binding, and Thresholds.For
			// answers Default for a window it has no key for, so that case keeps
			// the fallback — and nearThreshold ignores it anyway while
			// Reading.Known is false.
			Threshold: thr.For(h.Binding),
		}
		at, next := pollpolicy.Next(state, in, e.rand())

		// The allowance belongs to the identity, so the cadence is divided
		// among the accounts that share one — otherwise two accounts in an
		// organization each poll at the single-account rate and the pair spends
		// twice the budget.
		entry.NextPollAt = now.Add(pollpolicy.PerIdentity(at.Sub(now), identitySize))
		entry.Poll = usage.PollState{Interval: next.Interval, LastRateLimited: next.LastRateLimited}
		entry.Poll.LastBindingPct, entry.Poll.HasLastBinding = next.LastBindingPct, next.HasLastBinding
		c.Put(a.UUID, entry)
		e.scheduled(a.UUID, entry.NextPollAt)
		return nil
	})
	if err != nil {
		e.logf("recording %s's reading failed: %v", a.UUID, err)
	}
}

// pollStateOf lifts an entry's persisted history back into the poll policy's
// own type. Both writers of that history go through it, so a field added to one
// side cannot be silently dropped by the other.
func pollStateOf(e usage.Entry) pollpolicy.State {
	return pollpolicy.State{
		Interval:        e.Poll.Interval,
		LastRateLimited: e.Poll.LastRateLimited,
		LastBindingPct:  e.Poll.LastBindingPct,
		HasLastBinding:  e.Poll.HasLastBinding,
	}
}

// exhausted is "every window spent", read off the binding one: the binding
// window is whichever has least SLACK, so an account past the threshold there is
// past it everywhere that matters.
//
// It delegates rather than comparing here. The same question used to be asked in
// its own words on this line, against cfg.Threshold alone, and that answered a
// different question the moment a window could carry a threshold of its own or a
// scope could be opted into the ranking: the daemon would publish a state for an
// account measured against numbers, and a window set, the engine never used.
// Two spellings of one rule diverge silently, and this one has no user watching
// it — it decides a poll cadence and a status column, not a switch.
func exhausted(h strategy.Headroom) bool {
	spent, _ := strategy.Spent(h)
	return spent
}

// scheduled records the deadline a poll just set. The snapshot takes it from
// here rather than from the cache because a poll dispatched by this tick
// finishes AFTER the tick published — the tick never waits — so a snapshot
// built from the cache would report the schedule the poll replaced.
func (e *Engine) scheduled(uuid string, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec := e.polls[uuid]
	rec.next = at
	e.polls[uuid] = rec
}

func (e *Engine) record(uuid string, at time.Time, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec := e.polls[uuid]
	rec.at, rec.err = at, ""
	if err != nil {
		rec.err = err.Error()
	}
	e.polls[uuid] = rec
}

// publish builds the status document. It carries engine state and NOTHING a
// reader could take a quota number from: the promise that `list` and `status
// --json` can never disagree only holds while every figure has exactly one
// authoritative file, and quota's is usage.json.
func (e *Engine) publish(accounts []store.Account, cache *usage.Cache,
	ev switcher.Evaluation, cfg config.Config) {

	quarantined := make(map[string]bool, len(ev.Plan.Quarantined))
	for _, uuid := range ev.Plan.Quarantined {
		quarantined[uuid] = true
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	status := Status{}
	if ev.LiveKnown {
		status.ActiveUUID = ev.Live.UUID
	}
	if at, to := ev.LastSwitchAt, ev.LastSwitchTo; !at.IsZero() {
		status.LastSwitchAt, status.LastSwitchTo = at, to
	}
	for _, a := range accounts {
		row := AccountStatus{UUID: a.UUID}
		// The cache's own deadline is the fallback, and it is the one that
		// matters on a fresh start: it is what the PREVIOUS daemon scheduled,
		// and honouring it is what stops a restart loop from re-polling every
		// account immediately.
		if entry, ok := cache.Get(a.UUID); ok {
			row.NextPollAt = entry.NextPollAt
		}
		row.State = accountState(a, cache, quarantined[a.UUID], status.ActiveUUID, cfg)
		status.Accounts = append(status.Accounts, row)
	}
	e.status = status
}

func accountState(a store.Account, cache *usage.Cache, quarantined bool,
	activeUUID string, cfg config.Config) AccountState {

	switch {
	case a.Disabled:
		return StateDisabled
	case quarantined:
		return StateQuarantined
	case a.UUID == activeUUID:
		return StateActive
	}
	entry, ok := cache.Get(a.UUID)
	if !ok || entry.Snapshot == nil {
		// Unknown is NOT an empty account, and it must never render as 0%.
		return StateUnknown
	}
	h := strategy.HeadroomOf(entry.Snapshot, cfg.Thresholds())
	if !h.Known {
		return StateUnknown
	}
	if exhausted(h) {
		return StateExhausted
	}
	return StateCandidate
}
