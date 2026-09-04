package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/Kweiza/ccdaddy/internal/buildinfo"
	"github.com/Kweiza/ccdaddy/internal/cclink"
	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/ccver"
	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/config"
	"github.com/Kweiza/ccdaddy/internal/credhome"
	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/history"
	"github.com/Kweiza/ccdaddy/internal/identity"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/release"
	"github.com/Kweiza/ccdaddy/internal/store"
	"github.com/Kweiza/ccdaddy/internal/strategy"
	"github.com/Kweiza/ccdaddy/internal/switcher"
	"github.com/Kweiza/ccdaddy/internal/theme"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// Everything ccdad does is reverse-engineered against exactly one pinned Claude
// Code, and the risk register rates "Claude Code changes these internals
// between releases" High. doctor is the mitigation: the only thing that tells
// a user their Claude Code moved out from under ccdad BEFORE a switch destroys
// a credential.
//
// Two rules shape the whole file.
//
// The probe must not create what it probes. Bringing the daemon lock file into
// existence while checking for it destroys the one piece of genuine evidence
// that no daemon ever started here — and the same argument reaches the store
// directory, which is why nothing here calls store.Open: store.Open does an
// MkdirAll, so a diagnostic built on it would manufacture the very thing it was
// asked to report on. Every path below is stat-ed, and every reader used is one
// that returns "absent" rather than creating.
//
// And it reports; it does not repair. Whether doctor should attempt repairs or
// only report is still undecided, so there is deliberately no `--fix` and no
// code behind one. A repair added later has to be an explicit act by the user.
//
// The two checks this file once listed as absent — the leaked `ccdad run`
// session directories, and the stale `Claude Code-credentials` keychain item —
// are both here now, as checkSessions and checkKeychain. Neither changed the
// report-only rule: checkKeychain finds the item and prints the `security`
// command that removes it, rather than removing it — and says what running it
// costs, which is the half that was missing.
//
// checkClaudeVersion is the newest row and the one that most nearly broke the
// first rule. Its subject is the installed Claude Code, whose obvious probe is
// `claude --version` — and the native launcher resolves, and can UPDATE itself,
// when invoked, so that probe changes what it measures. internal/ccver reads the
// same answer out of the install layout instead, with a readlink and a
// package.json, and the rest of this file consumes it: the keychain row's remedy
// inverts across 2.1.113 and used to make the user decide which side they were
// on.

// checkLevel is how much a finding matters.
type checkLevel string

const (
	// levelOK is nothing to do.
	levelOK checkLevel = "ok"
	// levelWarn is worth knowing and not broken. An unrecognised credential key
	// is preserved by the deny-list; a fresh machine has no store yet.
	levelWarn checkLevel = "warn"
	// levelFail is something that will bite. It is the only level that changes
	// the exit code.
	levelFail checkLevel = "fail"
	// levelSkipped is a check that does not apply here — file modes on Windows,
	// where chmod is a no-op, or a check whose subject is missing.
	levelSkipped checkLevel = "skipped"
)

// levelRole is the verdict column's colour, and it is deliberately not four
// shades of one hue: ok, warn, fail and skipped are four different answers to
// "do I have to do something about this", and a reader picking the fail rows
// out of twenty-two should not have to judge saturation to do it. The word
// stays in the cell whatever the colour is, so a level that lost its colour to
// a 16-colour terminal or to NO_COLOR is still the word "fail" -- which is what
// keeps colour from being the only thing carrying the verdict.
//
// skipped is muted rather than left plain. "This check does not apply here" is
// an absence, not a pass, and it is a level real checks emit -- the Windows
// file-modes row, the non-macOS keychain row, every check whose subject is
// missing. Painting it like ok would tell a user their store was checked and
// fine when there is no store.
//
// checkLevel is a string, so a check literal can carry a value that is none of
// the four constants -- the literals in this file are positional, and a typo in
// the second field compiles. That value takes RoleDefault, which carries no
// colour at all: an unrecognised verdict must go out plain rather than
// silently inheriting fail's red and reporting a problem nobody found.
func levelRole(l checkLevel) theme.Role {
	switch l {
	case levelOK:
		return theme.RoleGaugeOK
	case levelWarn:
		return theme.RoleGaugeWarn
	case levelFail:
		return theme.RoleExhausted
	case levelSkipped:
		return theme.RoleMuted
	}
	return theme.RoleDefault
}

type check struct {
	Name   string
	Level  checkLevel
	Detail string
}

func newDoctorCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the layout ccdad depends on, and the hazards around it",
		Long: "doctor reports; it never repairs, and it never creates anything it is\n" +
			"checking for. Exit 0 when nothing failed, 1 when something did — a\n" +
			"warning is not a failure.",
		Args:          usageArgs(cobra.NoArgs),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := runChecks()
			failed := false
			for _, c := range checks {
				if c.Level == levelFail {
					failed = true
				}
			}

			if asJSON {
				rows := make([]map[string]any, 0, len(checks))
				for _, c := range checks {
					rows = append(rows, map[string]any{
						"name": c.Name, "level": string(c.Level), "detail": c.Detail,
					})
				}
				if err := writeJSON(cmd, map[string]any{
					"schemaVersion": 1,
					"ok":            !failed,
					"checks":        rows,
				}); err != nil {
					return err
				}
			} else {
				// Inside the else, not above the if: the --json document goes
				// straight to cmd.OutOrStdout() and never through the colour
				// writer, which is what keeps a machine-readable report free of
				// anything a forced profile would add to it.
				out, pal := renderTarget(cmd)
				rows := make([][]string, 0, len(checks))
				for _, c := range checks {
					rows = append(rows, []string{string(c.Level), c.Name, c.Detail})
				}
				// No header, exactly as before: this table's first column is the
				// verdict and a reader does not need to be told that the word
				// "fail" is a level.
				if err := columns(out, nil, rows, func(row, col int) lipgloss.Style {
					if col != 0 || row < 0 || row >= len(checks) {
						return lipgloss.NewStyle()
					}
					return pal.Style(levelRole(checks[row].Level))
				}); err != nil {
					return err
				}
			}
			if failed {
				// The command has already said what is wrong, in its own words
				// and in the format the caller asked for.
				return WithCode(errSilent, ExitFailure)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a machine-readable object on stdout")
	return cmd
}

// runChecks is the whole diagnostic, in a fixed order so two runs are diffable.
func runChecks() []check {
	root, rootErr := ccpath.StoreHome()
	if rootErr != nil {
		// Almost every other check derives its path from this one, so there is
		// nothing further to report and reporting each of them as "skipped"
		// would bury the single fact that explains all of them. The three that
		// do NOT derive from it — path, api-key and environment — are suppressed
		// here on the same argument rather than a different one: StoreHome fails
		// when no home directory can be resolved at all, which is the same
		// condition that leaves a binary off PATH and Claude Code's own config
		// unlocatable, so their answers would be three more symptoms above the
		// one cause. The fixed order the rest of this function keeps is a
		// property of a run that got that far.
		return []check{{"store", levelFail, rootErr.Error()}}
	}
	storeCheck, storeUsable := checkStore(root)
	// The account list is probed once and its verdict passed to the three rows
	// that take a set difference against it, rather than each of them deciding
	// for itself. Three independent decisions about one document is three
	// chances for them to disagree inside a single report.
	accountsCheck, accountsUsable := checkAccountsFile(root, storeUsable)

	live, liveSource, liveErr := loadLiveWithSource()
	// Read, never created: LoadGlobalConfig opens the file read-only and a
	// missing one resolves to an empty config. This file's rule is that the
	// probe must not CREATE what it probes, which is why store.Open is refused
	// three lines down and this is not.
	cfg, cfgErr := cclink.LoadGlobalConfig()
	report, probeErr := observeDaemon()
	// Probed once and passed to both consumers rather than probed twice. Two
	// calls could disagree — an update can land between them — and a report
	// whose keychain remedy contradicts its own version row is worse than
	// either answer alone. It creates nothing: stat, readlink and a read of a
	// package.json.
	install, installErr := probeClaudeInstall()
	// Read ONCE and handed to the two rows that have an opinion about it. Two
	// Loads could disagree -- a `ccdad config set` can land between them -- and
	// a report whose release row contradicts its own config row is worse than
	// either answer alone. It creates nothing: a missing file resolves to the
	// defaults without writing one, which is this file's first rule.
	settings, settingsErr := config.Load()
	// A file that could not be read leaves settings at its ZERO value, and a
	// zero bool is not the same answer as a key set to false. The config row is
	// where an unusable file is reported; every other row that reads a setting
	// falls back to the documented defaults, which is what the engine itself
	// runs on in that state.
	effective := settings
	if settingsErr != nil {
		effective = config.Defaults()
	}

	return []check{
		storeCheck,
		checkPath(),
		checkPermissions(root, storeUsable),
		checkLocks(report, probeErr),
		checkPidfile(storeUsable),
		checkStatusFile(report),
		checkTickHealth(report),
		checkUsageCache(storeUsable),
		checkHistory(storeUsable),
		checkEngineState(storeUsable),
		checkConfig(storeUsable, settings, settingsErr),
		checkManual(storeUsable, settings, settingsErr),
		checkUpdateCheck(report, effective),
		checkSessions(root, storeUsable),
		accountsCheck,
		checkProfiles(root, storeUsable, accountsUsable),
		checkPrimary(root, storeUsable, accountsUsable),
		checkCredentialFiles(root, storeUsable, accountsUsable),
		checkCodexRelogin(root, storeUsable, accountsUsable),
		checkCodexProxy(root, storeUsable, accountsUsable, report),
		checkCredentialHome(report),
		checkClaudeVersion(install, installErr),
		checkClaudeCode(live, liveErr),
		checkCredentialKeys(live, liveErr),
		checkKeychain(liveErr),
		checkEnvironment(),
		checkAPIKey(cfg, cfgErr),
		checkOAuthSource(live, liveErr, liveSource),
		checkMCPTools(),
	}
}

// checkMCPTools names which spelling this machine's ccdad MCP tools have.
//
// There are two, and they are not interchangeable: a server registered by
// `ccdad mcp install` produces mcp__ccdad__<tool>, and the same server reached
// through the plugin produces mcp__plugin_ccdad_ccdad__<tool>. A permission
// rule, hook matcher or allowed-tools entry written for one silently never
// fires under the other, so which one a machine has is worth a row.
//
// CLAUDE_PLUGIN_ROOT is the cheapest discriminator there is: Claude Code sets
// it for a plugin-launched server and for nothing else. It is unset in an
// ordinary shell, which is why the row falls back to the plugin registry there
// -- and it is SET when this row is produced by `ccdad doctor` running as an
// MCP tool, which is the case where the answer is being read by the thing
// calling the tools.
//
// It is always ok and never a warning. Saying that BOTH registrations exist
// would mean reading Claude Code's own config as well, and a second reader of
// that document is how two readers come to disagree about it. The collision is
// warned about where it is created, by the command that creates it.
func checkMCPTools() check {
	if root := os.Getenv("CLAUDE_PLUGIN_ROOT"); root != "" {
		return check{"mcp-tools", levelOK,
			"this ccdad was launched by the plugin, so its tools are named mcp__plugin_ccdad_ccdad__*"}
	}
	installs := installedCcdadPlugins()
	if len(installs) == 0 {
		return check{"mcp-tools", levelOK,
			"no ccdad plugin is installed; a server registered by `ccdad mcp install` has tools named mcp__ccdad__*"}
	}
	keys := make([]string, 0, len(installs))
	for _, p := range installs {
		keys = append(keys, p.Key)
	}
	return check{"mcp-tools", levelOK, fmt.Sprintf(
		"the ccdad plugin is installed (%s), so its tools are named mcp__plugin_ccdad_ccdad__* — "+
			"`ccdad mcp install` would replace it and rename them to mcp__ccdad__*",
		strings.Join(keys, ", "))}
}

