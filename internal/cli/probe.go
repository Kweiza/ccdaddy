package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// A window that has never been spent against reports no reset time, so an
// account holding one has no reset, no pace and no projection — the engine can
// neither say when it comes back nor project when it runs out. No amount of
// polling changes that: the endpoint starts reporting a reset only once
// something has actually been spent against the window. This command spends the
// smallest thing that counts.
//
// It is a scoped run rather than a switch, which is the whole reason it is safe
// to do unattended: the live login is never touched and a session in flight is
// never interrupted, so there is nothing to roll back afterwards.

// probePrompt is the smallest thing that spends a window. One word, because the
// answer is not the point and every token in it is the user's own quota.
//
// It carries no character cmd.exe acts on, which is what lets the Windows shim
// refusal below be about --model alone.
const probePrompt = "hi"

// probeTimeout bounds one probe's claude.
//
// Two minutes, which is deliberately several times longer than any HTTP timeout
// in this tree: a probe is a whole Claude Code starting and taking a turn, not a
// request. What the bound is really for is the other case — a claude that is not
// slow but WAITING, on a trust prompt or a login that nothing unattended will
// ever answer. The probe's credential home is deleted when the child returns, so
// an unbounded probe is an unbounded directory holding a live refresh token.
const probeTimeout = 2 * time.Minute

// probeCacheTimeout bounds the wait for the usage cache's lock while an attempt
// is stamped. The contention is a daemon poller mid-write, which is a
// sub-second read-modify-write.
const probeCacheTimeout = 5 * time.Second

// probeOptions is one invocation's inputs, gathered before any account is
// touched so that --all resolves claude and reads the clock once rather than
// once per account.
type probeOptions struct {
	// claude is the resolved binary. Never a bare name, for the reason
	// launchSpec gives.
	claude string
	// model is --model verbatim, "" for whatever claude defaults to.
	model string
	// window is the window this probe means to wake, derived from model.
	window usage.WindowName
	force  bool
	now    time.Time
}

// probeArgs is the child's command line.
//
// --max-turns 1 is what stops a model that decides to use a tool from turning a
// probe into a session: without it a single "hi" can become a run of turns, each
// one more of the quota this command exists to spend as little of as possible.
func probeArgs(model string) []string {
	args := []string{"-p", probePrompt, "--max-turns", "1"}
	if model != "" {
		args = append(args, "--"+daemon.ProbeModelFlag, model)
	}
	return args
}

// probeWindow is the window a probe with this --model wakes.
//
// The mapping is by FAMILY rather than by model name, through the same
// ModelFamily the ranking narrows a per-model window with: the wire gives a
// per-model weekly window only a display name — "Opus 4.5" — and a user typing
// --model types the family. A name carrying no family ccdad knows still spends
// the five-hour window, which is the window every account has and the only one
// such a probe can honestly promise to wake.
func probeWindow(model string) usage.WindowName {
	family, ok := strategy.ModelFamily(model)
	if !ok {
		return usage.WindowFiveHour
	}
	switch family {
	case "opus":
		return usage.WindowSevenDayOpus
	case "sonnet":
		return usage.WindowSevenDaySonnet
	}
	return usage.WindowFiveHour
}

// credentialWord names a stored token record in words a refusal can use.
func credentialWord(kind string) string {
	switch kind {
	case cclink.APIKeyKind:
		return "an API key"
	case "setup-token":
		return "a setup token"
	}
	return fmt.Sprintf("a %q credential", kind)
}

