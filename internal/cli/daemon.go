package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/view"
	"github.com/Kweiza/ccdaddy/internal/winerr"
)

// The daemon group is where the exit taxonomy earns the two codes it added to
// the reference tools' sets, so the mapping is the contract here and the
// commands are what serve it:
//
//	status   0 running · 5 not running · 1 cannot determine
//	start    0 started one · 3 one is already running · 1 cannot determine
//	stop     0 stopped it · 3 nothing was running · 5 nothing was listening ·
//	         1 cannot determine, or it would not go and could not be terminated
//	restart  0 there is a daemon running now · 1 otherwise
//	logs     0 printed · 5 there is no log to print
//
// `ccdad daemon status; [ $? -eq 5 ] && ccdad daemon start` is the idiom the
// split exists for, and it is safe only while 5 means a DEFINITE no. clauth
// returns 1 for both "no daemon" and "the lock is unusable" and its own comment
// names the consequence: on a filesystem where locks do not work — ENOLCK on an
// NFS or CIFS mount with no lock daemon — that loop respawns forever. Every
// verb below therefore asks the singleton first and refuses to act on an
// error, which is why "cannot determine" never reaches a spawn or a signal.
//
// 3 rather than 5 for "already running" and "nothing to stop" is the other half
// of the taxonomy: 3 means the world is already as you asked and 5 is a
// negative answer to a probe. Only `status` and `logs` are probes.
var (
	// singletonHeld is the liveness authority for start, stop and restart. They
	// do not need the published document, and Observe would read it anyway.
	singletonHeld = daemon.SingletonHeld
	spawnDaemon   = daemon.SpawnFrom
	// requestShutdown delivers the stop; it never waits for it. What proves the
	// daemon went is the singleton, polled below.
	requestShutdown = daemon.RequestShutdown
	// forceShutdown is the guarded escalation, and it exists on Windows alone —
	// everywhere else it answers errors.ErrUnsupported and this file reports the
	// timeout instead. See daemon.ForceShutdown for why the asymmetry is the
	// design rather than a gap.
	forceShutdown = daemon.ForceShutdown
	readDaemonPID = daemon.ReadPID
	// runDaemon is the loop itself, behind a seam so a test can drive the hidden
	// entrypoint without turning the test binary into a daemon.
	runDaemon = daemon.Run
	// spawnSuccessor replaces a daemon that gave up on its own tick loop. It is
	// separate from spawnDaemon because the two are started for opposite
	// reasons and only this one carries a recovery count.
	spawnSuccessor = daemon.SpawnSuccessor
)

var (
	// daemonPollInterval and daemonWaitTimeout bound every wait in this file.
	//
	// A bounded POLL of the lock, never a fixed sleep: a sleep long enough for a
	// loaded machine is wasted on every idle one, and a sleep short enough to
	// feel quick is a restart whose new daemon races the old one for the
	// singleton and loses. Ten seconds is the outer bound because the thing
	// being waited for is a tick that must finish — the daemon's tick executes
	// a credential swap, and cutting one short is what abandons Claude Code's
	// lock directories.
	daemonPollInterval = 50 * time.Millisecond
	daemonWaitTimeout  = 10 * time.Second

	// logFollowInterval is how often `daemon logs --follow` looks for new bytes.
	logFollowInterval = 250 * time.Millisecond
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Start, stop and inspect the background daemon",
		Long: "The daemon is self-managed: a detached process holding a singleton lock,\n" +
			"which is the only thing that can answer whether one is running.\n\n" +
			"Exit codes: status is 0 running, 5 not running, 1 cannot determine — so\n" +
			"'ccdad daemon status; [ $? -eq 5 ] && ccdad daemon start' never respawns\n" +
			"in a loop on a filesystem where locks do not work.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			// Cobra's own answer is to print help and exit 0. A caller that
			// meant to type a verb gets a usage error, as everywhere else.
			return UsageError("daemon needs a subcommand: one of start, stop, restart, status, logs")
		},
	}
	cmd.AddCommand(
		newDaemonStartCmd(),
		newDaemonStopCmd(),
		newDaemonRestartCmd(),
		newDaemonStatusCmd(),
		newDaemonLogsCmd(),
	)
	return cmd
}