// checkStore reports where ccdad's own state lives, and whether it is there.
//
// A missing store is a WARNING, not a failure, and the wording carries both
// readings on purpose. The daemon probe makes the same point one layer down: a
// fresh install and a mistyped CCDAD_HOME are indistinguishable at this layer —
// both are an *fs.PathError satisfying os.ErrNotExist — and making a new
// machine look broken is the worse of the two mistakes.
//
// A RELATIVE store is a failure, because store.Open refuses one outright: it
// would put a credentials tree in whatever directory ccdad happened to be run
// from, a different one each time, with live tokens in it.
func checkStore(root string) (check, bool) {
	if !filepath.IsAbs(root) {
		return check{"store", levelFail, fmt.Sprintf(
			"the store resolved to the relative path %q; set CCDAD_HOME to an absolute path", root)}, false
	}
	info, err := os.Stat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return check{"store", levelWarn, fmt.Sprintf(
			"%s does not exist — either ccdad has not run on this machine yet, or CCDAD_HOME points somewhere unintended", root)}, false
	case err != nil:
		return check{"store", levelFail, fmt.Sprintf("%s cannot be read: %v", root, err)}, false
	case !info.IsDir():
		return check{"store", levelFail, fmt.Sprintf("%s is not a directory", root)}, false
	}
	return check{"store", levelOK, root}, true
}

// checkPermissions holds the store to 0700 and everything in it to 0600.
//
// Windows gets no chmod and the ACL inherited from %USERPROFILE% is what
// protects the files there, so this is skipped rather than guessed at.
func checkPermissions(root string, usable bool) check {
	if runtime.GOOS == "windows" {
		return check{"permissions", levelSkipped, "file modes are not how Windows protects these; the %USERPROFILE% ACL is"}
	}
	if !usable {
		return check{"permissions", levelSkipped, "there is no store to check"}
	}

	var loose []string
	want := map[string]fs.FileMode{
		root:                         0o700,
		store.CredentialsDirAt(root): 0o700,
	}
	for path, mode := range want {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			loose = append(loose, fmt.Sprintf("%s cannot be read (%v)", path, err))
			continue
		}
		if info.Mode().Perm() != mode {
			loose = append(loose, fmt.Sprintf("%s is %04o, want %04o", path, info.Mode().Perm(), mode))
		}
	}

	// Every file that holds a token, by glob rather than through the store: the
	// point is to see what is on disk, not what ccdad believes it wrote.
	//
	// The two names are store's, and store says them itself now: the accessors
	// resolve nothing and open nothing, which is the property that had kept
	// them out of here. What stays local is the ENUMERATION -- store is asked
	// where to look and never what is there, because this check's whole subject
	// is a file on disk the store would not have written, and a listing that
	// came from the store could not contain one. If the names ever move,
	// TestDoctorReportsALooseCredentialFile fails on its own hand-spelled glob
	// rather than passing while checking nothing.
	files, _ := filepath.Glob(filepath.Join(store.CredentialsDirAt(root), "*"))
	files = append(files, store.AccountsFileAt(root))
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode().Perm() != 0o600 {
			loose = append(loose, fmt.Sprintf("%s is %04o, want 0600", path, info.Mode().Perm()))
		}
	}

	if len(loose) > 0 {
		sort.Strings(loose)
		return check{"permissions", levelFail, strings.Join(loose, "; ")}
	}
	// Named rather than "everything in it": this reads one level deep and skips
	// directories, so the per-session and per-profile homes `ccdad run` creates
	// are not covered here. checkSessions covers those.
	return check{"permissions", levelOK, "the store's own files and directories are 0700/0600"}
}

// checkLocks is the highest-value check here, and the easiest to get wrong.
//
// The question is not "is a daemon running". It is "do locks work on this
// filesystem at all" — ENOLCK on an NFS or CIFS mount with no lock daemon — and
// answering that with "no daemon" is precisely the `status || spawn` respawn
// loop the exit contract introduced exit 5 to prevent. A doctor that made that
// mistake would have reproduced clauth's bug inside the tool written to find
// it.
func checkLocks(report daemon.Report, probeErr error) check {
	if probeErr != nil {
		if errors.Is(probeErr, daemon.ErrLocksUnsupported) {
			return check{"locks", levelFail, fmt.Sprintf(
				"this filesystem cannot take a lock, so nothing here can tell whether a daemon is running: %v. "+
					"An NFS or CIFS mount with no lock daemon does this; move CCDAD_HOME onto local storage", probeErr)}
		}
		return check{"locks", levelFail, fmt.Sprintf("the singleton lock could not be probed: %v", probeErr)}
	}
	if report.State == daemon.DaemonRunning {
		return check{"locks", levelOK, "locking works here, and a daemon holds the singleton"}
	}
	return check{"locks", levelOK, "locking works here, and the singleton is free"}
}

// checkPidfile surfaces the one pidfile state the reader refuses to fold into
// "nothing to read": a body that IS committed and does not parse. pidfile.go
// names this command as the reader that needs to see it, because a supervisor
// that cannot tell "no daemon" from "this store is damaged" respawns forever.
func checkPidfile(usable bool) check {
	if !usable {
		return check{"pidfile", levelSkipped, "there is no store to check"}
	}
	pid, ok, err := daemon.ReadPID()
	switch {
	case err != nil:
		return check{"pidfile", levelFail, fmt.Sprintf("%s is damaged: %v", namePath(daemon.PIDPath()), err)}
	case !ok:
		return check{"pidfile", levelOK, "no pid recorded, which is what a stopped or never-started daemon leaves"}
	}
	// Deliberately not checked for liveness. The process may have died and the
	// number may have been recycled onto something unrelated; only the singleton
	// knows, and it was asked above.
	return check{"pidfile", levelOK, fmt.Sprintf("records pid %d (a recorded pid is never liveness evidence)", pid)}
}

func checkStatusFile(report daemon.Report) check {
	if report.StatusErr != nil {
		return check{"status-file", levelFail, fmt.Sprintf("%s cannot be read: %v", namePath(daemon.StatusPath()), report.StatusErr)}
	}
	if !report.HasStatus {
		return check{"status-file", levelOK, "no daemon has published one"}
	}
	// In the READER's zone, not the document's. The two agree for a document
	// this binary's daemon wrote, and they do not on the day of an upgrade: the
	// old daemon keeps running and keeps publishing until something stops it,
	// and what it publishes carries whatever zone each writer happened to hand
	// it. A line that printed 17:41Z under a KST clock is how this bug was
	// diagnosed as a nine-hour poll stall that was not happening.
	return check{"status-file", levelOK, fmt.Sprintf("schema %d, generated %s",
		report.Status.SchemaVersion, report.Status.GeneratedAt.In(readerZone()).Format("2006-01-02T15:04:05Z07:00"))}
}

// checkTickHealth is the difference between a daemon that is RUNNING and a
// daemon that is WORKING, and it exists because for three hours and twenty
// minutes there was nothing here that could tell them apart.
//
// The machine that asked for it had a daemon failing every tick -- 11,300 in a
// row, one a second, every one of them a switch that did not happen -- and
// `ccdad doctor` printed ok on every row it had, truthfully: the singleton WAS
// held, the pidfile DID name the process, the status document WAS fresh,
// because a failing tick still publishes. Every existing row asked about
// liveness. None asked whether anything was getting done.
//
// A daemon that has published nothing is SKIPPED and not ok, for the reason
// checkPidfile gives about liveness evidence: absent fields are also what a
// daemon too old to write them leaves, and reporting silence as health is the
// exact mistake this row was added to stop making.
func checkTickHealth(report daemon.Report) check {
	if report.State != daemon.DaemonRunning {
		return check{"tick-health", levelSkipped, "no daemon is running, so there is no tick loop to report on"}
	}
	if !report.HasStatus {
		return check{"tick-health", levelSkipped, "the running daemon has not published a status document yet"}
	}
	s := report.Status
	if !s.TickHealthReported {
		// Absence is not health. See Status.TickHealthReported: a daemon that
		// predates these fields publishes exactly what a healthy one does.
		return check{"tick-health", levelSkipped,
			"the running daemon does not report tick health, so this cannot say whether it is switching; " +
				"restart it with 'ccdad daemon restart' to find out"}
	}
	if s.TickFailures == 0 {
		return check{"tick-health", levelOK, "the daemon's last tick completed, so switching is not blocked"}
	}
	// The AGE of the run, not just its length. "A tick just failed" and
	// "nothing has worked since lunch" are the same count at 1 Hz for very
	// different lengths of time, and only the second one means the daemon has
	// stopped doing its job. In the reader's zone, for the reason
	// checkStatusFile gives.
	since := s.TickFailingSince.In(readerZone()).Format("2006-01-02T15:04:05Z07:00")
	detail := fmt.Sprintf("the tick loop has failed %d times in a row since %s, "+
		"so no switch has happened in that time: %s", s.TickFailures, since, s.LastTickError)
	if s.TickFailingSince.IsZero() {
		detail = fmt.Sprintf("the tick loop has failed %d times in a row, "+
			"so no switch has happened in that time: %s", s.TickFailures, s.LastTickError)
	}
	return check{"tick-health", levelFail, detail}
}

// checkUsageCache is a warning rather than a failure by the cache's own design:
// an unreadable cache leaves every account UNKNOWN, which the engine already
// knows how to handle. It is invisible everywhere else, which is why LoadError
// exists at all.
func checkUsageCache(usable bool) check {
	if !usable {
		return check{"usage-cache", levelSkipped, "there is no store to check"}
	}
	c, err := usage.LoadCache()
	if err != nil {
		return check{"usage-cache", levelFail, err.Error()}
	}
	if cerr := c.LoadError(); cerr != nil {
		return check{"usage-cache", levelWarn, fmt.Sprintf(
			"%v — every account will read as unknown until it is rewritten", cerr)}
	}
	return check{"usage-cache", levelOK, namePath(usage.CachePath())}
}

