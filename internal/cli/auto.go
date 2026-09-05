package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
)

// autoSchemaVersion is the NDJSON contract's version. The contract is ADDITIVE:
// fields and event kinds may be added, so a consumer must ignore both rather
// than fail on them, and this only moves for a change that removes or
// reinterprets something.
const autoSchemaVersion = 1

// autoTickInterval is the continuous form's cadence, the tick loop's one
// second. It matches the daemon deliberately: `auto` exists to be the daemon's
// engine with nothing detached, and an engine that evaluated on a different
// rhythm would not be describing the thing it is standing in for.
const autoTickInterval = time.Second

func newAutoCmd() *cobra.Command {
	var once, asJSON bool

	cmd := &cobra.Command{
		Use:   "auto",
		Short: "Run the auto-switch engine",
		Long: "With --once, run one evaluation and exit — the cron and testing surface for\n" +
			"the whole engine. Without it, evaluate continuously in the foreground until\n" +
			"interrupted, which is what the daemon does with nothing detached.\n\n" +
			"It never polls: it reads the same on-disk usage cache 'ccdad status' reads.\n" +
			"Run the daemon, or 'ccdad status --refresh', to freshen it.\n\n" +
			"It is Claude-only: it never moves the account codex is served from. Run the daemon\n" +
			"for that, or 'ccdad switch --provider codex' by hand.\n\n" +
			"Exit codes are the point. 0 switched; 3 nothing to do; 4 wanted to move and\n" +
			"could not, which is the one to alert on; 2 only ever a usage error.\n\n" +
			"--json emits NDJSON — one object per line, not one document. It is the only\n" +
			"ccdad command that does; every other --json prints a single object.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			em := newAutoEmitter(cmd, asJSON)
			s, err := store.Open()
			if err != nil {
				return err
			}
			if once {
				code, err := autoPass(cmd.Context(), em, s)
				if err != nil {
					return err
				}
				// A broken stream outranks the pass's own code. EPIPE reaches
				// ExecuteWith through it and becomes 0, because a reader that
				// has gone away is not an error and `ccdad auto --json | head -1`
				// exits 0; any other write failure becomes 1, because reporting
				// the evaluation's answer would claim an output nobody received.
				if em.err != nil {
					return em.err
				}
				if code == ExitOK {
					return nil
				}
				return WithCode(errSilent, code)
			}
			// SIGINT is trapped for this command's span only, the way `add`
			// traps it for its login. Left at its default disposition it kills
			// the process mid-tick, and a tick killed mid-swap abandons Claude
			// Code's three lock directories on disk — whose stale windows are
			// 60 s, 60 s and 15 s, so Claude Code's own refresh wedges for up to
			// a minute over a Ctrl-C.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return runAutoLoop(ctx, em, s, autoTickInterval)
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "run a single evaluation and exit")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit an NDJSON event stream on stdout")
	return cmd
}

