package switcher

import (
	"fmt"
	"time"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Evaluation is one pass of the engine over the store and the on-disk usage
// cache: what the ranking could see, and what it decided.
//
// It carries the degraded inputs rather than reporting them, because the two
// callers report differently — the CLI to a terminal, the daemon to a log and
// an NDJSON stream — and a package that printed would force one of them to
// parse the other's sentences.
type Evaluation struct {
	// Plan is the ranking's answer. Its Action maps onto the exit contract's
	// codes: ActionSwitch is 0, ActionStay is 3, ActionBlocked is 4.
	//
	// Decided says whether the ranking ran at all, and reading Plan without it
	// is the trap this field exists for: a zero Plan does not stringify to
	// nothing, it stringifies to plausible values — Reason(0) is "a better
	// target cleared every margin" and Mode(0) is "headroom" — so a report
	// built from one does not look empty, it looks wrong.
	Plan    strategy.Plan
	Decided bool
	// Target is Plan.Target resolved against the store, set only when the plan
	// says to switch.
	Target    store.Account
	HasTarget bool
	// Live is the account the credentials FILE names, which is the hysteresis
	// baseline and the value an unattended swap is conditional on.
	//
	// LiveState is the same read kept at four values rather than two. An
	// unattended caller needs it: LiveKnown false spans a machine with nobody
	// logged in and a machine whose live login this store cannot name, and only
	// the first of those is safe to overwrite.
	Live      store.Account
	LiveKnown bool
	LiveState LiveState
	// LiveErr is a live file that could not be read. It is not fatal: the
	// baseline is lost, so every candidate is compared against nothing, and
	// this is the state where moving to a known-good account matters most.
	LiveErr error
	// NoReadings means nothing has ever been polled, so there is no evidence to
	// choose on. Distinct from ActionBlocked, which is a choice that was made
	// and came back empty — exit 4 is "wanted, but no viable target", and both
	// end there, but only one of them is worth telling the user to run the
	// daemon about.
	NoReadings bool
	// Forced reports that Force overrode an anti-flap hold.
	Forced bool
	// ConfigErr is a config file that could not be used; the built-in defaults
	// were substituted. Refusing to switch because a threshold was mistyped is
	// a worse answer than switching on the documented default.
	ConfigErr error
	// StateErr is engine state that could not be read; the pass ran with no
	// cooldown and no quarantines.
	StateErr error
	// LastSwitchAt and LastSwitchTo are the cooldown stamp this pass read. The
	// daemon publishes them; nothing in the ranking needs them, because the
	// cooldown gate has already been applied by the time this is returned.
	LastSwitchAt time.Time
	LastSwitchTo string
}

// EvalOptions are the inputs that do not come from disk.
type EvalOptions struct {
	// Strategy overrides the configured one. `switch --strategy` is explicit
	// and the file is not, so the flag wins; leave HasStrategy false and the
	// config's own key decides, which is the daemon's case.
	Strategy    strategy.Strategy
	HasStrategy bool
	// Model is the model the chosen account is about to run, as the user typed
	// it. It narrows the ranking to the windows that bind for that model;
	// empty is the unqualified pass, which is what the daemon runs. There is no
	// Has- flag beside it because there is no config key to override: an empty
	// model IS the default, rather than the absence of an opinion.
	Model string
	// Force bypasses the anti-flap margins, and only those. It never bypasses
	// the credit gate: that one spends money, and a flag named "force" is not
	// the two independent opt-ins a switch onto a credit account requires.
	Force bool
	// Now is the clock. Zero means time.Now.
	Now time.Time
	// Config supplies the engine's knobs, and whatever went wrong getting them.
	// The config it returns is used either way: that IS the warning contract.
	//
	// Nil reads the file and falls back to the built-in defaults, which is
	// right for a one-shot — there is no previous config to keep. A daemon must
	// pass config.Reloader.Reload instead: the last-good-config rule says an
	// unusable file leaves the engine on the LAST CONFIG THAT PARSED, and
	// silently reverting a tuned threshold to stock because somebody mistyped an
	// edit is the failure that rule exists to prevent.
	Config func() (config.Config, error)
	// Provider narrows the pass to one provider's accounts. The ZERO value
	// reads as Claude, which is what keeps every existing call site on the
	// path it is on today without naming one.
	//
	// It is not a filter on the OUTPUT. It is handed to the candidate builder,
	// so an account of the other provider is never ranked, never wins, and is
	// never returned as a target -- which is the property that has to hold,
	// because a target is handed to a switch that rewrites Claude Code's
	// credentials file.
	Provider provider.ID
}

// provider is the pass's provider, defaulting a zero value to Claude.
func (o EvalOptions) provider() provider.ID {
	if o.Provider == "" {
		return provider.Claude
	}
	return o.Provider
}

// config resolves the engine's knobs. Both branches return a usable config;
// only the error differs.
func (o EvalOptions) config() (config.Config, error) {
	if o.Config != nil {
		return o.Config()
	}
	cfg, err := config.Load()
	if err != nil {
		return config.Defaults(), err
	}
	return cfg, nil
}

func (o EvalOptions) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

// Evaluate runs the engine once. It READS the usage cache and never fetches:
// that is the same rule `ccdad list` follows, and for the same reason —
// /api/oauth/usage allows roughly 28-30 requests per identity per rolling hour
// over a SLIDING window, so a command a user can run in a loop must not be a
// way to spend it.
//
// It takes no Claude Code lock at all. The swap does; this only decides.
func Evaluate(s *store.Store, opts EvalOptions) (Evaluation, error) {
	var ev Evaluation
	accounts := s.Accounts()
	now := opts.now()

	live, err := cclink.Load()
	if err != nil {
		// Not fatal, and NOT "nobody is live" either. Handing the nil blob to
		// LiveStateOf answers LiveNone, which is the one reading a caller must
		// not act on: it says the machine has nothing to lose, and an
		// unreadable store is exactly where it may have everything to lose.
		// Attribution here is the hysteresis baseline, and Execute re-reads
		// under the lock before it writes anything.
		ev.LiveErr = err
		ev.Live, ev.LiveState = store.Account{}, LiveUnreadable
	} else {
		ev.Live, ev.LiveState = LiveStateOf(live, accounts, s.Credentials)
	}
	ev.LiveKnown = ev.LiveState == LiveManaged
	liveUUID := ""
	if ev.LiveKnown {
		liveUUID = ev.Live.UUID
	}

	cache, err := usage.LoadCache()
	if err != nil {
		return ev, err
	}
	// An entry older than its account's AddedAt belonged to a previous account
	// at the same uuid, and letting it through would hand a fresh login the
	// headroom its predecessor had already spent.
	added := make(map[string]time.Time, len(accounts))
	for _, a := range accounts {
		added[a.UUID] = a.AddedAt
	}
	cache.Prune(added)

	cands := engineCandidates(s, accounts, cache, opts.provider())
	if !anyReading(cands) {
		// No evidence at all. Moving here would not be a choice, it would be a
		// reshuffle.
		ev.NoReadings = true
		return ev, nil
	}

	// The cooldown is read from DISK. A one-shot evaluation has no in-memory
	// anti-flap history, and one that ignored the cooldown would ping-pong
	// against a running daemon that is honouring it.
	st, err := strategy.LoadState()
	if err != nil {
		return ev, err
	}
	ev.StateErr = st.LoadError()
	ev.LastSwitchAt, ev.LastSwitchTo = st.LastSwitch()

	// A config that cannot be used is a warning, never a failure, because
	// refusing to switch over a mistyped threshold stops the engine silently,
	// which is the worse outcome.
	cfg, cerr := opts.config()
	ev.ConfigErr = cerr

	o := cfg.RankOptions(now)
	if opts.HasStrategy {
		o.Strategy = opts.Strategy
	}
	o.Model = opts.Model
	ev.Plan, ev.Decided = strategy.Decide(cands, o, cfg.StrategyConfig(), st, liveUUID), true

	if ev.Plan.Action == strategy.ActionStay && opts.Force {
		if best, ok := forceableTarget(ev.Plan, liveUUID); ok {
			ev.Forced = true
			ev.Plan.Action, ev.Plan.Target = strategy.ActionSwitch, best
		}
	}

	if ev.Plan.Action == strategy.ActionSwitch {
		target, ok := s.Get(ev.Plan.Target.UUID)
		if !ok {
			return ev, fmt.Errorf("the engine chose %q, which is no longer in the store", ev.Plan.Target.UUID)
		}
		ev.Target, ev.HasTarget = target, true
	}
	return ev, nil
}

// engineCandidates projects the store and the on-disk usage cache onto what the
// ranking takes, for ONE provider.
//
// The provider skip is stated here even though Installable already drops every
// Codex account -- a Codex blob holds no claudeAiOauth and no api-key record,
// so it is not installable. That is the reason to say it anyway: the skip that
// happens to work is a coincidence of what a Codex credential looks like
// today, and the rule is about which pool an account belongs to. It is also
// why the Codex lane has its own candidate builder rather than reusing this
// one with a different provider -- Installable would empty its pool.
func engineCandidates(s *store.Store, accounts []store.Account, c *usage.Cache, p provider.ID) []strategy.Candidate {
	out := make([]strategy.Candidate, 0, len(accounts))
	for _, a := range accounts {
		if a.Provider != p {
			continue
		}
		if !Installable(s.Credentials(a.UUID)) {
			continue
		}
		cand := strategy.Candidate{UUID: a.UUID, Kind: a.Kind, Disabled: a.Disabled, Elsewhere: a.Elsewhere, Primary: a.Primary}
		if e, ok := c.Get(a.UUID); ok {
			cand.Usage = e.Snapshot
			// The two stamps the scheduler wrote. The pre-emptive switch
			// projects across the gap between them, and taking it from the
			// cache rather than from the clock is what makes that gap the
			// engine's real blind interval — the 1800 s one a 429 earned
			// included.
			cand.FetchedAt, cand.NextPollAt = e.FetchedAt, e.NextPollAt
			// The warm-up's own record travels with them: hover's table
			// reports what the daemon would do about a stopped clock, and it
			// can only do that from the state the daemon gates on.
			cand.Probe = e.Probe
			// The poller's own 429 record. The pre-emptive switch is its only
			// reader; Candidate.LastRateLimited says why it is not a tier.
			cand.LastRateLimited = e.Poll.LastRateLimited
		}
		out = append(out, cand)
	}
	return out
}

// anyReading reports whether ccdad has ever read any of these accounts.
func anyReading(cands []strategy.Candidate) bool {
	for _, c := range cands {
		if c.Usage != nil {
			return true
		}
	}
	return false
}

// forceableTarget is the best account Force may move to: the top of the
// ranking, when that is not already the live one. A credit account is never
// returned, which is what keeps Force away from the credit gate.
func forceableTarget(plan strategy.Plan, liveUUID string) (strategy.Ranked, bool) {
	if len(plan.Result.Order) == 0 {
		return strategy.Ranked{}, false
	}
	best := plan.Result.Order[0]
	if best.UUID == liveUUID {
		return strategy.Ranked{}, false
	}
	return best, true
}

// Explain turns a plan into a sentence, adding the detail the reason alone
// cannot carry. Both callers use it, so the engine says the same thing in a
// terminal and in daemon.log.
func Explain(plan strategy.Plan) string {
	msg := plan.Reason.String()
	if plan.Reason == strategy.ReasonCreditGate {
		// The gate's own reason is the actionable half: "the credit gate
		// refused" tells nobody whether to raise max_auto_spend or to call
		// their organization.
		msg += " — " + plan.Credit.Reason.String()
		if plan.Credit.DisabledReason != "" {
			msg += " (" + plan.Credit.DisabledReason + ")"
		}
	}
	if plan.HasRetryAt {
		msg += fmt.Sprintf("; try again after %s", plan.RetryAt.Format(time.Kitchen))
	}
	if len(plan.Quarantined) > 0 {
		msg += fmt.Sprintf("; %d account(s) quarantined, re-run 'ccdad add' for them", len(plan.Quarantined))
	}
	return msg
}