// checkHistory is the only place a damaged series is ever mentioned. Every
// other surface renders an unparseable history exactly as it renders a machine
// that has not polled yet — no measured rate — so without this row a file that
// has silently stopped accumulating looks identical to a fresh install.
//
// A MISSING file is levelOK and not a warning. history.json appears only once a
// daemon has recorded a poll, so warning on its absence would warn on every
// install for as long as it takes the first poll to land, and on every machine
// that runs ccdad without the daemon at all.
//
// history.LoadHistory is used rather than anything that opens the store,
// because it is read-only on every path: an absent file is an empty series and
// no error, so the probe cannot bring into existence the evidence it is being
// asked to report on.
//
// A parse failure is a warning and a read failure is a failure, and that second
// half is a DEPARTURE from the row above rather than an echo of it.
// checkUsageCache cannot reach levelFail on a damaged file at all, because
// usage.LoadCache treats bytes it cannot read exactly like bytes it cannot
// parse — unknown, not fatal — since the next poll rewrites the cache either
// way, so both conditions arrive here through LoadError as a warning.
// history.LoadHistory returns its read failures instead, and this row passes
// them on at levelFail, because nothing rewrites an unreadable series:
// WithHistory reloads before it writes and returns that same error rather than
// saving, so the file stops accumulating until a human moves it aside. Two rows
// differing on one breakage looks like an oversight, which is why the
// difference is written down here and pinned by a test.
func checkHistory(usable bool) check {
	if !usable {
		return check{"history", levelSkipped, "there is no store to check"}
	}
	h, err := history.LoadHistory()
	if err != nil {
		return check{"history", levelFail, err.Error()}
	}
	if herr := h.LoadError(); herr != nil {
		return check{"history", levelWarn, fmt.Sprintf(
			"%v — no burn rate can be measured until it is rewritten", herr)}
	}
	return check{"history", levelOK, namePath(history.Path())}
}

// checkSessions reports the per-session credential directories `ccdad run`
// creates under the store.
//
// Each one holds a live refresh token at 0600. `run` deletes its own on the way
// out, but a machine powered off mid-session, a SIGKILL, or a run whose
// adopt-back failed all leave one — and nothing else in the tree would ever
// mention it again.
//
// It cannot tell a leftover from a session running RIGHT NOW, and does not
// pretend to: the tree's doctrine is that a recorded pid is not liveness
// evidence, and there is nothing else to read. So the text says both readings
// and the level stays a warning; deciding which one it is belongs to the human
// who knows whether they have a session open.
//
// Report-only, like every other check here, and for the reason the top of this
// file gives: there is no --fix, and deleting a directory that turns out to
// belong to a live session would take the credentials out from under it.
func checkSessions(root string, usable bool) check {
	if !usable {
		return check{"sessions", levelSkipped, "there is no store to check"}
	}
	container := filepath.Join(root, SessionsDirName)
	entries, err := os.ReadDir(container)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The container is created by `run`, never by this probe.
			return check{"sessions", levelOK, "no parallel-session credentials are left behind"}
		}
		return check{"sessions", levelFail, err.Error()}
	}

	var live []string
	for _, e := range entries {
		// Claude Code's legacy refresh lock is a directory named after the
		// session with ".lock" appended, created BESIDE it. It is not a
		// session, and counting it would double every number here.
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		live = append(live, e.Name())
	}
	if len(live) == 0 {
		return check{"sessions", levelOK, "no parallel-session credentials are left behind"}
	}
	sort.Strings(live)

	// The permissions check cannot see any of this: it reads one level deep and
	// skips directories, so a world-readable token inside a session would be
	// reported as "ok" by the check whose whole job is modes.
	if loose := looseSessions(container, live); len(loose) > 0 {
		return check{"sessions", levelFail, strings.Join(loose, "; ")}
	}
	return check{"sessions", levelWarn, fmt.Sprintf(
		"%d session credential director%s under %s (%s). A running 'ccdad run' owns one each; "+
			"any others hold a refresh token nothing is using, and are safe to delete once no session is open",
		len(live), plural(len(live), "y", "ies"), container, strings.Join(live, ", "))}
}

// looseSessions names every session directory or credential file that anyone
// but the owner can read.
//
// Windows has no mode bits and the inherited profile ACL is what protects
// these, so there is nothing here to answer.
func looseSessions(container string, names []string) []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	var loose []string
	for _, name := range names {
		dir := filepath.Join(container, name)
		want := map[string]fs.FileMode{
			dir: 0o700,
			filepath.Join(dir, ccpath.CredentialsFile): 0o600,
		}
		for path, mode := range want {
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.Mode().Perm() != mode {
				loose = append(loose, fmt.Sprintf("%s is %04o, want %04o", path, info.Mode().Perm(), mode))
			}
		}
	}
	sort.Strings(loose)
	return loose
}

// plural picks a suffix. It exists because "1 directories" in a diagnostic is
// the kind of thing a reader stops trusting the rest of the sentence over.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// checkEngineState is the anti-flap state: cooldown, last switch, quarantines.
// Unreadable, it degrades towards MORE switching rather than less, so it is a
// warning — but a permanently unreadable one means the cooldown never applies
// and nothing else would ever say so.
func checkEngineState(usable bool) check {
	if !usable {
		return check{"engine-state", levelSkipped, "there is no store to check"}
	}
	st, err := strategy.LoadState()
	if err != nil {
		return check{"engine-state", levelFail, err.Error()}
	}
	if serr := st.LoadError(); serr != nil {
		return check{"engine-state", levelWarn, fmt.Sprintf(
			"%v — the cooldown and every quarantine are being ignored until it is rewritten", serr)}
	}
	return check{"engine-state", levelOK, namePath(strategy.StatePath())}
}

// checkConfig answers whether ~/.ccdad/config.toml is doing anything.
//
// This is the one check whose subject fails SILENTLY by design:
// config.Reloader keeps the daemon on the last config that parsed when an edit
// breaks the file, which is right — a daemon that dies on a typo stops
// switching accounts — and it means a user can edit a threshold, see no error
// anywhere, and have nothing take effect. Here is where they find out.
//
// A missing file is OK rather than a warning: the defaults are a complete
// configuration and most machines never need one. An unusable file is a warning
// rather than a failure for the same reason — ccdad goes on working, on numbers
// the user did not choose.
func checkConfig(usable bool, settings config.Config, settingsErr error) check {
	if !usable {
		return check{"config", levelSkipped, "there is no store to check"}
	}
	path, perr := config.Path()
	if perr != nil {
		return check{"config", levelFail, perr.Error()}
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return check{"config", levelOK, fmt.Sprintf(
			"no %s, so the engine runs on the built-in defaults", config.FileName)}
	}
	// The load is the caller's, so this row and the release row cannot disagree
	// about one file.
	if settingsErr != nil {
		return check{"config", levelWarn, fmt.Sprintf(
			"%v — every value in it is being ignored and the engine is running on its defaults", settingsErr)}
	}
	detail := path
	if raw, rerr := os.ReadFile(path); rerr == nil {
		if unknown, uerr := config.UnknownKeys(raw); uerr == nil && len(unknown) > 0 {
			return check{"config", levelWarn, fmt.Sprintf(
				"%s carries keys this ccdad does not know, which are preserved but ignored: %v", path, unknown)}
		}
	}
	if settings.MaxAutoSpend > 0 {
		// Unattended spending is on. The risk register rates it High, and
		// doctor is where a user checks what their machine will do without them.
		detail = fmt.Sprintf("%s — unattended credit spending is armed up to %v", path, settings.MaxAutoSpend)
	}
	return check{"config", levelOK, detail}
}

// checkManual is a row of its own rather than a clause on the config row, and
// the reason is what a person opens `ccdad doctor` for: a fleet that has stopped
// switching. Every other row here would read `ok` in manual mode — the daemon is
// up, the locks work, the ticks complete, the polls land — and the one fact that
// explains the silence would be a fragment of a detail string on a row about a
// file path.
//
// It is `warn` and not `ok`, and never `fail`. It is not a fault: the user asked
// for it, and fail is the only level that changes doctor's exit code, so an
// intentional mode must not turn a health check red. But it is not `ok` either.
// A mode that suppresses the program's whole purpose has to be visible in the
// place people look when something is wrong, and a row printed at `ok` beside
// twenty other `ok` rows is a row nobody reads.
//
// It reports the mode being OFF as skipped rather than ok, which is the same
// call the sessions row makes for a machine with nothing to report: a row that
// says "ok, this mode you have never heard of is not on" spends a line of a
// twenty-line table on nothing.
func checkManual(usable bool, settings config.Config, settingsErr error) check {
	if !usable {
		return check{"manual-mode", levelSkipped, "there is no store to check"}
	}
	if settingsErr != nil {
		// The config row already carries the parse failure and says the engine
		// is on its defaults. Repeating it here would be a second sentence
		// about one file; what this row can still say is what the DEFAULT is.
		return check{"manual-mode", levelSkipped, fmt.Sprintf(
			"%s cannot be used, so the engine is on its defaults and switching automatically", config.FileName)}
	}
	if !settings.Manual {
		return check{"manual-mode", levelSkipped, "off, so ccdad switches accounts on its own"}
	}
	return check{"manual-mode", levelWarn, "on: ccdad is polling and ranking and will NOT move the live login. " +
		"Every other row here reads ok in this mode, which is why it has one of its own. " +
		"'ccdad strategy headroom' hands the wheel back; 'ccdad switch <account>' works either way"}
}

// checkUpdateCheck reports what the daemon's daily release check has found.
//
// It makes NO request. A probe must not create what it probes, and asking the
// origin here would answer a different question from the one this row is about:
// whether the DAEMON is asking, and what it last heard.
//
// It is `warn` at its worst. fail is the only level that changes doctor's exit
// code, and a release landing must not turn every health-check script in the
// world red on the day it ships.
func checkUpdateCheck(report daemon.Report, settings config.Config) check {
	level, detail := updateCheckVerdict(settings.UpdateCheck, report, buildinfo.Version)
	return check{"update-check", level, detail}
}

