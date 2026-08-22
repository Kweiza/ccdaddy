package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// switchStateTimeout bounds the wait for the engine state lock. The only other
// writer is the daemon stamping a cooldown, a sub-second write.
const switchStateTimeout = 5 * time.Second

// tokenRecordOf reports the ccdad token record a blob carries, if any.
//
// A token account is not installable as a Claude Code login: Claude Code reads
// an API key from ~/.claude.json and a setup token from an environment
// variable, never from the credentials file.
func tokenRecordOf(b cclink.Blob) (tokenRecord, bool) {
	raw, ok := b[tokenCredentialKey]
	if !ok {
		return tokenRecord{}, false
	}
	var rec tokenRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return tokenRecord{}, false
	}
	return rec, true
}

// envVarFor names the mechanism Claude Code actually reads a token kind from.
func envVarFor(kind string) string {
	if kind == "api-key" {
		return "ANTHROPIC_API_KEY"
	}
	return "CLAUDE_CODE_OAUTH_TOKEN"
}

// exactlyOneAccount is spelled out rather than delegated to cobra.ExactArgs so
// the violation carries this binary's exit code and a message that says what to
// do next. Cobra's own Args errors are plain errors, which would exit 1.
func exactlyOneAccount(verb string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return UsageError("%s needs exactly one account; run 'ccdad list' to see them", verb)
		}
		return nil
	}
}

// atMostOneAccount is spelled out rather than delegated to cobra.MaximumNArgs so
// the violation carries this binary's exit code and a message that says what to
// do next. Cobra's own Args errors are plain errors, which would exit 1.
//
// It replaced an exactlyOne form when `switch` gained its targetless grammar.
// The message matters more than the check: "needs exactly one account" was the
// error a user saw most often, and with a strategy form it is simply false.
func atMostOneAccount(verb string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) > 1 {
			return UsageError("%s takes at most one account; run 'ccdad list' to see them", verb)
		}
		return nil
	}
}

// checkSwitchFlags is spec §9.1's two rejections. They are asymmetric and easy
// to invert, so they are written out one per branch rather than folded together:
// --strategy is refused WITH an explicit target, --model WITHOUT --strategy.
func checkSwitchFlags(args []string, strategyName, model string) error {
	if strategyName != "" && len(args) == 1 {
		return UsageError("switch --strategy picks the account itself, so it cannot be given one as well; " +
			"drop the account, or drop --strategy")
	}
	if model != "" && strategyName == "" {
		return UsageError("switch --model only means something alongside --strategy")
	}
	if strategyName == "" {
		if len(args) == 0 {
			// The targetless grammar exists, but it is the --strategy one.
			// Letting a bare `switch` mean "engine, pick something" would make
			// the most easily mistyped command in the tree the one that
			// silently rewrites the live login.
			return UsageError("switch needs an account, or --strategy to let the engine choose one; " +
				"run 'ccdad list' to see them")
		}
		return nil
	}
	if _, ok := strategy.ParseStrategy(strategyName); !ok {
		return UsageError("unknown strategy %q: one of %s", strategyName, strings.Join(strategy.StrategyNames(), ", "))
	}
	return nil
}

// installable reports whether an account can become the live login at all.
//
// A token account is stored but has no claudeAiOauth record, so there is
// nothing for cclink to install. Left in the pool it can rank first and then
// fail the switch, turning a strategy the user asked for into an exit 2 they
// cannot act on — so it is excluded from the ranking rather than rejected after
// winning it. An explicit `switch <ACCT>` still names one and still gets the
// message that says which mechanism does read it.
func installable(creds cclink.Blob, err error) bool {
	if err != nil {
		return false
	}
	_, hasOAuth := creds["claudeAiOauth"]
	return hasOAuth
}