// newDaemonRunCmd is the hidden entrypoint Spawn re-execs into.
//
// It is a command rather than an argv check in main() because everything that
// makes it safe already lives in the command tree: the exit taxonomy, the
// argument validation, the error printing. The name comes from
// daemon.RunArg — the constant Spawn passes — so the two cannot drift apart,
// and its leading underscores keep it out of the namespace a user could type by
// accident. Hidden, so it never appears in help or in a completion script.
//
// Auto-start must never fire for THIS command. It is the child, and a child
// that auto-starts a child is a fork bomb rather than a bug; the guard belongs
// with the auto-start policy and is asserted there.
func newDaemonRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:           daemon.RunArg,
		Short:         "Run the daemon in the foreground (internal)",
		Hidden:        true,
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A lost race for the singleton is not a failure: it means another
			// daemon got there first, which is exactly what the singleton is
			// for, and "the world is already as you asked" is 3.
			err := runDaemon(cmd.Context(), daemon.EngineOptions())
			if errors.Is(err, daemon.ErrSingletonHeld) {
				return WithCode(err, ExitNothingToDo)
			}
			// The wedge hand-off, and it happens HERE for one reason: the
			// successor's first act is to take the singleton, and Run gives it
			// back in a defer -- so this is the earliest place the lock is
			// free. Doing it inside Run would have the replacement race the
			// process it is replacing and lose.
			var wedged *daemon.WedgedError
			if errors.As(err, &wedged) {
				fmt.Fprintf(cmd.ErrOrStderr(), "%v; starting a replacement\n", wedged)
				if serr := spawnSuccessor(wedged.NextRecovery); serr != nil {
					// Both facts, in that order. The wedge is why there is no
					// daemon; the spawn failure is why there will not be one.
					return errors.Join(err, fmt.Errorf("starting the replacement: %w", serr))
				}
				// The daemon did its job by standing down for a successor that
				// started. Reporting that as a failure would have every
				// supervisor treat a working recovery as an incident.
				return nil
			}
			return err
		},
	}
}

func newDaemonStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report whether a daemon is running (0 running, 5 not, 1 cannot tell)",
		Long: "status is a probe, not a dashboard: it answers in the exit code first.\n" +
			"For what the engine is doing, run 'ccdad status'.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemonStatus(cmd, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	return cmd
}

// runDaemonStatus answers in the exit code, and prints the same thing whether or
// not it could.
//
// The --json path emits its document and STILL exits non-zero, which is the
// whole reason this is not a dashboard: `{"running":false}` with exit 0
// rebuilds the ambiguity the code split exists to remove, and a supervisor
// written against it has to parse JSON to find out what a number already said.
func runDaemonStatus(cmd *cobra.Command, asJSON bool) error {
	report, probeErr := observeDaemon()

	code := ExitFailure
	switch report.State {
	case daemon.DaemonRunning:
		code = ExitOK
	case daemon.DaemonStopped:
		code = ExitProbeNegative
	}

	if asJSON {
		if err := writeJSON(cmd, map[string]any{
			"schemaVersion": 1,
			"daemon":        daemonJSON(report, probeErr),
		}); err != nil {
			return err
		}
	} else {
		out, pal := renderTarget(cmd)
		// The colour is on the VERDICT and not on the label, because the
		// verdict is what a reader is scanning for. Both branches keep their
		// sentence: "not running" is still the words "not running" with the
		// colour stripped off, so this is a second reading of the answer and
		// never the only one.
		switch report.State {
		case daemon.DaemonRunning:
			fmt.Fprintln(out, "ccdad daemon: "+
				pal.Style(theme.RoleActive).Render(view.DescribeRunning(report, timeNow())))
		case daemon.DaemonStopped:
			fmt.Fprintln(out, "ccdad daemon: "+
				pal.Style(theme.RoleMuted).Render("not running"))
		}
		// Three-valued and it stays three-valued: an unprobeable lock prints
		// nothing here and the error is the output, on stderr and unpainted.
		// "Unknown" folded into the grey of "not running" would be a probe that
		// answered when it could not, which is the one thing this command
		// exists not to do.
		if report.StatusErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "The daemon's status file could not be read: %v\n", report.StatusErr)
		}
	}

	switch code {
	case ExitOK:
		return nil
	case ExitProbeNegative:
		return WithCode(errSilent, ExitProbeNegative)
	default:
		// The only case with nothing rendered on the human path: there is no
		// answer to print, so the error IS the output.
		if asJSON {
			return WithCode(errSilent, ExitFailure)
		}
		return fmt.Errorf("cannot tell whether a daemon is running: %w", probeErr)
	}
}