// updateCheckVerdict is the row's decision, as a pure function of the three
// things it depends on, so the PRECEDENCE below can be asserted directly rather
// than through a store arranged to match one arm at a time.
//
// The arms are ORDERED and a machine can match several at once:
//
//   - switched off outranks everything. A machine that asked not to be told is
//     not one to warn about a release it was never going to look for.
//   - a newer release outranks a failed check. A release seen yesterday is
//     still out today, and today's failure is the less useful of the two facts.
//   - a check taken BEFORE this build outranks the equal case, because the two
//     are different worlds wearing one sentence. The recorded release is a
//     CACHE, so a machine that has just updated holds one taken before the
//     binary now reading it -- and the old arm fired on `latest <= current` and
//     answered "ccdad 0.9.7 is the newest release" out of a 0.9.8 build.
//     Observed minutes after 0.9.8 shipped. A row may not name a version older
//     than the one printing it as the newest thing there is.
//   - a comparison outranks "cannot compare", so a build with no comparable
//     version says so only when there is nothing better to say.
func updateCheckVerdict(enabled bool, report daemon.Report, running string) (checkLevel, string) {
	if !enabled {
		// The command is deliberately NOT gated by this key, and the row says
		// so: a key that silently disabled something a human typed would be a
		// worse surprise than the request it prevents.
		return levelOK, fmt.Sprintf(
			"%s is false in %s, so the daemon never asks whether a release is out; the update command is not gated by it",
			config.KeyUpdateCheck, config.FileName)
	}
	s := report.Status
	if !report.HasStatus || s.UpdateCheckedAt.IsZero() {
		return levelOK, "no daemon has checked for a release yet"
	}

	latest, latestOK := release.ParseTag(s.UpdateLatest)
	current, currentOK := release.ParseTag(running)
	// The reader's zone, for the reason checkStatusFile gives.
	when := s.UpdateCheckedAt.In(readerZone()).Format(time.RFC3339)

	if latestOK && currentOK && latest.Compare(current) > 0 {
		return levelWarn, fmt.Sprintf("ccdad %s is out and this is %s (checked %s) — %s",
			latest, current, when, release.BaseURL()+"/latest")
	}
	due := "the next tick"
	if !s.NextUpdateCheckAt.IsZero() {
		due = s.NextUpdateCheckAt.In(readerZone()).Format(time.RFC3339)
	}
	if s.UpdateCheckError != "" {
		return levelWarn, fmt.Sprintf("the last release check failed: %s (the next is due %s)", s.UpdateCheckError, due)
	}
	if latestOK && currentOK && latest.Compare(current) < 0 {
		// The check is a CACHE and this build is ahead of it, which is the
		// ordinary state of every machine for the first day after it updates.
		// It is not a fault -- fail is the only level that moves doctor's exit
		// code, and being newer than the last check is not something to go red
		// about -- but it is also not evidence that nothing newer exists, and
		// the arm below used to claim exactly that in the recorded release's
		// name.
		return levelOK, fmt.Sprintf(
			"the last release check predates this build: it saw %s and this is %s (checked %s), so it says "+
				"nothing about whether anything newer is out; the next is due %s",
			latest, current, when, due)
	}
	if latestOK && currentOK {
		return levelOK, fmt.Sprintf("ccdad %s is the newest release (checked %s)", latest, when)
	}
	return levelOK, fmt.Sprintf(
		"the newest release cannot be compared against this build's version %q (checked %s)", running, when)
}

// checkCredentialHome answers the question no other check can: is something
// ELSE driving the Claude Code login this ccdad manages.
//
// The daemon singleton is keyed on the STORE, so two shells with different
// CCDAD_HOME values each take their own and each write the same
// .credentials.json. cclock serialises the writes, so nothing is corrupt and
// nothing errors — the two engines simply undo each other's switches, and until
// this check existed there was no line anywhere in the tree that said so.
//
// It reports, like everything else here, and it creates nothing: credhome.Probe
// takes a SHARED try-lock with O_CREATE removed, so a machine where no engine
// has ever run still has no lock file after `ccdad doctor`.
//
// It asserts no file modes. checkPermissions skips them on Windows because the
// inherited profile ACL is what protects these files there, and a second check
// contradicting that in the same output is worse than a gap.
func checkCredentialHome(report daemon.Report) check {
	claim, err := credentialHomeClaim()
	switch {
	case errors.Is(err, credhome.ErrLocksUnsupported):
		// The same condition checkLocks names, on the OTHER filesystem. A local
		// store with a network ~/.claude reaches this one with the singleton
		// already taken, so the two are genuinely independent answers and the
		// wording has to say which filesystem is meant.
		return check{"credential-home", levelFail, fmt.Sprintf(
			"the filesystem holding Claude Code's credential home cannot take a lock, so ccdad cannot tell "+
				"whether a second store is driving this login: %v. An NFS or CIFS mount with no lock daemon "+
				"does this; the engine keeps running, but unguarded", err)}
	case err != nil:
		return check{"credential-home", levelFail, fmt.Sprintf(
			"the credential-home claim could not be probed: %v", err)}
	}

	detail := claim.Home
	if drift := credentialHomeDrift(report, claim.Home); drift != "" {
		// Inside a `ccdad run` session the two homes differ BY DESIGN, and the
		// sentence drift returns there says exactly that. A warning would land
		// on every doctor run inside every session, which is how a reader
		// learns to skip a row, and it would contradict its own detail. levelOK
		// with a detail worth reading is the judgement checkPrimary already
		// makes about a deliberate setting.
		level := levelWarn
		if _, inSession := currentScopedSession(); inSession {
			level = levelOK
		}
		return check{"credential-home", level, drift}
	}
	switch {
	case !claim.Held:
		return check{"credential-home", levelOK, detail + " — no ccdad engine is driving it"}
	case claim.Ours:
		return check{"credential-home", levelOK, fmt.Sprintf(
			"%s — driven by this store's engine (pid %d)", detail, claim.Owner.PID)}
	case !claim.Named:
		// Held is a kernel fact and the name is not, so this is a warning about
		// the NAME rather than about the claim. It is the transient state of an
		// engine that has taken the claim and not yet written its document.
		return check{"credential-home", levelWarn, fmt.Sprintf(
			"%s is held by an engine that has not named itself (%v); if it persists, something is "+
				"holding the claim and cannot write beside it", detail, claim.OwnerErr)}
	}
	return check{"credential-home", levelWarn, fmt.Sprintf(
		"%s is driven by the ccdad store at %s (pid %d), not by this one. Two stores on one login undo "+
			"each other's switches; point CLAUDE_CONFIG_DIR at a directory of this store's own, or stop "+
			"that engine", detail, claim.Owner.Store, claim.Owner.PID)}
}

// credentialHomeDrift catches a running daemon that is managing a DIFFERENT
// credential home from the one this shell resolves, and returns the sentence
// for it or "".
//
// A credential home the USER pointed somewhere is how this happens without
// anybody doing anything wrong. The two variables reach it by different routes
// and only one of them is unattended, which is worth keeping straight:
//
//   - CLAUDE_CONFIG_DIR on its own is NOT a reason to refuse an auto-start, and
//     rule 3 says why in as many words: it is where a great many people keep
//     their Claude Code configuration, and refusing there would turn the
//     feature off for all of them. So `ccdad status` in such a shell can spawn a
//     daemon that pins that home for life, with nobody having typed anything
//     about daemons.
//   - CLAUDE_SECURESTORAGE_CONFIG_DIR does not auto-start at all: rule 3
//     refuses whenever it is DEFINED. Reaching this state through it takes a
//     human typing `ccdad daemon start`, which is allowed outside a session
//     because a shell the user scoped themselves has told ccdad where its live
//     login is.
//
// Either way a later shell without the override resolves a different home,
// every file on the machine looks normal afterwards, and the daemon's own
// published document is the only place the two can be compared. This row cannot
// tell the two routes apart — it compares two resolved paths — which is why it
// names both.
//
// `ccdad run --full-profile` USED to reach here the same way and no longer
// does: auto-start's rule 3 gained a containment test at 3d9d2d6, and
// scopedSessionRefusals covers the verbs that START a daemon (`daemon start`,
// `daemon restart`, and the hidden `__daemon`) — the read-only daemon verbs are
// allowed in a session and always were. Do not read the prevented cause as
// evidence that this check is dead: the routes above have nothing preventing
// them, and this row is the only place they are ever named.
func credentialHomeDrift(report daemon.Report, resolved string) string {
	if report.State != daemon.DaemonRunning || !report.HasStatus {
		return ""
	}
	recorded := report.Status.CredentialHome
	// credhome.SamePath, never ==. ccdad manufactures the two spellings itself:
	// daemon.ChildEnv pins an absolute, symlink-resolved CLAUDE_CONFIG_DIR into
	// every daemon it spawns, while ccpath hands this shell's own spelling back
	// untouched. A trailing slash is enough — filepath.Abs cleans it for the
	// child and nothing cleans it here — and a string compare would then print
	// this warning on every doctor run forever, telling the user to restart a
	// daemon that is driving exactly the right directory.
	if recorded == "" || credhome.SamePath(recorded, resolved) {
		return ""
	}
	// Inside a `ccdad run` session the two homes differ BY DESIGN and the
	// daemon is not the side that moved -- this shell is. Prescribing a restart
	// there is wrong twice: it names the wrong side as the fault, and
	// scopedSessionRefusals refuses `ccdad daemon restart` in this very shell,
	// so the instruction cannot be carried out either. Every other credential
	// row in this report already carries scopedSessionNote for that reason, and
	// this one -- the only row that hands out an instruction -- did not.
	if note := scopedSessionNote(); note != "" {
		return fmt.Sprintf(
			"the running daemon is driving %s and this shell resolves %s%s. The daemon is serving the "+
				"machine's login and this shell deliberately is not, so nothing here is broken; read "+
				"this row from an ordinary shell", recorded, resolved, note)
	}
	return fmt.Sprintf(
		"the running daemon is driving %s, but this shell resolves %s — so its switches change a login "+
			"nothing here reads. A daemon started from a shell that resolved a different credential home "+
			"does this. CLAUDE_SECURESTORAGE_CONFIG_DIR names that home when it is set to a directory, "+
			"CLAUDE_CONFIG_DIR names it when that one is unset, and an EMPTY "+
			"CLAUDE_SECURESTORAGE_CONFIG_DIR sends Claude Code to ~/.claude rather than to either; "+
			"restart the daemon from the shell whose configuration you want it to serve",
		recorded, resolved)
}

// probeClaudeInstall is doctor's window onto which Claude Code is installed. It
// is a var for the same reason keychainProbe is one: the branch that matters —
// a machine still on the keychain era — cannot be reached from a development
// machine running a current release, and would ship having never been rendered.
var probeClaudeInstall = ccver.Probe

// checkClaudeVersion names the Claude Code this machine would run.
//
// It is a row of its own rather than a sentence inside checkClaudeCode, and the
// split is deliberate: "which Claude Code is installed" and "can ccdad read
// Claude Code's credentials file" are two probes with two independent failure
// modes — a launcher ccdad cannot classify says nothing about the credentials
// file, and an unreadable credentials file says nothing about the version.
// runChecks keeps one fact per row so two runs diff cleanly.
//
// The version is read, never asked for. ccver's header carries why: `claude
// --version` would spawn a ~300 MB process that resolves and can UPDATE itself
// on invocation, which is the one thing this file's opening rule forbids.
func checkClaudeVersion(install ccver.Install, err error) check {
	if errors.Is(err, ccver.ErrNoClaudeCode) {
		// Not a failure. ccdad manages logins for a Claude Code that may be
		// installed later, or installed somewhere this does not look, and a
		// machine being set up in either order must not read as broken — the
		// same judgement checkStore makes about a store that is not there yet.
		return check{"claude-version", levelWarn, "no claude launcher on PATH, or in the two places the native " +
			"and local installers write one, so ccdad cannot tell which Claude Code this machine runs"}
	}
	if err != nil {
		return check{"claude-version", levelWarn, fmt.Sprintf("ccdad could not look for a claude launcher: %v", err)}
	}
	if !install.Known {
		// A probe that could not answer is NOT an answer, which is the rule
		// checkKeychain states one check down. The keychain row below reads
		// this same result and keeps handing the user both remedies precisely
		// because of this branch.
		return check{"claude-version", levelWarn, fmt.Sprintf(
			"ccdad found a claude launcher and cannot name its version: %s", install.Why)}
	}
	if install.PreSecureStorageDir() {
		// The keychain half of this warning is GONE, and its absence is the
		// correction: every release reads the item, so "a stale item shadows
		// every switch" was never a property of old builds in particular. It is
		// how the arrangement works everywhere, and ccdad installs into the item
		// now. What is left is the variable, which really did arrive at 2.1.113.
		return check{"claude-version", levelFail, fmt.Sprintf(
			"%s is on the far side of the boundary ccdad is built on: it does not know "+
				"CLAUDE_SECURESTORAGE_CONFIG_DIR, on any platform, so `ccdad run`'s default scoping is ignored and "+
				"the session would run as the machine's live login. Upgrade Claude Code to %s or later, which added "+
				"the variable. Until then `ccdad run --full-profile` is the one mode that still scopes, because it "+
				"sets CLAUDE_CONFIG_DIR, which this build does read",
			install, ccver.LastPreSecureStorageDir.NextPatch())}
	}
	return check{"claude-version", levelOK, install.String()}
}