// probeSkip is why an account will not be probed, and whether naming it was a
// mistake the caller made.
//
// The two halves are different answers and the exit contract keeps them apart:
// an account that can NEVER be probed usefully is a usage error to name, while
// a window that already reports a reset time is the world already being how the
// caller asked for it. A --all pass reads both as a note and moves on.
//
// The order is the order of finality, and the first three are the ones --force
// may not bypass. The credential test comes first because every usage window
// ccdad can read comes from an OAuth login's refresh grant — the poller skips an
// account with no claudeAiOauth record every cadence — so a probe of a
// setup-token or API-key account spends real quota for a reading nothing could
// ever take. The keychain-era test comes second, and it is checked HERE rather
// than at the launch so that the quota notice is never printed for a probe that
// is about to be refused: on a Claude Code that predates
// CLAUDE_SECURESTORAGE_CONFIG_DIR the child would run as the machine's LIVE
// login and spend the wrong account's quota while ccdad recorded a probe of this
// one. refuseKeychainEra returns early for a token record, which is why the
// credential test above it is the one that answers those accounts.
//
// The displaced-auth test comes third, for the same reason as the second: an
// ANTHROPIC_AUTH_TOKEN or CLAUDE_CODE_OAUTH_TOKEN already exported in this
// shell is what claude actually authenticates the child with, ahead of the
// scoped credentials file this probe seeds — so the turn would be spent
// against whatever account that variable names while ccdad stamped THIS one as
// probed and started it on a six-hour cooldown for a reading it never took.
// `ccdad run`'s launch refuses the identical condition unconditionally (never
// gated by --full-profile); a probe's own scoping is no less exposed to it.
func probeSkip(a store.Account, blob cclink.Blob, entry usage.Entry, o probeOptions) (string, bool) {
	if rec, isToken := cclink.TokenRecordOf(blob); isToken {
		return fmt.Sprintf("%s's credential is %s rather than an OAuth login, and every usage window ccdad "+
			"can read comes from a login's refresh grant — so a probe would spend quota for a reading "+
			"nothing could ever take", a.Label(), credentialWord(rec.Kind)), true
	}
	if kerr := refuseKeychainEra(describeClaudeInstall(o.claude), blob, a.Label()); kerr != nil {
		return kerr.Error(), true
	}
	if derr := refuseDisplacedAuth(claudeOAuthEnvironment(), blob, a.Label()); derr != nil {
		return derr.Error(), true
	}
	if o.force {
		return "", false
	}
	if _, has := entry.Snapshot.ResetFor(o.window); has {
		return fmt.Sprintf("%s's %s window already reports a reset time, so a probe has nothing to wake; "+
			"pass --%s to spend the quota anyway", a.Label(), o.window, daemon.ProbeForceFlag), false
	}
	if !entry.MayProbe(o.now) {
		next := entry.Probe.LastAttemptAt.Add(usage.ProbeRetryAfter)
		return fmt.Sprintf("%s was probed at %s, and a probe is attempted at most once every %s, so the next "+
			"one may run at %s; pass --%s to spend the quota now",
			a.Label(), entry.Probe.LastAttemptAt.Format(time.RFC3339), usage.ProbeRetryAfter,
			next.Format(time.RFC3339), daemon.ProbeForceFlag), false
	}
	return "", false
}

// probeQuotaNoticer returns a function that says a probe spends the account's
// own quota, the first time an invocation is about to spend it.
//
// Once, not once per account: --all over five accounts is one command and one
// fact, and five copies of it is five lines a reader skips. Lazily, not before
// the loop: a pass that skipped every account spent nothing, and telling a user
// they were charged for it is worse than saying nothing.
func probeQuotaNoticer(w io.Writer) func() {
	said := false
	return func() {
		if said {
			return
		}
		said = true
		fmt.Fprintln(w, "note: a probe spends this account's own quota — one turn of Claude Code — because "+
			"the endpoint reports no reset time for a window until something has been spent against it.")
	}
}

