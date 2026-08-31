package daemon

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/identity"
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

// historyTimeout bounds the wait for the usage history's own lock, and it is
// deliberately far below cacheTimeout rather than equal to it.
//
// The recorder runs on the poll goroutine Engine.Wait blocks on, which is what
// the daemon drains on the way out, and the CLI gives that drain ten seconds
// before it force-kills the process — which on Windows is TerminateProcess,
// with no chance to finish anything. The two costs are not comparable: a sample
// that cannot get the lock inside two seconds loses one point of resolution
// from a series of hundreds, while a drain that overruns loses the process. So
// a miss here is dropped and never retried, and Record's (uuid, at) key means
// the next poll's sample is a new point rather than a duplicate of a lost one.
const historyTimeout = 2 * time.Second

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
	// FetchProfile reads /api/oauth/profile for the account a token belongs
	// to. It is a SECOND endpoint on the poll path and it is called rarely on
	// purpose: see poll, which spends a request on it only when the stored
	// profile is older than store.ProfileTTL.
	//
	// NIL MEANS DO NOT CALL, matching Freshen and ResolveOwner. A test that
	// has not asked for the profile endpoint cannot reach it by forgetting to
	// stub it, and a poll whose seam is nil simply skips the step.
	FetchProfile func(ctx context.Context, accessToken string) (*identity.Profile, error)
	// Freshen refreshes one account's stored credential so a swap does not
	// install a login Claude Code would rotate on sight. Nil means a stale
	// credential is refused rather than repaired, which is the safe direction
	// and the one a test gets by default.
	Freshen func(ctx context.Context, uuid string) (cclink.Blob, error)
	// ResolveOwner names the account an access token belongs to, by asking the
	// profile endpoint. It is the oracle resolveLive turns on, and the only
	// thing allowed to say a login is somebody else's: nothing on disk can tell
	// a rotated managed account from an unmanaged one. Nil means every
	// unnameable login reads as unresolved, which stands the swap down — the
	// safe direction, and the one a test gets by default.
	ResolveOwner func(ctx context.Context, accessToken string) (string, error)
	// SpawnProbe starts one probe and returns without waiting for it. It is a
	// func for the same reason the two above are: starting a process is the
	// thing a test in this package cannot arrange. Nil means the package's own
	// SpawnProbe.
	SpawnProbe func(uuid, model string) error
	// Now is the clock, and Rand the jitter source the poll policy wants.
	Now  func() time.Time
	Rand func() float64
	// PollTimeout bounds one poll. Zero means defaultPollTimeout.
	PollTimeout time.Duration
	// Log records what a tick decided. Nil is silent.
	Log func(format string, a ...any)
	// LatestRelease resolves the newest published release, as a tag such as
	// "v0.7.0". It is a func for the reason the poller's hooks above are, and
	// NIL IS THE DEFAULT ON PURPOSE: NewEngine leaves it unset, so the only
	// engine in this binary that can reach the release origin is the one
	// EngineOptions builds for the daemon process. internal/cli constructs a
	// real Engine of its own for a refresh, and this is what stops that
	// construction acquiring a network call nobody asked it for -- and what
	// stops a test in this package reaching the network by forgetting a stub.
	LatestRelease func(ctx context.Context) (string, error)

	reloader *config.Reloader

	mu       sync.Mutex
	cfg      config.Config
	cfgErr   error
	inFlight map[string]struct{}
	polls    map[string]pollRecord
	status   Status
	// The daily release check's whole state, beside cfg and polls under the
	// same lock. It is deliberately NOT inside status: publish() replaces that
	// field wholesale on every tick, so anything kept there would be erased
	// about once a second. Snapshot overlays these the way it overlays the poll
	// records, and for the same reason.
	updateCheckedAt   time.Time
	nextUpdateCheckAt time.Time
	updateInFlight    bool
	updateLatest      string
	updateErr         string
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
	// saidNoClaude and saidProbeSpends are the probe's two once-per-lifetime
	// lines, held here for the reason the three above are: this loop runs about
	// once a second, and a machine with no Claude Code on it stays that way.
	saidNoClaude    bool
	saidProbeSpends bool
	// saidStale holds the UUID whose stale-credential refusal has been logged,
	// not a flag, for the reason saidClaimNotice holds text: a second account
	// going stale is a different fact about a different account, and a bool
	// would report only whichever reached the top of the ranking first.
	saidStale string
	// saidUnattributed latches the stand-down on a login that could not be
	// resolved. Reached at 1 Hz for as long as the endpoint is unreachable,
	// which on a laptop that lost its connection is every tick until it comes
	// back.
	saidUnattributed bool

	// saidUnreadable latches the stand-down on a login STORE that could not be
	// read at all, which is a different sentence from saidUnattributed's and a
	// different remedy: that one waits for an identity, this one waits for the
	// machine. Latched for the same reason -- the tick reaches it at 1 Hz.
	saidUnreadable bool

	wg sync.WaitGroup
}

// pollRecord is what the last poll attempt of an account produced. It lives
// here rather than in the cache because it is engine state, and the rule that
// every field has exactly one authoritative file gives engine state to
// status.json alone.
type pollRecord struct {
	at  time.Time
	err string
	// next is the schedule this account's OWN poll earned, and hold a
	// stand-down another account's poll wrote. They are two fields for the
	// reason usage.Entry keeps them apart: different writers, and the stand-down
	// does not apply to whichever account is live right now. Folding them here
	// would publish a stand-down written for an account's predecessor as that
	// account's own deadline, for as long as half an hour after it became live.
	next time.Time
	hold time.Time
}

// NewEngine wires the real token source, usage client and jitter source.
//
// Rand is set HERE and nowhere else, because this is the only constructor a
// shipped binary reaches: pollpolicy takes its randomness as an argument to stay
// a pure function, so a nil source is not "no jitter", it is the midpoint
// sample -- and jitter(d, 0.5) is d exactly. Leaving it nil made every plus or
// minus ten percent guard in that package unreachable code while reading, in
// every comment, as though it were working.
//
// math/rand/v2's package-level Float64 is safe for concurrent use, which this
// needs: polls run one goroutine per account.
func NewEngine() *Engine {
	src := tokens.New()
	client := usage.NewClient()
	// One profile client for both seams: ResolveOwner and FetchProfile hit the
	// same endpoint, and two clients would mean two connection pools and two
	// sets of timeouts for one URL.
	profiles := identity.NewClient()
	return &Engine{
		AccessToken:  src.AccessToken,
		Freshen:      src.Freshen,
		ResolveOwner: ownerResolver(profiles),
		FetchUsage:   client.FetchUsage,
		FetchProfile: profiles.FetchProfile,
		Rand:         rand.Float64,
		reloader:     config.NewReloader(),
		inFlight:     map[string]struct{}{},
		polls:        map[string]pollRecord{},
		cfg:          config.Defaults(),
	}
}