// checkClaudeCode is the risk register's actual mitigation: has Claude Code's
// layout moved.
//
// cclink.Load's refusals ARE the drift signals — a symlink at the path, a file
// over the 1 MiB cap, a body that is not JSON — and switch deliberately cannot
// repair the last of those, because overwriting destroys the machine-scoped keys
// still in it. So this is the place a user finds out.
func checkClaudeCode(live cclink.Blob, err error) check {
	// Resolved independently of the store root: CCDAD_HOME can be set while
	// HOME is not, which resolves ccdad's own tree and leaves Claude Code's
	// unlocatable — a real drift state, and one only this check can name.
	home, homeErr := ccpath.CredentialHome()
	if homeErr != nil {
		return check{"claude-code", levelFail, homeErr.Error()}
	}
	path := filepath.Join(home, ccpath.CredentialsFile)
	// Inside a `ccdad run` session this file is the SESSION's, and every
	// sentence below would otherwise describe it as the machine's live login.
	// Measured before this line existed: a user who ran doctor to find out why
	// a switch had done nothing was told that the switch's own file read fine.
	scope := scopedSessionNote()
	if err != nil {
		return check{"claude-code", levelFail, fmt.Sprintf("%s: %v%s", path, err, scope)}
	}
	if len(live) == 0 {
		return check{"claude-code", levelWarn, fmt.Sprintf(
			"no login in %s — Claude Code has not logged in on this machine, or it keeps its credentials elsewhere%s", home, scope)}
	}
	return check{"claude-code", levelOK, fmt.Sprintf("%s reads as %d top-level %s%s",
		path, len(live), plural(len(live), "key", "keys"), scope)}
}

// scopedSessionNote is the clause every credential answer in this report needs
// when ccdad is running inside one of its own `ccdad run` sessions: without
// it, each check describes the session's copy of Claude Code's state as though
// it were the machine's.
//
// It says "session" rather than repeating the whole explanation in each check
// — checkEnvironment carries the long form, and the two are read together.
func scopedSessionNote() string {
	session, ok := currentScopedSession()
	if !ok {
		return ""
	}
	return fmt.Sprintf(" — but this shell is inside a `ccdad run` session (%s), so that is the session's own login and not the machine's live one",
		session.describe())
}

// checkCredentialKeys is the unknown-key probe's "on startup" half, and the
// last part of that probe that was still missing.
//
// Unrecognised keys are a warning, not a failure: the swap is a DENY-list, so
// anything ccdad has never heard of is preserved rather than destroyed. What
// this is telling the user is that ccdad is behind Claude Code — six machine
// keys drifted in after clauth's one-key carry list was written, which is why
// this is demonstrated drift rather than a hypothetical.
func checkCredentialKeys(live cclink.Blob, err error) check {
	if err != nil {
		return check{"credential-keys", levelSkipped, "the credentials file could not be read"}
	}
	unknown := cclink.UnknownKeys(live)
	if len(unknown) == 0 {
		return check{"credential-keys", levelOK, "every top-level key is one ccdad knows"}
	}
	sort.Strings(unknown)
	return check{"credential-keys", levelWarn, fmt.Sprintf(
		"unrecognised top-level keys: %s. They are preserved on a switch, but an ACCOUNT-scoped one would leak between accounts until ccdad is updated",
		strings.Join(unknown, ", "))}
}

// keychainProbe is doctor's window onto the macOS Keychain, and it is a
// variable so the tests can answer for it.
var keychainProbe = cclink.ProbeCredentialKeychainItem

// keychainDetailer is an error that already knows how to explain itself to a
// user. cclink.KeychainError is the one that does; matching on the BEHAVIOUR
// rather than on that concrete type is what lets a test describe a locked
// keychain without cclink having to export the classification it keeps private.
type keychainDetailer interface{ Detail() string }

// checkKeychain reports the credential item on macOS.
//
// IT IS NOT A STALENESS CHECK ANY MORE. It was written as one, on the premise
// that 2.1.113 removed the keychain backend and left the item behind as a
// leftover -- inert on anything installable today, worth deleting only against
// a future downgrade. That premise was measured again and is false: 2.1.234,
// 2.1.238 and 2.1.251 each spawn `security find-generic-password`, and the
// combinator reads the item BEFORE .credentials.json on all of them.
//
// So where an item exists it is the login, and this row's job is to say so.
// The line it replaces -- "nothing is broken right now. Removing it is cleanup"
// -- is how a live credential store got read as a leftover while every switch
// silently did nothing, and it was the most dangerous sentence in the tool.
//
// It no longer takes the install. Which release is on the machine decided the
// remedy while there were two; there is one now, and it is the same on every
// version, so reading the install here would only invite the old split back.
func checkKeychain(liveErr error) check {
	found, err := keychainProbe(context.Background())
	item := found.Item

	if errors.Is(err, cclink.ErrKeychainUnsupported) {
		return check{"keychain", levelSkipped, "there is no macOS Keychain on this platform"}
	}
	if err != nil {
		// A probe that could not answer is NOT an absence. Saying "no item"
		// because a locked keychain refused to be read is the same mistake as
		// reporting "no daemon" for a filesystem where locks do not work, one
		// check up -- and it matters more now: the item is the login, so an
		// unanswered probe leaves it unknown whether a switch can reach Claude
		// Code at all.
		var explained keychainDetailer
		if errors.As(err, &explained) {
			return check{"keychain", levelWarn, fmt.Sprintf(
				"ccdad could not read the %s item, so it cannot tell whether a switch reaches Claude Code: %s",
				item.Service, explained.Detail())}
		}
		return check{"keychain", levelWarn, fmt.Sprintf(
			"ccdad could not read the %s item, so it cannot tell whether a switch reaches Claude Code: %v",
			item.Service, err)}
	}
	if !found.Present {
		return check{"keychain", levelOK, fmt.Sprintf(
			"no item for %q under %s, so .credentials.json is the login Claude Code reads",
			item.Account, keychainNameList(found.Checked))}
	}
	// PRESENT IS NOT READABLE, and this row asserted the strongest sentence in
	// the report on the weaker fact. Present spawns `find-generic-password`
	// WITHOUT -w, on purpose -- an attributes lookup can never raise the "wants
	// to use your keychain" dialog -- so it answers 0 for an item whose secret
	// the keychain is refusing. Measured on a machine whose login keychain had
	// locked: this row was green and said the item "is what every request
	// authenticates with" while every ccdad command was failing with exit 36
	// and Claude Code had been serving from .credentials.json for eight hours.
	//
	// liveErr is the read that already happened, one row up -- no second spawn,
	// so no new chance of a prompt. An item is present, so LoadWithSource
	// reached the keychain half and any error it carries is the item's.
	if liveErr != nil {
		return check{"keychain", levelWarn, fmt.Sprintf(
			"the keychain item %q for %q exists, but its secret could not be READ (%v) -- so it is not what "+
				"anything is authenticating with right now. Claude Code's credential store falls back to "+
				".credentials.json when the item refuses; ccdad does not, so every command that asks who is "+
				"live fails instead and a switch cannot reach the login. On macOS this is a locked login "+
				"keychain: run `security unlock-keychain ~/Library/Keychains/login.keychain-db`, then restart "+
				"the daemon FROM THAT SHELL -- a successor inherits the audit session that is refusing",
			item.Service, item.Account, liveErr)}
	}
	return check{"keychain", levelOK, fmt.Sprintf(
		"the keychain item %q for %q is Claude Code's live login: its credential store reads that item BEFORE "+
			".credentials.json, so it is what every request authenticates with. ccdad installs into it on every "+
			"switch, so a swap reaches Claude Code rather than only the file. Do not delete it -- it is the login, "+
			"and the next credential write would put it back anyway",
		item.Service, item.Account)}
}

// keychainNameList renders the names a probe looked for. It is a list rather
// than one name because a decomposed CLAUDE_CONFIG_DIR splits the item into two
// spellings, and an "ok" that named only one of them would be the same
// half-answer this check exists to stop giving.
func keychainNameList(items []cclink.KeychainItem) string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, strconv.Quote(item.Service))
	}
	return strings.Join(names, " or ")
}

// checkEnvironment names the variables that make a switch pointless.
//
// Each of these outranks the stored login, so with one set a switch writes a
// credentials file nothing reads. The list is every VARIABLE ccdad knows to be
// a displacer -- `displacingAuth` names more than this, because it resolves
// both axes and some of what wins is a file or a setting rather than a
// variable:
//
//   - ANTHROPIC_AUTH_TOKEN, which the bundle carries beside CLAUDE_CODE_OAUTH_TOKEN
//     in the list it attributes a session's credential to ("from
//     ANTHROPIC_AUTH_TOKEN", "from CLAUDE_CODE_OAUTH_TOKEN"). It outranks
//     CLAUDE_CODE_OAUTH_TOKEN, which is the reverse of the order this comment
//     used to imply -- see internal/identity/oauth.go.
//   - ANTHROPIC_API_KEY.
//   - CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR, which Claude Code reports under the
//     SAME source name as ANTHROPIC_API_KEY, which is why identity's
//     DisplacesOAuth puts it on that side too.
//
// The two this check does NOT name are files rather than variables — the
// apiKeyHelper setting and primaryApiKey in ~/.claude.json — and which source
// actually wins is a resolution rather than a list. checkAPIKey answers that,
// and the two are read together. Reporting a variable here that the resolver
// then reports as not winning is deliberate: this check answers "is anything
// set that could defeat a switch", which is the question a user brings.
//
// switch.go warns about CLAUDE_CODE_OAUTH_TOKEN when the user tries, and only
// about that one — the fuller list is `displacingAuth`, which runs AFTER an
// API-key activation. For the other three, this is the only place that says so
// before the switch rather than after it.
//
// The VALUES are never printed. A diagnostic is the output a user pastes into an
// issue, and every one of these is a live credential.
//
// TWO THINGS THIS ROW CANNOT SAY, and the oauth-source row below is where they
// are said. The first: not every source is a variable. A session host injects a
// token at a path compiled into Claude Code, and an Anthropic CLI profile is a
// directory -- neither has a name to unset, and listing them here would tell a
// user to unset something that does not exist. The second: two of the variables
// above INVERT on a session host, because Claude Code skips ANTHROPIC_AUTH_TOKEN
// and the apiKeyHelper when it believes it is inside one. This row over-reports
// there, which is the direction it already errs in on purpose.
//
// Its OK sentence says "no environment VARIABLE", and the word is load-bearing:
// without it the row claims more than it looked at, which is exactly the lie
// the hazard list was widened to stop telling.
func checkEnvironment() check {
	var hazards []string
	for _, name := range []string{
		"CLAUDE_CODE_OAUTH_TOKEN",
		"ANTHROPIC_AUTH_TOKEN",
		"ANTHROPIC_API_KEY",
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
		"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR",
	} {
		if os.Getenv(name) != "" {
			hazards = append(hazards, name)
		}
	}

	// The path overrides are not hazards; they are just worth seeing, because
	// they decide which files everything above was talking about.
	var paths []string
	for _, name := range []string{"CCDAD_HOME", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			paths = append(paths, fmt.Sprintf("%s=%s", name, v))
		}
	}
	suffix := ""
	if len(paths) > 0 {
		suffix = ". Set: " + strings.Join(paths, ", ")
	}

	// A path override pointing into ccdad's OWN sessions or profiles container
	// is not merely worth seeing: it means every answer above described a
	// `ccdad run` session rather than the machine. Listing the variable
	// without saying so — which is what this check did — is how the override
	// stayed invisible in the report that exists to surface it.
	if session, inSession := currentScopedSession(); inSession {
		return check{"environment", levelWarn, fmt.Sprintf(
			"this shell is inside a `ccdad run` session: %s. Every answer above describes that session, "+
				"not the live login, and the commands that would write Claude Code's own state refuse in here%s",
			session.describe(), suffix)}
	}

	if len(hazards) > 0 {
		return check{"environment", levelWarn, fmt.Sprintf(
			"%s set — Claude Code reads these instead of the credentials file, so a switch would have no effect%s",
			joinAnd(hazards), suffix)}
	}
	return check{"environment", levelOK, "no environment variable set that would make a switch a no-op" + suffix}
}