// probeOne runs one account's probe.
//
// The shape is `ccdad run`'s, to the letter and on purpose: newSession, the
// account's stored login written into it, and a deferred adopt-back followed by
// removal. That is the code that has already been made to survive a refresh
// token the server rotated mid-session, a claude that never starts, and a panic,
// and a second copy of it would be a second copy of all three bugs.
//
// It seeds the credential home outright rather than going through authorise,
// and that is a consequence of probeSkip rather than a shortcut: every stored
// token record is refused before this is reached, so the only credential shape
// that arrives here is an OAuth login — the one case authorise answers by
// handing back the session's own environment and asking for the file to be
// written.
//
// The defer is registered as soon as there is a directory and before anything
// that can fail — the seed, the launch — so a failed seed, a non-zero claude and
// a panic all tear the session down. The one case that deliberately keeps it is
// an adopt-back that failed, because the file may then be the only copy of a
// token the server has already rotated to; that is exactly the leftover
// `ccdad doctor`'s sessions check reports, and this command's note says so in
// the same words `run`'s does.
func probeOne(cmd *cobra.Command, a store.Account, blob cclink.Blob, o probeOptions) error {
	session, err := newSession(a.UUID)
	if err != nil {
		return err
	}
	defer func() {
		if aerr := adoptBack(a.UUID, session.home); aerr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"note: %s's probe session credentials could not be carried back into the store (%v).\n"+
					"They are kept at %s; 'ccdad doctor' reports it.\n", a.Label(), aerr, session.home)
			return
		}
		if rerr := removeSession(session.home); rerr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "note: could not remove the probe session directory %s (%v); "+
				"'ccdad doctor' reports it.\n", session.home, rerr)
		}
	}()

	if serr := seedSession(blob, session.home); serr != nil {
		return serr
	}
	code, cerr := startChild(launchSpec{
		Path:    o.claude,
		Args:    probeArgs(o.model),
		Env:     session.env,
		Timeout: probeTimeout,
		Silent:  true,
	})
	if cerr == nil && code != ExitOK {
		cerr = fmt.Errorf("claude exited %d", code)
	}
	// Stamped whatever happened, and before the error is returned: the six-hour
	// gate exists for the failures, and a failure that did not record the
	// attempt would be retried at the caller's own cadence forever.
	if rerr := usage.RecordProbe(probeCacheTimeout, a.UUID, timeNow(), cerr); rerr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %s's probe could not be recorded (%v); it may be attempted "+
			"again sooner than it should be.\n", a.Label(), rerr)
	}
	return cerr
}