// freshen adapts the engine's refresher to the hook switcher.Execute takes.
//
// The context is the TICK's, and bounded by the same pollTimeout one poll gets:
// a refresh that hangs must not hold the swap open past the tick that asked for
// it. Nil Freshen stays nil rather than becoming a func that returns an error,
// so Execute's own "cannot refresh" branch is the one that reports it.
func (e *Engine) freshen(ctx context.Context) func(string) (cclink.Blob, error) {
	if e.Freshen == nil {
		return nil
	}
	return func(uuid string) (cclink.Blob, error) {
		ctx, cancel := context.WithTimeout(ctx, e.pollTimeout())
		defer cancel()
		return e.Freshen(ctx, uuid)
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
		// The same rule usage.Entry.PollAt applies, applied at READ time rather
		// than when the record was written: whether an account is live changes
		// after a switch, and a stand-down recorded while it was an alternate
		// must stop applying the moment it stops being one.
		next := rec.next
		if out.Accounts[i].UUID != out.ActiveUUID && rec.hold.After(next) {
			next = rec.hold
		}
		if !next.IsZero() {
			out.Accounts[i].NextPollAt = next
		}
	}
	// The four release-check fields, overlaid here for the reason the poll
	// times above are: publish() replaces e.status wholesale on every tick, so
	// state that has to outlive one iteration cannot live in it.
	out.UpdateCheckedAt = e.updateCheckedAt
	out.NextUpdateCheckAt = e.nextUpdateCheckAt
	out.UpdateLatest = e.updateLatest
	out.UpdateCheckError = e.updateErr
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

	// Above the evaluation AND above the cache load, which is the position
	// rather than a place it fitted: a machine whose usage cache cannot be read
	// returns from this function early, and a machine whose ranking pass fails
	// every tick is exactly the machine whose user needs to hear that a fix
	// shipped.
	e.checkForRelease(ctx, cfg, now)

	cache, err := usage.LoadCache()
	if err != nil {
		return err
	}

	ev, evErr := switcher.Evaluate(s, switcher.EvalOptions{
		Now:    now,
		Config: func() (config.Config, error) { return cfg, cfgErr },
	})
	if evErr != nil {
		// The configured table, not hoverThresholds(cfg, ev): a failed
		// evaluation is the one case where ev.Plan cannot be trusted for
		// anything it did not itself set, including whether Hover ran.
		e.publish(accounts, cache, ev, configuredThresholds(cfg), quarantinedSet(ev))
		return evErr
	}

	res, swapErr := e.act(ctx, s, ev)
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
	active, activeKnown := activeAfter(ev, res)
	quarantined := quarantinedSet(ev)
	// The same table `ev` was just ranked against, not a second one rebuilt from
	// cfg. Under hover the two disagree on purpose — hover's threshold divides
	// the pool by the accounts THIS pass judged usable, which dispatch and
	// publish cannot see from cfg alone — and a rebuilt table would publish a
	// candidate the ranking had already excluded, poll it at the wrong cadence,
	// and probe the wrong binding window.
	thresholds := hoverThresholds(cfg, ev)
	e.dispatch(ctx, s, accounts, cache, cfg, thresholds, now, active, activeKnown, quarantined)
	e.publish(accounts, cache, ev, thresholds, quarantined)

	if swapErr != nil {
		return swapErr
	}
	if res.Outcome == switcher.Switched {
		e.logf("%s", switchLogLine(ev, res.Target))
	}
	return nil
}

// switchLogLine is what the daemon records when it moves the live login.
//
// It carries the REASON and the figures the margin was judged on, and that is
// the whole point of it. A log that records only the target answers "did it
// move" and nothing else -- so a daemon that alternated between two accounts
// every 121 seconds for twenty-five minutes left no way to say which margin
// kept clearing, or by how much. 121 s is HoverCooldown plus one tick, which
// already says the ranking wanted to move on every evaluation; what it does not
// say is why, and the readings behind it aged out of the series before anyone
// looked.
//
// Everything here was one line away in the same scope. The cost of carrying it
// is a longer line in a file that is capped at 32 MiB and rotates.
//
// The three absences are rendered as absences rather than as zeroes. An
// unreadable headroom is the ordinary state for a seat metered in credits,
// whose binding window AllWindows deliberately does not carry, and "slack=0
// thr=0" is a sentence that reads like a measurement and is not one.
func switchLogLine(ev switcher.Evaluation, target store.Account) string {
	var b strings.Builder
	b.WriteString("switched to ")
	b.WriteString(target.Label())

	if ev.LiveKnown && ev.Live.UUID != "" {
		b.WriteString(" from ")
		b.WriteString(ev.Live.Label())
	} else if ev.Live.UUID != "" || ev.Live.Email != "" {
		b.WriteString(" from ")
		b.WriteString(ev.Live.Label())
	} else {
		b.WriteString(" from no attributable login")
	}

	if !ev.Decided {
		b.WriteString(": no ranking ran")
		return b.String()
	}

	b.WriteString(": ")
	b.WriteString(ev.Plan.Reason.String())

	h := ev.Plan.Target.Headroom
	if !h.Known {
		b.WriteString(" (headroom unreadable)")
		return b.String()
	}
	fmt.Fprintf(&b, " (binding=%s slack=%.1f thr=%.1f used=%.1f)",
		h.Binding, h.Slack, h.Threshold, 100-h.Pct)
	return b.String()
}