// checkOAuthSource answers which OAuth-shaped credential Claude Code would
// authenticate a session with, and it is a row rather than a sentence inside
// `environment` because it answers a different question: not "is anything set
// that could defeat a switch" but "what actually wins".
//
// WHY THE EARLIER FIVE-SOURCE RULING DOES NOT BAR THIS ROW. That ruling refused
// a list of API-key sources because primaryApiKey is one ccdad WRITES for every
// api-key account, so the list would have warned about ccdad's own steady state
// forever -- TestDoctorDoesNotWarnAboutTheKeyCcdadItselfStores is that
// regression. Nothing here is written by ccdad: not the host's token file, not
// the descriptor, not an Anthropic CLI profile. The one source on this axis
// ccdad does write is the login, and the login is this row's OK answer. The
// next reviewer should not refuse this by analogy.
//
// EVERY BRANCH IS A WARNING, NEVER A FAILURE. levelFail is the only level that
// changes the exit code, and on the machine where the host's token file is
// normal -- a CCR-hosted session -- that file is the correct working state.
// Failing there would hand exit 1 to every hosted session that is working as
// designed.
//
// scopedSessionNote is appended to the three outcomes that describe the
// CREDENTIALS FILE, and to no others. Inside a `ccdad run` session that file is
// the session's own copy; every other source's subject is the machine's however
// the session was started, and the clause would soften a sentence that is true
// of it.
//
// No value is printed on any branch. Every source here names a live credential.
// loadLiveWithSource is the seam the tests fix the store name through. doctor
// runs on the machine it is diagnosing, so a test cannot arrange for a keychain
// item to exist without touching the developer's own.
var loadLiveWithSource = cclink.LoadWithSource

func checkOAuthSource(live cclink.Blob, liveErr error, liveSource cclink.CredentialSource) check {
	// AN UNREADABLE CREDENTIALS FILE STILL ANSWERS MOST OF THIS. The login is
	// the resolver's LAST branch, so every source above it is decidable without
	// the file -- and those are the sources with no variable behind them, which
	// no other row can name. Declining outright used to hand the user off to the
	// environment row, which this file's own comment says cannot cover them.
	login := switcher.LoginOf(live)
	if liveErr != nil {
		login = identity.Login{}
	}
	source, ok := claudeOAuthEnvironment().Resolve(login)
	if !ok {
		return check{"oauth-source", levelWarn,
			"CLAUDE_BG_AUTH_SNAPSHOT_PATH names a token snapshot, and Claude Code consumes it before it " +
				"looks at anything else on this axis — so which credential a session ends up on cannot be " +
				"said from here. Reading the snapshot to find out would mean reading a credential, and the " +
				"first `claude` that starts deletes it. Check the host session that set the variable"}
	}

	switch source {
	case identity.OAuthLogin:
		// The store that ANSWERED, not a constant. Naming the file here on a
		// machine whose login is the keychain item made this row contradict the
		// keychain row two lines above, in the same report.
		return check{"oauth-source", levelOK,
			"Claude Code would authenticate with the login in " + liveSource.String() + scopedSessionNote()}
	case identity.OAuthNone:
		// With the file unreadable this is "nothing ABOVE the login wins", not
		// "nothing wins" -- the last branch is the one that could not be
		// decided, and saying otherwise would assert a negative about a file
		// ccdad could not open.
		if liveErr != nil {
			return check{"oauth-source", levelWarn, fmt.Sprintf(
				"nothing outranks the credentials file, but that file cannot be read (%v) — so whether Claude "+
					"Code has an OAuth credential at all cannot be said from here. The claude-code row is "+
					"where that file's own fault is reported", liveErr)}
		}
		// A login OBJECT with no user:inference scope is not a credential, and
		// the distinction is the whole reason this branch is split. Claude Code
		// takes a login only when its scopes carry that one, so a Console
		// sign-in leaves a well-formed record in the file that authenticates
		// nothing -- and "no OAuth credential resolves" on its own reads as a
		// bug in ccdad rather than as the actionable fact it is.
		if _, hasRecord := live["claudeAiOauth"]; hasRecord && !login.SignsInForInference() {
			return check{"oauth-source", levelWarn,
				"the login store holds a login, but its scopes do not carry user:inference — Claude " +
					"Code takes a login as a credential only when they do, so no OAuth credential resolves " +
					"at all. Sign in again with `ccdad login`" + scopedSessionNote()}
		}
		return check{"oauth-source", levelOK,
			"no OAuth credential resolves, so nothing here would displace a login a switch installs" +
				scopedSessionNote()}
	}

	detail := fmt.Sprintf(
		"Claude Code would take its OAuth credential from %s, and it reads that BEFORE the login in the "+
			"credentials file — so a switch would write a login nothing reads. %s",
		source, source.Remedy())
	if source == identity.OAuthHostTokenFile {
		// The one source with no variable to unset and no way for ccdad to
		// scope around it: `ccdad run` scopes CLAUDE_CONFIG_DIR and
		// CLAUDE_SECURESTORAGE_CONFIG_DIR, and this path reads neither.
		detail += " There is no variable to unset: the path is compiled into Claude Code, and `ccdad run` " +
			"does not scope around it either — that path ignores CLAUDE_CONFIG_DIR and " +
			"CLAUDE_SECURESTORAGE_CONFIG_DIR. Claude Code reports such a session's credential as " +
			source.SourceName()
	}
	return check{"oauth-source", levelWarn, detail}
}

// joinAnd is "a", "a and b", "a, b and c". It exists because checkEnvironment
// now names four variables and `strings.Join(…, " and ")` reads as a chant once
// there are more than two of them.
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// checkAPIKey answers which API key Claude Code would actually use, and whether
// that displaces the login ccdad manages.
//
// checkEnvironment names the variables a user can see. This one runs identity's
// resolver, because two of the five sources it models are not visible from the
// environment at all — the apiKeyHelper setting, and primaryApiKey in
// ~/.claude.json.
//
// Listing those five as hazards would be the wrong check, and this is the
// reason the item that asked for one is not implemented that way. primaryApiKey
// is the single source that does NOT displace an OAuth login — identity's
// DisplacesOAuth reads `BE()`, which turns the gate off for the first five
// sources and not for the stored key — and `ccdad switch` WRITES it for every
// api-key account. A check that warned on it would warn on ccdad's own steady
// state, on the machine of every user who has an api-key account, forever. What
// matters is which source WINS, and whether that win makes a switch a no-op.
//
// It creates nothing, which is what lets it read two files the rest of this
// command does not: LoadGlobalConfig opens read-only and resolves a missing
// file to an empty config, and the helper probe is an os.ReadFile that skips
// what it cannot read.
//
// The key is never printed, for checkEnvironment's reason.
func checkAPIKey(cfg *cclink.GlobalConfig, cfgErr error) check {
	// cfg is nil when the read failed, and claudeAPIKeyEnvironment takes that:
	// the approval list and the stored key are simply absent.
	env := claudeAPIKeyEnvironment(cfg)
	_, source := env.Resolve()

	if cfgErr != nil {
		// Nothing else in this command reads ~/.claude.json, so an unreadable
		// one would otherwise be invisible in the report. It is a warning
		// rather than a failure because ccdad goes on working.
		//
		// The question this check exists for stays ANSWERABLE without the file,
		// and saying "cannot tell" for both halves would be the weaker answer.
		// Every displacing source is visible from the environment and the
		// settings — and the one source the config carries, the stored key, is
		// the one that never displaces. So whether a switch survives is
		// certain; only WHICH source wins is not, because the approval list
		// that decides between them lives in the file that could not be read.
		if env.EnvKey != "" || env.FileDescriptorKey || env.Helper {
			return check{"api-key", levelWarn, fmt.Sprintf(
				"a key from the environment or an apiKeyHelper resolves ahead of the login, so a switch "+
					"would write a file nothing reads. Which of them wins cannot be said, because %v", cfgErr)}
		}
		return check{"api-key", levelWarn, fmt.Sprintf(
			"nothing that displaces a login is set, so a switch takes effect — but %v, so ccdad cannot "+
				"say whether a key is stored in it", cfgErr)}
	}

	// The one state where the answer depends on how Claude Code is started, and
	// it is reported rather than resolved: claudeAPIKeyEnvironment hard-codes
	// Interactive because ccdad is being asked about a session that has not
	// started yet, and picking one of the two answers would be wrong half the
	// time. noteEnvKeyApproval says the same thing at the moment of a switch.
	approval := ""
	if env.EnvKeyNeedsApproval() {
		approval = ". ANTHROPIC_API_KEY is set but is not in Claude Code's approved list, so an interactive " +
			"session refuses it while `claude -p` uses it outright — the answer above is the interactive one"
	}

	switch {
	case source == identity.APIKeyNone:
		return check{"api-key", levelOK,
			"no API key resolves, so Claude Code authenticates with the login in the credentials file" + approval}
	case source.DisplacesOAuth():
		return check{"api-key", levelWarn, fmt.Sprintf(
			"Claude Code would take its key %s, and a key from there makes it ignore the credentials file "+
				"entirely — so a switch would write a login nothing reads%s", "from "+source.String(), approval)}
	}
	// APIKeyManaged: the source ccdad itself writes for an api-key account.
	// Saying so is the point rather than staying silent — it is the state a
	// working api-key account leaves behind, and a user who found it in
	// ~/.claude.json needs to be told it is not the thing breaking their switch.
	return check{"api-key", levelOK, fmt.Sprintf(
		"Claude Code would use %s, which does NOT displace a login — a switch still takes effect%s",
		source.String(), approval)}
}