// engineCandidates projects the store and the on-disk usage cache onto what the
// ranking takes.
//
// It READS the cache and never fetches. That is the same rule `ccdad list`
// follows (§9.1) and for the same reason: /api/oauth/usage allows roughly 28-30
// requests per identity per rolling hour over a SLIDING window, so a command a
// user can run in a loop must not be a way to spend it.
func engineCandidates(s *store.Store, accounts []store.Account, c *usage.Cache) []strategy.Candidate {
	out := make([]strategy.Candidate, 0, len(accounts))
	for _, a := range accounts {
		if !installable(s.Credentials(a.UUID)) {
			continue
		}
		cand := strategy.Candidate{UUID: a.UUID, Kind: a.Kind, Disabled: a.Disabled}
		if e, ok := c.Get(a.UUID); ok {
			cand.Usage = e.Snapshot
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

// chooseTarget is the targetless grammar: the engine picks, under §7.2's
// margins and §7.3's gate.
func chooseTarget(cmd *cobra.Command, s *store.Store, accounts []store.Account,
	liveUUID, strategyName string, force bool) (store.Account, error) {
	stderr := cmd.ErrOrStderr()

	cache, err := usage.LoadCache()
	if err != nil {
		return store.Account{}, err
	}
	// An entry older than its account's AddedAt belonged to a previous account
	// at the same uuid, and letting it through would hand a fresh login the
	// headroom its predecessor had already spent.
	added := make(map[string]time.Time, len(accounts))
	for _, a := range accounts {
		added[a.UUID] = a.AddedAt
	}
	cache.Prune(added)

	cands := engineCandidates(s, accounts, cache)
	if !anyReading(cands) {
		// No evidence at all. Moving here would not be a choice, it would be a
		// reshuffle — and §9.3 reserves 4 for "wanted, but no viable target".
		fmt.Fprintln(stderr, "ccdad has no usage readings yet, so there is nothing to choose on.")
		fmt.Fprintln(stderr, "Name an account explicitly, or let the daemon poll first.")
		return store.Account{}, WithCode(errSilent, ExitBlocked)
	}

	// The cooldown is read from DISK. A one-shot command has no in-memory
	// anti-flap history, and a bare `switch` that ignored the cooldown would
	// ping-pong against a running daemon that is honouring it.
	st, err := strategy.LoadState()
	if err != nil {
		return store.Account{}, err
	}
	if lerr := st.LoadError(); lerr != nil {
		fmt.Fprintf(stderr, "note: the auto-switch state could not be read (%v); "+
			"proceeding with no cooldown and no quarantines.\n", lerr)
	}

	// Parsed, not defaulted: checkSwitchFlags has already refused an unknown
	// name, so anything reaching here is one the ranking knows.
	chosen, _ := strategy.ParseStrategy(strategyName)
	opts := strategy.Options{Now: time.Now(), Strategy: chosen}
	// The zero Config is the full §7.2 default set, and its MaxAutoSpend of 0 is
	// §7.3's documented default rather than an omission: until `ccdad config`
	// (task 47) can raise it, a targetless switch never spends money and the
	// credit gate answers CreditNotOptedIn out loud.
	plan := strategy.Decide(cands, opts, strategy.Config{}, st, liveUUID)

	if plan.Action == strategy.ActionStay && force {
		// --force is the explicit bypass of §7.2's margins, and only of those.
		// It never bypasses §7.3's credit gate: that one spends money, and a
		// flag named "force" is not the two independent opt-ins §7.3 requires.
		if best, ok := forceableTarget(plan, liveUUID); ok {
			fmt.Fprintf(stderr, "note: --force is overriding the anti-flap hold (%s).\n", plan.Reason)
			plan.Action, plan.Target = strategy.ActionSwitch, best
		}
	}

	switch plan.Action {
	case strategy.ActionSwitch:
		target, ok := s.Get(plan.Target.UUID)
		if !ok {
			return store.Account{}, fmt.Errorf("the engine chose %q, which is no longer in the store", plan.Target.UUID)
		}
		return target, nil

	case strategy.ActionBlocked:
		fmt.Fprintf(stderr, "No account can be switched to: %s.\n", explain(plan))
		return store.Account{}, WithCode(errSilent, ExitBlocked)

	default:
		fmt.Fprintf(stderr, "Staying put: %s.\n", explain(plan))
		return store.Account{}, WithCode(errSilent, ExitNothingToDo)
	}
}

// forceableTarget is the best account --force may move to: the top of the
// ranking, when that is not already the live one. A credit account is never
// returned, which is what keeps --force away from §7.3.
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

// explain turns a plan into a sentence, adding the detail the reason alone
// cannot carry.
func explain(plan strategy.Plan) string {
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

func newSwitchCmd() *cobra.Command {
	var force bool
	var strategyName, model string

	cmd := &cobra.Command{
		Use:   "switch [ACCOUNT]",
		Short: "Make an account the live Claude Code login",
		Long: "ACCOUNT may be a display index, an alias, an email address, or a uuid prefix.\n\n" +
			"With no ACCOUNT, --strategy lets the auto-switch engine choose, under the same\n" +
			"anti-flap margins the daemon uses and reading the same on-disk usage cache.\n" +
			"It never polls: run the daemon, or 'ccdad list --refresh', to freshen the cache.\n\n" +
			"--model is accepted for forward compatibility and currently changes nothing:\n" +
			"the ranking has no model dimension yet, so honouring it would mean inventing one.",
		Args:          atMostOneAccount("switch"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkSwitchFlags(args, strategyName, model); err != nil {
				return err
			}

			s, err := store.Open()
			if err != nil {
				return err
			}
			accounts := s.Accounts()

			// A live file we cannot read is not a reason to refuse the switch:
			// attribution here is only the already-on optimization, and Activate
			// re-reads under the lock anyway. This is the state where switching
			// to a known-good account matters most.
			live, err := cclink.Load()
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: could not read the current login (%v); switching anyway\n", err)
				live = nil
			}
			// attributeWith, not attributeLogin, and deliberately: this asks
			// "is the FILE already this account", because the file is what a
			// switch rewrites. attributeLogin would answer about the
			// environment token instead, which switching does not touch — and
			// the override is reported separately below.
			//
			// It is also the ONLY acceptable hysteresis baseline. store.go
			// documents ActiveUUID as a display HINT that goes stale the moment
			// the user runs /login inside Claude Code, and a margin measured
			// against a stale baseline compares the candidate to an account
			// that is not there.
			current, liveOK := attributeWith(live, accounts, s.Credentials)
			liveUUID := ""
			if liveOK {
				liveUUID = current.UUID
			}

			var target store.Account
			if len(args) == 1 {
				target, err = store.Resolve(accounts, args[0])
				if err != nil {
					// Every Resolve failure is the caller naming something that
					// does not exist, which is a usage error under the exit
					// contract.
					return UsageError("%s", err.Error())
				}
			} else {
				target, err = chooseTarget(cmd, s, accounts, liveUUID, strategyName, force)
				if err != nil {
					return err
				}
			}

			creds, err := s.Credentials(target.UUID)
			if err != nil {
				return err
			}
			// An account can hold both a browser login and a token. The OAuth
			// record is what goes in the credentials file, so a token sitting
			// beside it must not make the account look uninstallable — only an
			// account with NO OAuth record is unswitchable.
			if _, hasOAuth := creds["claudeAiOauth"]; !hasOAuth {
				if rec, isToken := tokenRecordOf(creds); isToken {
					return UsageError("%s is an %s account; Claude Code reads that credential from %s, so there is nothing to install in the credentials file",
						target.Label(), rec.Kind, envVarFor(rec.Kind))
				}
			}

			if liveOK && current.UUID == target.UUID && !force {
				fmt.Fprintf(cmd.ErrOrStderr(), "Already on %s.\n", target.Label())
				return WithCode(errSilent, ExitNothingToDo)
			}

			// Spec §4.3: drift in the credentials file is demonstrated, not
			// hypothetical — six machine keys appeared after clauth's carry list
			// was written. Merge preserves what it does not recognize, but the
			// operator still needs to know a new key exists.
			if unknown := cclink.UnknownKeys(live); len(unknown) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"note: unrecognized keys in the credentials file are being preserved unchanged: %s\n",
					strings.Join(unknown, ", "))
			}

			if err := cclink.Activate(creds); err != nil {
				return err
			}
			if err := s.SetActive(target.UUID); err != nil {
				return err
			}
			recordSwitch(cmd, target.UUID)

			fmt.Fprintf(cmd.ErrOrStderr(), "Switched to %s.\n", target.Label())
			// Claude Code reads CLAUDE_CODE_OAUTH_TOKEN in preference to the
			// credentials file, so with that variable set the switch has done its
			// work and still changed nothing about what Claude Code uses.
			if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"Note: CLAUDE_CODE_OAUTH_TOKEN is set, and Claude Code reads it in preference to the credentials file. "+
						"Unset it for this switch to take effect.")
			}
			// Claude Code re-reads the credentials file on its next request, so a
			// running session picks this up without a restart.
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"activate even when this account is already live, and override the anti-flap hold on a targetless switch")
	cmd.Flags().StringVar(&strategyName, "strategy", "",
		"let the engine choose the account: one of "+strings.Join(strategy.StrategyNames(), ", ")+" (no ACCOUNT)")
	cmd.Flags().StringVar(&model, "model", "",
		"reserved; accepted only alongside --strategy and currently has no effect on the ranking")
	return cmd
}

// recordSwitch stamps §7.2's cooldown after the swap has succeeded.
//
// An EXPLICIT switch stamps it too, and that is the point: the user has just
// chosen an account, and a daemon evaluating ten seconds later must not
// immediately override the choice. Stamping before the swap would let a switch
// that FAILED hold the engine off its own retry.
//
// It never fails the command. The credentials file has already been written by
// the time this runs, so returning an error here would report a completed
// switch as a failure.
func recordSwitch(cmd *cobra.Command, uuid string) {
	if err := strategy.WithState(switchStateTimeout, func(st *strategy.State) error {
		st.RecordSwitch(uuid, time.Now())
		return nil
	}); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"note: the auto-switch cooldown could not be recorded (%v); the daemon may switch again sooner than it should.\n", err)
	}
}