// runAutoLoop is the continuous form: the daemon's tick loop, in the
// foreground, holding the daemon's singleton.
//
// Taking the singleton is the answer to this command's one open question. Two
// processes executing switches would fight the cooldown and the quarantine
// list, which live on DISK and are read once per evaluation, so neither would
// see the other's decision until after acting on a stale one. One of them has
// to refuse, and the daemon is the one with a reason to be there.
//
// The refusal is 3, not 1. A daemon already running the engine is the world
// being as the caller asked for it, which is exactly what exit.go reserves 3
// for — and ErrSingletonHeld is a distinct sentinel from ErrLocksUnsupported
// for this reason, so a filesystem with no working locks still reports a
// failure.
//
// The credential-home claim is the second exclusion and a different question:
// the singleton keeps two engines out of one STORE, and the claim keeps two
// stores off one Claude Code login. Its refusal is 4 rather than 3, because
// another store's engine driving this login is not the world being as the
// caller asked — it is the state exit.go reserves 4 to alert on.
//
// The return is NAMED, and it has to be: the release defers below assign to it,
// and with an unnamed return those assignments land in a dead local. That was
// already true of the singleton's release before the claim was added — a
// failure to give the singleton back was being discarded silently, which is the
// one error on this path a next invocation actually trips over.
func runAutoLoop(ctx context.Context, em *autoEmitter, s *store.Store, interval time.Duration) (err error) {
	single, err := daemon.AcquireSingleton()
	if errors.Is(err, daemon.ErrSingletonHeld) {
		em.say("A ccdad daemon is already running the engine, so this would fight it for the cooldown.")
		em.say("Watch it with 'ccdad daemon logs', or run 'ccdad auto --once' for a single evaluation.")
		return WithCode(errSilent, ExitNothingToDo)
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, single.Release()) }()

	// The other axis, and the same policy the daemon applies: only an engine
	// that demonstrably holds this credential home stops us. Everything else is
	// reported and run through, because refusing on a filesystem that cannot
	// lock would take `ccdad auto` away from every machine with a network home.
	claim, claimErr := credhome.Acquire()
	switch {
	case errors.Is(claimErr, credhome.ErrClaimed):
		em.say("%v", claimErr)
		return WithCode(errSilent, ExitBlocked)
	case claimErr != nil:
		em.notice("running without the credential-home claim (%v); if a second ccdad store is "+
			"driving this Claude Code login, nothing here will notice", claimErr)
	default:
		defer func() { err = errors.Join(err, claim.Release()) }()
		if claim.OwnerErr != nil {
			em.notice("holding the credential-home claim, but could not record who holds it: %v", claim.OwnerErr)
		}
	}

	// A failing PASS does not stop the loop — the daemon's does not either, and
	// a transient I/O error is not a reason to stop switching accounts. A
	// broken STREAM does: there is nobody left to report to, and the
	// alternative is writing into a dead pipe once a second forever.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	loop := &daemon.Loop{
		Interval: interval,
		Tick: func(ctx context.Context) error {
			_, perr := autoPass(ctx, em, s)
			if em.err != nil {
				stop()
				return em.err
			}
			return perr
		},
	}
	if lerr := loop.Run(runCtx); lerr != nil {
		return lerr
	}
	if em.err != nil {
		return em.err
	}
	// Reached only through the context, which here is the interrupt.
	return WithCode(errSilent, ExitInterrupted)
}