// checkAccountsFile answers whether ccdad still has an account list, and it is
// the row the three set-differences below hang off.
//
// THE STATE IT EXISTS FOR. store.load treats a missing accounts.toml as an
// empty store and returns no error — which is right for the store, because a
// machine where nothing has been added yet is exactly that, and wrong for a
// diagnostic, because a machine whose document was deleted is exactly that too
// and the two are the same bytes. `ccdad status` shows no accounts, `ccdad
// switch` has nothing to switch to, and not one
// line anywhere says the document is gone.
//
// AND UNTIL THIS ROW EXISTED THE REPORT WAS WORSE THAN SILENT. Every check that
// takes a set difference against the account list — profiles, primary-accounts,
// credential-files — reads that empty list as the truth. So a deleted document
// turns every credential file in the store into an orphan, and the
// credential-files row's remedy is "Delete <paths> once you have looked". A
// user whose accounts.toml was deleted was being told by `ccdad doctor` to
// delete every login they had left. That is why this row comes BEFORE those
// three and gates them, rather than sitting beside them saying the same thing
// in a fourth sentence.
//
// THE GATE IS "IS THE ACCOUNT LIST THE TRUTH", NOT "DOES THE FILE EXIST", and
// the difference is a fresh install. No document and no stored credentials is a
// machine nobody has added an account to: the empty list IS the truth there,
// the three rows below answer correctly from it, and turning them into
// "skipped" on every new machine would cost real signal to describe a state
// that is not a problem. No document WITH credential files beside it is the
// deleted case, and only that one closes the gate.
//
// Credential files are the only evidence read, deliberately. Profile
// directories would be a second signal, and taking it would mean respelling
// checkProfiles' entry rules here — the .lock suffix, the IsDir test — which is
// the duplicate store.go's own accessors were introduced to remove. One
// evidence, named, and store.OrphanCredentialsAt is the same reader
// credential-files uses, so the two rows cannot come to different conclusions
// about the same directory.
//
// It creates nothing: a stat, and the two readers that take a root and open
// nothing.
func checkAccountsFile(root string, usable bool) (check, bool) {
	if !usable {
		return check{"accounts-file", levelSkipped, "there is no store to check"}, false
	}
	path := store.AccountsFileAt(root)

	_, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		accounts, err := store.AccountsAt(root)
		if err != nil {
			// Named once here rather than three times below. The three rows
			// that would each have reported this as their own failure now skip
			// and point at this one, so the report carries one cause instead of
			// three symptoms.
			return check{"accounts-file", levelFail, fmt.Sprintf(
				"%s is there but cannot be read, so ccdad has no account list at all: %v", path, err)}, false
		}
		return check{"accounts-file", levelOK, fmt.Sprintf(
			"%s names %d account%s", path, len(accounts), plural(len(accounts), "", "s"))}, true
	case !errors.Is(statErr, os.ErrNotExist):
		return check{"accounts-file", levelFail, fmt.Sprintf("%s cannot be read: %v", path, statErr)}, false
	}

	stranded, err := store.OrphanCredentialsAt(root)
	if err != nil {
		// The document is missing AND the evidence that would say whether that
		// matters cannot be gathered. Answering "nothing has been added here"
		// from a read that failed is the reassuring lie this row exists to
		// remove, which is the refusal checkProfiles and checkPrimary already
		// make one directory over.
		return check{"accounts-file", levelFail, fmt.Sprintf(
			"%s does not exist, and %s cannot be read to tell whether that is a new machine or a deleted "+
				"account list: %v", path, store.CredentialsDirAt(root), err)}, false
	}
	if len(stranded) == 0 {
		return check{"accounts-file", levelOK, fmt.Sprintf(
			"%s does not exist yet, and no credentials are stored beside it — nothing has been added on this machine",
			path)}, true
	}

	return check{"accounts-file", levelFail, fmt.Sprintf(
		"%s is GONE, and %d credential file%s still %s beside it (%s). ccdad keeps its whole account list in "+
			"that one document, so every account on this machine is now invisible: `ccdad status` has no accounts and "+
			"`ccdad switch` has nothing to switch to. Do NOT delete those files — each is a login you can "+
			"still recover. Put the document back from a backup, `ccdad import` an export, or run `ccdad add` "+
			"once per account",
		path, len(stranded), plural(len(stranded), "", "s"), plural(len(stranded), "sits", "sit"),
		strings.Join(stranded, ", "))}, false
}

// noAccountList is what the three set-difference rows say when
// checkAccountsFile has closed the gate. They cannot answer without a
// trustworthy account list, and answering anyway is how a deleted document
// became advice to delete a login.
const noAccountList = "the account list cannot be trusted, so nothing can be matched against it — see the accounts-file row"

// checkProfiles reports a `ccdad run --full-profile` profile whose account is
// gone.
//
// <store>/profiles/<uuid> is not scratch space: `ccdad run --full-profile`
// writes an API-key account's primaryApiKey into that profile's own Claude Code
// config, so an orphan is a credential nothing else on this machine would ever
// mention again. `ccdad remove` deletes the profile and `uninstall` takes the
// whole store, so the path that CREATES orphans going forward is closed. What
// is left is a store restored from an export, an account removed by an older
// ccdad, or a uuid that changed.
//
// It is a SET DIFFERENCE and not a count, and that is the whole design. A
// profile whose account still exists is legitimate persistent state and is not
// reported at all; a report that counted directories would tell every user of
// --full-profile that their working setup is a problem.
//
// The account list comes from store.AccountsAt, never store.Open: Open does an
// MkdirAll and this file's rule is that the probe must not create what it
// probes. TestDoctorCreatesNothing is the test that keeps it that way.
func checkProfiles(root string, usable, accountsUsable bool) check {
	if !usable {
		return check{"profiles", levelSkipped, "there is no store to check"}
	}
	if !accountsUsable {
		return check{"profiles", levelSkipped, noAccountList}
	}
	container := filepath.Join(root, ProfilesDirName)
	entries, err := os.ReadDir(container)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The container is created by `run`, never by this probe.
			return check{"profiles", levelOK, "no --full-profile profiles are stored here"}
		}
		return check{"profiles", levelFail, err.Error()}
	}

	accounts, err := store.AccountsAt(root)
	if err != nil {
		// Without the account list there is no set to difference against, and
		// answering "no orphans" from a failed read would be the lie this check
		// exists to remove.
		return check{"profiles", levelFail, fmt.Sprintf(
			"the profiles under %s cannot be matched against the account list: %v", container, err)}
	}
	known := make(map[string]struct{}, len(accounts))
	for _, a := range accounts {
		known[a.UUID] = struct{}{}
	}

	var orphans []string
	for _, e := range entries {
		// checkSessions's rule, for the same reason: Claude Code's legacy
		// refresh lock is a directory named after its neighbour with ".lock"
		// appended, and counting one as a profile would report an orphan for
		// every profile in daily use.
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		if _, ok := known[e.Name()]; ok {
			continue
		}
		orphans = append(orphans, e.Name())
	}
	if len(orphans) == 0 {
		return check{"profiles", levelOK, "every profile here belongs to an account this store still has"}
	}
	sort.Strings(orphans)

	// A warning, not a failure: nothing is broken by an orphan sitting there.
	// What it costs is a stored API key with no account left to name it, which
	// is worth a sentence and is not something to delete on ccdad's initiative
	// — whether doctor should ever repair is undecided, and this file reports.
	return check{"profiles", levelWarn, fmt.Sprintf(
		"%d profile director%s under %s belong%s to no account this store has (%s). Each may hold that "+
			"account's API key in its own Claude Code config; `ccdad remove` no longer leaves these, so they "+
			"are from an older ccdad, a restored export, or a uuid that changed",
		len(orphans), plural(len(orphans), "y", "ies"), container,
		plural(len(orphans), "s", ""), strings.Join(orphans, ", "))}
}

// checkCredentialFiles reports a stored credential file that no account names.
//
// It is the mirror of checkProfiles, one directory over and with more at stake:
// a leaked profile MAY hold an API key, and a leaked credential file always
// holds a live refresh token at 0600. No command a user would reach for can
// find one. `ccdad status`, `ccdad remove` and the account rows in this very
// report all read accounts.toml, and an orphan is by definition a uuid that
// document does not carry — so it stays invisible to every one of them,
// indefinitely. The permissions row above is the sole exception and not a
// substitute: it globs the same directory, so it names an orphan only when that
// orphan's mode is ALSO wrong, and says nothing about the ones at 0600.
//
// It is NOT a report about the past, and the tempting summary that it is would
// be wrong. adfdbb5 made the store transaction reverse the credential files a
// REFUSED batch wrote, which closed the error path and only that one. The
// reversal now runs from a defer rather than from that one return, and the
// transaction holds SIGINT for the span of its own write, so the two ways a
// user actually reached this state -- a panic, and Ctrl-C partway through a
// multi-account `ccdad import` -- are closed too.
//
// What is left is not closable at any price. SIGKILL and a power cut take the
// process out between any two instructions with the credential file down and
// the document unwritten, and internal/store/interrupt.go names the one signal
// deliberately left untrapped beside them. Add the two states no reversal can
// reach backwards into -- a store written by a build older than all of this,
// and a reversal whose own os.Remove was itself refused -- and this row has
// permanent work. Every one of those is silent without it.
//
// A warning rather than a failure, and a report rather than a repair — this
// file's second rule, and the place it is most tempting to break. Deleting a
// file the store cannot explain is not ccdad's call: a half-restored store is
// exactly when a user wants to look before anything else does, and the token in
// it may be the only copy of a login they still want.
//
// It creates no STORE: store.OrphanCredentialsAt still reads the directory and
// the document without Open's MkdirAll, so a root that is not there yields no
// orphans rather than being brought into existence. It does now take the store
// lock — mutate's own re-read-inside-the-lock ordering is what this row used
// to race, so a probe caught between Add writing a credential file and the
// document that names it could call the file an orphan — which can create the
// zero-byte lock marker on a store old enough to have gone without one, and
// can hold this check for up to the lock's own timeout behind a live
// transaction. Both are store.OrphanCredentialsAt's comment to make in full.
func checkCredentialFiles(root string, usable, accountsUsable bool) check {
	if !usable {
		return check{"credential-files", levelSkipped, "there is no store to check"}
	}
	if !accountsUsable {
		return check{"credential-files", levelSkipped, noAccountList}
	}
	orphans, err := store.OrphanCredentialsAt(root)
	if err != nil {
		// Answering "nothing is orphaned" from a failed read would be the
		// reassuring lie this row exists to remove — checkProfiles and
		// checkPrimary refuse the same way for the same reason.
		// store says which of the two reads failed; naming only the matching
		// here would report a directory that cannot be opened as a mismatch.
		return check{"credential-files", levelFail, fmt.Sprintf(
			"the stored credential files cannot be checked against the account list: %v", err)}
	}
	if len(orphans) == 0 {
		return check{"credential-files", levelOK,
			"every stored credential file belongs to an account this store still has"}
	}
	return check{"credential-files", levelWarn, fmt.Sprintf(
		"%d credential file%s belong%s to no account this store has (%s). Each holds a live refresh token "+
			"at 0600, and nothing on this machine can find it: `ccdad status` and `ccdad remove` read "+
			"accounts.toml, which is exactly what does not name %s. Delete %s once you have looked; doctor "+
			"reports and never removes",
		len(orphans), plural(len(orphans), "", "s"), plural(len(orphans), "s", ""),
		strings.Join(orphans, ", "), plural(len(orphans), "it", "them"),
		plural(len(orphans), "it", "them"))}
}