func newProbeCmd() *cobra.Command {
	var all, force bool
	var model, uuid string

	cmd := &cobra.Command{
		Use:   daemon.ProbeArg + " [ACCOUNT]",
		Short: "Spend one turn of an account's quota so a window with no reset time reports one",
		Long: "A window that has never been spent against reports no reset time, so ccdad cannot say\n" +
			"when the account comes back or project when it runs out. Only spending against the\n" +
			"window makes the endpoint report a reset, so this runs one turn of Claude Code —\n" +
			"`claude -p " + probePrompt + " --max-turns 1` — as that account, and schedules a poll a minute later.\n\n" +
			"IT SPENDS THE ACCOUNT'S OWN QUOTA. That is the whole mechanism, not a side effect.\n\n" +
			"The live login is never touched and a session in flight is never interrupted: the\n" +
			"probe gets a credential home of its own holding only that account's login, the same\n" +
			"one `ccdad run` builds, and it is deleted when the turn ends.\n\n" +
			"ACCOUNT may be a display index, an alias, an email address, or a uuid prefix. --uuid\n" +
			"takes an exact uuid and nothing else, for callers that must not depend on a display\n" +
			"ordinal that is recompacted whenever an account is removed.\n\n" +
			"--model names the family the turn is spent against, which is how a per-model weekly\n" +
			"window is woken: 'opus' and 'sonnet' wake theirs, and any other name still wakes the\n" +
			"five-hour window every account has.\n\n" +
			"An account that already reports a reset time for that window is not probed, and a\n" +
			"probe is attempted at most once every " + usage.ProbeRetryAfter.String() + " per account. --force spends the quota\n" +
			"through both of those; it does not make an account without an OAuth login probeable,\n" +
			"because nothing could read the result.",
		Args:          usageArgs(cobra.MaximumNArgs(1)),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			named := 0
			for _, on := range []bool{ref != "", uuid != "", all} {
				if on {
					named++
				}
			}
			if named == 0 {
				return UsageError("probe needs an account, --%s, or --all; run 'ccdad list' to see them",
					daemon.ProbeUUIDFlag)
			}
			if named > 1 {
				return UsageError("name an account, or --%s, or --all — not more than one of them",
					daemon.ProbeUUIDFlag)
			}

			s, err := store.Open()
			if err != nil {
				return err
			}

			// Resolved before anything is spent, and a usage error rather than a
			// runtime one: a machine with no Claude Code cannot probe however
			// often it is asked, so a caller has to change something rather than
			// retry. `run` reports the same failure as a runtime error because it
			// is one refusal among many there; here it is the whole command.
			path, err := lookClaude("claude")
			if err != nil {
				return UsageError("a probe runs Claude Code, and `claude` is not on this PATH (%v). "+
					"Install it with 'npm i -g @anthropic-ai/claude-code', or put it on PATH", err)
			}

			o := probeOptions{
				claude: path,
				model:  model,
				window: probeWindow(model),
				force:  force,
				now:    timeNow(),
			}
			// The Windows shim rule, refused rather than resolved past. `run`
			// resolves past a shim for every .cmd it can read, because the
			// argument cmd.exe would re-split is the user's own prompt and
			// refusing would block the session they asked for; here every
			// argument is ccdad's except --model, and no model family name
			// contains a character cmd.exe acts on. Refusing is a complete
			// answer, and it keeps one copy of the resolution dance.
			//
			// Asked only when cmd.exe is actually in the launch. A native
			// claude.exe, or any claude off Windows, never sees a shell, so a
			// --model nobody would type is not a reason to refuse there.
			if cmdShimTarget(path) {
				if bad := unsafeForCmdShim(probeArgs(model)); bad != "" {
					return UsageError("%s is a cmd.exe shim, and cmd.exe would re-interpret %q rather than "+
						"pass it on. Name the model family plainly — 'opus', 'sonnet' — or install the "+
						"native claude.exe", path, bad)
				}
			}

			var targets []store.Account
			switch {
			case all:
				for _, a := range s.Accounts() {
					// A disabled account is one the engine will not switch to, so
					// a reading for it buys nothing and the quota would be spent
					// for nobody. A disabled account NAMED explicitly is still
					// probed: that is a human asking.
					if a.Disabled {
						continue
					}
					targets = append(targets, a)
				}
			case uuid != "":
				a, ok := s.Get(uuid)
				if !ok {
					return UsageError("no account has the uuid %q", uuid)
				}
				targets = []store.Account{a}
			default:
				a, rerr := store.Resolve(s.Accounts(), ref)
				if rerr != nil {
					return UsageError("%s", rerr.Error())
				}
				targets = []store.Account{a}
			}

			cache, err := usage.LoadCache()
			if err != nil {
				return err
			}
			stderr := cmd.ErrOrStderr()
			notice := probeQuotaNoticer(stderr)
			spent, failed := 0, 0
			for _, a := range targets {
				blob, cerr := s.Credentials(a.UUID)
				if cerr != nil {
					if !all {
						return cerr
					}
					fmt.Fprintf(stderr, "note: %s: %v\n", a.Label(), cerr)
					continue
				}
				entry, _ := cache.Get(a.UUID)
				if reason, mistake := probeSkip(a, blob, entry, o); reason != "" {
					if all {
						fmt.Fprintf(stderr, "note: %s\n", reason)
						continue
					}
					if mistake {
						return UsageError("%s", reason)
					}
					fmt.Fprintf(stderr, "%s\n", reason)
					return WithCode(errSilent, ExitNothingToDo)
				}
				notice()
				spent++
				if perr := probeOne(cmd, a, blob, o); perr != nil {
					failed++
					fmt.Fprintf(stderr, "note: %s's probe failed: %v\n", a.Label(), perr)
				}
			}
			if spent == 0 {
				fmt.Fprintln(stderr, "No account needed a probe.")
				return WithCode(errSilent, ExitNothingToDo)
			}
			// Exit 1 only when every probe that was attempted failed. A --all
			// pass where one account woke and another did not DID the thing it
			// was asked to do, and a supervisor that retried it would spend the
			// quota of the account that already worked.
			if failed == spent {
				return WithCode(errSilent, ExitFailure)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "probe every enabled account whose window has no reset time")
	cmd.Flags().StringVar(&uuid, daemon.ProbeUUIDFlag, "",
		"the exact uuid to probe, for callers that must not resolve a display ordinal")
	cmd.Flags().StringVar(&model, daemon.ProbeModelFlag, "",
		"the model family to spend the turn against, which selects the weekly window the probe wakes")
	cmd.Flags().BoolVar(&force, daemon.ProbeForceFlag, false,
		"spend the quota even when ccdad believes the probe is unnecessary")
	return cmd
}