// autoPass is one evaluation and, if the engine says so, the swap that follows.
//
// It returns the exit code the pass earned. The continuous form discards it —
// there is no exit to give it to — and reports through the event stream
// instead, which is why the code is a return value rather than an error: an
// error here would mean the pass failed, and "nothing to do" is not a failure.
func autoPass(ctx context.Context, em *autoEmitter, s *store.Store) (ExitCode, error) {
	ev, err := switcher.Evaluate(s, switcher.EvalOptions{})
	if err != nil {
		return ExitFailure, err
	}
	if ev.LiveErr != nil {
		em.notice("could not read the current login (%v); ranking without it", ev.LiveErr)
	}
	if ev.StateErr != nil {
		em.notice("the auto-switch state could not be read (%v); "+
			"proceeding with no cooldown and no quarantines", ev.StateErr)
	}
	if ev.ConfigErr != nil {
		// A mistyped threshold must not stop the engine.
		em.notice("%v; using the built-in defaults", ev.ConfigErr)
	}

	if ev.NoReadings {
		// Distinct from ActionBlocked, which is a choice that was made and came
		// back empty. Both are 4, but only this one is fixed by polling.
		em.evaluated(ev, "blocked", "no usage readings yet")
		em.say("ccdad has no usage readings yet, so there is nothing to choose on.")
		em.say("Run the daemon, or 'ccdad status --refresh', to fill the cache.")
		return ExitBlocked, nil
	}

	em.evaluated(ev, autoActionName(ev.Plan.Action), ev.Plan.Reason.String())
	if ev.Forced {
		em.notice("an anti-flap hold was overridden (%s)", ev.Plan.Reason)
	}

	switch ev.Plan.Action {
	case strategy.ActionBlocked:
		em.say("No account can be switched to: %s.", switcher.Explain(ev.Plan))
		return ExitBlocked, nil
	case strategy.ActionStay:
		em.say("Staying put: %s.", switcher.Explain(ev.Plan))
		return ExitNothingToDo, nil
	}

	live := ""
	if ev.LiveKnown {
		live = ev.Live.UUID
	}
	res, err := switcher.Execute(s, switcher.Request{
		Target: ev.Target, LiveUUID: live, Unattended: true,
		Freshen: freshenWith(ctx),
	})
	if len(res.UnknownKeys) > 0 && (res.Outcome == switcher.Switched || err != nil) {
		em.notice("unrecognized keys in the credentials file are being preserved unchanged: %s",
			strings.Join(res.UnknownKeys, ", "))
	}
	if err != nil {
		return ExitFailure, err
	}

	switch res.Outcome {
	case switcher.AlreadyOn:
		em.unchanged(res, "already-on")
		em.say("Already on %s.", ev.Target.Label())
		if res.ProfileSyncErr != nil {
			em.notice("%v; Claude Code's displayed account name may still name the previous login", res.ProfileSyncErr)
		}
		return ExitNothingToDo, nil
	case switcher.Raced:
		em.unchanged(res, "raced")
		em.say("Stood down: the live login changed while this switch was being decided.")
		return ExitNothingToDo, nil
	case switcher.Overridden:
		// 4, not 3. The engine wanted to move and cannot make any difference by
		// moving, and the fix is one the operator has to make.
		em.unchanged(res, "overridden")
		em.say("%s", switcher.DisplacementNote("Not switching: ", res))
		return ExitBlocked, nil
	case switcher.Stale:
		// 4, with Overridden and Contended, rather than 3. The engine wanted to
		// move and did not, so the world is NOT as the caller asked -- and an
		// operator running this from cron is exactly who needs to hear that the
		// account it would have moved to is carrying a login Claude Code would
		// rotate on sight.
		em.unchanged(res, "stale")
		em.say("Not switching to %s: its stored login is one Claude Code would refresh on sight.",
			ev.Target.Label())
		if res.FreshenErr != nil {
			em.notice("refreshing it failed: %v", res.FreshenErr)
		}
		em.say("Installing it would hand Claude Code a rotation that moves the refresh token out " +
			"from under ccdad's copy. Its next poll refreshes the grant; `ccdad status --refresh` " +
			"does it now.")
		return ExitBlocked, nil
	case switcher.Contended:
		// 4 for the same reason Overridden is 4: the engine wanted to move,
		// cannot usefully do so, and the fix is the operator's. Not 3 — 3 means
		// the world is already as the caller asked, and exit.go says operators
		// may ignore it, which would make this the one state nobody hears about.
		em.unchanged(res, "contended")
		em.say("Not switching: the ccdad store at %s (pid %d) is driving this Claude Code login.",
			res.Claim.Owner.Store, res.Claim.Owner.PID)
		em.say("Two stores on one login undo each other's switches. Point CLAUDE_CONFIG_DIR at a " +
			"directory of this store's own, or stop that engine.")
		return ExitBlocked, nil
	case switcher.Unattributed:
		// 4, with Overridden, Stale and Contended: the engine wanted to move and
		// did not, so the world is not as the caller asked. It used to reach the
		// default arm below and exit 1 calling itself an outcome ccdad does not
		// know -- a correct, deliberate stand-down reported to cron as a bug in
		// the tool.
		em.unchanged(res, "unattributed")
		em.say("Not switching: the credentials file holds a login this store cannot name.")
		em.say("Overwriting it could re-present a superseded grant and take that account down. " +
			"Claude Code rotates a login's refresh token on every refresh and ccdad matches on that " +
			"token, so a managed account stops being recognisable the moment it refreshes; " +
			"`ccdad add` re-registers one whose token has moved on.")
		return ExitBlocked, nil
	case switcher.Unreadable:
		// Same family, different remedy: that one waits for an identity, this
		// one waits for the machine.
		em.unchanged(res, "unreadable")
		em.say("Not switching: this machine's login store cannot be read, so whether any account is " +
			"live cannot be established.")
		em.say("On macOS this is a locked login keychain: run `security unlock-keychain " +
			"~/Library/Keychains/login.keychain-db`, then run this again from that shell.")
		return ExitBlocked, nil
	case switcher.Switched:
		// Named rather than left to fall through, so a completed swap says so in
		// exactly one place.
		//
		// THE DEFAULT BELOW IS NOT A BUILD BREAK, and the comment here used to
		// claim it was ("this switch has no default"). It has one, so an Outcome
		// added later compiles fine and exits 1 at runtime describing itself as
		// unknown -- which is what Unattributed did, to the one consumer that
		// cannot see the machine. The default is kept for a value no build of
		// this tree can produce; every value switcher declares gets an arm, and
		// that is a review rule rather than a compiler one.
	default:
		return ExitFailure, fmt.Errorf("the switch reported an outcome this ccdad does not know (%d): %s",
			res.Outcome, res.Outcome)
	}

	if res.Claim.Notice != "" {
		// The claim could not be read definitively. That is never a reason to
		// stand down — see credhome.Verdict — and always a reason to say so.
		em.notice("%s", res.Claim.Notice)
	}
	if res.CooldownErr != nil {
		em.notice("%v; the next evaluation may switch again sooner than it should", res.CooldownErr)
	}
	if res.KeyErr != nil {
		em.notice("the API key stored in Claude Code's config could not be cleared (%v); "+
			"it stays inert while this login is live", res.KeyErr)
	}
	if res.ProfileSyncErr != nil {
		em.notice("%v; Claude Code's displayed account name may still name the previous login", res.ProfileSyncErr)
	}
	em.switched(res)
	em.say("Switched to %s.", res.Target.Label())
	if res.ClearedKey {
		em.say("Removed %s's API key from Claude Code's config.", res.ClearedKeyOwner.Label())
	}
	return ExitOK, nil
}