// checkPrimary names the accounts marked primary, because that flag is the one
// per-account setting that switches the credit ceiling OFF.
//
// It is levelOK either way, which matches how the config row reports an armed
// max_auto_spend: armed spending is something a human typed, not a fault, and a
// warning on a deliberate setting is a warning readers learn to skip past. What
// this row is for is that the flag is nearly invisible everywhere else — one
// parenthesis on a listing and a key in --json — so a seat armed months ago is
// otherwise found only by opening accounts.toml.
//
// It creates nothing: store.AccountsAt performs the read Open would, without
// Open's MkdirAll.
func checkPrimary(root string, usable, accountsUsable bool) check {
	if !usable {
		return check{"primary-accounts", levelSkipped, "there is no store to check"}
	}
	if !accountsUsable {
		return check{"primary-accounts", levelSkipped, noAccountList}
	}
	accounts, err := store.AccountsAt(root)
	if err != nil {
		// Answering "none are primary" out of a failed read would be exactly
		// the reassuring lie this row exists to remove.
		return check{"primary-accounts", levelFail, fmt.Sprintf("the account list cannot be read: %v", err)}
	}
	var marked []string
	for _, a := range accounts {
		if a.Primary {
			marked = append(marked, a.Label())
		}
	}
	if len(marked) == 0 {
		return check{"primary-accounts", levelOK,
			"no account is marked primary, so every credit account is behind the credit gate"}
	}
	sort.Strings(marked)
	return check{"primary-accounts", levelOK, fmt.Sprintf(
		"%d account%s marked primary and ranked beside the subscriptions (%s); "+
			"credit.max_auto_spend does not hold %s back",
		len(marked), plural(len(marked), " is", "s are"), strings.Join(marked, ", "),
		plural(len(marked), "it", "them"))}
}

// checkPath answers whether typing `ccdad` in a new shell finds this binary.
//
// It reads TWO facts, because in the ordinary case they disagree: `ccdad
// setup-path` writes a block into a startup file, and the PATH of the process
// running doctor right now does not show it until a new shell has read that
// file. Which of the two it is decides whether the advice is "open a new shell"
// or "run a command", so reporting either alone would send half of the users
// who need it to the wrong remedy.
//
// It is never a failure, and that is a judgement about doctor's taxonomy rather
// than a convenience: ccdad invoked by its absolute path works exactly as well,
// so this does not meet this file's bar for levelFail ("something that will
// bite"). It is also what lets the shared doctor and `--json` contract
// fixtures stay as they are — their binary lives in a t.TempDir() that is by
// construction not on PATH, and a warning moves neither the exit code nor `ok`.
//
// It creates nothing: os.Executable, the live PATH, and a read of the startup
// files setup-path would have written.
func checkPath() check {
	exe, err := executablePath()
	if err != nil {
		return check{"path", levelWarn, fmt.Sprintf(
			"ccdad cannot locate its own binary, so it cannot tell whether it is on PATH: %v", err)}
	}
	dir := filepath.Dir(exe)

	// onPathList, not a hand-rolled split: it is setup-path's own decider, it
	// matches whole components rather than substrings, and it is already tested
	// against both platforms' rules.
	if onPathList(os.Getenv("PATH"), dir, livePathRules) {
		return check{"path", levelOK, fmt.Sprintf("%s is on PATH", dir)}
	}

	places, perr := pathRegistrations(dir)
	if perr != nil {
		// A scan that could not finish is not evidence that nothing is
		// registered — the same rule uninstall applies to the same reader.
		return check{"path", levelWarn, fmt.Sprintf(
			"%s is not on this shell's PATH, and the startup files that would say otherwise could not be "+
				"read: %v", dir, perr)}
	}
	if len(places) > 0 {
		return check{"path", levelWarn, fmt.Sprintf(
			"%s is not on THIS shell's PATH, but ccdad has registered it in %s — open a new shell, or "+
				"source that file",
			dir, joinAnd(places))}
	}
	return check{"path", levelWarn, fmt.Sprintf(
		"%s is not on PATH, so `ccdad` only works by its full path. `%s setup-path` adds it", dir, exe)}
}

// codexAccountsAt is the store's Codex accounts, read WITHOUT opening the
// store.
//
// store.Open creates the directory it opens, and this file's first rule is that
// the probe must not create what it probes. AccountsAt reads the document and
// nothing else, which is what checkPrimary and checkProfiles already use it for.
func codexAccountsAt(root string) ([]store.Account, error) {
	accounts, err := store.AccountsAt(root)
	if err != nil {
		return nil, err
	}
	var out []store.Account
	for _, a := range accounts {
		if a.Provider == provider.Codex {
			out = append(out, a)
		}
	}
	return out, nil
}

// codexBlobAt reads one account's stored credential blob directly, for the same
// reason codexAccountsAt exists: opening the store would create it.
func codexBlobAt(root, uuid string) (cclink.Blob, bool) {
	raw, err := os.ReadFile(filepath.Join(store.CredentialsDirAt(root), uuid+".json"))
	if err != nil {
		return nil, false
	}
	var b cclink.Blob
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, false
	}
	return b, true
}

// checkCodexRelogin names the Codex accounts whose refresh grant the endpoint
// has rejected.
//
// It is a WARNING and not a failure, on this file's own taxonomy: the machine
// works, every other account rotates, and one account is out of service until a
// person runs a command. What makes the row worth having is that nothing else
// says so -- the account looks ordinary in `ccdad status`, and the first symptom
// is a codex session answering a branded 401.
//
// The mark is compared against the token the account CURRENTLY holds, which is
// what makes a re-login self-clearing: `ccdad codex add` stores a new token and
// the mark stops matching, with nothing having had to remember to clear it. A
// row that reported the bare mark would send a user to re-run a command they
// have already run.
func checkCodexRelogin(root string, usable, accountsUsable bool) check {
	if !usable {
		return check{"codex-relogin", levelSkipped, "there is no store to check"}
	}
	if !accountsUsable {
		return check{"codex-relogin", levelSkipped, noAccountList}
	}
	accounts, err := codexAccountsAt(root)
	if err != nil {
		// Answering "every grant is live" out of a failed read would be exactly
		// the reassuring lie this row exists to remove.
		return check{"codex-relogin", levelFail, fmt.Sprintf("the account list cannot be read: %v", err)}
	}
	if len(accounts) == 0 {
		return check{"codex-relogin", levelSkipped, "there are no codex accounts"}
	}
	var dead []string
	for _, a := range accounts {
		if a.CodexReloginFor == "" {
			continue
		}
		blob, ok := codexBlobAt(root, a.UUID)
		if !ok {
			continue
		}
		if codexauth.NeedsRelogin(a, blob) {
			dead = append(dead, a.Label())
		}
	}
	if len(dead) == 0 {
		return check{"codex-relogin", levelOK, fmt.Sprintf(
			"%d codex account%s, and every stored grant is one the endpoint has not rejected",
			len(accounts), plural(len(accounts), "", "s"))}
	}
	sort.Strings(dead)
	return check{"codex-relogin", levelWarn, fmt.Sprintf(
		"the refresh grant behind %s has been rejected, so ccdad cannot serve codex from %s until "+
			"somebody logs in again: run `ccdad codex add`. Nothing else reports this -- the account "+
			"looks ordinary in `ccdad status`, and the first symptom is a codex session answering an error",
		joinAnd(dead), plural(len(dead), "it", "them"))}
}

// checkCodexProxy answers whether a codex session launched right now would
// reach ccdad at all.
//
// The port is read from the published status document because that is where a
// process fact lives; there is no file of its own for it. A daemon that is
// running and has published no port is the one arm that is SKIPPED rather than
// warned about, and the arm below says why.
//
// The fallback is the one arm that asks the reader to DO something. A proxy
// that came up on a different port than the one it resolved leaves every codex
// session started before it talking to a port nothing is listening on, and
// codex's own symptom for that is an endless "Reconnecting" with no error text
// at all -- so a user who is not told will read it as a network problem.
func checkCodexProxy(root string, usable, accountsUsable bool, report daemon.Report) check {
	if !usable {
		return check{"codex-proxy", levelSkipped, "there is no store to check"}
	}
	if !accountsUsable {
		return check{"codex-proxy", levelSkipped, noAccountList}
	}
	accounts, err := codexAccountsAt(root)
	if err != nil {
		return check{"codex-proxy", levelFail, fmt.Sprintf("the account list cannot be read: %v", err)}
	}
	if len(accounts) == 0 {
		return check{"codex-proxy", levelSkipped, "there are no codex accounts"}
	}
	if report.State != daemon.DaemonRunning {
		return check{"codex-proxy", levelWarn,
			"no daemon is running, so nothing is serving codex: a codex session launched through ccdad " +
				"would wait for a proxy that is not there. `ccdad daemon start` starts one"}
	}
	port := report.Status.CodexProxyPort
	if port == 0 {
		// SKIPPED and not warn, and the difference is the whole judgment in this
		// arm. A running daemon that publishes no port is either one whose
		// listener did not come up or one from a build that has no listener to
		// publish, and the status document alone cannot tell those apart. While
		// the proxy is not in the build the second is EVERY machine that has a
		// codex account, so warning here would paint a row yellow for a whole
		// release on evidence of nothing -- and a row that is always yellow is a
		// row nobody reads by the time it means something. The state worth a
		// warning is a port that is published and unreachable, and answering that
		// takes a request this check does not make.
		return check{"codex-proxy", levelSkipped, "the running daemon has published no codex proxy port"}
	}
	if report.Status.CodexProxyFellBack {
		return check{"codex-proxy", levelWarn, fmt.Sprintf(
			"the codex proxy is on port %d, which is not the port it resolved -- something else held that "+
				"one. Any codex session started before this daemon is talking to a port nothing is "+
				"listening on and shows an endless reconnect with no error: quit and relaunch those "+
				"sessions", port)}
	}
	detail := fmt.Sprintf("the codex proxy is listening on 127.0.0.1:%d", port)
	if n := report.Status.CodexUnroutedLaunches; n > 0 {
		return check{"codex-proxy", levelWarn, fmt.Sprintf(
			"%s, but %d codex session%s had to be launched without it and %s spending whatever "+
				"~/.codex holds, which ccdad neither chose nor can see",
			detail, n, plural(n, "", "s"), plural(n, "is", "are"))}
	}
	return check{"codex-proxy", levelOK, detail}
}
