package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
)

// envVarFor names the mechanism Claude Code actually reads a token kind from.
func envVarFor(kind string) string {
	if kind == "api-key" {
		return "ANTHROPIC_API_KEY"
	}
	return "CLAUDE_CODE_OAUTH_TOKEN"
}

// chooseTarget is the targetless grammar: the engine picks, under the
// anti-flap margins and the credit gate. The decision itself is
// switcher.Evaluate — the same call `ccdad auto` and the daemon make, because
// the moment there are two copies the hand-verified path and the unattended
// path diverge. What stays here is the wording.
func chooseTarget(cmd *cobra.Command, s *store.Store, strategyName, model string, force bool) (store.Account, error) {
	stderr := cmd.ErrOrStderr()

	// Parsed, not defaulted: checkSwitchFlags has already refused an unknown
	// name, so anything reaching here is one the ranking knows. It is always
	// present on this path — a targetless switch IS the --strategy grammar — so
	// the config's own strategy key reaches the daemon rather than this command.
	chosen, _ := strategy.ParseStrategy(strategyName)
	ev, err := switcher.Evaluate(s, switcher.EvalOptions{
		Strategy: chosen, HasStrategy: true, Force: force, Model: model,
	})
	if err != nil {
		return store.Account{}, err
	}
	if ev.LiveErr != nil {
		fmt.Fprintf(stderr, "note: could not read the current login (%v); ranking without it\n", ev.LiveErr)
	}
	if ev.StateErr != nil {
		fmt.Fprintf(stderr, "note: the auto-switch state could not be read (%v); "+
			"proceeding with no cooldown and no quarantines.\n", ev.StateErr)
	}
	if ev.ConfigErr != nil {
		fmt.Fprintf(stderr, "note: %v; using the built-in defaults.\n", ev.ConfigErr)
	}
	// Hover derives the ranking for itself, so the strategy this command was
	// given never reached it: Options.withHover overwrites Strategy with
	// headroom before Rank runs, which is what makes the flag rank exactly as
	// if it had not been typed.
	//
	// This is the rule checkSwitchFlags already applies to an unplaceable
	// --model, and it is SHARPER here: --strategy is mandatory for the
	// targetless grammar, so under hover every attended switch types a value the
	// engine drops. Refusing is not available for that same reason -- it would
	// leave no way to run a targetless `ccdad switch` at all while hover is on
	// -- so the flag is honoured as far as it goes and the override is named.
	//
	// Plan.Hover is the signal rather than a second read of the config, because
	// it reports the pass THIS answer came from. Where no pass ran there was no
	// ranking for the flag to have been dropped out of, and the two sentences
	// the caller gets instead already say so.
	if ev.Plan.Hover != nil {
		fmt.Fprintf(stderr, "note: hover is on and derives the ranking for itself, so --strategy %s was not applied. "+
			"Name an account to choose one yourself, or run 'ccdad hover off' to hand the ranking back.\n", strategyName)
	}
	if ev.Forced {
		// --force is the explicit bypass of the anti-flap margins, and only
		// of those.
		fmt.Fprintf(stderr, "note: --force is overriding the anti-flap hold (%s).\n", ev.Plan.Reason)
	}
	if ev.NoReadings {
		fmt.Fprintln(stderr, "ccdad has no usage readings yet, so there is nothing to choose on.")
		fmt.Fprintln(stderr, "Name an account explicitly, or let the daemon poll first.")
		return store.Account{}, WithCode(errSilent, ExitBlocked)
	}

	switch ev.Plan.Action {
	case strategy.ActionSwitch:
		return ev.Target, nil
	case strategy.ActionBlocked:
		fmt.Fprintf(stderr, "No account can be switched to: %s.\n", switcher.Explain(ev.Plan))
		return store.Account{}, WithCode(errSilent, ExitBlocked)
	default:
		fmt.Fprintf(stderr, "Staying put: %s.\n", switcher.Explain(ev.Plan))
		return store.Account{}, WithCode(errSilent, ExitNothingToDo)
	}
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

// checkSwitchFlags opens with the two flag rejections that are asymmetric and
// easy to invert, so they are written out one per branch rather than folded
// together: --strategy is refused WITH an explicit target, --model WITHOUT
// --strategy. The rest of the function is ordinary argument checking.
func checkSwitchFlags(args []string, strategyName, model string) error {
	if strategyName != "" && len(args) == 1 {
		return UsageError("switch --strategy picks the account itself, so it cannot be given one as well; " +
			"drop the account, or drop --strategy")
	}
	if model != "" && strategyName == "" {
		return UsageError("switch --model only means something alongside --strategy")
	}
	if model != "" {
		// Refused here rather than absorbed by the engine. A name ccdad cannot
		// place narrows nothing, so honouring it silently would rank exactly as
		// if the flag had not been typed — and the user would be told nothing
		// while believing they had excluded another model's spent cap.
		if _, ok := strategy.ModelFamily(model); !ok {
			return UsageError("cannot tell which model family %q is: name one of %s, "+
				"with or without a version", model, strings.Join(strategy.ModelFamilyNames(), ", "))
		}
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
			"--model names the model the session will run, which NARROWS the ranking: the\n" +
			"weekly caps scoped to other models stop counting against an account, so one\n" +
			"whose Opus week is spent can still be chosen for a Sonnet session. Caps that\n" +
			"are not per-model always count.\n\n" +
			"While hover is on it derives the ranking for itself, so the strategy named here\n" +
			"is not applied and the switch says so on stderr. --strategy is still required by\n" +
			"the targetless grammar; name an ACCOUNT to choose one yourself, or turn hover\n" +
			"off to hand the ranking back.",
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
			// AttributeFile, not AttributeLogin, and deliberately: this asks
			// "is the FILE already this account", because the file is what a
			// switch rewrites. AttributeLogin would answer about the
			// environment token instead, which switching does not touch.
			//
			// It is also the ONLY acceptable hysteresis baseline. store.go
			// documents ActiveUUID as a display HINT that goes stale the moment
			// the user runs /login inside Claude Code, and a margin measured
			// against a stale baseline compares the candidate to an account
			// that is not there.
			//
			// The executor asks the same question again under the lock, and its
			// answer is the one that decides the swap. This one only feeds the
			// engine, which has to rank before any lock is taken.
			current, liveOK := switcher.AttributeFile(live, accounts, s.Credentials)
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
				target, err = chooseTarget(cmd, s, strategyName, model, force)
				if err != nil {
					return err
				}
			}

			// The two token paths stay here rather than in the executor. A
			// setup token has no file to be installed into at all, and an
			// api-key account is installed by two writes to ~/.claude.json and
			// the credentials file — a different sequence, under a different
			// lock, that the engine never asks for because an account with no
			// quota windows has nothing for a usage-aware ranking to compare.
			creds, err := s.Credentials(target.UUID)
			if err != nil {
				return err
			}
			// An account can hold both a browser login and a token. The OAuth
			// record is what goes in the credentials file and is the stronger
			// credential, so a token sitting beside it must not divert the
			// switch — only an account with NO OAuth record takes a token path.
			if _, hasOAuth := creds["claudeAiOauth"]; !hasOAuth {
				if rec, isToken := cclink.TokenRecordOf(creds); isToken {
					if rec.Kind != cclink.APIKeyKind {
						return setupTokenRefusal(target.Label())
					}
					return switchToAPIKey(cmd, s, target, rec.Token, live, force)
				}
			}

			// Attended: nobody is left guessing, so the executor reports rather
			// than refuses. Its Unattended half is the daemon's.
			res, err := switcher.Execute(s, switcher.Request{
				Target: target, LiveUUID: liveUUID, Force: force,
				Freshen: freshenWith(cmd.Context()),
			})
			// The unknown-key probe: drift in the credentials file is
			// demonstrated, not hypothetical — six machine keys appeared after
			// clauth's carry list was written. Merge preserves what it does not
			// recognize, but the operator still needs to know a new key exists.
			//
			// Reported for a merge that happened, and for one that failed part
			// way, because the keys are in the file either way. NOT for a no-op:
			// that never rewrites the file, so "being preserved unchanged" would
			// describe a write that did not happen.
			if len(res.UnknownKeys) > 0 && (res.Outcome == switcher.Switched || err != nil) {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"note: unrecognized keys in the credentials file are being preserved unchanged: %s\n",
					strings.Join(res.UnknownKeys, ", "))
			}
			if err != nil {
				return err
			}
			noteProfileSync(cmd, res.ProfileSyncErr)
			if res.Outcome == switcher.AlreadyOn {
				fmt.Fprintf(cmd.ErrOrStderr(), "Already on %s.\n", target.Label())
				return WithCode(errSilent, ExitNothingToDo)
			}
			// Refused, not failed: nothing was written, and the account is
			// installable again as soon as its grant is refreshed. Reported
			// with the reason because "switch failed" sends the user to
			// re-login, which is the one repair this state does not need.
			if res.Outcome == switcher.Stale {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"Not switching to %s: its stored login is one Claude Code would refresh on sight.\n",
					target.Label())
				if res.FreshenErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "  Refreshing it failed: %v\n", res.FreshenErr)
				}
				fmt.Fprintln(cmd.ErrOrStderr(),
					"  Installing it would hand Claude Code a rotation that moves the refresh token\n"+
						"  out from under ccdad's copy. Try `ccdad list --refresh` first.")
				return WithCode(errSilent, ExitBlocked)
			}

			noteCooldown(cmd, res.CooldownErr)
			noteReleasedAPIKey(cmd, res)
			// From the Result rather than a fresh probe: Execute decided it
			// under Claude Code's credential locks, at the moment of the write,
			// and re-asking here would answer about a different instant.
			noteCredentialHomeClaim(cmd, res.Claim)

			fmt.Fprintf(cmd.ErrOrStderr(), "Switched to %s.\n", target.Label())
			// Claude Code reads CLAUDE_CODE_OAUTH_TOKEN in preference to the
			// credentials file, so with that variable set the switch has done its
			// work and still changed nothing about what Claude Code uses.
			if res.EnvTokenWins {
				// The source, not a variable name: three of the sources this
				// fires for have no variable, and switcher.DisplacementNote is
				// the one place that wording lives.
				fmt.Fprintln(cmd.ErrOrStderr(), switcher.DisplacementNote("Note: ", res))
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
		"the model this session will run: ignore other models' weekly caps when ranking (needs --strategy)")
	return cmd
}