func autoActionName(a strategy.Action) string {
	switch a {
	case strategy.ActionSwitch:
		return "switch"
	case strategy.ActionBlocked:
		return "blocked"
	default:
		return "stay"
	}
}

// autoEmitter writes both halves of this command's output: the NDJSON stream on
// stdout, and the human sentences that go to stderr in EVERY mode.
//
// The split is the `--json` contract's, and for `auto` it is load-bearing
// rather than tidy: a single stray line on stdout ends
// `ccdad auto --json | jq`, and the notices are exactly the lines a degraded
// run produces.
type autoEmitter struct {
	cmd  *cobra.Command
	enc  *json.Encoder
	json bool
	now  func() time.Time
	// last suppresses a repeat of the previous human line. The continuous form
	// evaluates once a second and almost always reaches the same answer; a
	// terminal filling with sixty identical "Staying put" lines a minute is a
	// terminal nobody watches. The stream is not deduplicated — a consumer
	// wants every tick, and dropping one would make a gap ambiguous.
	last string
	// err is the first write failure on the stream, held so the command can
	// return it. EPIPE reaches ExecuteWith through it, which is what makes
	// `ccdad auto --json | head -1` exit 0 rather than dying on SIGPIPE.
	err error
}

func newAutoEmitter(cmd *cobra.Command, asJSON bool) *autoEmitter {
	em := &autoEmitter{cmd: cmd, json: asJSON, now: time.Now}
	if asJSON {
		// A compact encoder, deliberately not writeJSON: that one calls
		// SetIndent, and an indented object spans several lines, which is not
		// NDJSON and cannot be read by anything line-oriented.
		em.enc = json.NewEncoder(cmd.OutOrStdout())
	}
	return em
}

// say writes one human sentence to stderr.
func (e *autoEmitter) say(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	if line == e.last {
		return
	}
	e.last = line
	fmt.Fprintln(e.cmd.ErrOrStderr(), line)
}

// notice reports a degraded input or a non-fatal failure. It is a sentence on
// stderr AND an event on the stream, because a consumer that only reads stdout
// must still be able to alert on a config it could not parse.
func (e *autoEmitter) notice(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	e.say("note: %s", msg)
	e.emit(map[string]any{"kind": "notice", "message": msg})
}

