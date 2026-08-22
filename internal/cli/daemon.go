package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/daemon"
)

// The daemon group is where §9.3's exit taxonomy earns the two codes it added
// to the reference tools' sets, so the mapping is the contract here and the
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
// of §9.3: 3 means the world is already as you asked and 5 is a negative answer
// to a probe. Only `status` and `logs` are probes.
var (
	// singletonHeld is the liveness authority for start, stop and restart. They
	// do not need the published document, and Observe would read it anyway.
	singletonHeld = daemon.SingletonHeld
	spawnDaemon   = daemon.Spawn
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
)

var (
	// daemonPollInterval and daemonWaitTimeout bound every wait in this file.
	//
	// A bounded POLL of the lock, never a fixed sleep: a sleep long enough for a
	// loaded machine is wasted on every idle one, and a sleep short enough to
	// feel quick is a restart whose new daemon races the old one for the
	// singleton and loses. Ten seconds is the outer bound because the thing
	// being waited for is a tick that must finish — §8.4's tick executes a
	// credential swap, and cutting one short is what abandons Claude Code's
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
			err := runDaemon(cmd.Context(), daemon.Options{})
			if errors.Is(err, daemon.ErrSingletonHeld) {
				return WithCode(err, ExitNothingToDo)
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
		switch report.State {
		case daemon.DaemonRunning:
			fmt.Fprintln(cmd.OutOrStdout(), "ccdad daemon: "+describeRunning(report))
		case daemon.DaemonStopped:
			fmt.Fprintln(cmd.OutOrStdout(), "ccdad daemon: not running")
		}
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

// describeRunning is the one-line human form of a live daemon.
func describeRunning(report daemon.Report) string {
	line := "running"
	if report.HasStatus && report.Status.PID != 0 {
		line += fmt.Sprintf(" (pid %d", report.Status.PID)
		if !report.Status.StartedAt.IsZero() {
			line += ", up " + humanDuration(timeNow().Sub(report.Status.StartedAt))
		}
		line += ")"
	}
	return line
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

// startDaemon spawns a detached daemon and waits for it to take the singleton.
//
// Waiting is not politeness. Spawn returns as soon as the process is started,
// and a `daemon start` that returned there would be followed by a `daemon
// status` reporting 5 — the daemon is real, it just has not reached its first
// lock yet. The wait is what makes the two commands compose.
func startDaemon(cmd *cobra.Command) error {
	if err := spawnDaemon(); err != nil {
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
// what carries it through §8.4's rotation. A follower that keeps the handle
// goes on reading the RENAMED inode — every line after the first rotation
// silently lost, forever — and on Windows a handle opened without
// FILE_SHARE_DELETE blocks the rename outright, wedging rotation for as long as
// the follower is attached. Go's os.Open does pass share-delete, so only the
// first of those is live here; the fix for it happens to be the fix for both.
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

// pumpLog copies whatever is past offset and reports where to resume.
func pumpLog(w io.Writer, path string, offset int64, seen os.FileInfo) (int64, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Rotated away and not recreated yet. The next poll reads the new
			// file from the top.
			return 0, nil, nil
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