// daemonJSON renders the daemon half of a payload. `ccdad status --json` nests
// exactly this object under "daemon" too, so a consumer reads .daemon.state
// from either command and neither can drift from the other.
//
// The schemaVersion inside is the published DOCUMENT's, not the CLI payload's.
// They are different numbers about different things, which is why this object
// is nested rather than flattened into its caller.
func daemonJSON(report daemon.Report, probeErr error) map[string]any {
	d := map[string]any{"state": report.State.String()}
	if probeErr != nil {
		d["error"] = probeErr.Error()
	}
	if report.HasStatus {
		d["pid"] = report.Status.PID
		d["schemaVersion"] = report.Status.SchemaVersion
		d["generatedAt"] = report.Status.GeneratedAt
		if !report.Status.StartedAt.IsZero() {
			d["startedAt"] = report.Status.StartedAt
		}
		if report.Status.Stopped {
			d["stopped"] = true
		}
		// The daily release check, as the SAME four flat keys the published
		// document carries rather than a nested object: a nested form would be
		// wire-incompatible with the document for no gain, and a consumer that
		// reads status.json directly and one that reads this payload would then
		// need two spellings of one fact. Each is behind its own zero guard, so
		// an ordinary payload does not carry four fields that are always empty.
		if !report.Status.UpdateCheckedAt.IsZero() {
			d["updateCheckedAt"] = report.Status.UpdateCheckedAt
		}
		if !report.Status.NextUpdateCheckAt.IsZero() {
			d["nextUpdateCheckAt"] = report.Status.NextUpdateCheckAt
		}
		if report.Status.UpdateLatest != "" {
			d["updateLatest"] = report.Status.UpdateLatest
		}
		if report.Status.UpdateCheckError != "" {
			d["updateCheckError"] = report.Status.UpdateCheckError
		}
	}
	return d
}

func newDaemonStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "start",
		Short:         "Start a detached daemon (3 if one is already running)",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			held, err := singletonHeld()
			if err != nil {
				// Not 3 and not 5: nothing is known, so nothing is started.
				return cannotTell(err)
			}
			if held {
				fmt.Fprintf(cmd.ErrOrStderr(), "A ccdad daemon is already running%s.\n", runningPIDSuffix())
				return WithCode(errSilent, ExitNothingToDo)
			}
			return startDaemon(cmd)
		},
	}
}

func newDaemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon (3 if there is none)",
		Long: "stop asks the daemon to finish the tick in flight and shut down, then\n" +
			"waits for it to release the singleton. It never escalates to a kill: a\n" +
			"tick cut short mid-swap abandons Claude Code's lock directories.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stopped, err := stopDaemon(cmd)
			if err != nil {
				return err
			}
			if !stopped {
				fmt.Fprintln(cmd.ErrOrStderr(), "No ccdad daemon is running.")
				// 3, not 5: the world is already as the caller asked. 5 is
				// reserved for `status`, whose whole job is to answer the
				// question rather than to act on it.
				return WithCode(errSilent, ExitNothingToDo)
			}
			return nil
		},
	}
}

func newDaemonRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Stop any running daemon, wait for the lock to clear, and start one",
		Long: "restart is not 'stop; start'. The new daemon must not go for the\n" +
			"singleton while the old one still holds it, so the wait is a bounded poll\n" +
			"of the lock rather than a sleep long enough to usually work.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// stopDaemon returns only once the singleton has actually cleared,
			// which is the precondition the spawn below depends on. Nothing was
			// running is not an error here: restart's contract is the END state.
			if _, err := stopDaemon(cmd); err != nil {
				return err
			}
			return startDaemon(cmd)
		},
	}
}

func startDaemon(cmd *cobra.Command) error { return startDaemonFrom(cmd, "") }

// startDaemonFrom spawns a detached daemon from exe and waits for it to take
// the singleton. "" means "resolve it the way Spawn always did", which is what
// startDaemon above passes and what every caller but one wants.
//
// Waiting is not politeness. Spawn returns as soon as the process is started,
// and a `daemon start` that returned there would be followed by a `daemon
// status` reporting 5 — the daemon is real, it just has not reached its first
// lock yet. The wait is what makes the two commands compose.
//
// `ccdad update` is the one caller that names a file: it has just written a new
// binary and restarts from that exact path rather than from whatever
// os.Executable resolves to a moment later.
func startDaemonFrom(cmd *cobra.Command, exe string) error {
	if err := refuseAStoreThisBuildCannotUse(cmd); err != nil {
		return err
	}
	if err := refuseAClaimedCredentialHome(cmd); err != nil {
		return err
	}
	if err := repairOrRefuseAnUnreadableLogin(cmd); err != nil {
		return err
	}
	if err := spawnDaemon(exe); err != nil {
		return err
	}
	up, err := waitForSingleton(true)
	if err != nil {
		return cannotTell(err)
	}
	if !up {
		return fmt.Errorf("a daemon was started but had not taken the singleton after %s; %s may say why",
			daemonWaitTimeout, namePath(daemon.LogPath()))
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Started the ccdad daemon%s.\n", runningPIDSuffix())
	return nil
}

// storeVersionCheck is CheckVersionAt, as a seam for the same reason
// credentialHomeClaim below is one.
var storeVersionCheck = store.CheckVersionAt

// refuseAStoreThisBuildCannotUse stops a start that would immediately be
// refused by the child, and it has to happen HERE for the reason
// refuseAClaimedCredentialHome below does: Spawn detaches the child, so its
// own refusal reaches daemon.log and nowhere else. Without this, `daemon
// start` runs all the way through the spawn and the ten-second singleton wait,
// then reports "a daemon was started but had not taken the singleton after
// 10s" — true, but not the reason, which the child had already written to
// daemon.log the moment it read the document.
//
// The document is the one daemon.Run itself refuses at start: a version-2
// header over rows with no provider, which is what a ccdad that predates
// Codex support leaves behind after reading a version-2 document and writing
// it back without a key it does not know. Narrowed to ErrProviderMissing to
// match daemon.Run's own narrowing — a document this process cannot open or
// parse describes a damaged machine, not an unsupported one, and the daemon
// runs through those rather than refusing at start, so this pre-check must
// not refuse a start the child itself would have tolerated.
func refuseAStoreThisBuildCannotUse(cmd *cobra.Command) error {
	root, err := ccpath.StoreHome()
	if err != nil {
		// Left for the spawn to report: it resolves the identical path and
		// fails the identical way, with the identical message.
		return nil
	}
	verr := storeVersionCheck(root)
	if !errors.Is(verr, store.ErrProviderMissing) {
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Not starting: %v.\n", verr)
	return WithCode(errSilent, ExitBlocked)
}

// credentialHomeClaim is the probe, as a seam, so a test can describe another
// store's engine without starting one.
var credentialHomeClaim = credhome.ProbeSettled

// refuseAClaimedCredentialHome stops a start that would immediately be refused
// by the child, and it has to happen HERE rather than in daemon.Run.
//
// Two things make the parent the only place this answer is usable. Spawn
// detaches the child and releases the process, so nothing the child returns
// reaches this command — its refusal would go to daemon.log and nowhere else.
// And the child takes the SINGLETON before the claim, holding it for the whole
// ~200 ms claim retry while it is being refused: waitForSingleton polls every
// 50 ms, sees it held, and `daemon start` prints "Started the ccdad daemon."
// and exits 0 for a process that is already dead.
//
// It refuses with 4, not 3. `ccdad daemon status; [ $? -eq 5 ] && ccdad daemon
// start` is the supervisor idiom this file's exit split exists for, and 3 is
// the code exit.go tells operators to ignore — so 3 here is that loop spinning
// forever on a machine nobody is told about. 4 is "wanted, blocked, alert on
// this", which is exactly the situation.
//
// The test is HELD-AND-NOT-OURS, named or not, and it has to match what the
// child will actually do: credhome.Acquire refuses a claim it cannot attribute
// exactly as it refuses one it can, because the lock is held either way. A
// parent that started the daemon on a nameless holder would be printing
// "starting anyway" over a child that is already doomed — which is this
// function's whole failure mode, reached by the other door.
//
// The probe is the SETTLED one for that reason. Held-but-unnamed is transient
// by construction, and refusing a start on a microsecond of a departing
// engine's shutdown would be its own defect.
//
// A probe that could not ANSWER is different and starts the daemon: the daemon
// degrades rather than refusing on that, so the refusal would be made on behalf
// of a child that would have run.
func refuseAClaimedCredentialHome(cmd *cobra.Command) error {
	out := cmd.ErrOrStderr()
	s, err := credentialHomeClaim()
	switch {
	case err != nil:
		fmt.Fprintf(out, "ccdad could not tell whether another store's engine is driving "+
			"Claude Code's credential home (%v); starting anyway.\n", err)
		return nil
	case !s.Held, s.Ours:
		return nil
	case !s.Named:
		fmt.Fprintf(out, "Not starting: an engine that will not name itself is already driving %s (%v).\n",
			s.Home, s.OwnerErr)
		fmt.Fprintln(out, "Two stores on one Claude Code login undo each other's switches. Point "+
			"CLAUDE_CONFIG_DIR at a directory of this store's own, or stop that engine.")
		return WithCode(errSilent, ExitBlocked)
	}
	fmt.Fprintf(out, "Not starting: the ccdad store at %s (pid %d) is already driving %s.\n",
		s.Owner.Store, s.Owner.PID, s.Home)
	fmt.Fprintln(out, "Two stores on one Claude Code login undo each other's switches. Point "+
		"CLAUDE_CONFIG_DIR at a directory of this store's own, or stop that engine.")
	return WithCode(errSilent, ExitBlocked)
}

// loginStoreRead and unlockLoginKeychain are seams, for the reason
// credentialHomeClaim above is one: a test cannot arrange a macOS audit session
// that refuses, and it must never be able to raise a real password prompt.
var loginStoreRead = func() error { _, err := cclink.Load(); return err }

var unlockLoginKeychain = cclink.UnlockLoginKeychain

// loginStoreSurvivesRestart is the classification, as a seam of its own, because
// cclink.keychainFailure is unexported: a test in this package cannot build the
// one error that answers true, and stubbing the READ alone would leave the
// decision untested.
var loginStoreSurvivesRestart = cclink.SurvivesRestart

// repairOrRefuseAnUnreadableLogin stops a start that would produce a daemon
// inert for its whole life, and offers the one repair that works.
//
// IT BELONGS HERE for the reason refuseAClaimedCredentialHome names: Spawn
// detaches the child, so the child's own stand-down reaches daemon.log and
// nowhere else. The human who typed the command gets "Started the ccdad daemon
// (pid N)." and exit 0 over a process that will never switch. Measured
// 2026-09-01: five restarts between 12:41 and 13:10, each answered by that
// stand-down in the log and by "Started" at the terminal.
//
// THE TEST IS SurvivesRestart AND NOT "the read failed". macOS scopes
// errSecInteractionNotAllowed to the AUDIT SESSION, and Setsid changes the
// POSIX session but neither the audit session nor the Mach bootstrap namespace
// -- so a child inherits THAT refusal with certainty. Every other read failure
// may clear on its own, and refusing there would turn a self-healing wedge into
// a machine with no daemon at all.
//
// ATTENDED, IT REPAIRS RATHER THAN REFUSES, because the repair is one command
// and it is the command that worked: the keychain unlock is scoped to the
// session too, so unlocking HERE is what a daemon started here inherits. ccdad
// never sees the password -- UnlockLoginKeychain hands stdio to
// /usr/bin/security and reads an exit code -- and this is deliberately not on
// the auto-start path, which spawns without coming through here: an incidental
// `ccdad status` must never ask for a keychain password.
func repairOrRefuseAnUnreadableLogin(cmd *cobra.Command) error {
	err := loginStoreRead()
	if err == nil || !loginStoreSurvivesRestart(err) {
		return nil
	}
	out := cmd.ErrOrStderr()
	if stdinIsTTY() {
		fmt.Fprintf(out, "This session cannot read Claude Code's login: %v\n", err)
		fmt.Fprintln(out, "macOS unlocks a keychain per SESSION, so a daemon started here would inherit "+
			"the refusal and stand down on every tick. Unlocking it in this session is what a daemon "+
			"started from here inherits instead.")
		fmt.Fprintln(out, "The prompt below is /usr/bin/security's own -- ccdad never sees the password.")
		if uerr := unlockLoginKeychain(context.Background()); uerr != nil {
			fmt.Fprintf(out, "That did not complete: %v\n", uerr)
		}
		if err = loginStoreRead(); err == nil || !loginStoreSurvivesRestart(err) {
			return nil
		}
	}
	fmt.Fprintf(out, "Not starting: this session cannot read Claude Code's login (%v), and a daemon "+
		"started here would inherit that for its whole life -- macOS scopes the refusal to the audit "+
		"session, which a child inherits.\n", err)
	fmt.Fprintln(out, "Unlock the keychain in this session, then start again:")
	fmt.Fprintln(out, "    security unlock-keychain && ccdad daemon start")
	return WithCode(errSilent, ExitBlocked)
}

// stopDaemon asks a running daemon to stop and waits for the singleton to
// clear. It reports whether there was one to stop, so `stop` can map that to 3
// while `restart` treats it as nothing to do.
func stopDaemon(cmd *cobra.Command) (bool, error) {
	held, err := singletonHeld()
	if err != nil {
		return false, cannotTell(err)
	}
	if !held {
		return false, nil
	}

	pid, ok, err := readDaemonPID()
	switch {
	case err != nil:
		// The pidfile reader folds every legitimate "nothing to read" into
		// ok=false; an error here is a body that IS committed and does not
		// parse. There is nothing to signal, and guessing is how an unrelated
		// process gets terminated.
		return false, fmt.Errorf("a daemon holds the singleton but %s does not parse, so there is no pid to ask: %w",
			namePath(daemon.PIDPath()), err)
	case !ok:
		// A daemon that has taken the lock but not yet written its pid, which is
		// a window of microseconds during startup.
		return false, fmt.Errorf("a daemon holds the singleton but has not recorded a pid in %s yet; try again in a moment",
			namePath(daemon.PIDPath()))
	}

	if err := requestShutdown(pid); err != nil {
		if errors.Is(err, daemon.ErrNoShutdownListener) {
			// Windows can answer this directly: the named event either exists
			// or it does not, and the stopping side never creates it. A daemon
			// holding the singleton with nothing listening is a negative answer
			// to a probe, not a runtime failure — and not something to escalate
			// on, because the graceful request was never delivered at all.
			fmt.Fprintf(cmd.ErrOrStderr(),
				"The daemon at pid %d holds the singleton but is not listening for a shutdown request: %v\n", pid, err)
			return false, WithCode(errSilent, ExitProbeNegative)
		}
		return false, err
	}
	gone, err := waitForSingleton(false)
	if err != nil {
		return false, cannotTell(err)
	}
	if !gone {
		// The request went out and was not acted on. This is where — and only
		// where — a terminate is on the table, and only on a platform that has
		// a cross-check to put behind it.
		if ferr := forceShutdown(pid); ferr != nil {
			if errors.Is(ferr, errors.ErrUnsupported) {
				return false, fmt.Errorf("the daemon at pid %d was asked to stop and still held the singleton %s later",
					pid, daemonWaitTimeout)
			}
			return false, fmt.Errorf("the daemon at pid %d did not stop, and terminating it was refused: %w", pid, ferr)
		}
		fmt.Fprintf(cmd.ErrOrStderr(),
			"The daemon at pid %d did not stop when asked, so it was terminated.\n", pid)
		if gone, err = waitForSingleton(false); err != nil {
			return false, cannotTell(err)
		}
		if !gone {
			return false, fmt.Errorf("the daemon at pid %d was terminated and still holds the singleton", pid)
		}
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Stopped the ccdad daemon (pid %d).\n", pid)
	return true, nil
}

// waitForSingleton polls until the lock reads want, and reports whether it got
// there before the deadline.
//
// It polls the LOCK and never the pid. A pid stops existing at a moment nothing
// can observe without a race, and it may be recycled onto something unrelated;
// the singleton is released by the kernel when the process dies, which is the
// one fact about a daemon that is never stale.
//
// A probe that errors ends the wait immediately rather than being retried until
// the deadline: "cannot determine" does not become "not running" by being asked
// again, and the caller has to see it as itself.
func waitForSingleton(want bool) (bool, error) {
	deadline := time.Now().Add(daemonWaitTimeout)
	for {
		held, err := singletonHeld()
		if err != nil {
			return false, err
		}
		if held == want {
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		time.Sleep(daemonPollInterval)
	}
}

// runningPIDSuffix is " (pid N)" when a pid can be read, and empty otherwise.
//
// A pidfile that cannot be read costs a number in a sentence and nothing else.
// It is never the reason a command fails, and `ccdad doctor` is where a damaged
// one gets reported.
func runningPIDSuffix() string {
	pid, ok, err := readDaemonPID()
	if err != nil || !ok {
		return ""
	}
	return fmt.Sprintf(" (pid %d)", pid)
}

// cannotTell keeps the one distinction this whole group is built around from
// being lost in a wrapper: a lock that could not be probed is exit 1.
func cannotTell(err error) error {
	return fmt.Errorf("cannot tell whether a daemon is running: %w", err)
}

func newDaemonLogsCmd() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:           "logs",
		Short:         "Print the daemon's log (5 if there is none)",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemonLogs(cmd, lines, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing as the daemon writes")
	cmd.Flags().IntVarP(&lines, "lines", "n", 200, "trailing lines to print first; 0 prints the whole file")
	return cmd
}

// runDaemonLogs prints the log, and 5 when there is not one.
//
// 5 rather than 1, and it does not depend on whether a daemon is running: the
// question "is there a log to read" has a negative answer, which is what 5
// means, and a machine where no daemon has ever started is the ordinary case
// rather than a failure. Treating it as 1 is what would make a supervisor that
// collects logs alert on a fresh install.
func runDaemonLogs(cmd *cobra.Command, lines int, follow bool) error {
	path, err := daemon.LogPath()
	if err != nil {
		return err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(cmd.ErrOrStderr(), "There is no daemon log at %s.\n", path)
			return WithCode(errSilent, ExitProbeNegative)
		}
		return fmt.Errorf("reading %s: %w", path, err)
	}

	out := cmd.OutOrStdout()
	tail := tailLines(body, lines)
	if _, err := out.Write(tail); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	return followLog(cmd.Context(), out, path, int64(len(body)), logFollowInterval)
}

// tailLines returns the last n lines of body, or all of it when n is not
// positive.
func tailLines(body []byte, n int) []byte {
	if n <= 0 || len(body) == 0 {
		return body
	}
	// The final newline terminates the last line rather than separating it from
	// another, so it is not counted.
	end := len(body)
	if body[end-1] == '\n' {
		end--
	}
	count := 0
	for i := end - 1; i >= 0; i-- {
		if body[i] != '\n' {
			continue
		}
		count++
		if count == n {
			return body[i+1:]
		}
	}
	return body
}

// followLog prints what is appended to path until ctx is cancelled.
//
// It opens and closes the file on every poll instead of holding it, which is
// what carries it through the daemon's log rotation. A follower that keeps the
// handle goes on reading the RENAMED inode — every line after the first
// rotation silently lost, forever — and on Windows it also blocks the rename
// outright, wedging rotation for as long as the follower is attached:
// os.Open goes through syscall.Open, which asks for FILE_SHARE_READ and
// FILE_SHARE_WRITE and NOT FILE_SHARE_DELETE. Opening per poll answers both.
//
// Per-poll opening does not make the two sides miss each other, it only makes
// the window small. A poll's brief handle can still catch a rename, which is
// why a rotator retries; a rename in flight can still catch a poll's open,
// which is why pumpLog treats a retryable open error as "try the next poll"
// rather than as the end of the follow.
//
// Rotation is detected by file IDENTITY rather than by a shrinking size. A
// daemon that rotates and then writes more than the old offset within one poll
// would defeat a size comparison, and os.SameFile is the answer the standard
// library already has.
func followLog(ctx context.Context, w io.Writer, path string, from int64, interval time.Duration) error {
	offset, seen := from, os.FileInfo(nil)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
		var err error
		if offset, seen, err = pumpLog(w, path, offset, seen); err != nil {
			return err
		}
	}
}

// openLogFile and logOpenRetryable are seams, swapped together, so a test can
// drive the follower's open failures without depending on a Windows errno.
// Injecting the errno alone would prove nothing off Windows, where the real
// classifier answers no to everything; injecting the classifier alone leaves no
// way to make the open fail.
var (
	openLogFile      = os.Open
	logOpenRetryable = winerr.Retryable
)

// pumpLog copies whatever is past offset and reports where to resume.
func pumpLog(w io.Writer, path string, offset int64, seen os.FileInfo) (int64, os.FileInfo, error) {
	f, err := openLogFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Rotated away and not recreated yet. The next poll reads the new
			// file from the top.
			return 0, nil, nil
		}
		if logOpenRetryable(err) {
			// Someone else has the file for a moment -- on Windows, the
			// antivirus scanner or the search indexer that meets the rotation.
			// Wait for the next poll rather than ending, and hold the position
			// rather than resetting it: whether the file was replaced is not
			// known yet, and the next open that succeeds settles that by
			// identity, exactly as an uninterrupted poll would.
			//
			// Unbounded, like the branch above it. This loop IS the retry, and
			// a log that has become permanently unreadable is not a case this
			// needs to catch: `daemon logs` reads the whole file before it ever
			// starts following, so a standing permission problem is reported
			// there, before the first poll.
			return offset, seen, nil
		}
		return offset, seen, fmt.Errorf("reading %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return offset, seen, fmt.Errorf("measuring %s: %w", path, err)
	}
	if (seen != nil && !os.SameFile(seen, info)) || info.Size() < offset {
		offset = 0
	}
	if info.Size() > offset {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return offset, info, fmt.Errorf("seeking in %s: %w", path, err)
		}
		n, err := io.Copy(w, f)
		offset += n
		if err != nil {
			return offset, info, fmt.Errorf("printing %s: %w", path, err)
		}
	}
	return offset, info, nil
}