// activeAfter is the account Claude Code is logged in as once this tick's swap
// has been accounted for, and whether that could be established at all.
//
// The second value is not decoration. AttributeFile matches the live file by
// REFRESH TOKEN, and the server rotates that token whenever anything refreshes,
// so "which account is live could not be worked out" is a state a working
// machine reaches — and it is not the same fact as "no account is live".
// Collapsing the two into an empty uuid makes every comparison against it fail
// open, which for the probe means spending a turn against the account a session
// is running on: the one probe that can revoke the token that session is using.
//
// Execute's answer is preferred over the evaluation's because it was taken under
// the claim lock, after the evaluation and after any swap.
func activeAfter(ev switcher.Evaluation, res switcher.Result) (string, bool) {
	if res.Outcome == switcher.Switched {
		return res.Target.UUID, true
	}
	if res.LiveKnown {
		return res.Live.UUID, true
	}
	if ev.LiveKnown {
		return ev.Live.UUID, true
	}
	return "", false
}

// act executes the plan, when the plan is to move.
func (e *Engine) act(ctx context.Context, s *store.Store, ev switcher.Evaluation) (switcher.Result, error) {
	if ev.NoReadings || ev.Plan.Action != strategy.ActionSwitch || !ev.HasTarget {
		return switcher.Result{}, nil
	}
	live := ""
	if ev.LiveKnown {
		live = ev.Live.UUID
	}
	// A login the file cannot name is resolved BEFORE the swap, not read as
	// "nobody is live". Claude Code rotating a managed account's refresh token
	// leaves exactly this state, and the engine used to answer it by installing
	// over the session that caused it.
	//
	// The oracle either repairs the store — the rotated pair goes back into its
	// account's snapshot, and this tick continues with a baseline again — or it
	// establishes that the login is somebody else's, which is a machine ccdad
	// exists to move off. Anything else stands the swap down; Execute enforces
	// that too, and this is where it is decided with a name attached.
	// A store that could not be READ is not a store with nobody in it, and it is
	// not an identity problem the oracle can solve either -- resolveLive would
	// re-read the same store and fail the same way. The swap stands down here
	// rather than at the write, which is what stops the tick from failing on
	// every one of its 1 Hz passes: on the machine this was found on that was
	// 28,557 consecutive failures over eight hours, and the daemon spent its
	// whole replacement budget on a fault no fresh process could clear.
	if ev.LiveState == switcher.LiveUnreadable {
		if !e.saidUnreadable {
			e.saidUnreadable = true
			e.logf("not switching: this machine's login store cannot be read (%v), so whether an account "+
				"is live cannot be established, and installing over it could revoke a running session. "+
				"On macOS this is a locked login keychain -- run `security unlock-keychain "+
				"~/Library/Keychains/login.keychain-db` and restart the daemon FROM THAT SHELL, because "+
				"a successor inherits the audit session that is refusing", ev.LiveErr)
		}
		return switcher.Result{Outcome: switcher.Unreadable, Target: ev.Target}, nil
	}
	e.saidUnreadable = false

	foreign := false
	if ev.LiveState == switcher.LiveUnattributed {
		owner, verdict := e.resolveLive(ctx, s)
		switch verdict {
		case liveAdopted:
			live = owner.UUID
			if e.saidUnattributed {
				e.saidUnattributed = false
				e.logf("adopted the login Claude Code rotated for %s; it is live again by name", owner.Label())
			}
		case liveForeign:
			foreign = true
		default:
			if !e.saidUnattributed {
				e.saidUnattributed = true
				e.logf("not switching: the credentials file holds a login this store cannot name and " +
					"the profile endpoint could not say whose it is. Overwriting it could revoke an " +
					"account mid-rotation, so nothing is written until it can be named")
			}
			return switcher.Result{Outcome: switcher.Unattributed, Target: ev.Target}, nil
		}
	} else {
		e.saidUnattributed = false
	}
	res, err := switcher.Execute(s, switcher.Request{
		Target: ev.Target, LiveUUID: live, Unattended: true, LiveForeign: foreign,
		// The engine's clock, not the swap's own: every other decision this
		// tick is dated from it, and a staleness answer taken from a
		// different clock is a different tick's answer.
		Now:     e.now,
		Freshen: e.freshen(ctx),
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
	case switcher.Stale:
		// Latched PER ACCOUNT, like saidClaimNotice rather than like
		// saidOverridden: this state lasts until that account's next poll
		// refreshes its grant, which is minutes away, and the tick loop runs at
		// about 1 Hz. A bool would also hide the second account going stale
		// behind the first.
		if e.saidStale != res.Target.UUID {
			e.saidStale = res.Target.UUID
			if res.FreshenErr != nil {
				e.logf("not switching to %s: its stored login is one Claude Code would refresh on "+
					"sight, and refreshing it here failed: %v", res.Target.Label(), res.FreshenErr)
			} else {
				e.logf("not switching to %s: its stored login is one Claude Code would refresh on sight",
					res.Target.Label())
			}
		}
		return res, nil
	}
	e.saidStale = ""
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
	if res.ProfileSyncErr != nil {
		e.logf("Claude Code's displayed account name could not be updated: %v", res.ProfileSyncErr)
	}
	return res, nil
}

// dispatch starts a poll for every account that is due, and returns without
// waiting for any of them.
func (e *Engine) dispatch(ctx context.Context, s *store.Store, accounts []store.Account,
	cache *usage.Cache, cfg config.Config, thresholds func(uuid string) strategy.Thresholds,
	now time.Time, active string, activeKnown bool, quarantined map[string]bool) {

	members := identityMembers(accounts)
	for _, a := range accounts {
		if !pollable(s, a) {
			continue
		}
		// An account another machine owns is not this machine's to read on a
		// cadence. The reading spends a budget shared with whoever IS driving
		// it, and this machine can never rank it, so the request buys nothing.
		//
		// The gate is HERE and not in pollable() on purpose: pollable is also
		// the hand-held refresh path's gate, and a human who names an account
		// has said what they want more clearly than the flag has -- the same
		// reading `ccdad switch` already gives a Disabled account. Only the
		// unattended cadence is withheld.
		//
		// The live account is the exception, and it is not a courtesy: the
		// hysteresis baseline, the threshold test and the pre-emption projection
		// are all statements about the account Claude Code is logged in as.
		// Going blind on that one makes every tick the no-baseline case, which
		// the anti-flap cooldown is deliberately exempt from -- so the machine
		// would name a target every tick.
		if a.Elsewhere && !(activeKnown && a.UUID == active) {
			continue
		}
		entry, has := cache.Get(a.UUID)
		if !due(entry, has, now, a.UUID == active) {
			continue
		}
		if !e.claim(a.UUID) {
			// A poll from an earlier tick is still running. Starting a second
			// one would spend the identity's budget twice for one reading and
			// race two writers into the same cache row.
			//
			// Ahead of the PROBE decision as well, and not only the poll. That
			// poll has not committed yet, and when it does it writes an ordinary
			// cadence over the one-minute schedule RecordProbe leaves behind —
			// so the poll meant to read what the probe woke would land ten
			// minutes out instead of one, on a turn of quota already spent.
			continue
		}
		// A probe REPLACES this tick's poll rather than being scheduled beside
		// it, and the ordering is what makes that true: the account is one a
		// poller was about to reach, so nothing is polled more often because of a
		// probe, and the entry's reading is already older than the serve TTL —
		// which is what lets the poll a minute out actually happen instead of
		// being held at the freshness floor.
		//
		// Only a probe that was actually QUEUED replaces the poll, which is why
		// the claim is released here rather than in probeNow. One the daemon
		// could not start — no Claude Code on this PATH — replaces nothing, and
		// skipping the poll for it as well would leave an account unread forever:
		// the reading is what says the window still has no reset, so a tick that
		// never refreshes it can never find out that it does.
		if w, model, want := e.probeDue(a, entry, cfg, thresholds, now, active, activeKnown, quarantined); want &&
			e.probeNow(a, w, model, now) {
			e.release(a.UUID)
			continue
		}
		e.wg.Add(1)
		go func(a store.Account) {
			defer e.wg.Done()
			defer e.release(a.UUID)
			// Discarded: a tick reports through the status document, and
			// the failure is already in this account's pollRecord.
			_ = e.poll(ctx, a, cfg, thresholds, members[identityOf(a)], a.UUID == active)
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

// identityMembers maps each identity to every account that draws on it. The
// scheduler needs both halves: HOW MANY accounts share the budget, which is the
// divisor, and WHICH ones, because the danger band holds the others back.
func identityMembers(accounts []store.Account) map[string][]string {
	out := make(map[string][]string, len(accounts))
	for _, a := range accounts {
		id := identityOf(a)
		out[id] = append(out[id], a.UUID)
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

// due applies both gates: the serve TTL the last poll chose for this reading,
// which is the same rule the read path enforces and the only thing standing
// between a busy fleet and the endpoint's hourly allowance, and the schedule that
// poll set.
//
// The TTL is read through ScheduledTTL, which is the flat ServeTTL unless a
// future version wrote a LONGER one. It used to be the entry's own, so that the
// danger band's 30 s could unlock this gate for the band's 60 s cadence — which
// made the one structural bound on the hourly allowance a value the policy could
// rewrite. It cannot any more, and the band no longer asks it to.
//
// live is passed in because a stand-down written for the account that WAS live
// must not hold the one that is now.
func due(e usage.Entry, has bool, now time.Time, live bool) bool {
	if !has {
		return true
	}
	if e.FreshWithin(now, e.ScheduledTTL()) {
		return false
	}
	at := e.PollAt(live)
	if at.IsZero() {
		return true
	}
	return !now.Before(at)
}

// probeDue reports whether this tick should spend a turn of an account's own
// quota to start one of its windows' clocks, which window that is, and which
// model family the turn has to be spent against to reach it.
//
// Every condition is a way this goes wrong without it:
//
//   - probe_unknown off is the user saying not to spend their quota unasked.
//   - a disabled account is one the engine will not switch to, so a warm clock
//     for it buys nothing and the quota would be spent for nobody. A QUARANTINED
//     one is the same sentence: it is out of rotation, and the only thing that
//     ever quarantines is a refresh token the endpoint has already rejected —
//     which is the very credential a probe would seed its Claude Code with. That
//     errand cannot authenticate, so it would fail every rung forever.
//   - an account with no cold window is either unreadable — nothing to poll
//     rather than a stopped clock, and the answer to that is another poll, not a
//     turn of quota — or one where every window's clock is already running,
//     which ColdWindow's own doc explains.
//   - the ladder is what keeps a probe that FAILS — a login prompt, a model
//     outage — from being retried at this loop's 1 Hz.
//
// The rollover instant ColdWindow reports is handed to MayProbe rather than
// discarded, and that is the whole shape of the change this function carries.
// The old gate was one interval since the last attempt, which cannot express the
// thing that actually paces a healthy account: its own five-hour window running
// down. Six hours against a five-hour window left the clock stopped for an hour
// of every cycle — about 4.2-4.6 hours a day per account, measured — and it was
// stopped in the hour right after the rollover, when starting it is worth the
// most. MayProbe's rollover arm is "one attempt per rollover" with no interval
// in it at all.
//
// It does not stop at the account's BINDING window. strategy.HeadroomOf picks
// the single window with the least slack, because that is the one the ranking
// cares about — but a five-hour window can have a stopped clock beside a weekly
// cap that binds tighter and has a running one of its own, and the five-hour
// clock is the one whose lockout a warm-up shortens. `ccdad hover status` reads
// the same ColdWindow and the same gate (strategy.HoverWindow.Warmup), so what
// the table says is queued and what this fires on cannot drift.
//
// The live account is skipped, and that is the one exclusion worth spelling out.
// It is the account something else is already spending against, so a probe of it
// is the one probe that duplicates work outright — and it is the one that can
// break a session in flight: a probe runs its own Claude Code out of its own
// credential home holding the SAME OAuth login, and the server rotates the
// refresh token whenever either of them refreshes, revoking the copy the live
// session is using. What happens instead is nothing: the window gets its clock
// from the user's own next turn, which is the event a warm-up stands in for, and
// `ccdad probe <account>` stays available to a human who wants it now.
func (e *Engine) probeDue(a store.Account, entry usage.Entry, cfg config.Config,
	thresholds func(uuid string) strategy.Thresholds,
	now time.Time, active string, activeKnown bool, quarantined map[string]bool) (usage.WindowName, string, bool) {

	if !cfg.ProbeUnknown || a.Disabled || quarantined[a.UUID] {
		return "", "", false
	}
	// Which account is live has to be KNOWN, not merely unequal. An empty active
	// uuid is "could not be worked out" as well as "nobody", and the two have
	// opposite answers here: the first is the state where a probe is most
	// dangerous, because the account it would run against may be the one Claude
	// Code is logged in as. Nothing is lost by waiting — an engine that cannot
	// name the live account is about to switch, and a switch names it.
	if !activeKnown || a.UUID == active {
		return "", "", false
	}
	// thresholds(a.UUID), not cfg.Thresholds(): under hover the candidate window
	// set — and so the window a warm-up would start — is chosen against the same
	// per-account table the ranking used, not the raw config bundle hover
	// otherwise ignores.
	thr := thresholds(a.UUID)
	w, rollover, ok := strategy.ColdWindow(entry.Snapshot, "", thr, now)
	if !ok {
		return "", "", false
	}
	// The one refusal that is about money rather than about clocks. A turn spent
	// on an account with nothing left in a window can be billed to metered
	// credits, and unattended overage takes two independent opt-ins that a warm
	// clock is not one of. It is asked here rather than left to the child so that
	// the table and the daemon refuse on the same predicate.
	if strategy.WarmUpWouldSpendCredits(entry.Snapshot, "", thr) {
		return "", "", false
	}
	if !entry.MayProbe(now, w, rollover, !rollover.IsZero()) {
		return "", "", false
	}
	return w, probeModel(w), true
}

// probeModel is the model family a turn must be spent against to reach one
// window, or "" when any model reaches it.
//
// A model-scoped weekly window arrives named for its scope's DISPLAY name —
// "Opus 4.5" — and ModelFamily is what already reduces one of those to the token
// a --model argument is written with. Only the DISPLAY half is handed to it, and
// that is the whole point of taking the name apart rather than matching over it:
// ModelFamily is a substring match, the full name carries the scope KEY, and a
// deployment whose keys include one like sonnet_tier would otherwise have a
// family read out of a string that names no model at all — a turn of quota spent
// against a window nobody asked about.
//
// Everything else gets "", meaning any model reaches it: the turn is spent on
// whatever claude defaults to, which still wakes the five-hour window and is the
// honest answer for a scope ccdad cannot name.
//
// What this cannot express is a model VERSION. A cap scoped to "Opus 4.1" is
// probed with --model opus, which claude resolves to whatever it calls opus
// today, and if that is a different build the turn lands on a different window.
// The ladder is what bounds that, and it is why usage.ProbeState.ColdStreaks is
// keyed per window rather than per account: a window in this state is judged
// ineffective by every reading that follows a warm-up of it, climbs to the
// six-hour ceiling, and stays there — while the five-hour window beside it keeps
// its own clean record and its own rollover-paced schedule.
func probeModel(w usage.WindowName) string {
	if family, ok := strategy.FixedWindowFamily(w); ok {
		return family
	}
	// The prefix is asked for rather than spelled, so this cannot drift from the
	// one place a scoped name is assembled.
	prefix := string(usage.ScopedWindowName(usage.ScopeModel, ""))
	display, ok := strings.CutPrefix(string(w), prefix)
	if !ok {
		return ""
	}
	if family, ok := strategy.ModelFamily(display); ok {
		return family
	}
	return ""
}

// probeNow queues one probe and moves the poll that will read it out of the way.
// It reports whether the attempt was RECORDED, which is what makes it safe for
// the caller to skip this tick's poll: a recorded attempt has already scheduled
// that poll a minute out, and an unrecorded one has scheduled nothing.
func (e *Engine) probeNow(a store.Account, w usage.WindowName, model string, now time.Time) bool {
	// Asked here rather than once per tick: this is reached only for an account
	// that is otherwise worth warming, which on a healthy fleet is one account
	// at a time. The child inherits this process's PATH through ChildEnv, so what
	// resolves here is what will resolve there.
	//
	// Asked BEFORE the attempt is recorded, and that order is deliberate: a
	// machine with no Claude Code on it has not made a failed attempt — nothing
	// was tried and nothing was spent — so it must not consume a rung of the
	// ladder, or the account's clock stays stopped long after claude is finally
	// installed.
	if err := ProbeAvailable(); err != nil {
		if !e.saidNoClaude {
			e.saidNoClaude = true
			e.logf("not probing: %v. An account whose window has no clock running stays "+
				"unknown — no reset time, no pace, no projection — and pays a full five hours "+
				"of lockout the first time it is used. Install Claude Code, or "+
				"set probe_unknown = false to stop looking", err)
		}
		return false
	}
	if !e.saidProbeSpends {
		e.saidProbeSpends = true
		e.logf("probing accounts whose binding window reports no reset time. A probe SPENDS that " +
			"account's own quota — one turn of Claude Code — because the endpoint reports no reset " +
			"until something has been spent against the window. Set probe_unknown = false to stop.")
	}
	// Stamped BEFORE the spawn, and that order is the whole gate. The child is a
	// separate process, so a spawn that never starts would otherwise leave the
	// account probe-due on the very next tick, forever. The child stamps again
	// once it knows the outcome; both writers go through the cache's own lock and
	// the later one lands last. Neither stamp advances a streak — the reading
	// does, in commit — so the double write cannot count one attempt twice.
	if err := usage.RecordProbe(cacheTimeout, a.UUID, now, w, nil); err != nil {
		e.logf("recording %s's probe failed: %v", a.UUID, err)
		return false
	}
	if err := e.spawnProbe(a.UUID, model); err != nil {
		e.logf("probing %s failed to start: %v", a.UUID, err)
	}
	return true
}

func (e *Engine) spawnProbe(uuid, model string) error {
	if e.SpawnProbe != nil {
		return e.SpawnProbe(uuid, model)
	}
	return SpawnProbe(uuid, model)
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
func (e *Engine) poll(ctx context.Context, a store.Account, cfg config.Config,
	thresholds func(uuid string) strategy.Thresholds, identity []string, active bool) error {
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
		e.handleFailure(a, cfg, thresholds, err, now, identity, active)
		return err
	}
	e.commit(a, snap, now, identity, thresholds, active, nil)
	e.refreshProfile(ctx, a, token, now)
	return nil
}

// refreshProfile re-reads the account's profile when the stored one has aged
// past store.ProfileTTL, and writes the four fields a profile decides back to
// the store.
//
// WHY THIS EXISTS. Tier, RateLimitTier, SeatTier and OrganizationUUID were
// written once, by `ccdad add`, and by nothing else for the life of the
// installation. That was invisible until a switch began writing
// oauthAccount.seatTier from the stored value: an account added before this
// tree read seat_tier at all carries "", which Claude Code cannot tell from a
// pro or max seat that genuinely has none, so a money-metered enterprise seat
// silently loses the Opus tier its own `Zu()` would grant it. The warning
// `ccdad add` already prints when a profile lookup fails -- "the tier will fill
// in on the first usage refresh" -- was simply untrue until this ran.
//
// IT IS DELIBERATELY AFTER commit AND CANNOT AFFECT IT. The usage reading is
// what a poll exists for and it is already recorded by the time this runs, so a
// profile endpoint that is down, slow or rate-limiting costs the fleet nothing
// that matters. Every failure here is logged and dropped for that reason: there
// is no schedule to back off, because the only thing a failure delays is a
// re-read of a fact that changes a handful of times in an account's life.
//
// IT SPENDS A SECOND REQUEST, which is why the gate is a day rather than a
// tick. The endpoint's allowance belongs to the identity and every poll already
// spends one on usage; doubling that permanently, for a value that is stable
// for months, would buy nothing. store.ProfileTTL is Claude Code's own figure.
func (e *Engine) refreshProfile(ctx context.Context, a store.Account, token string, now time.Time) {
	if e.FetchProfile == nil || token == "" || !a.ProfileStale(now) {
		return
	}
	p, err := e.FetchProfile(ctx, token)
	if err != nil {
		e.logf("re-reading %s's profile failed: %v", a.UUID, err)
		return
	}
	// Out here rather than inside commit's usage-cache callback, for the reason
	// history.Record and ApplyUsage are both out there: this takes the store's
	// mkdir mutex, and taking one of those while holding the usage cache's
	// would hold the cache shut against every reader for as long as the store
	// happened to be contended.
	if serr := store.WithStore(func(s *store.Store) error {
		return s.ApplyProfile(a.UUID, p, now)
	}); serr != nil && !errors.Is(serr, store.ErrNotFound) {
		// An account that is no longer in the store is an ordinary race with
		// `ccdad remove`, not a failure worth logging.
		e.logf("recording %s's re-read profile failed: %v", a.UUID, serr)
	}
}

// handleFailure decides what a failed poll means. Only ONE of the failures says
// anything about the account.
func (e *Engine) handleFailure(a store.Account, cfg config.Config,
	thresholds func(uuid string) strategy.Thresholds, err error,
	now time.Time, identity []string, active bool) {
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
		e.commit(a, nil, now, identity, thresholds, active, nil)
		return

	case errors.Is(err, usage.ErrRateLimited):
		var se *usage.StatusError
		var retry time.Duration
		var hasRetry bool
		if errors.As(err, &se) {
			retry, hasRetry = se.RetryAfter()
		}
		e.commit(a, nil, now, identity, thresholds, active, func(st pollpolicy.State) pollpolicy.State {
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
	e.commit(a, nil, now, identity, thresholds, active, nil)
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
	identity []string, thresholds func(uuid string) strategy.Thresholds, active bool,
	adjust func(pollpolicy.State) pollpolicy.State) {

	// Resolved once, before the cache callback: it is read twice inside and the
	// value is the same for the whole call. thresholds(a.UUID) rather than
	// cfg.Thresholds() so that under hover the poll cadence and the Exhausted
	// status below are measured against the same per-account table the ranking
	// just used, not the raw config bundle hover otherwise ignores.
	thr := thresholds(a.UUID)
	err := usage.WithCache(cacheTimeout, func(c *usage.Cache) error {
		entry, had := c.Get(a.UUID)
		state := pollStateOf(entry)
		if adjust != nil {
			state = adjust(state)
		}

		// The verdict on the last warm-up, taken BEFORE FetchedAt is moved: it
		// is the stamp that says whether this reading is one the warm-up has
		// been waiting for. Judged is where the rule lives; what matters here is
		// only that it runs on every commit, including the ones with no reading
		// at all, and that it runs exactly once per attempt because this same
		// callback then writes the FetchedAt that ends the wait.
		entry.Probe = entry.Probe.Judged(entry.FetchedAt, snap, now)

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

		// HeadroomOrCredit, not HeadroomOf: a seat metered only in money has no
		// plan window, so the window-only axis leaves Known false and both
		// urgency bands below exclude it by their first line. A seat at 99% of
		// its balance would poll on the lazy cadence forever.
		h := strategy.HeadroomOrCredit(entry.Snapshot, thr)
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
		// twice the budget. Share is what carries the danger band's one
		// exemption; PerIdentity would divide the cadence that must not be
		// divided.
		entry.NextPollAt = now.Add(pollpolicy.Share(at.Sub(now), len(identity), in))
		entry.ServeTTL = pollpolicy.ServeTTLFor(in)
		// Aim the next look at the moment a clock runs down, so the tick that
		// finds the window cold is the one just after the rollover rather than
		// one the idle cadence happens to land later. Without this the warm-up
		// waits for a poll to DISCOVER the cold window and then for the next due
		// tick to act on it, and both intervals are the ten-minute idle cadence:
		// about 900 s of stopped clock per cycle, all of it in the minutes right
		// after the reset.
		//
		// StandDownUntil is deliberately not touched, and Entry.PollAt still
		// takes the later of the two. So this is a guarantee that the clock is
		// restarted at rollover UNLESS the endpoint is refusing us — congestion
		// outranks the clock, which is the same order every other schedule here
		// obeys.
		entry.NextPollAt = warmClamp(entry.NextPollAt, e.warmTarget(entry.Snapshot, thr, now), now, entry.ScheduledTTL())
		entry.Poll = usage.PollState{Interval: next.Interval, LastRateLimited: next.LastRateLimited}
		entry.Poll.LastBindingPct, entry.Poll.HasLastBinding = next.LastBindingPct, next.HasLastBinding
		// entry.StandDownUntil is read and written back untouched, and that is
		// what makes the two writers safe: this account's poll never edits the
		// field its identity's live account owns, so the order the two goroutines
		// finish in cannot change the outcome.
		c.Put(a.UUID, entry)
		// The two are recorded apart for the reason the entry keeps them apart:
		// whether this account is live is a fact about now, and the reader
		// applies it.
		e.scheduled(a.UUID, entry.NextPollAt)
		e.stoodDown(a.UUID, entry.StandDownUntil)
		if pollpolicy.InDangerBand(in) {
			e.standDown(c, identity, a.UUID, now)
		}
		return nil
	})
	if err != nil {
		e.logf("recording %s's reading failed: %v", a.UUID, err)
	}

	// The series is appended to HERE — after usage.WithCache has returned and
	// released its lock, never from inside its callback. Both are cclock mkdir
	// mutexes on the same store, and nothing else in this tree holds one while
	// taking another. Nesting them would not deadlock, because the two lock
	// directories differ; it would do something quieter and worse — hold the
	// usage cache shut against every reader and every other poller for as long
	// as the series lock happened to be contended, which is a wait bounded only
	// by historyTimeout.
	//
	// Only a poll that produced a reading writes one. The other three callers
	// of commit are failure paths that re-stamp scheduling state and leave the
	// last good reading alone, so a sample taken there would place the PREVIOUS
	// reading's percentage at the current instant: a flat segment that never
	// happened, dragging a measured burn rate toward zero for as long as the
	// outage lasts.
	//
	// A failure is logged and dropped. The reading itself is already committed
	// above, which is the part that had to succeed.
	if snap != nil {
		if herr := history.Record(historyTimeout, a.UUID, historySample(snap, now), now); herr != nil {
			e.logf("appending %s's sample to the usage history failed: %v", a.UUID, herr)
		}
		// The classification, revised from the same reading and under the same
		// gate. `ccdad add` and `add-token` both classify from the profile
		// alone -- neither calls the usage endpoint -- so an account whose
		// profile was unreadable, or whose billing_type says something this
		// build has no rule for, is filed on a guess. This is the only thing
		// that ever revises that guess, and it is also the only writer of the
		// stored credit balance.
		//
		// Out here for the same reason history.Record is: it takes the store's
		// mkdir mutex, and taking one of those while holding the usage cache's
		// would hold the cache shut against every reader for as long as the
		// store happened to be contended.
		//
		// snap != nil is the gate ApplyUsage's own signature demands. A poll
		// that produced no reading is not evidence, and a credit account that
		// has run out reports overage off and no windows -- re-classifying on
		// that would file it as a subscription at the moment it went broke.
		if serr := store.WithStore(func(s *store.Store) error {
			return s.ApplyUsage(a.UUID, snap, now)
		}); serr != nil && !errors.Is(serr, store.ErrNotFound) {
			// A reading for an account that is no longer in the store is an
			// ordinary race with `ccdad remove`, not a failure worth logging.
			e.logf("revising %s's classification from its reading failed: %v", a.UUID, serr)
		}
	}
}

// historySample copies one reading into the shape the series stores. It is only
// ever handed a reading: a poll that produced none is not a sample, and its
// caller is the gate that says so.
//
// It COPIES rather than keeping the snapshot. usage.Entry.Snapshot is shared and
// read-only — the cache hands one pointer to every reader — so a series holding
// it would publish whatever some later poll made of it.
//
// Three tri-states cross here and none of them may flatten. A window whose
// utilization could not be read is ABSENT from the sample rather than present at
// zero, because nothing read is not nothing used. Credit is recorded only where
// the account both reports overage enabled and answers with a balance, because a
// money figure that cannot be read refuses rather than defaulting. And a nil
// Limit is the account reporting no monthly cap, which means unlimited — a zero
// would mean a cap of nothing, the opposite verdict.
func historySample(s *usage.Snapshot, at time.Time) history.Sample {
	windows := s.AllWindows()
	sample := history.Sample{At: at}
	for _, w := range windows {
		pct, ok := w.Percent()
		if !ok {
			continue
		}
		r := history.Reading{Pct: pct}
		if reset, ok := w.Reset(); ok {
			// Truncated to the minute because the endpoint regenerates
			// resets_at's sub-second part from its own clock on every request:
			// two readings of one unrolled window carry different microseconds,
			// and a reader that segments a series wherever the reset changed
			// would see every consecutive pair as a rollover. Nothing is lost,
			// because no window rolls over on a sub-minute boundary.
			r.Reset = reset.Truncate(time.Minute)
		}
		if sample.Windows == nil {
			sample.Windows = make(map[usage.WindowName]history.Reading, len(windows))
		}
		sample.Windows[w.Name] = r
	}
	if s.ExtraUsage.State == usage.ExtraUsageEnabled {
		if used, ok := s.ExtraUsage.UsedCredits(); ok {
			c := history.Credit{Used: used, Currency: s.ExtraUsage.CurrencyCode()}
			if limit, ok := s.ExtraUsage.MonthlyLimit(); ok {
				c.Limit = &limit
			}
			sample.Credit = &c
		}
	}
	return sample
}

// warmTarget is when the next of this account's clocks runs down, plus the
// margin that keeps the reading aimed at it from arriving before the rollover
// it is aimed at. Zero means there is nothing to aim at.
//
// Zero is the ordinary answer for the account this exists to serve, and that is
// not a contradiction: an account whose five-hour clock is already stopped has
// no future rollover to schedule against, and it does not need one — it is
// probe-due on this very tick. The schedule matters for the account whose clock
// is RUNNING, which is the one that will go cold later and unattended.
//
// The margin is jittered across [ProbeWakeMargin, 2*ProbeWakeMargin) because
// rollover instants cluster: a fleet warmed in one batch rolls over in one
// batch — the live one warmed three accounts within 19 minutes — and the anchors
// themselves land on ten-minute boundaries. An unjittered margin would put
// several `claude -p` children on the same second.
func (e *Engine) warmTarget(s *usage.Snapshot, thr strategy.Thresholds, now time.Time) time.Time {
	at, ok := strategy.NextResetAmong(s, "", thr, now)
	if !ok {
		return time.Time{}
	}
	return at.Add(usage.ProbeWakeMargin + time.Duration(e.rand()*float64(usage.ProbeWakeMargin)))
}

// warmClamp is the next poll instant, given the one the cadence chose and the
// one a rollover wants.
//
// Three cases, and the middle one is the only surprising one:
//
//   - the cadence would look at or after the target: take the target. This is
//     the ordinary idle account, whose ten-minute cadence would otherwise walk
//     past the rollover.
//   - the cadence would look shortly BEFORE the target: pull that look back so
//     the reading it takes is a full ttl old by the time the target arrives.
//     Without this the poll at the target is refused by due()'s own freshness
//     floor — the reading is younger than ScheduledTTL — and the warm-up slips a
//     whole interval on roughly a third of cycles. The pull-back is skipped when
//     it would land in the past, because a reading taken now cannot be made
//     older by scheduling.
//   - the cadence would look long before the target: leave it. Its reading will
//     have aged past the floor on its own.
//
// A zero target means there is nothing to aim at and the cadence stands.
func warmClamp(natural, target, now time.Time, ttl time.Duration) time.Time {
	if target.IsZero() {
		return natural
	}
	if !natural.Before(target) {
		return target
	}
	if pull := target.Add(-ttl); natural.After(pull) && pull.After(now) {
		return pull
	}
	return natural
}

// standDown pushes every other account on one identity to the congestion ceiling
// while the live one is inside the danger band.
//
// It writes StandDownUntil and never NextPollAt, because the two are different
// facts with different writers: the schedule an account's own poll earned —
// including whatever floor a 429 bought it — belongs to that poll, and this runs
// from somebody else's. usage.Entry.PollAt takes the later of the two, so a
// stand-down can hold an account back and can never let one out early.
//
// An account with NO entry is skipped rather than given one. It has never been
// read, which makes it the account on this identity the engine can least afford
// to leave unrankable, and one request buys a candidate to switch to; a
// stand-down written over the top of nothing buys half an hour of the same
// blindness.
func (e *Engine) standDown(c *usage.Cache, identity []string, live string, now time.Time) {
	for _, uuid := range identity {
		if uuid == live {
			continue
		}
		entry, ok := c.Get(uuid)
		if !ok {
			continue
		}
		if now.Before(entry.StandDownUntil) {
			// Already standing down, and NOT renewed. This is the difference
			// between yielding a share of the budget and never being polled
			// again: the band recommits every 60 s, so a deadline pushed forward
			// on every one of those moves away faster than the clock approaches
			// it, and an alternate would go unread for as long as the live
			// account stayed in the band — which is exactly the span in which
			// the engine needs an alternate it can rank. Renewing only once the
			// previous one has expired is the two requests an hour that keep it
			// rankable, which is what a stand-down was defined to be.
			//
			// It also subsumes the guard this replaces: from a deadline already
			// past, now plus a jittered ceiling is always later, so a stand-down
			// can never move backwards.
			continue
		}
		entry.StandDownUntil = pollpolicy.StandDownUntil(now, e.rand())
		c.Put(uuid, entry)
		e.stoodDown(uuid, entry.StandDownUntil)
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

// empty is "some window this account carries has nothing left in it", which is
// a different fact from exhausted and is published under its own name.
//
// It delegates for the same reason exhausted does: the raw-versus-pace
// distinction has exactly one implementation in the ranking, and a second
// spelling here would be the copy that drifts.
func empty(h strategy.Headroom) bool {
	out, _ := strategy.OutOfQuota(h)
	return out
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

// stoodDown records a stand-down against an account, whoever wrote it. Snapshot
// applies it only while that account is not the live one.
func (e *Engine) stoodDown(uuid string, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec := e.polls[uuid]
	rec.hold = at
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
// quarantinedSet is the accounts this pass is holding out of rotation, as a
// lookup. It is built once per tick because both the scheduler and the status
// document ask about it, and a second walk of the same slice is a second place
// for the two to disagree.
// configuredThresholds is every account's answer when hover is off, or when
// there is no evaluation this tick trusts enough to derive one from: the
// config bundle, unchanged for every uuid.
func configuredThresholds(cfg config.Config) func(uuid string) strategy.Thresholds {
	thr := cfg.Thresholds()
	return func(string) strategy.Thresholds { return thr }
}

// hoverThresholds is the one table this tick's dispatch, commit, probeDue and
// publish all measure against.
//
// internal/cli/status.go's rowThresholds solves the identical problem for
// rendering — route through the engine's own HoverPlan when hover is on,
// because hover divides the quota by a pool (usable, non-quarantined
// accounts) that only a ranking pass knows — and this is the same fix for the
// daemon's own scheduling and state-publishing paths, which had never made
// that call at all: they read cfg.Thresholds() directly, so hover changed what
// `ccdad auto` ranked on without changing what the daemon polled on, probed
// on, or reported as engine.state.
//
// ev.Plan.Hover is nil when hover is on but the pass never ran — nothing has
// been polled yet — and the configured bundle is the only answer there is
// nothing to derive one from.
func hoverThresholds(cfg config.Config, ev switcher.Evaluation) func(uuid string) strategy.Thresholds {
	if !cfg.Hover || ev.Plan.Hover == nil {
		return configuredThresholds(cfg)
	}
	return ev.Plan.Hover.For
}

func quarantinedSet(ev switcher.Evaluation) map[string]bool {
	out := make(map[string]bool, len(ev.Plan.Quarantined))
	for _, uuid := range ev.Plan.Quarantined {
		out[uuid] = true
	}
	return out
}

func (e *Engine) publish(accounts []store.Account, cache *usage.Cache,
	ev switcher.Evaluation, thresholds func(uuid string) strategy.Thresholds, quarantined map[string]bool) {

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
		// account immediately. A stand-down is part of that answer for every
		// account except the live one, exactly as it is for dispatch — a
		// document that said 60 s about an account the dispatcher will not touch
		// for half an hour is the disagreement this document exists to prevent.
		if entry, ok := cache.Get(a.UUID); ok {
			row.NextPollAt = entry.PollAt(a.UUID == status.ActiveUUID)
		}
		row.State = accountState(a, cache, quarantined[a.UUID], status.ActiveUUID, thresholds)
		status.Accounts = append(status.Accounts, row)
	}
	e.status = status
}

func accountState(a store.Account, cache *usage.Cache, quarantined bool,
	activeUUID string, thresholds func(uuid string) strategy.Thresholds) AccountState {

	// There is no case for the primary flag, and its absence is the decision:
	// primary says where the engine RANKS an account, never that it should be
	// left alone. Giving it an arm here would publish a usable seat as though its
	// owner had held it out of rotation.
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
	// HeadroomOrCredit, not HeadroomOf: Unknown means "nobody could read this",
	// and a seat metered only in money was read perfectly well -- it just has no
	// plan window to be read on. Publishing Unknown for the whole fleet makes
	// every consumer keyed on the states below inert.
	h := strategy.HeadroomOrCredit(entry.Snapshot, thresholds(a.UUID))
	if !h.Known {
		return StateUnknown
	}
	// Empty is tested first: an empty account is necessarily past its threshold
	// too, and the more specific answer is the one worth publishing.
	if empty(h) {
		return StateEmpty
	}
	if exhausted(h) {
		return StateExhausted
	}
	return StateCandidate
}
