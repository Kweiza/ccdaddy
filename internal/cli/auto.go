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

// autoTickInterval is the continuous form's cadence, §8.4's one second. It
// matches the daemon deliberately: `auto` exists to be the daemon's engine with
// nothing detached, and an engine that evaluated on a different rhythm would
// not be describing the thing it is standing in for.
const autoTickInterval = time.Second

func newAutoCmd() *cobra.Command {
	var once, asJSON bool

	cmd := &cobra.Command{
		Use:   "auto",
		Short: "Run the auto-switch engine",
		Long: "With --once, run one evaluation and exit — the cron and testing surface for\n" +
			"the whole engine. Without it, evaluate continuously in the foreground until\n" +
			"interrupted, which is what the daemon does with nothing detached.\n\n" +
			"It never polls: it reads the same on-disk usage cache 'ccdad list' reads.\n" +
			// `ccdad list --refresh` is task 43 and is not in the tree; naming
			// it here told the user to run a flag the binary rejects. Put it
			// back beside the daemon when that flag lands.
			"Run 'ccdad daemon start' to freshen it.\n\n" +
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
				code, err := autoPass(em, s)
				if err != nil {
					return err
				}
				// A broken stream outranks the pass's own code. EPIPE reaches
				// ExecuteWith through it and becomes 0, which is §9.3's
				// "`ccdad auto --json | head -1` exits 0"; any other write
				// failure becomes 1, because reporting the evaluation's answer
				// would claim an output nobody received.
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
// being as the caller asked for it, which is exactly what §9.3 reserves 3 for —
// and ErrSingletonHeld is a distinct sentinel from ErrLocksUnsupported for this
// reason, so a filesystem with no working locks still reports a failure.
func runAutoLoop(ctx context.Context, em *autoEmitter, s *store.Store, interval time.Duration) error {
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

	// A failing PASS does not stop the loop — the daemon's does not either, and
	// a transient I/O error is not a reason to stop switching accounts. A
	// broken STREAM does: there is nobody left to report to, and the
	// alternative is writing into a dead pipe once a second forever.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	loop := &daemon.Loop{
		Interval: interval,
		Tick: func(context.Context) error {
			_, perr := autoPass(em, s)
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
func autoPass(em *autoEmitter, s *store.Store) (ExitCode, error) {
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
		// §7.6 rule 4: a mistyped threshold must not stop the engine.
		em.notice("%v; using the built-in defaults", ev.ConfigErr)
	}

	if ev.NoReadings {
		// Distinct from ActionBlocked, which is a choice that was made and came
		// back empty. Both are 4, but only this one is fixed by polling.
		em.evaluated(ev, "blocked", "no usage readings yet")
		em.say("ccdad has no usage readings yet, so there is nothing to choose on.")
		em.say("Run 'ccdad daemon start' to fill the cache.")
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
		return ExitNothingToDo, nil
	case switcher.Raced:
		em.unchanged(res, "raced")
		em.say("Stood down: the live login changed while this switch was being decided.")
		return ExitNothingToDo, nil
	case switcher.Overridden:
		// 4, not 3. The engine wanted to move and cannot make any difference by
		// moving, and the fix is one the operator has to make.
		em.unchanged(res, "overridden")
		em.say("Not switching: CLAUDE_CODE_OAUTH_TOKEN is set, and Claude Code reads it in " +
			"preference to the credentials file.")
		em.say("Unset it, or nothing the engine does can change what a session authenticates as.")
		return ExitBlocked, nil
	}

	if res.CooldownErr != nil {
		em.notice("%v; the next evaluation may switch again sooner than it should", res.CooldownErr)
	}
	if res.KeyErr != nil {
		em.notice("the API key stored in Claude Code's config could not be cleared (%v); "+
			"it stays inert while this login is live", res.KeyErr)
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
// The split is §9.4's, and for `auto` it is load-bearing rather than tidy: a
// single stray line on stdout ends `ccdad auto --json | jq`, and the notices
// are exactly the lines a degraded run produces.
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
		payload["retryAt"] = ev.Plan.RetryAt.UTC().Format(time.RFC3339)
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
	payload["at"] = e.now().UTC().Format(time.RFC3339Nano)
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
		}
		if r.HasRecovery {
			row["recoversAt"] = r.RecoversAt.UTC().Format(time.RFC3339)
			row["returnsInsideHorizon"] = r.ReturnsInsideHorizon
		}
		if r.HasWeeklyReset {
			row["weeklyResetsAt"] = r.WeeklyResetsAt.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return out
}