func (e *autoEmitter) evaluated(ev switcher.Evaluation, action, reason string) {
	payload := map[string]any{
		"kind":   "evaluated",
		"action": action,
		"reason": reason,
	}
	if ev.LiveKnown {
		payload["live"] = accountJSON(ev.Live)
	}
	if !ev.Decided {
		// The ranking never ran. Everything below is read off a zero Plan,
		// which stringifies to plausible-looking values rather than to nothing.
		e.emit(payload)
		return
	}
	payload["explanation"] = switcher.Explain(ev.Plan)
	payload["mode"] = ev.Plan.Result.Mode.String()
	if ev.HasTarget {
		payload["target"] = accountJSON(ev.Target)
	}
	if ev.Plan.Result.AllOverThreshold {
		payload["allOverThreshold"] = true
	}
	if ev.Plan.SubscriptionExhausted {
		payload["subscriptionExhausted"] = true
	}
	if len(ev.Plan.Quarantined) > 0 {
		payload["quarantined"] = ev.Plan.Quarantined
	}
	if ev.Plan.HasRetryAt {
		payload["retryAt"] = ev.Plan.RetryAt.In(readerZone()).Format(time.RFC3339)
	}
	if ev.Plan.CreditConsulted {
		credit := map[string]any{
			"allow":  ev.Plan.Credit.Allow,
			"reason": ev.Plan.Credit.Reason.String(),
			"room":   ev.Plan.Credit.Room,
		}
		if ev.Plan.Credit.DisabledReason != "" {
			credit["disabledReason"] = ev.Plan.Credit.DisabledReason
		}
		payload["credit"] = credit
	}
	if order := rankingJSON(ev.Plan.Result.Order); len(order) > 0 {
		payload["order"] = order
	}
	e.emit(payload)
}

func (e *autoEmitter) switched(res switcher.Result) {
	payload := map[string]any{
		"kind": "switched",
		"to":   accountJSON(res.Target),
	}
	if res.LiveKnown {
		payload["from"] = accountJSON(res.Live)
	}
	if res.ClearedKey {
		payload["clearedAPIKeyOf"] = accountJSON(res.ClearedKeyOwner)
	}
	if res.EnvTokenWins {
		payload["envTokenWins"] = true
	}
	e.emit(payload)
}

// unchanged is a swap the engine decided on and did not perform. reason names
// which of Execute's three stand-downs it was.
func (e *autoEmitter) unchanged(res switcher.Result, reason string) {
	payload := map[string]any{
		"kind":   "unchanged",
		"reason": reason,
		"target": accountJSON(res.Target),
	}
	if res.LiveKnown {
		payload["live"] = accountJSON(res.Live)
	}
	e.emit(payload)
}

func (e *autoEmitter) emit(payload map[string]any) {
	if !e.json || e.err != nil {
		return
	}
	payload["schemaVersion"] = autoSchemaVersion
	payload["at"] = e.now().In(readerZone()).Format(time.RFC3339Nano)
	if err := e.enc.Encode(payload); err != nil {
		// Held rather than returned: the caller is mid-decision and the event
		// is a report of it, so a broken stream must not change what the engine
		// does — only what the command exits with.
		e.err = fmt.Errorf("writing the event stream: %w", err)
	}
}

// rankingJSON renders the subscription pool in the order the engine ranked it,
// with the figures it ranked on — so a consumer can explain the choice rather
// than only observe it.
func rankingJSON(order []strategy.Ranked) []map[string]any {
	out := make([]map[string]any, 0, len(order))
	for _, r := range order {
		row := map[string]any{"uuid": r.UUID, "kind": r.Kind.String()}
		if r.Headroom.Known {
			row["headroomPct"] = r.Headroom.Pct
			// The axis the order was actually made on. headroomPct alone cannot
			// explain a pool ranked under a per-window table: two accounts can
			// be ordered against 100 minus their utilization one way and against
			// their own thresholds the other.
			row["slack"] = r.Headroom.Slack
			row["windowThreshold"] = r.Headroom.Threshold
		}
		// The second term of every derived threshold. Under hover it is no
		// longer 100 divided by the pool, so a consumer that has the threshold
		// and the elapsed share still cannot close the arithmetic without it.
		if r.HoverShare > 0 {
			row["hoverShare"] = r.HoverShare
		}
		// Why this account's share is wider than the pool's slice: quota its
		// own rotation cannot reach before the named window resets.
		if r.Stranded > 0 {
			row["strandedPct"] = r.Stranded
			row["strandedWindow"] = string(r.StrandedWindow)
		}
		if r.HasRecovery {
			row["recoversAt"] = r.RecoversAt.In(readerZone()).Format(time.RFC3339)
			row["returnsInsideHorizon"] = r.ReturnsInsideHorizon
		}
		if r.HasWeeklyReset {
			row["weeklyResetsAt"] = r.WeeklyResetsAt.In(readerZone()).Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out
}
