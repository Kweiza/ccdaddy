package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/codexswitch"
	"github.com/Kweiza/ccdaddy/internal/provider"
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
	if ev.Plan.Hover != nil && strategyName != "" {
		// Guarded on the NAME, not only on hover: --provider reaches this path
		// with no --strategy at all, and the unguarded form printed
		// "so --strategy  was not applied" with a hole where the name goes.
		fmt.Fprintf(stderr, "note: hover is on and derives the ranking for itself, so --strategy %s was not applied. "+
			"Name an account to choose one yourself, or select another policy with 'ccdad strategy'.\n", strategyName)
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
			return UsageError("%s needs exactly one account; run 'ccdad status' to see them", verb)
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
			return UsageError("%s takes at most one account; run 'ccdad status' to see them", verb)
		}
		return nil
	}
}

// switchProvider reads the --provider flag. Empty is Claude, which is what
// every invocation written before this flag existed means.
func switchProvider(providerName string) (provider.ID, error) {
	if providerName == "" {
		return provider.Claude, nil
	}
	p, err := provider.Parse(providerName)
	if err != nil {
		return "", UsageError("--provider takes %s or %s, not %q",
			provider.Claude, provider.Codex, providerName)
	}
	return p, nil
}

// checkSwitchFlags opens with the two flag rejections that are asymmetric and
// easy to invert, so they are written out one per branch rather than folded
// together: --strategy is refused WITH an explicit target, --model WITHOUT
// --strategy. The rest of the function is ordinary argument checking.
func checkSwitchFlags(args []string, strategyName, model, providerName string) error {
	p, err := switchProvider(providerName)
	if err != nil {
		return err
	}
	// The two Claude ranking knobs, refused for codex rather than ignored. The
	// codex lane forces pre-emption off and hover off and ranks on one
	// threshold, so a --strategy honoured here would narrow nothing and a
	// --model would name a family the codex windows have no scope for. Silently
	// dropping either is how a user comes to believe they excluded something.
	if p == provider.Codex && strategyName != "" {
		return UsageError("switch --provider codex ranks on the codex table, which takes no strategy; " +
			"drop --strategy, or name the account you want")
	}
	if p == provider.Codex && model != "" {
		return UsageError("switch --model names a Claude model family, so it means nothing with --provider codex")
	}
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
			// `--provider codex` IS a targetless grammar of its own: it names
			// the pool, and the codex ranking has no strategy to be given.
			// Nothing about it can rewrite the live login, which is what the
			// refusal below is protecting.
			if p == provider.Codex {
				return nil
			}
			// The targetless grammar exists, but it is the --strategy one.
			// Letting a bare `switch` mean "engine, pick something" would make
			// the most easily mistyped command in the tree the one that
			// silently rewrites the live login.
			return UsageError("switch needs an account, or --strategy to let the engine choose one; " +
				"run 'ccdad status' to see them")
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
	var strategyName, model, providerName string

	cmd := &cobra.Command{
		Use:   "switch [ACCOUNT]",
		Short: "Make an account the live Claude Code login",
		Long: "ACCOUNT may be a display index, an alias, an email address, or a uuid prefix.\n\n" +
			"With no ACCOUNT, --strategy lets the auto-switch engine choose, under the same\n" +
			"anti-flap margins the daemon uses and reading the same on-disk usage cache.\n" +
			"It never polls: run the daemon, or 'ccdad status --refresh', to freshen the cache.\n\n" +
			"--model names the model the session will run, which NARROWS the ranking: the\n" +
			"weekly caps scoped to other models stop counting against an account, so one\n" +
			"whose Opus week is spent can still be chosen for a Sonnet session. Caps that\n" +
			"are not per-model always count.\n\n" +
			"While hover is on it derives the ranking for itself, so the strategy named here\n" +
			"is not applied and the switch says so on stderr. --strategy is still required by\n" +
			"the targetless grammar; name an ACCOUNT to choose one yourself, or select\n" +
			"another policy with 'ccdad strategy'.",
		Args:          atMostOneAccount("switch"),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkSwitchFlags(args, strategyName, model, providerName); err != nil {
				return err
			}
			asserted, err := switchProvider(providerName)
			if err != nil {
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
			} else if asserted == provider.Codex {
				root, rerr := codexRoot()
				if rerr != nil {
					return rerr
				}
				target, err = chooseCodexTarget(cmd, s, root)
				if err != nil {
					return err
				}
			} else {
				target, err = chooseTarget(cmd, s, strategyName, model, force)
				if err != nil {
					return err
				}
			}

			// --provider is an ASSERTION about the account that was named, and
			// it is checked here -- after Resolve and before anything is
			// written. It exists because one person's Claude and Codex accounts
			// commonly share an email address, so a reference that resolved to
			// the wrong provider yesterday can resolve to the other one today;
			// a script that means to move codex must not silently rewrite the
			// live Claude login instead.
			if providerName != "" && target.Provider != asserted {
				return UsageError("%s is a %s account and --provider %s was asserted; "+
					"run 'ccdad status' to see which is which",
					target.Label(), target.Provider, asserted)
			}

			// The codex branch, BEFORE the credential read below. A Codex
			// account's blob holds no claudeAiOauth and no token record, so
			// falling through would take the setup-token refusal path and
			// report a shape mismatch about an account that is not broken.
			//
			// What a codex switch IS: a pointer file. Codex holds no token on
			// this machine -- ccdad's proxy rewrites the bearer per request --
			// so there is no credential to install and nothing to lock.
			if target.Provider == provider.Codex {
				root, rerr := codexRoot()
				if rerr != nil {
					return rerr
				}
				return runCodexSwitch(cmd, root, target)
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
				// switcher.ErrNotClaude reaches here for a Codex account: the
				// package-prefixed sentinel names no account and offers no
				// remedy, so it is named here in the register this command's
				// other refusals use — the Stale branch below is the nearest
				// one.
				if errors.Is(err, switcher.ErrNotClaude) {
					return UsageError("%s is a Codex account, and `ccdad switch` installs a Claude "+
						"account's login. A Codex account is served through ccdad's proxy; there is "+
						"nothing to install", target.Label())
				}
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
						"  out from under ccdad's copy. Try `ccdad status --refresh` first.")
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
	cmd.Flags().StringVar(&providerName, "provider", "",
		"assert the named account's provider (claude or codex), or with no ACCOUNT pick the best codex account")
	return cmd
}

// runCodexSwitch repoints ccdad's codex proxy at one account.
//
// It writes a pointer and stamps a cooldown, and that is all. Nothing here
// touches Claude Code's credentials file, takes a Claude Code lock, or reads a
// stored login -- which is why it takes a root and an account rather than the
// store.
//
// The sentence names the NEW THREAD because that is the honest scope of the
// change: the proxy keeps a thread with the account that produced its earlier
// turns, so a session already running goes on being billed where it was.
func runCodexSwitch(cmd *cobra.Command, root string, target store.Account) error {
	if serving, ok := codexswitch.ReadServing(root); ok && serving == target.UUID {
		// Exit 3 is "the world is already as you asked". Reporting 0 would tell
		// a cron job it changed something it did not.
		fmt.Fprintf(cmd.ErrOrStderr(), "Codex is already served from %s.\n", target.Label())
		return WithCode(errSilent, ExitNothingToDo)
	}
	if err := codexswitch.Execute(root, target.UUID); err != nil {
		if errors.Is(err, codexswitch.ErrPointerMovedUnstamped) {
			// The pointer already moved -- Execute writes it before it stamps
			// the switch cooldown, and only the stamp failed. Reporting a
			// plain failure here would be a lie: codex now serves target, just
			// without the cooldown that holds a poll off an immediate
			// re-switch. Say what is actually being served, the same honest
			// split the daemon's own codexTick makes on this same error.
			fmt.Fprintf(cmd.ErrOrStderr(),
				"Serving codex from %s from the next new thread, but its switch cooldown was not recorded: %v\n"+
					"  A poll shortly after this could repoint again immediately.\n",
				target.Label(), err)
			return WithCode(errSilent, ExitFailure)
		}
		return err
	}
	if daemonIsRunning() {
		fmt.Fprintf(cmd.ErrOrStderr(), "Serving codex from %s from the next new thread.\n", target.Label())
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"Serving codex from %s from the next new thread, once the daemon runs.\n", target.Label())
	return nil
}

// chooseCodexTarget is the targetless codex grammar: the lane picks, under the
// same margins the daemon applies.
//
// It is switcher.EvaluateCodex and not a second ranking, for the reason
// chooseTarget is switcher.Evaluate: the moment there are two copies, the
// hand-verified path and the unattended path diverge. What stays here is the
// wording.
//
// --force is deliberately not honoured. It bypasses the anti-flap margins for a
// switch that installs a credential and can be undone by switching back; a
// codex repoint is already reversible in one command, and the cooldown it would
// bypass is the only thing standing between two shells and a pointer that moves
// on every invocation.
func chooseCodexTarget(cmd *cobra.Command, s *store.Store, root string) (store.Account, error) {
	stderr := cmd.ErrOrStderr()
	ev, err := switcher.EvaluateCodex(s, root, switcher.EvalOptions{Provider: provider.Codex}, nil)
	if err != nil {
		return store.Account{}, err
	}
	if ev.ConfigErr != nil {
		fmt.Fprintf(stderr, "note: %v; using the built-in defaults.\n", ev.ConfigErr)
	}
	if ev.StateErr != nil {
		fmt.Fprintf(stderr, "note: the auto-switch state could not be read (%v); "+
			"proceeding with no cooldown and no quarantines.\n", ev.StateErr)
	}
	// NoReadings is checked BEFORE Decided, and that order is load-bearing.
	// EvaluateCodex sets Decided only after both of its early returns, so
	// NoReadings == true always arrives with Decided == false -- checking
	// !ev.Decided first would swallow the no-readings case and tell a user
	// with several unpolled codex accounts "There are no codex accounts."
	// !ev.Decided then covers the one case that remains: a store with no
	// codex accounts at all.
	if ev.NoReadings {
		fmt.Fprintln(stderr, "ccdad has no codex usage readings yet, so there is nothing to choose on.")
		fmt.Fprintln(stderr, "Name an account explicitly, or let the daemon poll first.")
		return store.Account{}, WithCode(errSilent, ExitBlocked)
	}
	if !ev.Decided {
		fmt.Fprintln(stderr, "There are no codex accounts. Run 'ccdad codex add' to log one in.")
		return store.Account{}, WithCode(errSilent, ExitBlocked)
	}

	switch ev.Plan.Action {
	case strategy.ActionSwitch:
		return ev.Target, nil
	case strategy.ActionBlocked:
		fmt.Fprintf(stderr, "No codex account can be switched to: %s.\n", switcher.Explain(ev.Plan))
		return store.Account{}, WithCode(errSilent, ExitBlocked)
	default:
		fmt.Fprintf(stderr, "Staying put: %s.\n", switcher.Explain(ev.Plan))
		return store.Account{}, WithCode(errSilent, ExitNothingToDo)
	}
}
